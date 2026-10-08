package gaiadesk

// A server or proxy that drops or stalls a connection, on a raw socket
// (rawserver_test.go): the SDK fails with a typed transport error within
// its timeouts, retries only where the policy allows, never re-sends a
// request that changes something (nor lets net/http do it), and never hangs.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const rawDesk = "123456789"

// rawBound is how long any one call may take before the test calls it a
// hang: it fails, rather than the suite sitting stuck.
const rawBound = 10 * time.Second

// rawClient is a hosted-API client of s: e2e off, retries with a 5 ms base
// delay, the given idle and response timeouts.
func rawClient(t *testing.T, s *rawServer, retries int, idle, response time.Duration) *Client {
	t.Helper()
	c, err := New("ak_t", WithBaseURL(s.url), WithDeskToken("gdagt_t"), WithE2E(E2EOff), WithWarningHandler(quiet),
		WithRetry(RetryPolicy{MaxAttempts: retries + 1, BaseDelay: 5 * time.Millisecond, MaxDelay: time.Second}),
		WithIdleTimeout(idle), WithResponseTimeout(response))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// within runs f, failing the test if it has not returned within rawBound.
func within[T any](t *testing.T, what string, f func() (T, error)) (T, time.Duration, error) {
	t.Helper()
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	start := time.Now()
	go func() {
		v, err := f()
		ch <- result{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, time.Since(start), r.err
	case <-time.After(rawBound):
		t.Fatalf("%s: no answer within %v: the SDK hung", what, rawBound)
	}
	panic("unreachable")
}

// fails runs f within the bound and returns its *Error, which must be of class.
func fails[T any](t *testing.T, what string, class Class, f func() (T, error)) (*Error, time.Duration) {
	t.Helper()
	_, took, err := within(t, what, f)
	e := errOf(t, err)
	if e.Class != class {
		t.Fatalf("%s: class %q (kind %q), want %q: %v", what, e.Class, e.Kind, class, err)
	}
	return e, took
}

func wantKind(t *testing.T, what string, e *Error, kind ErrorKind, reason string) {
	t.Helper()
	if e.Kind != kind || e.Reason != reason {
		t.Fatalf("%s: kind %q reason %q, want %q %q: %v", what, e.Kind, e.Reason, kind, reason, e)
	}
}

func wantCount(t *testing.T, s *rawServer, method string, lo, hi int) {
	t.Helper()
	if n := s.count(method); n < lo || n > hi {
		t.Fatalf("the server saw %d %s request(s), want %d..%d", n, method, lo, hi)
	}
}

var rawModeNames = map[rawMode]string{
	rawCloseBeforeResponse: "CloseBeforeResponse", rawResetBeforeResponse: "ResetBeforeResponse", rawCloseAfterBody: "CloseAfterBody",
}

func TestRawDroppedBeforeAnyResponseByte_AReadIsRetried_ThenUnreachableNetwork(t *testing.T) {
	for _, mode := range []rawMode{rawCloseBeforeResponse, rawResetBeforeResponse} {
		t.Run(rawModeNames[mode], func(t *testing.T) {
			s := newRawServer(t, mode)
			c := rawClient(t, s, 2, time.Second, 30*time.Second)
			bg := context.Background()
			e, _ := fails(t, "DownloadBytes", ClassUnreachable, func() ([]byte, error) { return c.DownloadBytes(bg, rawDesk, "/tmp/x") })
			wantKind(t, "DownloadBytes", e, KindNetwork, ReasonNetwork)
			if e.Op != "GET /desks/123456789/files" {
				t.Fatalf("op %q", e.Op)
			}
			// The first try and the SDK's two retries: a GET is safe to send
			// again. (net/http itself re-sends a GET only on a REUSED
			// connection; every connection here is fresh, so exactly 3.)
			wantCount(t, s, "GET", 3, 3)
			fails(t, "Stats", ClassUnreachable, func() (*StatsReport, error) { return c.Stats(bg, rawDesk) })
			wantCount(t, s, "GET", 6, 6)
			once := rawClient(t, s, 0, time.Second, 30*time.Second)
			fails(t, "Stats once", ClassUnreachable, func() (*StatsReport, error) { return once.Stats(bg, rawDesk) })
			wantCount(t, s, "GET", 7, 7)
		})
	}
}

// nonSeeker hides a reader's Seek: the SDK reads it into memory first.
type nonSeeker struct{ io.Reader }

func TestRawDroppedBeforeAnyResponseByte_AnUploadOrAnExecIsNeverSentTwice(t *testing.T) {
	for _, mode := range []rawMode{rawCloseBeforeResponse, rawResetBeforeResponse, rawCloseAfterBody} {
		t.Run(rawModeNames[mode], func(t *testing.T) {
			s := newRawServer(t, mode)
			c := rawClient(t, s, 2, time.Second, 30*time.Second)
			bg := context.Background()
			big := make([]byte, 4<<20)
			e, _ := fails(t, "Upload (streamed)", ClassUnreachable, func() (*CopyResult, error) {
				return c.Upload(bg, rawDesk, "/tmp/big", bytes.NewReader(big))
			})
			wantKind(t, "Upload", e, KindNetwork, ReasonNetwork)
			wantCount(t, s, "PUT", 1, 1)
			fails(t, "Upload (in memory)", ClassUnreachable, func() (*CopyResult, error) {
				return c.Upload(bg, rawDesk, "/tmp/mem", nonSeeker{bytes.NewReader(big)})
			})
			wantCount(t, s, "PUT", 2, 2)
			fails(t, "Exec", ClassUnreachable, func() (*ExecResult, error) { return c.Exec(bg, rawDesk, ExecRequest{Command: "deploy"}) })
			wantCount(t, s, "POST", 1, 1)
			// An idempotency key does not make the SDK send it again either.
			fails(t, "Exec with a key", ClassUnreachable, func() (*ExecResult, error) {
				return c.Exec(bg, rawDesk, ExecRequest{Command: "deploy"}, WithIdempotencyKey("k-1"))
			})
			wantCount(t, s, "POST", 2, 2)
			e, _ = fails(t, "ExecStream", ClassUnreachable, func() (*Stream, error) { return c.ExecStream(bg, rawDesk, ExecRequest{Command: "deploy"}) })
			wantKind(t, "ExecStream", e, KindNetwork, ReasonNetwork)
			wantCount(t, s, "POST", 3, 3)
			fails(t, "StartJob", ClassUnreachable, func() (*Job, error) {
				return c.StartJob(bg, rawDesk, JobRequest{Name: "nightly", Command: "make"})
			})
			wantCount(t, s, "POST", 4, 4)
			wantCount(t, s, "GET", 0, 0)
		})
	}
}

// net/http re-sends a request on a pooled connection that died before any
// answer when it thinks the request idempotent: a GET, or ANY method with an
// Idempotency-Key header and a rewindable body (GetBody). The SDK must stop
// it for a request that changes something.
func TestRawPooledConnectionDies_NetHTTPNeverResendsAMutatingRequest(t *testing.T) {
	s := newRawServer(t, rawKeepAliveThenClose)
	c := rawClient(t, s, 0, time.Second, 30*time.Second)
	bg := context.Background()
	// Warm a pooled connection (answered, kept alive) ...
	if _, _, err := within(t, "Stats", func() (*StatsReport, error) { return c.Stats(bg, rawDesk) }); err != nil {
		t.Fatal(err)
	}
	// ... then an exec with an idempotency key on it: the server reads it and
	// closes. net/http must not send it again on a fresh connection (which
	// would answer: the command would have run twice, silently).
	e, _ := fails(t, "Exec with a key", ClassUnreachable, func() (*ExecResult, error) {
		return c.Exec(bg, rawDesk, ExecRequest{Command: "deploy"}, WithIdempotencyKey("k-1"))
	})
	wantKind(t, "Exec", e, KindNetwork, ReasonNetwork)
	wantCount(t, s, "POST", 1, 1)
	// The same for a job and an upload.
	if _, _, err := within(t, "Stats", func() (*StatsReport, error) { return c.Stats(bg, rawDesk) }); err != nil {
		t.Fatal(err)
	}
	fails(t, "StartJob with a key", ClassUnreachable, func() (*Job, error) {
		return c.StartJob(bg, rawDesk, JobRequest{Name: "nightly", Command: "make"}, WithIdempotencyKey("k-2"))
	})
	wantCount(t, s, "POST", 2, 2)
	if _, _, err := within(t, "Stats", func() (*StatsReport, error) { return c.Stats(bg, rawDesk) }); err != nil {
		t.Fatal(err)
	}
	fails(t, "Upload", ClassUnreachable, func() (*CopyResult, error) {
		return c.Upload(bg, rawDesk, "/tmp/up", bytes.NewReader(make([]byte, 64<<10)))
	})
	wantCount(t, s, "PUT", 1, 1)
	// A GET is safe to send again, and net/http does: the stats call on the
	// dead pooled connection is answered by a fresh one, with retries off.
	gets := s.count("GET")
	if _, _, err := within(t, "Stats", func() (*StatsReport, error) { return c.Stats(bg, rawDesk) }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := within(t, "Stats (re-sent by net/http)", func() (*StatsReport, error) { return c.Stats(bg, rawDesk) }); err != nil {
		t.Fatalf("net/http no longer re-sends a GET on a dead pooled connection (update the README): %v", err)
	}
	wantCount(t, s, "GET", gets+3, gets+3)
}

func TestRawStalledMidDownload_ConnectionLostTimeout_NoPartialFile(t *testing.T) {
	s := newRawServer(t, rawStallMidBody)
	c := rawClient(t, s, 2, time.Second, 30*time.Second)
	bg := context.Background()
	body, _, err := within(t, "OpenDownload", func() (io.ReadCloser, error) { return c.OpenDownload(bg, rawDesk, "/tmp/x") })
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	buf := make([]byte, 16)
	got, _, err := within(t, "Read", func() (int, error) { return io.ReadAtLeast(body, buf, 5) })
	if err != nil || string(buf[:got]) != "hello" {
		t.Fatalf("read %q, %v", buf[:got], err)
	}
	e, took := fails(t, "Read (stalled)", ClassConnectionLost, func() (int, error) { return body.Read(buf) })
	wantKind(t, "Read", e, KindTimeout, "timeout")
	if !strings.Contains(e.Message, "WithIdleTimeout") || e.ExitCode != 255 || took > 5*time.Second {
		t.Fatalf("message %q exit %d took %v", e.Message, e.ExitCode, took)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "stalled")
	e, _ = fails(t, "DownloadFile", ClassConnectionLost, func() (*CopyResult, error) { return c.DownloadFile(bg, rawDesk, "/tmp/x", file) })
	wantKind(t, "DownloadFile", e, KindTimeout, "timeout")
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("a failed download left %v behind", left)
	}
	e, _ = fails(t, "DownloadBytes", ClassConnectionLost, func() ([]byte, error) { return c.DownloadBytes(bg, rawDesk, "/tmp/x") })
	wantKind(t, "DownloadBytes", e, KindTimeout, "timeout")
	wantCount(t, s, "GET", 3, 3) // a timeout after the answer began is not retried
}

func TestRawStalledMidJSON_ConnectionLostTimeout(t *testing.T) {
	s := newRawServer(t, rawStallMidJSON)
	c := rawClient(t, s, 0, time.Second, 30*time.Second)
	e, took := fails(t, "Stats", ClassConnectionLost, func() (*StatsReport, error) { return c.Stats(context.Background(), rawDesk) })
	wantKind(t, "Stats", e, KindTimeout, "timeout")
	if !strings.Contains(e.Message, "WithIdleTimeout") || took > 5*time.Second {
		t.Fatalf("message %q took %v", e.Message, took)
	}
	retrying := rawClient(t, s, 2, time.Second, 30*time.Second)
	fails(t, "Stats (retries on)", ClassConnectionLost, func() (*StatsReport, error) { return retrying.Stats(context.Background(), rawDesk) })
	wantCount(t, s, "GET", 2, 2)
}

func TestRawStalledMidStream_TheStreamEndsWithATimeoutError(t *testing.T) {
	s := newRawServer(t, rawStallMidEvents)
	c := rawClient(t, s, 2, time.Second, 30*time.Second)
	bg := context.Background()
	st, _, err := within(t, "ExecStream", func() (*Stream, error) { return c.ExecStream(bg, rawDesk, ExecRequest{Command: "tail -f log"}) })
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	exit, _, err := within(t, "Copy", func() (*Exit, error) { return st.Copy(&stdout, io.Discard) })
	if err != nil || stdout.String() != "hi" {
		t.Fatalf("stdout %q, %v", stdout.String(), err)
	}
	if exit.Error == nil || exit.Error.Kind != "connection_lost" || exit.Error.Reason != "timeout" || exit.ExitCode != 255 {
		t.Fatalf("exit %+v error %+v", exit, exit.Error)
	}
	if !errors.Is(exit.Err(), ErrConnectionLost) {
		t.Fatalf("Exit.Err() %v", exit.Err())
	}
	logs, _, err := within(t, "FollowJobLogs", func() (*Stream, error) { return c.FollowJobLogs(bg, rawDesk, "build", nil) })
	if err != nil {
		t.Fatal(err)
	}
	lexit, _, _ := within(t, "Wait", func() (*Exit, error) { return logs.Wait(), nil })
	if lexit.Error == nil || lexit.Error.Kind != "connection_lost" || lexit.Error.Reason != "timeout" {
		t.Fatalf("logs exit %+v", lexit)
	}
}

func TestRawSilentServer_UnreachableTimeout_NotRetried(t *testing.T) {
	s := newRawServer(t, rawSilent)
	c := rawClient(t, s, 2, time.Second, time.Second)
	bg := context.Background()
	e, took := fails(t, "Stats", ClassUnreachable, func() (*StatsReport, error) { return c.Stats(bg, rawDesk) })
	wantKind(t, "Stats", e, KindTimeout, "timeout")
	if !strings.Contains(e.Message, "WithResponseTimeout") || took > 5*time.Second {
		t.Fatalf("message %q took %v", e.Message, took)
	}
	// The body is never read: the response timeout bounds sending it too.
	e, _ = fails(t, "Upload", ClassUnreachable, func() (*CopyResult, error) {
		return c.Upload(bg, rawDesk, "/tmp/big", bytes.NewReader(make([]byte, 4<<20)))
	})
	wantKind(t, "Upload", e, KindTimeout, "timeout")
	wantCount(t, s, "GET", 1, 1)
	wantCount(t, s, "PUT", 1, 1)
	// The caller's cancellation still wins, and promptly.
	patient := rawClient(t, s, 2, time.Second, 10*time.Minute)
	cctx, cancel := context.WithTimeout(bg, 200*time.Millisecond)
	defer cancel()
	_, took, err := within(t, "Stats (cancelled)", func() (*StatsReport, error) { return patient.Stats(cctx, rawDesk) })
	if !errors.Is(err, ErrInterrupted) || took > 5*time.Second {
		t.Fatalf("%v after %v", err, took)
	}
}

func TestRawStress_300DroppedRequests_NeverHang(t *testing.T) {
	s := newRawServer(t, rawCloseBeforeResponse)
	c := rawClient(t, s, 1, time.Second, 30*time.Second)
	bg := context.Background()
	up := make([]byte, 512<<10)
	modes := []rawMode{rawCloseBeforeResponse, rawResetBeforeResponse, rawCloseAfterBody}
	for i := range 300 {
		s.setMode(modes[i%3])
		var err error
		if i%2 == 0 {
			_, _, err = within(t, "DownloadBytes", func() ([]byte, error) { return c.DownloadBytes(bg, rawDesk, "/tmp/x") })
		} else {
			_, _, err = within(t, "Upload", func() (*CopyResult, error) { return c.Upload(bg, rawDesk, "/tmp/up", bytes.NewReader(up)) })
		}
		e := errOf(t, err)
		if e.Class != ClassUnreachable || e.Kind != KindNetwork {
			t.Fatalf("iteration %d (%s): %v", i, rawModeNames[modes[i%3]], err)
		}
	}
	wantCount(t, s, "PUT", 150, 150) // every upload sent exactly once
	wantCount(t, s, "GET", 300, 600) // every read tried twice (plus any re-send by net/http)
}

func TestTimeoutsAreChecked(t *testing.T) {
	for _, o := range []Option{WithIdleTimeout(-time.Nanosecond), WithResponseTimeout(-2 * time.Second)} {
		_, err := New("ak", o)
		isUsage(t, err)
		_, err = NewLocal(o)
		isUsage(t, err)
		_, err = NewLAN("https://192.168.1.20:8443/v1", strings.Repeat("ab", 32), WithDeskToken("gdagt_t"), o)
		isUsage(t, err)
	}
	// 0 is no limit.
	if _, err := New("ak", WithIdleTimeout(0), WithResponseTimeout(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocal(WithIdleTimeout(time.Second), WithResponseTimeout(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLAN("https://192.168.1.20:8443/v1", strings.Repeat("ab", 32), WithDeskToken("gdagt_t"), WithIdleTimeout(time.Second)); err != nil {
		t.Fatal(err)
	}
}
