package gaiadesk

// End-to-end encryption on the API transport, against a mock API that is
// also the desk (internal/mockapi): every operation sealed gives exactly
// what it gives in the clear, the API never sees the command, env, stdin,
// path or file bytes, and the modes, pins and retries behave. A port of the
// TypeScript SDK's test/api-e2e.test.ts.

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Gaia-Desk/gaiadesk-go/internal/mockapi"
)

const (
	sealedDesk  = "111111111"
	oldDesk     = "222222222" // no end-to-end key
	mustDesk    = "333333333" // requires it
	asleepDesk  = "444444444" // requires it, offline until woken
	goneDesk    = "555555555" // offline, no wake path
	staleDesk   = "666666666" // requires it; the first lookup lists no key
	rotatedDesk = "777777777"
)

func e2eMock(t *testing.T) *mockapi.Server {
	s := mockapi.New(map[string]*mockapi.Desk{
		sealedDesk:  {Secret: key1},
		oldDesk:     {},
		mustDesk:    {Secret: key1, Required: true},
		asleepDesk:  {Secret: key1, Required: true, Offline: true, Wakeable: true},
		goneDesk:    {Secret: key1, Offline: true},
		staleDesk:   {Secret: key1, Required: true, HideKeyLookups: 1},
		rotatedDesk: {Secret: key1},
	})
	t.Cleanup(s.Close)
	return s
}

// blind runs f, and asserts the API saw nothing of the canary.
func blind[T any](t *testing.T, s *mockapi.Server, f func() (T, error)) (T, error) {
	t.Helper()
	before := len(s.Requests())
	v, err := f()
	seen := s.Requests()[before:]
	if len(seen) == 0 {
		t.Fatal("no request was made")
	}
	for _, q := range seen {
		raw, _ := json.Marshal([]any{q.Path, q.Query, q.Header, string(q.Body)})
		if strings.Contains(string(raw), canary) {
			t.Fatalf("the API saw the canary in %s %s: %.300s", q.Method, q.Path, raw)
		}
	}
	return v, err
}

// same runs a call sealed (the API seeing none of it) and in the clear:
// equal answers.
func same[T any](t *testing.T, s *mockapi.Server, f func(c *Client) (T, error)) T {
	t.Helper()
	sealedC, plainC := newClient(t, s), newClient(t, s, WithE2E(E2EOff))
	before := len(s.Sealed())
	sealed, err := blind(t, s, func() (T, error) { return f(sealedC) })
	if err != nil {
		t.Fatalf("sealed: %v", err)
	}
	if len(s.Sealed()) <= before {
		t.Fatal("it did not go sealed")
	}
	plain, err := f(plainC)
	if err != nil {
		t.Fatalf("plain: %v", err)
	}
	if !reflect.DeepEqual(sealed, plain) {
		t.Fatalf("sealed and plain differ:\n%+v\n%+v", sealed, plain)
	}
	return sealed
}

type errShape struct {
	Class    Class
	Kind     ErrorKind
	Reason   string
	Status   int
	Message  string
	Desk     string
	ExitCode int
}

func shape(err error) errShape {
	e := AsError(err)
	if e == nil {
		return errShape{Message: "not an *Error: " + err.Error()}
	}
	return errShape{e.Class, e.Kind, e.Reason, e.Status, e.Message, e.Desk, e.ExitCode}
}

func sameError(t *testing.T, s *mockapi.Server, f func(c *Client) error) errShape {
	t.Helper()
	sealedC, plainC := newClient(t, s), newClient(t, s, WithE2E(E2EOff))
	_, err := blind(t, s, func() (int, error) { return 0, f(sealedC) })
	if err == nil {
		t.Fatal("sealed: no error")
	}
	perr := f(plainC)
	if perr == nil {
		t.Fatal("plain: no error")
	}
	if shape(err) != shape(perr) {
		t.Fatalf("sealed and plain errors differ:\n%+v\n%+v", shape(err), shape(perr))
	}
	return shape(err)
}

type drained struct {
	Out, Err string
	Exit     Exit
}

func drain(st *Stream, err error) (drained, error) {
	if err != nil {
		return drained{}, err
	}
	var d drained
	for c := range st.Chunks() {
		if c.Stream == "stdout" {
			d.Out += c.Text()
		} else {
			d.Err += c.Text()
		}
	}
	d.Exit = *st.Exit()
	return d, nil
}

func TestE2EExecSealedInBody(t *testing.T) {
	s := e2eMock(t)
	cx := ctx(t)
	lookups := func() int {
		n := 0
		for _, r := range s.Requests() {
			if r.Method == "GET" && r.Path == "/v1/desks/"+sealedDesk {
				n++
			}
		}
		return n
	}
	r := same(t, s, func(c *Client) (*ExecResult, error) {
		return c.Exec(cx, sealedDesk, ExecRequest{Command: "echo " + canary, Env: map[string]string{"SECRET": canary}, Stdin: strings.NewReader(canary), Cwd: "/srv/" + canary, Timeout: 30 * time.Second})
	})
	want := "ran: echo " + canary + " é\nenv: SECRET=" + canary + "\nstdin: " + canary + "\ncwd: /srv/" + canary + "\ntimeout: 30\n"
	if r.Stdout != want {
		t.Fatalf("stdout %q", r.Stdout)
	}
	var sent mockapi.Recorded
	execs := 0
	for _, q := range s.Requests() {
		if q.Path == "/v1/desks/"+sealedDesk+"/exec" {
			execs++
			if execs == 1 {
				sent = q
			}
		}
	}
	var body map[string]map[string]any
	if err := json.Unmarshal(sent.Body, &body); err != nil || len(body) != 1 || body["e2e"] == nil {
		t.Fatalf("body %s", sent.Body)
	}
	if len(body["e2e"]) != 4 || body["e2e"]["v"] != 1.0 {
		t.Fatalf("e2e %v", body["e2e"])
	}
	c := newClient(t, s)
	_ = must[*ExecResult](t)(c.Exec(cx, sealedDesk, ExecRequest{Command: "warm"}))
	n := lookups()
	_ = must[*ExecResult](t)(c.Exec(cx, sealedDesk, ExecRequest{Command: "again"}))
	if lookups() != n {
		t.Fatal("the desk key was not cached")
	}
}

func TestE2EExecStream(t *testing.T) {
	s := e2eMock(t)
	cx := ctx(t)
	r := same(t, s, func(c *Client) (drained, error) {
		return drain(c.ExecStream(cx, sealedDesk, ExecRequest{Command: "echo " + canary, Env: map[string]string{"K": canary}}))
	})
	if r.Out != "ran: echo "+canary+" é\nenv: K="+canary+"\n" || r.Err != "warn\n" || r.Exit.ExitCode != 0 || r.Exit.Result == nil {
		t.Fatalf("%+v", r)
	}
	// The desk lost mid-stream: the server's plaintext error event ends it.
	lost := same(t, s, func(c *Client) (drained, error) {
		return drain(c.ExecStream(cx, sealedDesk, ExecRequest{Command: "lose"}))
	})
	if lost.Exit.Error == nil || lost.Exit.Error.Kind != "connection_lost" || lost.Exit.ExitCode != 255 {
		t.Fatalf("%+v", lost.Exit)
	}
}

func TestE2EJobsLogsWaitKillStats(t *testing.T) {
	s := e2eMock(t)
	cx := ctx(t)
	same(t, s, func(c *Client) (*Job, error) {
		return c.StartJob(cx, sealedDesk, JobRequest{Name: "build", Command: "make " + canary, Env: map[string]string{"CI": canary}, Shell: ShellBash, Cwd: canary})
	})
	same(t, s, func(c *Client) ([]Job, error) { return c.Jobs(cx, sealedDesk) })
	logs := same(t, s, func(c *Client) (*JobLogs, error) { return c.JobLogs(cx, sealedDesk, "build", &LogsOptions{Tail: 10}) })
	if logs.Output != "tail 10\n" {
		t.Fatalf("logs %q", logs.Output)
	}
	f := same(t, s, func(c *Client) (drained, error) { return drain(c.FollowJobLogs(cx, sealedDesk, "build", nil)) })
	if f.Out != "line1\nline2 é\n" || f.Exit.Job == nil || f.Exit.Job.Name != "build" {
		t.Fatalf("follow %+v", f)
	}
	w := same(t, s, func(c *Client) (*JobWaitResult, error) { return c.WaitJob(cx, sealedDesk, "build", time.Minute) })
	if w.TimedOut || w.Job.State != "exited" || *w.Job.ExitCode != 3 {
		t.Fatalf("wait %+v", w)
	}
	held := same(t, s, func(c *Client) (*JobWaitResult, error) { return c.WaitJob(cx, sealedDesk, "held", WaitForever) })
	if held.Job.Name != "held" {
		t.Fatalf("held %+v", held)
	}
	same(t, s, func(c *Client) (*Job, error) { return c.KillJob(cx, sealedDesk, "build") })
	same(t, s, func(c *Client) (*StatsReport, error) { return c.Stats(cx, sealedDesk) })
	var tail mockapi.Recorded
	for _, q := range s.Requests() {
		if strings.HasSuffix(q.Path, "/logs") && q.Header.Get("GaiaDesk-E2E") != "" && q.Query.Get("follow") == "" {
			tail = q
		}
	}
	if len(tail.Query) != 0 {
		t.Fatalf("tail travels inside the sealed request: %v", tail.Query)
	}
}

func TestE2EDeskErrors(t *testing.T) {
	s := e2eMock(t)
	cx := ctx(t)
	e := sameError(t, s, func(c *Client) error { _, err := c.Exec(cx, sealedDesk, ExecRequest{Command: "refuse"}); return err })
	if e.Class != ClassRefused || e.Reason != "token_refused" || e.Status != 403 || e.Desk != sealedDesk || !strings.Contains(e.Message, "no exec scope") {
		t.Fatalf("%+v", e)
	}
	w := sameError(t, s, func(c *Client) error { _, err := c.WaitJob(cx, sealedDesk, "held-gone", WaitForever); return err })
	if w.Class != ClassFailed || !strings.Contains(w.Message, `no job named "held-gone"`) {
		t.Fatalf("%+v", w)
	}
	l := sameError(t, s, func(c *Client) error { _, err := c.JobLogs(cx, sealedDesk, "missing", nil); return err })
	if l.Message != `no job named "missing"` {
		t.Fatalf("%+v", l)
	}
	// A stream refused in its first moments is the call's error (sealed too).
	f := sameError(t, s, func(c *Client) error { _, err := c.FollowJobLogs(cx, sealedDesk, "missing", nil); return err })
	if f.Message != `no job named "missing"` || f.Status != 422 {
		t.Fatalf("%+v", f)
	}
}

func TestE2EFiles(t *testing.T) {
	s := e2eMock(t)
	cx := ctx(t)
	big := make([]byte, 150*1024)
	for i := range big {
		big[i] = byte(i % 251)
	}
	marked := []byte(canary + " file\n")
	up := same(t, s, func(c *Client) (*CopyResult, error) {
		return c.Upload(cx, sealedDesk, "docs/"+canary+".txt", bytes.NewReader(marked))
	})
	if up.Bytes != uint64(len(marked)) {
		t.Fatalf("%+v", up)
	}
	var put mockapi.Recorded
	for _, q := range s.Requests() {
		if q.Method == "PUT" && q.Header.Get("Content-Type") == "application/x-ndjson" {
			put = q
		}
	}
	if len(put.Query) != 0 {
		t.Fatalf("the path travels sealed: %v", put.Query)
	}
	c := newClient(t, s)
	if _, err := blind(t, s, func() (*CopyResult, error) { return c.Upload(cx, sealedDesk, "big.bin", bytes.NewReader(big)) }); err != nil {
		t.Fatal(err)
	}
	last := s.Last()
	if frames := strings.Split(strings.TrimSpace(string(last.Body)), "\n"); len(frames) != 4 {
		t.Fatalf("48 KiB per frame: %d frames", len(frames))
	}
	got, err := blind(t, s, func() ([]byte, error) { return c.DownloadBytes(cx, sealedDesk, "big.bin") })
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("download big: %v", err)
	}
	down := same(t, s, func(c *Client) ([]byte, error) { return c.DownloadBytes(cx, sealedDesk, "docs/"+canary+".txt") })
	if !bytes.Equal(down, marked) {
		t.Fatalf("%q", down)
	}
	sameError(t, s, func(c *Client) error { _, err := c.DownloadBytes(cx, sealedDesk, "missing"); return err })
	_, err = c.DownloadBytes(cx, sealedDesk, "truncated")
	if e := errOf(t, err); e.Class != ClassConnectionLost || e.Reason != ReasonIncomplete {
		t.Fatalf("%v", err)
	}
	// An empty file is one flagged frame.
	empty := same(t, s, func(c *Client) (*CopyResult, error) { return c.Upload(cx, sealedDesk, "empty", bytes.NewReader(nil)) })
	if empty.Bytes != 0 {
		t.Fatalf("%+v", empty)
	}
}

func TestE2ETokens(t *testing.T) {
	s := e2eMock(t)
	cx := ctx(t)
	mint := func(c *Client) (*MintResult, error) {
		return c.CreateToken(cx, TokenRequest{Desks: []string{sealedDesk}, Name: "bot-" + canary, Expires: time.Hour, Scopes: []string{ScopeExec, ScopeAdmin}})
	}
	sealed, err := blind(t, s, func() (*MintResult, error) { return mint(owner(t, s)) })
	if err != nil {
		t.Fatal(err)
	}
	plain := must[*MintResult](t)(mint(owner(t, s, WithE2E(E2EOff))))
	if !reflect.DeepEqual(sealed, plain) || sealed.Tokens[0].Token.Label != "bot-"+canary || sealed.Tokens[0].Secret == "" {
		t.Fatalf("%+v %+v", sealed, plain)
	}
	if !reflect.DeepEqual(sealed.Tokens[0].Token.Scopes, []string{"exec", "admin"}) {
		t.Fatalf("scopes %v", sealed.Tokens[0].Token.Scopes)
	}
	a := must[[]TokenInfo](t)(owner(t, s).Tokens(cx, sealedDesk))
	b := must[[]TokenInfo](t)(owner(t, s, WithE2E(E2EOff)).Tokens(cx, sealedDesk))
	if !reflect.DeepEqual(a, b) {
		t.Fatal("token lists differ")
	}
	ra := must[*Revoked](t)(owner(t, s).RevokeToken(cx, sealedDesk, "tok1"))
	rb := must[*Revoked](t)(owner(t, s, WithE2E(E2EOff)).RevokeToken(cx, sealedDesk, "tok1"))
	if *ra != *rb || ra.Revoked != "tok1" {
		t.Fatal("revokes differ")
	}
	n := 0
	for _, o := range s.Sealed() {
		if strings.HasPrefix(o, "token_") {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("%d sealed token ops", n)
	}
}

func TestE2EAutoNoKeyWarnsOnce(t *testing.T) {
	s := e2eMock(t)
	cx := ctx(t)
	var w warnings
	c := newClient(t, s, WithWarningHandler(w.add))
	before := len(s.Plain())
	_ = must[*StatsReport](t)(c.Stats(cx, oldDesk))
	_ = must[*StatsReport](t)(c.Stats(cx, oldDesk))
	if len(s.Plain()) != before+2 {
		t.Fatal("not in the clear")
	}
	mine := w.about(oldDesk)
	if len(mine) != 1 || !strings.Contains(mine[0], "not end-to-end encrypted") {
		t.Fatalf("warnings %v", mine)
	}
}

func TestE2ERequire(t *testing.T) {
	s := e2eMock(t)
	cx := ctx(t)
	req := newClient(t, s, WithE2E(E2ERequire))
	before := len(s.Requests())
	_, err := req.Exec(cx, oldDesk, ExecRequest{Command: "echo " + canary})
	if e := errOf(t, err); !errors.Is(err, ErrE2E) || !errors.Is(err, ErrRefused) || e.Reason != ReasonE2EUnavailable || e.Desk != oldDesk {
		t.Fatalf("%v", err)
	}
	_, err = req.Stats(cx, goneDesk)
	if !errors.Is(err, ErrE2E) || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("%v", err)
	}
	for _, q := range s.Requests()[before:] {
		if !strings.HasSuffix(q.Path, "/wake") && strings.Count(q.Path, "/") != 3 {
			t.Fatalf("only lookups and wakes: %s %s", q.Method, q.Path)
		}
	}
	wakes := strings.Join(s.Wakes(), ",")
	if !strings.Contains(wakes, oldDesk) || !strings.Contains(wakes, goneDesk) {
		t.Fatalf("wakes %s", wakes)
	}
	// Asleep, but woken: then sealed.
	r, err := blind(t, s, func() (*ExecResult, error) { return req.Exec(cx, asleepDesk, ExecRequest{Command: "echo " + canary}) })
	if err != nil || r.Exit != 0 || !strings.Contains(strings.Join(s.Wakes(), ","), asleepDesk) {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestE2ERequiredDeskAndRetry(t *testing.T) {
	s := e2eMock(t)
	cx := ctx(t)
	before := len(s.Sealed())
	_ = must[*StatsReport](t)(newClient(t, s).Stats(cx, mustDesk))
	if len(s.Sealed()) != before+1 {
		t.Fatal("a desk that requires it is sealed in auto too")
	}
	count := func() int {
		n := 0
		for _, q := range s.Requests() {
			if q.Path == "/v1/desks/"+staleDesk+"/stats" {
				n++
			}
		}
		return n
	}
	n := count()
	r := must[*StatsReport](t)(newClient(t, s).Stats(cx, staleDesk))
	if r.CPUPercent != 5 || count() != n+2 || s.Sealed()[len(s.Sealed())-1] != "stats" {
		t.Fatalf("refused once, then sealed: %d", count()-n)
	}
	_, err := newClient(t, s, WithE2E(E2EOff)).Stats(cx, mustDesk)
	if e := errOf(t, err); e.Reason != ReasonE2ERequired || e.Status != 409 || e.Class != ClassRefused {
		t.Fatalf("%v", err)
	}
}

func TestE2ERotatedKey(t *testing.T) {
	s := e2eMock(t)
	cx := ctx(t)
	c := newClient(t, s)
	_ = must[*StatsReport](t)(c.Stats(cx, rotatedDesk))
	s.Desk(rotatedDesk).Secret = key2 // rotated, the old key forgotten
	count := func() int {
		n := 0
		for _, q := range s.Requests() {
			if q.Path == "/v1/desks/"+rotatedDesk+"/stats" {
				n++
			}
		}
		return n
	}
	n := count()
	if r := must[*StatsReport](t)(c.Stats(cx, rotatedDesk)); r.CPUPercent != 5 || count() != n+2 {
		t.Fatalf("sealed again once: %d", count()-n)
	}
}

func TestE2EPinnedKeys(t *testing.T) {
	s := e2eMock(t)
	cx := ctx(t)
	before := len(s.Requests())
	_, err := newClient(t, s, WithE2EKeys(map[string]string{sealedDesk: pubOf(t, key2)})).Exec(cx, sealedDesk, ExecRequest{Command: "echo " + canary})
	if e := errOf(t, err); !errors.Is(err, ErrE2E) || e.Reason != ReasonE2EKeyMismatch {
		t.Fatalf("%v", err)
	}
	for _, q := range s.Requests()[before:] {
		if q.Path != "/v1/desks/"+sealedDesk {
			t.Fatalf("only the lookup: %s", q.Path)
		}
	}
	_ = must[*StatsReport](t)(newClient(t, s, WithE2EKeys(map[string]string{sealedDesk: pubOf(t, key1)})).Stats(cx, sealedDesk))
	// goneDesk lists no key while offline: the pinned key seals anyway.
	c := newClient(t, s, WithE2E(E2ERequire), WithE2EKeys(map[string]string{goneDesk: pubOf(t, key1)}))
	r, err := blind(t, s, func() (*ExecResult, error) { return c.Exec(cx, goneDesk, ExecRequest{Command: "echo " + canary}) })
	if err != nil || r.Exit != 0 {
		t.Fatalf("%v", err)
	}
	_, err = New("ak_x", WithE2EKeys(map[string]string{sealedDesk: "short"}))
	isUsage(t, err)
	_, err = New("ak_x", WithE2E("always"))
	isUsage(t, err)
	_, err = NewLocal(WithE2E(E2ERequire))
	isUsage(t, err)
}

func TestE2EHostileServer(t *testing.T) {
	s := e2eMock(t)
	cx := ctx(t)
	c := newClient(t, s)
	s.SetTamper("flip")
	_, err := c.Stats(cx, sealedDesk)
	if e := errOf(t, err); e.Class != ClassProtocol || e.Reason != ReasonE2EDecryptFailed {
		t.Fatalf("%v", err)
	}
	d, err := drain(c.ExecStream(cx, sealedDesk, ExecRequest{Command: "x"}))
	if err != nil || d.Exit.Error == nil || d.Exit.Error.Kind != "protocol" {
		t.Fatalf("%v %+v", err, d.Exit)
	}
	_, err = c.Exec(cx, sealedDesk, ExecRequest{Command: "refuse"})
	if e := errOf(t, err); e.Class != ClassRefused || !strings.Contains(e.Message, "did not open") {
		t.Fatalf("the placeholder, said to be unopened: %v", err)
	}
	s.SetTamper("plaintext")
	_, err = c.Stats(cx, sealedDesk)
	if e := errOf(t, err); e.Class != ClassProtocol || e.Reason != ReasonE2EUnsealedAnswer {
		t.Fatalf("%v", err)
	}
	p, err := drain(c.ExecStream(cx, sealedDesk, ExecRequest{Command: "x"}))
	if err != nil || p.Out != "" || p.Exit.Error == nil || p.Exit.Error.Kind != "protocol" {
		t.Fatalf("no forged output: %v %+v", err, p)
	}
	s.SetTamper("")
	if r := must[*StatsReport](t)(c.Stats(cx, sealedDesk)); r.CPUPercent != 5 {
		t.Fatal("honest again, fine again")
	}
}

func TestE2EAdminExec(t *testing.T) {
	s := e2eMock(t)
	cx := ctx(t)
	// No admin scope on the token: refused (200, exit 254) as a typed error.
	e := sameError(t, s, func(c *Client) error {
		_, err := c.Exec(cx, sealedDesk, ExecRequest{Command: "id -u", Admin: true})
		return err
	})
	if e.Class != ClassRefused || e.Reason != ReasonAdminScopeMissing || e.ExitCode != 254 {
		t.Fatalf("%+v", e)
	}
	// The scope, but the desk's switch is off.
	e = sameError(t, s, func(c *Client) error {
		_, err := c.Exec(cx, sealedDesk, ExecRequest{Command: "id -u", Admin: true}, UseDeskToken("gdagt_admin"))
		return err
	})
	if e.Reason != ReasonAdminNotEnabled {
		t.Fatalf("%+v", e)
	}
	s.Desk(sealedDesk).AdminEnabled = true
	r := same(t, s, func(c *Client) (*ExecResult, error) {
		return c.Exec(cx, sealedDesk, ExecRequest{Command: "id -u", Admin: true}, UseDeskToken("gdagt_admin"))
	})
	if !strings.Contains(r.Stdout, "(as administrator) id -u") {
		t.Fatalf("%q", r.Stdout)
	}
	var sent map[string]any
	for _, q := range s.Requests() {
		if strings.HasSuffix(q.Path, "/exec") && q.Header.Get("GaiaDesk-E2E") == "" && !bytes.Contains(q.Body, []byte(`"e2e"`)) {
			_ = json.Unmarshal(q.Body, &sent)
		}
	}
	if sent["admin"] != true {
		t.Fatalf("admin not in the plaintext spec: %v", sent)
	}
	// A confined token cannot be minted with admin.
	_, err := owner(t, s).CreateToken(cx, TokenRequest{Desks: []string{sealedDesk}, Name: "x", Scopes: []string{ScopeAdmin}, Cwd: "/srv"})
	isUsage(t, err)
}
