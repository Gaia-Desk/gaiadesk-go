package gaiadesk

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// JobRequest starts a background job (the API's JobSpec, as
// `gaiadesk-cli run --detach`).
type JobRequest struct {
	// Name names the job: letters, digits, . _ - (not starting with -).
	Name string
	// Command is the command line (or Argv its pieces).
	Command string
	Argv    []string
	// Cwd is the directory the job starts in on the desk.
	Cwd string
	// Shell runs the command (default: `sh -c` on macOS/Linux, `cmd /c` on
	// Windows; never ShellNone or ShellDefault).
	Shell Shell
	// Env are environment variables for the job (never logged by the desk).
	Env map[string]string
	// Priority is "low", "normal" or "high".
	Priority string
	// CPUPercent caps its share of the whole machine, 1 to 100 (0: none).
	CPUPercent int
	// MemMB caps its memory in megabytes (0: none).
	MemMB uint64
	// KeepAwake keeps the desk awake while it runs (nil: the desk's default).
	KeepAwake *bool
}

func (r JobRequest) spec() (map[string]any, error) {
	if err := checkJobName(r.Name); err != nil {
		return nil, err
	}
	var command []string
	switch {
	case r.Command != "" && r.Argv != nil:
		return nil, usageError("give Command or Argv, not both")
	case r.Argv != nil:
		command = r.Argv
	default:
		command = []string{r.Command}
	}
	if len(command) == 0 || len(command) == 1 && isBlank(command[0]) {
		return nil, usageError("a job needs a command")
	}
	limits := map[string]any{}
	if r.Priority != "" {
		if r.Priority != "low" && r.Priority != "normal" && r.Priority != "high" {
			return nil, usageError("priority is low, normal or high")
		}
		limits["priority"] = r.Priority
	}
	if r.CPUPercent != 0 {
		if r.CPUPercent < 1 || r.CPUPercent > 100 {
			return nil, usageError("cpu is a share of the whole machine, 1 to 100")
		}
		limits["cpu_percent"] = r.CPUPercent
	}
	if r.MemMB != 0 {
		limits["mem_mb"] = r.MemMB
	}
	if r.KeepAwake != nil {
		limits["keep_awake"] = *r.KeepAwake
	}
	spec := map[string]any{"name": r.Name, "command": command, "limits": limits}
	if r.Cwd != "" {
		if err := checkCwd(r.Cwd); err != nil {
			return nil, err
		}
		spec["cwd"] = r.Cwd
	}
	if r.Shell != "" {
		if !jobShells[r.Shell] {
			return nil, usageError("a job's shell is one of sh, bash, zsh, cmd, pwsh, powershell (not %q)", r.Shell)
		}
		spec["shell"] = wireShell(r.Shell)
	}
	if r.Env != nil {
		if err := checkEnv(r.Env); err != nil {
			return nil, err
		}
		spec["env"] = r.Env
	}
	return spec, nil
}

// prep checks a desk and the call options.
func prep(desk string, opts []CallOption) (string, callConfig, error) {
	d, err := checkDesk(desk)
	if err != nil {
		return "", callConfig{}, err
	}
	call, err := callOpts(opts)
	return d, call, err
}

func jobPath(desk, name string) string { return deskPath(desk) + "/jobs/" + url.PathEscape(name) }

// StartJob starts a background job: `POST /desks/{id}/jobs`. It runs under
// the desk's GaiaDesk and outlives the call; its end is the `job.finished`
// webhook (or WaitJob).
func (c *Client) StartJob(ctx context.Context, desk string, job JobRequest, opts ...CallOption) (*Job, error) {
	d, call, err := prep(desk, opts)
	if err != nil {
		return nil, err
	}
	spec, err := job.spec()
	if err != nil {
		return nil, err
	}
	return get[Job](ctx, c, &request{method: http.MethodPost, path: deskPath(d) + "/jobs", json: spec, call: call,
		e2e: &e2eOp{desk: d, op: "job_start", request: map[string]any{"op": "job_start", "spec": spec}}})
}

// Jobs lists the desk's background jobs: `GET /desks/{id}/jobs`.
func (c *Client) Jobs(ctx context.Context, desk string, opts ...CallOption) ([]Job, error) {
	d, call, err := prep(desk, opts)
	if err != nil {
		return nil, err
	}
	l, err := get[struct {
		Jobs *[]Job `json:"jobs"`
	}](ctx, c, &request{method: http.MethodGet, path: deskPath(d) + "/jobs", call: call,
		e2e: &e2eOp{desk: d, op: "job_list", request: map[string]any{"op": "job_list"}}})
	if err != nil {
		return nil, err
	}
	if l.Jobs == nil {
		return nil, protocolErr("GET "+deskPath(d)+"/jobs", "the GaiaDesk API listed no jobs")
	}
	return *l.Jobs, nil
}

func protocolErr(op, msg string) *Error {
	e := newError(ClassProtocol, "", msg)
	e.Op = op
	return e
}

// KillJob stops a job and everything it started:
// `DELETE /desks/{id}/jobs/{name}`.
func (c *Client) KillJob(ctx context.Context, desk, name string, opts ...CallOption) (*Job, error) {
	d, call, err := prep(desk, opts)
	if err != nil {
		return nil, err
	}
	if err := checkJobName(name); err != nil {
		return nil, err
	}
	return get[Job](ctx, c, &request{method: http.MethodDelete, path: jobPath(d, name), call: call,
		e2e: &e2eOp{desk: d, op: "job_kill", request: map[string]any{"op": "job_kill", "name": name}}})
}

// LogsOptions says how much of a job's output to read.
type LogsOptions struct {
	// Tail is the last this many bytes (0: all the desk keeps).
	Tail int
}

func logsRequest(desk, name string, o *LogsOptions, follow bool, call callConfig) (*request, error) {
	if err := checkJobName(name); err != nil {
		return nil, err
	}
	q := url.Values{}
	inner := map[string]any{"op": "job_logs", "name": name}
	if o != nil && o.Tail != 0 {
		if o.Tail < 0 {
			return nil, usageError("tail is a number of bytes")
		}
		q.Set("tail", strconv.Itoa(o.Tail))
		inner["tail"] = o.Tail
	}
	r := &request{method: http.MethodGet, path: jobPath(desk, name) + "/logs", query: q, call: call,
		e2e: &e2eOp{desk: desk, op: "job_logs", request: inner}}
	if follow {
		q.Set("follow", "1")
		inner["follow"] = true
		r.accept = "text/event-stream"
	}
	return r, nil
}

// JobLogs reads a job and the end of its output:
// `GET /desks/{id}/jobs/{name}/logs`.
func (c *Client) JobLogs(ctx context.Context, desk, name string, o *LogsOptions, opts ...CallOption) (*JobLogs, error) {
	d, call, err := prep(desk, opts)
	if err != nil {
		return nil, err
	}
	r, err := logsRequest(d, name, o, false, call)
	if err != nil {
		return nil, err
	}
	return get[JobLogs](ctx, c, r)
}

// FollowJobLogs streams a job's output as it is written, until the job
// ends: `GET /desks/{id}/jobs/{name}/logs?follow=1`. Closing the stream
// stops following; the job goes on.
func (c *Client) FollowJobLogs(ctx context.Context, desk, name string, o *LogsOptions, opts ...CallOption) (*Stream, error) {
	d, call, err := prep(desk, opts)
	if err != nil {
		return nil, err
	}
	r, err := logsRequest(d, name, o, true, call)
	if err != nil {
		return nil, err
	}
	return c.stream(ctx, r, "logs", name)
}

// WaitForever is WaitJob's timeout for "until the job ends" (or the
// context is done).
const WaitForever time.Duration = -1

// WaitJob waits until a job is no longer running, or timeout passes
// (0: answer at once; WaitForever: until it ends): the job as it ended, or
// (TimedOut) as it stands. The API holds one wait at most WaitMax, so a
// longer one waits again. A held answer that failed after its 200 began is
// its typed error, as any other failure.
func (c *Client) WaitJob(ctx context.Context, desk, name string, timeout time.Duration, opts ...CallOption) (*JobWaitResult, error) {
	d, call, err := prep(desk, opts)
	if err != nil {
		return nil, err
	}
	if err := checkJobName(name); err != nil {
		return nil, err
	}
	if timeout < 0 && timeout != WaitForever {
		return nil, usageError("timeout is >= 0, or WaitForever")
	}
	path := jobPath(d, name) + "/wait"
	op := "GET " + path
	started := time.Now()
	for {
		left := WaitMax.Seconds()
		if timeout >= 0 {
			left = math.Max(0, (timeout - time.Since(started)).Seconds())
		}
		secs := int64(math.Min(WaitMax.Seconds(), math.Ceil(left)))
		r := &request{method: http.MethodGet, path: path, query: url.Values{"timeout": {strconv.FormatInt(secs, 10)}}, call: call,
			e2e: &e2eOp{desk: d, op: "job_wait", request: map[string]any{"op": "job_wait", "name": name, "timeout_ms": secs * 1000}}}
		raw, err := c.call(ctx, r)
		if err != nil {
			return nil, err
		}
		// A held wait that failed after its 200 began: the envelope, in the body.
		if env, ok := errorEnvelope(raw); ok {
			e := fromEnvelope(env, op, 200, raw)
			if env.Status == 0 {
				e.Status = 0
			}
			return nil, e
		}
		var res struct {
			Job      *json.RawMessage `json:"job"`
			TimedOut *bool            `json:"timed_out"`
		}
		if json.Unmarshal(raw, &res) != nil || res.Job == nil || res.TimedOut == nil {
			return nil, protocolErr(op, "the GaiaDesk API answered a wait without a job")
		}
		out, err := decode[JobWaitResult](raw, op)
		if err != nil {
			return nil, err
		}
		over := timeout >= 0 && time.Since(started) >= timeout
		if !out.TimedOut || over || timeout == 0 {
			return out, nil
		}
	}
}
