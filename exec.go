package gaiadesk

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"
)

// ExecRequest is one command to run on a desk (the API's ExecSpec).
type ExecRequest struct {
	// Command is ONE command line, given to the desk's shell verbatim.
	Command string
	// Argv is an argument vector instead: each entry quoted for the shell,
	// so the program receives exactly it (ShellNone: run directly).
	Argv []string
	// Shell runs the command (default: the desk's own).
	Shell Shell
	// Cwd is the directory it runs in (relative: from the desk user's home,
	// or a confined token's folder).
	Cwd string
	// Env are environment variables for the command (never logged by the
	// server or the desk: at most their names).
	Env map[string]string
	// Stdin is read whole and sent up front, as text (UTF-8). The API takes
	// no stdin while the command runs.
	Stdin io.Reader
	// Timeout stops the command after this long (rounded up to seconds;
	// 0: the API's limit, 15 minutes).
	Timeout time.Duration
	// Admin runs it as administrator (root / SYSTEM): it needs a desk token
	// with the `admin` scope and the desk owner's Admin access; else the
	// result is exit 254 with a `refused` error whose reason is one of
	// ReasonAdminScopeMissing, ReasonAdminNotEnabled, ReasonAdminDenied,
	// ReasonAdminUnavailable.
	Admin bool
	// Check makes a non-zero exit (or a timeout) a *CommandError.
	Check bool
}

// spec is the ExecSpec the API takes.
func (r ExecRequest) spec() (map[string]any, error) {
	spec := map[string]any{}
	switch {
	case r.Command != "" && r.Argv != nil:
		return nil, usageError("give Command or Argv, not both")
	case r.Argv != nil:
		if len(r.Argv) == 0 || len(r.Argv) == 1 && r.Argv[0] == "" {
			return nil, usageError("exec needs a command")
		}
		spec["argv"] = r.Argv
	default:
		if trimmed := r.Command; trimmed == "" || isBlank(trimmed) {
			return nil, usageError("exec needs a command")
		}
		spec["command"] = r.Command
	}
	if r.Shell != "" {
		if !execShells[r.Shell] {
			return nil, usageError("shell is one of default, none, sh, bash, zsh, cmd, pwsh, powershell (not %q)", r.Shell)
		}
		spec["shell"] = wireShell(r.Shell)
	}
	if r.Env != nil {
		if err := checkEnv(r.Env); err != nil {
			return nil, err
		}
		spec["env"] = r.Env
	}
	if r.Cwd != "" {
		if err := checkCwd(r.Cwd); err != nil {
			return nil, err
		}
		spec["cwd"] = r.Cwd
	}
	if r.Timeout < 0 {
		return nil, usageError("timeout must be >= 0")
	}
	if r.Timeout > 0 {
		spec["timeout_secs"] = wholeSeconds(r.Timeout)
	}
	if r.Stdin != nil {
		b, err := io.ReadAll(r.Stdin)
		if err != nil {
			e := newError("", "local", "cannot read stdin: "+err.Error())
			e.Kind = KindLocal
			return nil, e
		}
		if !utf8.Valid(b) {
			return nil, usageError("stdin must be UTF-8 text over the API (it takes stdin as text)")
		}
		spec["stdin"] = string(b)
	}
	if r.Admin {
		spec["admin"] = true
	}
	return spec, nil
}

func isBlank(s string) bool {
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}
	return true
}

// neverRan: the result says why in Error, it has no code of its own, and it
// did not merely run out of time.
func neverRan(r *ExecResult) bool {
	return r.RemoteCode == nil && !r.TimedOut && r.Error != nil
}

// Exec runs one command on the desk: `POST /desks/{id}/exec`. A command
// that ran is a result whatever its exit code; one the desk refused to
// start (or that never ran) is its typed error. With Check, a non-zero
// exit is a *CommandError.
func (c *Client) Exec(ctx context.Context, desk string, req ExecRequest, opts ...CallOption) (*ExecResult, error) {
	d, err := checkDesk(desk)
	if err != nil {
		return nil, err
	}
	spec, err := req.spec()
	if err != nil {
		return nil, err
	}
	call, err := callOpts(opts)
	if err != nil {
		return nil, err
	}
	r := &request{method: http.MethodPost, path: deskPath(d) + "/exec", json: spec, call: call,
		e2e: &e2eOp{desk: d, op: "exec", request: map[string]any{"op": "exec", "spec": spec}}}
	res, err := get[ExecResult](ctx, c, r)
	if err != nil {
		return nil, err
	}
	if neverRan(res) {
		e := newError(Class(res.Error.Kind), res.Error.Reason, firstNonEmpty(res.Error.Message, "the command did not run"))
		e.Desk, e.Op, e.ExitCode = firstNonEmpty(res.Error.Desk, res.Desk), r.op(), res.Exit
		return nil, e
	}
	if req.Check && res.Exit != 0 {
		why := fmt.Sprintf("exited %d", res.Exit)
		if res.TimedOut {
			why = "timed out"
		}
		e := newError(ClassFailed, "", fmt.Sprintf("command on desk %s %s", res.Desk, why))
		e.Desk, e.Op, e.ExitCode = res.Desk, r.op(), res.Exit
		return res, &CommandError{Err: e, Result: res}
	}
	return res, nil
}

// ExecStream runs one command on the desk and streams its output:
// `POST /desks/{id}/exec?stream=1`. The error is the call's failure before
// the stream started (refused, unreachable, …); a failure after is the
// stream's Exit. Close the stream (or read it to the end) when done.
func (c *Client) ExecStream(ctx context.Context, desk string, req ExecRequest, opts ...CallOption) (*Stream, error) {
	d, err := checkDesk(desk)
	if err != nil {
		return nil, err
	}
	if req.Check {
		return nil, usageError("Check applies to Exec; a stream's Exit has the exit code")
	}
	spec, err := req.spec()
	if err != nil {
		return nil, err
	}
	call, err := callOpts(opts)
	if err != nil {
		return nil, err
	}
	r := &request{method: http.MethodPost, path: deskPath(d) + "/exec", json: spec, call: call,
		query: url.Values{"stream": {"1"}}, accept: "text/event-stream",
		e2e: &e2eOp{desk: d, op: "exec", request: map[string]any{"op": "exec", "spec": spec, "stream": true}}}
	return c.stream(ctx, r, "exec", "")
}

// stream starts an SSE request.
func (c *Client) stream(ctx context.Context, r *request, kind, job string) (*Stream, error) {
	sctx, cancel := context.WithCancel(ctx)
	res, seal, err := c.do(sctx, r)
	if err != nil {
		cancel()
		return nil, err
	}
	src := eventSource(newSSEReader(res.Body))
	if seal != nil {
		src = newUnsealer(src.(*sseReader), seal, kind, r.op())
	}
	return newStream(sctx, cancel, r.op(), kind, job, src, res.Body), nil
}
