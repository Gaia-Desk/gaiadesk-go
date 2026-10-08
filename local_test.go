package gaiadesk

// The local transport (the desk's own /v1 over its Unix socket) and the lan
// transport (its LAN gateway over pinned TLS): defaults, credentials, the
// same operations and errors, and what they do not serve.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Gaia-Desk/gaiadesk-go/internal/mockapi"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestPipeUserAndPaths(t *testing.T) {
	for in, want := range map[string]string{"Charlie": "charlie", "Charlie Brown": "charlie_brown", `DOMAIN\Ana.Lee-2`: "domain_ana.lee-2", "Élodie": "_lodie", strings.Repeat("x", 100): strings.Repeat("x", 64), "": "user"} {
		if got := PipeUser(in); got != want {
			t.Fatalf("%q: %q", in, got)
		}
	}
	if LocalPipeName(env(map[string]string{"USERNAME": "Bob Smith"}), "") != `\\.\pipe\gaiadesk-api-bob_smith` ||
		LocalPipeName(env(nil), "Alice") != `\\.\pipe\gaiadesk-api-alice` ||
		LocalPipeName(env(map[string]string{"USERNAME": "bob"}), "alice") != `\\.\pipe\gaiadesk-api-bob` ||
		LocalPipeName(env(nil), "") != `\\.\pipe\gaiadesk-api-user` ||
		LocalPipeName(env(map[string]string{"GAIADESK_API_PIPE": `\\.\pipe\custom`}), "bob") != `\\.\pipe\custom` {
		t.Fatal("pipe names")
	}
	checks := [][2]string{
		{LocalSocketPath(env(nil), "/home/ana", "linux"), "/home/ana/.gaiadesk/api.sock"},
		{LocalTokenPath(env(nil), "/Users/ana", "darwin"), "/Users/ana/.gaiadesk/api-token"},
		{LocalSocketPath(env(map[string]string{"GAIADESK_API_DIR": "/run/gd/"}), "/home/ana", "linux"), "/run/gd/api.sock"},
		{LocalAPIDir(env(map[string]string{"GAIADESK_API_DIR": "relative/dir"}), "/home/ana", "linux"), "/home/ana/.gaiadesk"},
		{LocalTokenPath(env(nil), `C:\Users\ana`, "windows"), `C:\Users\ana\.gaiadesk\api-token`},
		{LocalTokenPath(env(map[string]string{"GAIADESK_API_DIR": `D:\gd`}), `C:\Users\ana`, "windows"), `D:\gd\api-token`},
	}
	for _, c := range checks {
		if c[0] != c[1] {
			t.Fatalf("%q, want %q", c[0], c[1])
		}
	}
}

func TestNewLocalValidates(t *testing.T) {
	for _, f := range []func() (*Client, error){
		func() (*Client, error) { return NewLocal(WithBaseURL("http://x/v1")) },
		func() (*Client, error) { return NewLocal(WithE2E(E2EOff)) },
		func() (*Client, error) { return NewLocal(WithAdminToken("")) },
		func() (*Client, error) { return NewLocal(WithSocketPath(" ")) },
		func() (*Client, error) { return NewLocal(WithHTTPClient(nil)) },
	} {
		_, err := f()
		isUsage(t, err)
	}
	c := must[*Client](t)(NewLocal())
	if c.Transport() != TransportLocal {
		t.Fatal(c.Transport())
	}
}

const adminToken = "gdlocal_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func localMock(t *testing.T) (*mockapi.Server, string) {
	if runtime.GOOS == "windows" {
		t.Skip("the Unix-socket mock; the named pipe is dialled the same way through net/http")
	}
	// A short directory: Unix socket paths are limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "gd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	_ = os.WriteFile(filepath.Join(dir, "api-token"), []byte(adminToken+"\n"), 0o600)
	l, err := net.Listen("unix", filepath.Join(dir, "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	s := mockapi.NewOn(l, map[string]*mockapi.Desk{okDesk: {}}, mockapi.Local)
	t.Cleanup(s.Close)
	return s, dir
}

func TestLocalTransport(t *testing.T) {
	s, dir := localMock(t)
	cx := ctx(t)
	c := must[*Client](t)(NewLocal(WithEnv(map[string]string{"GAIADESK_API_DIR": dir})))
	r := must[*ExecResult](t)(c.Exec(cx, okDesk, ExecRequest{Command: "hostname"}))
	if r.Exit != 0 || r.Stdout != "ran: hostname é\n" {
		t.Fatalf("%+v", r)
	}
	if l := s.Last(); l.Header.Get("Authorization") != "Bearer "+adminToken || l.Header.Get("X-GaiaDesk-Desk-Token") != "" {
		t.Fatalf("the admin token from its file: %+v", l.Header)
	}
	// An agent token speaks instead.
	agent := must[*Client](t)(NewLocal(WithEnv(map[string]string{"GAIADESK_API_DIR": dir}), WithDeskToken("gdagt_x")))
	_ = must[*StatsReport](t)(agent.Stats(cx, okDesk))
	if l := s.Last(); l.Header.Get("X-GaiaDesk-Desk-Token") != "gdagt_x" || l.Header.Get("Authorization") != "" {
		t.Fatalf("%+v", l.Header)
	}
	// The same streams, files, waits and desks.
	st := must[*Stream](t)(c.ExecStream(cx, okDesk, ExecRequest{Command: "x"}))
	if x := st.Wait(); x.ExitCode != 0 {
		t.Fatalf("%+v", x)
	}
	_ = must[*CopyResult](t)(c.Upload(cx, okDesk, "a.txt", strings.NewReader("hi")))
	if b := must[[]byte](t)(c.DownloadBytes(cx, okDesk, "a.txt")); string(b) != "hi" {
		t.Fatal(string(b))
	}
	if l := must[*DeskList](t)(c.Desks(cx)); len(l.Devices) != 1 {
		t.Fatal(l)
	}
	_ = must[*JobWaitResult](t)(c.WaitJob(cx, okDesk, "held", WaitForever))
	// Hosted-only routes are refused without a request.
	n := len(s.Requests())
	_, err := c.Desk(cx, okDesk)
	isUsage(t, err)
	_, err = c.Audit(cx, nil)
	isUsage(t, err)
	_, err = c.Webhooks(cx)
	isUsage(t, err)
	_, err = c.SupportSessions(cx, nil)
	isUsage(t, err)
	_, err = c.Wake(cx, okDesk, 0)
	isUsage(t, err)
	if len(s.Requests()) != n {
		t.Fatal("a hosted-only route was sent")
	}
	// A wrong admin token is the desk's refusal.
	bad := must[*Client](t)(NewLocal(WithSocketPath(filepath.Join(dir, "api.sock")), WithAdminToken("nope")))
	_, err = bad.Stats(cx, okDesk)
	if e := errOf(t, err); e.Reason != ReasonUnauthenticated || e.Status != 401 {
		t.Fatal(err)
	}
}

func TestLocalUnavailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket paths")
	}
	dir := t.TempDir()
	cx := ctx(t)
	c := must[*Client](t)(NewLocal(WithEnv(map[string]string{"GAIADESK_API_DIR": dir}), WithAdminToken("gdlocal_x"), WithRetry(NoRetry)))
	_, err := c.Stats(cx, okDesk)
	if e := errOf(t, err); !errors.Is(err, ErrUnreachable) || e.Reason != ReasonLocalAPIUnavailable || !strings.Contains(e.Message, "Local API on") || !strings.Contains(e.Message, dir) {
		t.Fatalf("%+v", e)
	}
	// No token file and no agent token.
	c = must[*Client](t)(NewLocal(WithEnv(map[string]string{"GAIADESK_API_DIR": dir}), WithRetry(NoRetry)))
	_, err = c.Stats(cx, okDesk)
	if e := errOf(t, err); e.Reason != ReasonLocalAPIUnavailable || !strings.Contains(e.Message, "no local admin token") {
		t.Fatalf("%+v", e)
	}
	_ = os.WriteFile(filepath.Join(dir, "api-token"), []byte("  \n"), 0o600)
	_, err = c.Stats(cx, okDesk)
	if e := errOf(t, err); e.Kind != KindLocal || !strings.Contains(e.Message, "empty") {
		t.Fatalf("%+v", e)
	}
}

func fingerprint(cert []byte) string {
	sum := sha256.Sum256(cert)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func TestNormalizeFingerprint(t *testing.T) {
	want := strings.TrimSuffix(strings.Repeat("ab:", 32), ":")
	for _, in := range []string{strings.Repeat("AB", 32), strings.Repeat("AB:", 31) + "AB", "SHA256:" + strings.Repeat("ab", 32), "sha-256=" + strings.Repeat("ab ", 32)} {
		if got := must[string](t)(NormalizeFingerprint(in)); got != want {
			t.Fatalf("%q: %q", in, got)
		}
	}
	for _, bad := range []string{"", "ab", strings.Repeat("zz", 32)} {
		_, err := NormalizeFingerprint(bad)
		isUsage(t, err)
	}
}

func TestLANTransport(t *testing.T) {
	s := mockapi.NewTLS(map[string]*mockapi.Desk{okDesk: {}})
	t.Cleanup(s.Close)
	cx := ctx(t)
	fp := fingerprint(s.Certificate())
	c := must[*Client](t)(NewLAN(s.URL, fp, WithDeskToken("gdagt_lan")))
	if c.Transport() != TransportLAN {
		t.Fatal(c.Transport())
	}
	r := must[*ExecResult](t)(c.Exec(cx, okDesk, ExecRequest{Command: "uname"}))
	if r.Exit != 0 || s.Last().Header.Get("X-GaiaDesk-Desk-Token") != "gdagt_lan" || s.Last().Header.Get("Authorization") != "" {
		t.Fatalf("%+v", s.Last().Header)
	}
	st := must[*Stream](t)(c.FollowJobLogs(cx, okDesk, "build", nil))
	if x := st.Wait(); x.ExitCode != 0 {
		t.Fatal(x)
	}
	// Another certificate: refused before any request is written.
	other := strings.Repeat("25", 32)
	bad := must[*Client](t)(NewLAN(s.URL, other, WithDeskToken("gdagt_lan"), WithRetry(NoRetry)))
	n, conns := len(s.Requests()), s.Connections()
	_, err := bad.Stats(cx, okDesk)
	var fpe *FingerprintMismatchError
	if !errors.As(err, &fpe) || !errors.Is(err, ErrFingerprintMismatch) || !errors.Is(err, ErrUnreachable) || fpe.Err.Reason != ReasonFingerprintMismatch {
		t.Fatalf("%T %v", err, err)
	}
	if fpe.Actual != strings.ToLower(must[string](t)(NormalizeFingerprint(fp))) || !strings.HasPrefix(fpe.Expected, "25:25") || !strings.Contains(fpe.Error(), "Do not proceed") {
		t.Fatalf("%+v", fpe)
	}
	if len(s.Requests()) != n || s.Connections() == conns {
		t.Fatal("a request reached a server whose certificate did not match (or no handshake)")
	}
	// Agent tokens only.
	noToken := must[*Client](t)(NewLAN(s.URL, fp))
	_, err = noToken.Stats(cx, okDesk)
	isUsage(t, err)
	_, err = noToken.Stats(cx, okDesk, UseDeskToken("gdagt_call"))
	if err != nil || s.Last().Header.Get("X-GaiaDesk-Desk-Token") != "gdagt_call" {
		t.Fatal(err)
	}
	for _, f := range []func() (*Client, error){
		func() (*Client, error) { return NewLAN("http://x:7443/v1", fp) },
		func() (*Client, error) { return NewLAN(s.URL, "") },
		func() (*Client, error) { return NewLAN(s.URL, "abc") },
		func() (*Client, error) { return NewLAN(s.URL, fp, WithE2E(E2EAuto)) },
		func() (*Client, error) { return NewLAN(s.URL, fp, WithAdminToken("gdlocal_x")) },
	} {
		_, err := f()
		isUsage(t, err)
	}
}
