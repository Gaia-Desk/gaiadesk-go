package gaiadesk

// The hosted API transport on its own: choosing it, what it sends (headers,
// bodies, query), error envelopes, retries, streams and their ends, held
// waits, files, and the operations a transport does not serve.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Gaia-Desk/gaiadesk-go/internal/mockapi"
)

const (
	offlineDesk = "987654321"
	usageDesk   = "876543210"
)

func plainMock(t *testing.T) *mockapi.Server {
	s := mockapi.New(map[string]*mockapi.Desk{
		okDesk:      {},
		offlineDesk: {Unreachable: true, Offline: true},
		usageDesk:   {Usage: true},
		asleepDesk:  {Offline: true, Wakeable: true},
	})
	t.Cleanup(s.Close)
	return s
}

func body(t *testing.T, r mockapi.Recorded) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("body %q: %v", r.Body, err)
	}
	return m
}

func jsonEq(t *testing.T, got map[string]any, want string) {
	t.Helper()
	var w map[string]any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, w) {
		g, _ := json.Marshal(got)
		t.Fatalf("body %s, want %s", g, want)
	}
}

func TestNewValidates(t *testing.T) {
	c := must[*Client](t)(New("ak_x"))
	if c.Transport() != TransportAPI || c.BaseURL() != DefaultBaseURL {
		t.Fatal(c.BaseURL())
	}
	for _, f := range []func() (*Client, error){
		func() (*Client, error) { return New("") },
		func() (*Client, error) { return New(" ") },
		func() (*Client, error) { return New("ak_x", WithBaseURL("ftp://x")) },
		func() (*Client, error) { return New("ak_x", WithDeskToken(" ")) },
		func() (*Client, error) { return New("ak_x", WithSocketPath("/x.sock")) },
		func() (*Client, error) { return New("ak_x", WithAdminToken("gdlocal_x")) },
		func() (*Client, error) { return New("ak_x", WithRetry(RetryPolicy{MaxAttempts: -1})) },
		func() (*Client, error) {
			return New("ak_x", WithRetry(RetryPolicy{MaxAttempts: 3, MaxRetryWait: -time.Second}))
		},
	} {
		_, err := f()
		isUsage(t, err)
	}
}

func TestWireHeadersAndCallOptions(t *testing.T) {
	s := plainMock(t)
	cx := ctx(t)
	c := newClient(t, s)
	_ = must[*StatsReport](t)(c.Stats(cx, okDesk))
	last := s.Last()
	if last.Header.Get("Authorization") != "Bearer ak_test" || last.Header.Get("X-GaiaDesk-Desk-Token") != "gdagt_test" || last.Path != "/v1/desks/"+okDesk+"/stats" {
		t.Fatalf("%+v", last)
	}
	if !strings.HasPrefix(last.Header.Get("User-Agent"), "gaiadesk-go/") {
		t.Fatal(last.Header.Get("User-Agent"))
	}
	var info ResponseInfo
	_ = must[*StatsReport](t)(c.Stats(cx, okDesk, UseDeskToken("gdagt_other"), WakeFor(30*time.Second), CaptureResponse(&info)))
	last = s.Last()
	if last.Header.Get("X-GaiaDesk-Desk-Token") != "gdagt_other" || last.Query.Get("wake_s") != "30" {
		t.Fatalf("%+v", last)
	}
	if info.Status != 200 || !strings.HasPrefix(info.RequestID, "req_") || info.RateLimitLimit != 600 || info.RateLimitRemaining != 597 || info.Sealed {
		t.Fatalf("%+v", info)
	}
	_, err := c.Stats(cx, okDesk, WakeFor(121*time.Second))
	isUsage(t, err)
	_, err = c.Stats(cx, okDesk, UseDeskToken(""))
	isUsage(t, err)
	_ = must[[]TokenInfo](t)(owner(t, s).Tokens(cx, okDesk))
	if s.Last().Header.Get("X-GaiaDesk-Desk-Token") != "" {
		t.Fatal("no desk token unless one is given")
	}
	_ = must[*ExecResult](t)(c.Exec(cx, okDesk, ExecRequest{Command: "x"}, WithIdempotencyKey("exec-1")))
	if s.Last().Header.Get("Idempotency-Key") != "exec-1" {
		t.Fatal("idempotency key")
	}
	_, err = c.Exec(cx, okDesk, ExecRequest{Command: "x"}, WithIdempotencyKey("bad\nkey"))
	isUsage(t, err)
}

func TestExecSpec(t *testing.T) {
	s := plainMock(t)
	cx := ctx(t)
	c := newClient(t, s)
	r := must[*ExecResult](t)(c.Exec(cx, okDesk, ExecRequest{Command: "hostname", Shell: ShellSh, Timeout: 10 * time.Minute, Cwd: "/srv", Stdin: strings.NewReader("in")}))
	last := s.Last()
	if last.Method != "POST" || last.Header.Get("Content-Type") != "application/json" {
		t.Fatal(last.Method)
	}
	jsonEq(t, body(t, last), `{"command":"hostname","shell":"sh","cwd":"/srv","timeout_secs":600,"stdin":"in"}`)
	if r.Exit != 0 || r.Desk != okDesk || *r.RemoteCode != 0 || r.Error != nil {
		t.Fatalf("%+v", r)
	}
	_ = must[*ExecResult](t)(c.Exec(cx, okDesk, ExecRequest{Argv: []string{"ls", "-l"}}))
	jsonEq(t, body(t, s.Last()), `{"argv":["ls","-l"]}`)
	_ = must[*ExecResult](t)(c.Exec(cx, okDesk, ExecRequest{Command: "deploy", Shell: ShellPowerShell, Env: map[string]string{"STAGE": "prod", "EMPTY": ""}}))
	jsonEq(t, body(t, s.Last()), `{"command":"deploy","shell":"pwsh","env":{"STAGE":"prod","EMPTY":""}}`)
	_ = must[*ExecResult](t)(c.Exec(cx, okDesk, ExecRequest{Command: "x", Timeout: 1200 * time.Millisecond}))
	jsonEq(t, body(t, s.Last()), `{"command":"x","timeout_secs":2}`)
	st := must[*Stream](t)(c.ExecStream(cx, okDesk, ExecRequest{Command: "x", Env: map[string]string{"A": "1"}}))
	st.Wait()
	if s.Last().Query.Get("stream") != "1" || s.Last().Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("%+v", s.Last())
	}
	jsonEq(t, body(t, s.Last()), `{"command":"x","env":{"A":"1"}}`)

	before := len(s.Requests())
	for _, bad := range []ExecRequest{
		{},
		{Command: "  "},
		{Argv: []string{}},
		{Command: "x", Argv: []string{"y"}},
		{Command: "x", Env: map[string]string{"A=B": "secret-value"}},
		{Command: "x", Env: map[string]string{"A": "nul\x00"}},
		{Command: "x", Shell: "fish"},
		{Command: "x", Cwd: " "},
		{Command: "x", Timeout: -1},
		{Command: "x", Stdin: bytes.NewReader([]byte{0xff, 0xfe})},
	} {
		_, err := c.Exec(cx, okDesk, bad)
		isUsage(t, err)
		if strings.Contains(err.Error(), "secret-value") {
			t.Fatal("an env error named the value")
		}
	}
	for _, desk := range []string{"", " ", "-x", "a b"} {
		_, err := c.Exec(cx, desk, ExecRequest{Command: "x"})
		isUsage(t, err)
	}
	_, err := c.ExecStream(cx, okDesk, ExecRequest{Command: "x", Check: true})
	isUsage(t, err)
	if len(s.Requests()) != before {
		t.Fatal("something was sent for a bad request")
	}
}

func TestExecCheckAndNeverRan(t *testing.T) {
	s := plainMock(t)
	cx := ctx(t)
	c := newClient(t, s)
	r, err := c.Exec(cx, okDesk, ExecRequest{Command: "exit 3", Check: true})
	var ce *CommandError
	if !errors.As(err, &ce) || !errors.Is(err, ErrCommand) || !errors.Is(err, ErrFailed) || ce.Result.Exit != 3 || r == nil || r.Exit != 3 || ce.Err.ExitCode != 3 {
		t.Fatalf("%v %+v", err, r)
	}
	r = must[*ExecResult](t)(c.Exec(cx, okDesk, ExecRequest{Command: "exit 3"}))
	if r.Exit != 3 {
		t.Fatal("a non-zero exit is a result without Check")
	}
	_, err = c.Exec(cx, okDesk, ExecRequest{Command: "refuse"})
	if e := errOf(t, err); e.Class != ClassRefused || e.Status != 403 || e.ExitCode != 254 || e.Reason != "token_refused" {
		t.Fatalf("%v", err)
	}
	_, err = c.Exec(cx, okDesk, ExecRequest{Command: "x", Admin: true})
	if e := errOf(t, err); e.Class != ClassRefused || e.Reason != ReasonAdminScopeMissing || e.ExitCode != 254 || e.Desk != okDesk {
		t.Fatalf("%+v", e)
	}
}

func TestJobsAndTokensSpecs(t *testing.T) {
	s := plainMock(t)
	cx := ctx(t)
	c := newClient(t, s)
	yes := true
	job := must[*Job](t)(c.StartJob(cx, okDesk, JobRequest{Name: "build", Command: "make all", Priority: "low", CPUPercent: 50, MemMB: 2048, KeepAwake: &yes, Cwd: "src"}))
	jsonEq(t, body(t, s.Last()), `{"name":"build","command":["make all"],"limits":{"priority":"low","cpu_percent":50,"mem_mb":2048,"keep_awake":true},"cwd":"src"}`)
	if job.Name != "build" || job.State != "running" || *job.PID != 42 {
		t.Fatalf("%+v", job)
	}
	_ = must[*Job](t)(c.StartJob(cx, okDesk, JobRequest{Name: "b", Argv: []string{"Get-Date"}, Shell: ShellPowerShell, Env: map[string]string{"CI": "1"}}))
	jsonEq(t, body(t, s.Last()), `{"name":"b","command":["Get-Date"],"limits":{},"shell":"pwsh","env":{"CI":"1"}}`)
	before := len(s.Requests())
	for _, bad := range []JobRequest{{Name: "-x", Command: "x"}, {Name: "b", Command: ""}, {Name: "b", Command: "x", Shell: ShellNone}, {Name: "b", Command: "x", Priority: "urgent"}, {Name: "b", Command: "x", CPUPercent: 101}, {Name: "b", Command: "x", Env: map[string]string{"": "v"}}} {
		_, err := c.StartJob(cx, okDesk, bad)
		isUsage(t, err)
	}
	_, err := c.KillJob(cx, okDesk, "-x")
	isUsage(t, err)
	_, err = c.JobLogs(cx, okDesk, "b", &LogsOptions{Tail: -1})
	isUsage(t, err)
	if len(s.Requests()) != before {
		t.Fatal("sent a bad job")
	}
	jobs := must[[]Job](t)(c.Jobs(cx, okDesk))
	if len(jobs) != 1 || jobs[0].Name != "build" {
		t.Fatalf("%+v", jobs)
	}
	logs := must[*JobLogs](t)(c.JobLogs(cx, okDesk, "build", &LogsOptions{Tail: 10}))
	if last := s.Last(); last.Path != "/v1/desks/"+okDesk+"/jobs/build/logs" || last.Query.Get("tail") != "10" || logs.Output != "tail 10\n" {
		t.Fatalf("%+v %q", last, logs.Output)
	}
	killed := must[*Job](t)(c.KillJob(cx, okDesk, "build"))
	if s.Last().Method != "DELETE" || killed.State != "killed" {
		t.Fatal("kill")
	}
	o := owner(t, s)
	m := must[*MintResult](t)(o.CreateToken(cx, TokenRequest{Desks: []string{okDesk}, Name: "bot", Expires: 24 * time.Hour, Cwd: "/srv", LowPriv: true}))
	jsonEq(t, body(t, s.Last()), `{"name":"bot","expires_secs":86400,"scopes":["exec","cp","jobs"],"cwd":"/srv","low_priv":true}`)
	if m.Tokens[0].Secret != "gdagt_minted_secret" || m.Tokens[0].Desk != okDesk {
		t.Fatalf("%+v", m)
	}
	_ = must[*MintResult](t)(o.CreateToken(cx, TokenRequest{Desks: []string{okDesk}, Name: "ops", Scopes: []string{ScopeExec, ScopeShell, ScopeAdmin}}))
	jsonEq(t, body(t, s.Last()), `{"name":"ops","expires_secs":604800,"scopes":["exec","shell","admin"]}`)
	// A later desk failing: the tokens already minted come with the error.
	partial, err := o.CreateToken(cx, TokenRequest{Desks: []string{okDesk, "000000001"}, Name: "bot"})
	if errOf(t, err).Reason != ReasonUnknownDesk || partial == nil || len(partial.Tokens) != 1 {
		t.Fatalf("%v %+v", err, partial)
	}
	_ = must[*Revoked](t)(o.RevokeToken(cx, okDesk, "9f3a1c2b7d004e11"))
	if l := s.Last(); l.Method != "DELETE" || l.Path != "/v1/desks/"+okDesk+"/tokens/9f3a1c2b7d004e11" {
		t.Fatalf("%+v", l)
	}
	for _, bad := range []TokenRequest{{Name: "x"}, {Desks: []string{okDesk}}, {Desks: []string{okDesk}, Name: "x", Scopes: []string{}}, {Desks: []string{okDesk}, Name: "x", Scopes: []string{ScopeAdmin}, LowPriv: true}} {
		_, err := o.CreateToken(cx, bad)
		isUsage(t, err)
	}
	_, err = o.RevokeToken(cx, okDesk, "")
	isUsage(t, err)
	// An API key may not administer tokens.
	_, err = c.Tokens(cx, okDesk)
	if e := errOf(t, err); e.Class != ClassRefused || e.Reason != "session_required" {
		t.Fatalf("%v", err)
	}
}

func TestWaitJob(t *testing.T) {
	s := plainMock(t)
	cx := ctx(t)
	c := newClient(t, s)
	done := must[*JobWaitResult](t)(c.WaitJob(cx, okDesk, "failing", 10*time.Minute))
	if l := s.Last(); l.Path != "/v1/desks/"+okDesk+"/jobs/failing/wait" || l.Query.Get("timeout") != "600" {
		t.Fatalf("%+v", l)
	}
	if done.TimedOut || done.Job.State != "exited" || *done.Job.ExitCode != 3 {
		t.Fatalf("the job's own code is a result: %+v", done)
	}
	now := must[*JobWaitResult](t)(c.WaitJob(cx, okDesk, "slow", 0))
	if !now.TimedOut || now.Job.State != "running" || s.Last().Query.Get("timeout") != "0" {
		t.Fatalf("%+v", now)
	}
	forever := must[*JobWaitResult](t)(c.WaitJob(cx, okDesk, "build", WaitForever))
	if forever.TimedOut || s.Last().Query.Get("timeout") != "870" {
		t.Fatal("no timeout: the API's longest, again until it ends")
	}
	var info ResponseInfo
	held := must[*JobWaitResult](t)(c.WaitJob(cx, okDesk, "held", WaitForever, CaptureResponse(&info)))
	if held.Job.Name != "held" || !info.Held {
		t.Fatal("leading keep-alive spaces are still JSON")
	}
	_, err := c.WaitJob(cx, okDesk, "held-fail", WaitForever)
	if e := errOf(t, err); e.Class != ClassConnectionLost || e.Reason != "desk_disconnected" || e.Status != 502 {
		t.Fatalf("%+v", e)
	}
	_, err = c.WaitJob(cx, okDesk, "held-gone", WaitForever)
	if e := errOf(t, err); !errors.Is(err, ErrFailed) || e.Status != 422 {
		t.Fatalf("a late failed in a held 200: %+v", e)
	}
	_, err = c.WaitJob(cx, okDesk, "nope", WaitForever)
	if e := errOf(t, err); e.Class != ClassFailed || e.Status != 422 {
		t.Fatalf("%+v", e)
	}
	n := len(s.Waits())
	slow := must[*JobWaitResult](t)(c.WaitJob(cx, okDesk, "slow", 300*time.Millisecond))
	waits := s.Waits()[n:]
	if !slow.TimedOut || len(waits) < 1 || waits[0] != "1" {
		t.Fatalf("a short timeout is asked as is: %v", waits)
	}
	_, err = c.WaitJob(cx, okDesk, "-x", 0)
	isUsage(t, err)
	_, err = c.WaitJob(cx, okDesk, "x", -5)
	isUsage(t, err)
}

func TestFiles(t *testing.T) {
	s := plainMock(t)
	cx := ctx(t)
	c := newClient(t, s)
	r := must[*CopyResult](t)(c.Upload(cx, okDesk, "notes/a.txt", strings.NewReader("hello")))
	l := s.Last()
	if l.Method != "PUT" || l.Query.Get("path") != "notes/a.txt" || l.Header.Get("Content-Type") != "application/octet-stream" || string(l.Body) != "hello" || r.Direction != "upload" {
		t.Fatalf("%+v", l)
	}
	if b := must[[]byte](t)(c.DownloadBytes(cx, okDesk, "elsewhere.txt")); string(b) != "contents of elsewhere.txt\n" {
		t.Fatalf("%q", b)
	}
	// A plain io.Reader (not seekable) is read first; sizes and bodies match.
	_ = must[*CopyResult](t)(c.Upload(cx, okDesk, "pipe.txt", io.MultiReader(strings.NewReader("a"), strings.NewReader("b"))))
	if string(s.Last().Body) != "ab" {
		t.Fatal(string(s.Last().Body))
	}
	dir := t.TempDir()
	local := filepath.Join(dir, "report.csv")
	_ = os.WriteFile(local, []byte("1,2\n"), 0o600)
	up := must[*CopyResult](t)(c.UploadFile(cx, local, okDesk, "reports/"))
	if s.Last().Query.Get("path") != "reports/report.csv" || up.Bytes != 4 {
		t.Fatalf("a folder keeps the name: %v", s.Last().Query)
	}
	_, err := c.UploadFile(cx, dir, okDesk, "x/")
	isUsage(t, err)
	_, err = c.UploadFile(cx, filepath.Join(dir, "nope"), okDesk, "x")
	if errOf(t, err).Kind != KindLocal {
		t.Fatal(err)
	}
	got := must[*CopyResult](t)(c.DownloadFile(cx, okDesk, "reports/report.csv", dir+string(os.PathSeparator)))
	b, _ := os.ReadFile(filepath.Join(dir, "report.csv"))
	if got.Destination != filepath.Join(dir, "report.csv") || string(b) != "1,2\n" || got.Bytes != 4 {
		t.Fatalf("%+v %q", got, b)
	}
	_, err = c.Upload(cx, okDesk, "fails", strings.NewReader("x"))
	if e := errOf(t, err); e.Class != ClassFailed || !strings.Contains(e.Message, "failed to copy") {
		t.Fatalf("%v", err)
	}
	_, err = c.DownloadBytes(cx, okDesk, "missing")
	if e := errOf(t, err); e.Class != ClassFailed || e.Status != 422 || e.Reason != "not_found" {
		t.Fatalf("%v", err)
	}
	// A transfer that breaks off is an error, never a clean short file.
	target := filepath.Join(dir, "broken.bin")
	_, err = c.DownloadFile(cx, okDesk, "broken", target)
	if e := errOf(t, err); e.Class != ClassConnectionLost {
		t.Fatalf("%v", err)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatal("a broken download left a file")
	}
	_, err = c.Upload(cx, okDesk, "", strings.NewReader("x"))
	isUsage(t, err)
}

func TestErrorEnvelopes(t *testing.T) {
	s := plainMock(t)
	cx := ctx(t)
	noToken, _ := New("ak_test", WithBaseURL(s.URL), WithWarningHandler(quiet))
	_, err := noToken.Stats(cx, okDesk)
	e := errOf(t, err)
	if !errors.Is(err, ErrRefused) || e.Kind != KindRefused || e.Reason != ReasonDeskTokenRequired || e.Status != 403 || e.ExitCode != 254 || e.Desk != okDesk || !strings.HasPrefix(e.RequestID, "req_") || e.Op != "GET /desks/"+okDesk+"/stats" {
		t.Fatalf("%+v", e)
	}
	if !strings.Contains(e.Error(), e.RequestID) || len(e.Body) == 0 {
		t.Fatal(e.Error())
	}
	c := newClient(t, s)
	_, err = c.Exec(cx, offlineDesk, ExecRequest{Command: "x"})
	if e := errOf(t, err); !errors.Is(err, ErrUnreachable) || e.Status != 409 || e.Kind != KindOffline || e.Reason != "offline" || e.Desk != offlineDesk {
		t.Fatalf("%+v", e)
	}
	_, err = c.Exec(cx, usageDesk, ExecRequest{Command: "x"})
	if e := errOf(t, err); !errors.Is(err, ErrUsage) || e.Status != 400 {
		t.Fatalf("%+v", e)
	}
	_, err = c.Stats(cx, "000000001")
	if e := errOf(t, err); e.Kind != KindUnknownDesk || e.Status != 404 {
		t.Fatalf("%+v", e)
	}
	_, err = newClient(t, s, WithRetry(NoRetry)).Stats(cx, mockapi.LimitedDesk)
	if e := errOf(t, err); e.Status != 429 || e.Reason != ReasonRateLimited || e.RetryAfter != 7*time.Second || !e.Temporary() {
		t.Fatalf("%+v", e)
	}
	_, err = c.Stats(cx, mockapi.HTMLDesk)
	if e := errOf(t, err); !errors.Is(err, ErrProtocol) || e.Status != 500 || !strings.Contains(e.Message, "no error envelope") {
		t.Fatalf("%+v", e)
	}
	down, _ := New("ak_test", WithBaseURL("http://127.0.0.1:1/v1"), WithDeskToken("t"), WithRetry(NoRetry), WithWarningHandler(quiet), WithE2E(E2EOff))
	_, err = down.Stats(cx, okDesk)
	if e := errOf(t, err); !errors.Is(err, ErrUnreachable) || e.Kind != KindNetwork || e.Reason != ReasonNetwork || e.ExitCode != 255 || e.Err == nil {
		t.Fatalf("%+v", e)
	}
	_, err = down.ExecStream(cx, okDesk, ExecRequest{Command: "x"})
	if errOf(t, err).Kind != KindNetwork {
		t.Fatal(err)
	}
}

func TestRetries(t *testing.T) {
	s := plainMock(t)
	cx := ctx(t)
	c := newClient(t, s)
	// 429 with Retry-After 0: retried, any method.
	s.FailNext(2, 429, "refused", ReasonRateLimited, "0")
	if r := must[*ExecResult](t)(c.Exec(cx, okDesk, ExecRequest{Command: "x"})); r.Exit != 0 {
		t.Fatal("retried after 429")
	}
	// 503 on a GET: retried; on a POST: not (it may have run).
	s.FailNext(1, 503, "unreachable", "", "")
	_ = must[*StatsReport](t)(c.Stats(cx, okDesk))
	s.FailNext(1, 503, "unreachable", "", "")
	_, err := c.Exec(cx, okDesk, ExecRequest{Command: "x"})
	if errOf(t, err).Status != 503 {
		t.Fatal(err)
	}
	// Out of attempts: the last error.
	s.FailNext(3, 502, "connection_lost", "", "")
	_, err = c.Stats(cx, okDesk)
	if errOf(t, err).Status != 502 {
		t.Fatal(err)
	}
	// A Retry-After longer than MaxRetryWait (60 s) is not waited.
	s.FailNext(1, 429, "refused", ReasonRateLimited, "120")
	_, err = c.Stats(cx, okDesk)
	if errOf(t, err).RetryAfter != 2*time.Minute {
		t.Fatal(err)
	}
	// WithoutRetry and NoRetry make one try.
	s.FailNext(1, 429, "refused", ReasonDeskBusy, "0")
	_, err = c.Stats(cx, okDesk, WithoutRetry())
	if errOf(t, err).Reason != ReasonDeskBusy {
		t.Fatal(err)
	}
	// An upload is resent from a seekable reader.
	s.FailNext(1, 429, "refused", ReasonRateLimited, "0")
	_ = must[*CopyResult](t)(c.Upload(cx, okDesk, "again.txt", strings.NewReader("same bytes")))
	if string(s.Last().Body) != "same bytes" {
		t.Fatal(string(s.Last().Body))
	}
	// A refusal is never retried.
	execs := func() int {
		k := 0
		for _, q := range s.Requests() {
			if strings.HasSuffix(q.Path, "/exec") {
				k++
			}
		}
		return k
	}
	n := execs()
	_, _ = c.Exec(cx, offlineDesk, ExecRequest{Command: "x"})
	if execs() != n+1 {
		t.Fatal("an unreachable desk was retried")
	}
}

func TestStreams(t *testing.T) {
	s := plainMock(t)
	cx := ctx(t)
	c := newClient(t, s)
	st := must[*Stream](t)(c.ExecStream(cx, okDesk, ExecRequest{Command: "echo hi"}))
	var out, errOut bytes.Buffer
	x, err := st.Copy(&out, &errOut)
	if err != nil || out.String() != "ran: echo hi é\n" || errOut.String() != "warn\n" || x.ExitCode != 0 || x.Result == nil || x.Result.Desk != okDesk {
		t.Fatalf("%v %q %q %+v", err, out.String(), errOut.String(), x)
	}
	if _, err := st.Recv(); err != io.EOF {
		t.Fatal("after the end: io.EOF")
	}
	if x.Err() != nil {
		t.Fatal(x.Err())
	}
	// A refused command in the stream's first moments is the call's error.
	noToken, _ := New("ak_test", WithBaseURL(s.URL), WithWarningHandler(quiet))
	_, err = noToken.ExecStream(cx, okDesk, ExecRequest{Command: "x"})
	if e := errOf(t, err); e.Reason != ReasonDeskTokenRequired || e.ExitCode != 254 {
		t.Fatalf("%+v", e)
	}
	_, err = c.ExecStream(cx, okDesk, ExecRequest{Command: "refuse"})
	if errOf(t, err).Status != 403 {
		t.Fatal(err)
	}
	// The desk lost mid-stream: its last event.
	lost := must[*Stream](t)(c.ExecStream(cx, okDesk, ExecRequest{Command: "lose"})).Wait()
	if lost.ExitCode != 255 || lost.Error.Kind != "connection_lost" || lost.Err() == nil || !errors.Is(lost.Err(), ErrConnectionLost) {
		t.Fatalf("%+v", lost)
	}
	// Following a job: output, then its end.
	f := must[*Stream](t)(c.FollowJobLogs(cx, okDesk, "build", &LogsOptions{Tail: 5}))
	if q := s.Last().Query; q.Get("follow") != "1" || q.Get("tail") != "5" {
		t.Fatal(q)
	}
	var text string
	for ch := range f.Chunks() {
		text += ch.Text()
	}
	if text != "line1\nline2 é\n" || f.Exit().ExitCode != 0 || f.Exit().Job.Name != "build" || f.Exit().StderrTail != "job build exited (exit 0)" {
		t.Fatalf("%q %+v", text, f.Exit())
	}
	// Close stops it: 130.
	f = must[*Stream](t)(c.FollowJobLogs(cx, okDesk, "build", nil))
	_ = f.Close()
	if x := f.Wait(); x.ExitCode != 130 || x.StderrTail != "interrupted" {
		t.Fatalf("%+v", x)
	}
	// A cancelled context: interrupted, before or during.
	cancelled, cancel := context.WithCancel(cx)
	cancel()
	_, err = c.Stats(cancelled, okDesk)
	if e := errOf(t, err); !errors.Is(err, ErrInterrupted) || e.ExitCode != 130 || !errors.Is(err, context.Canceled) {
		t.Fatalf("%+v", e)
	}
	during, cancel2 := context.WithCancel(cx)
	g := must[*Stream](t)(c.ExecStream(during, okDesk, ExecRequest{Command: "x"}))
	_, _ = g.Recv()
	cancel2()
	if x := g.Wait(); x.ExitCode != 130 {
		t.Fatalf("%+v", x)
	}
}

func TestSSEParser(t *testing.T) {
	text := ": keep-alive\r\nevent: stdout\r\ndata: {\"event\":\"stdout\",\"data\":\"a\"}\r\n\r\n:ping\n\nevent: x\ndata: line1\ndata: line2\n\ndata: {\"event\":\"exit\",\"exit\":0}"
	want := []sseEvent{{"stdout", `{"event":"stdout","data":"a"}`}, {"x", "line1\nline2"}, {"message", `{"event":"exit","exit":0}`}}
	for _, size := range []int{1, 2, 3, 7, len(text)} {
		var p sseParser
		var got []sseEvent
		for i := 0; i < len(text); i += size {
			got = append(got, p.feed(text[i:min(len(text), i+size)])...)
		}
		got = append(got, p.end()...)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("chunks of %d: %+v", size, got)
		}
	}
	var p sseParser
	if len(p.feed("data: a\r")) != 0 || len(p.feed("\ndata:b\r\r")) != 0 {
		t.Fatal("a trailing \\r may be half of \\r\\n")
	}
	if got := p.feed("\n"); !reflect.DeepEqual(got, []sseEvent{{"message", "a\nb"}}) {
		t.Fatalf("%+v", got)
	}
	// Bytes split mid-character arrive whole.
	r := newSSEReader(&oneByte{b: []byte("data: é\n\n")})
	ev, err := r.next()
	if err != nil || ev.data != "é" {
		t.Fatalf("%+v %v", ev, err)
	}
}

type oneByte struct{ b []byte }

func (o *oneByte) Read(p []byte) (int, error) {
	if len(o.b) == 0 {
		return 0, io.EOF
	}
	p[0] = o.b[0]
	o.b = o.b[1:]
	return 1, nil
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"90": 90 * time.Second, "30s": 30 * time.Second, "10m": 10 * time.Minute, "1h30m": 90 * time.Minute, "7d": 7 * 24 * time.Hour, "2w": 14 * 24 * time.Hour} {
		if got := must[time.Duration](t)(ParseDuration(in)); got != want {
			t.Fatalf("%s: %v", in, got)
		}
	}
	for _, bad := range []string{"5 fortnights", "", "x"} {
		_, err := ParseDuration(bad)
		isUsage(t, err)
	}
}
