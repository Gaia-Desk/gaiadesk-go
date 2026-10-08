package gaiadesk

// The retry rule, on a raw socket (rawserver_test.go): a request is sent
// again only when that cannot run anything twice.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRetryConnectionNeverMade_AnyMethodRetried(t *testing.T) {
	// A port that refuses, then a server on it ~100 ms later.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	bg := context.Background()
	// Retries off: refused at once, unreachable/network.
	off, err := New("ak_t", WithBaseURL("http://"+addr+"/v1"), WithDeskToken("gdagt_t"), WithE2E(E2EOff), WithWarningHandler(quiet), WithRetry(NoRetry))
	if err != nil {
		t.Fatal(err)
	}
	e, took := fails(t, "Exec (retries off)", ClassUnreachable, func() (*ExecResult, error) { return off.Exec(bg, rawDesk, ExecRequest{Command: "deploy"}) })
	wantKind(t, "Exec", e, KindNetwork, ReasonNetwork)
	if took > 2*time.Second {
		t.Fatalf("took %v", took)
	}
	// Backoff 100 ms: the retries wait 50–100 ms then 100–200 ms, past the
	// server's arrival.
	c, err := New("ak_t", WithBaseURL("http://"+addr+"/v1"), WithDeskToken("gdagt_t"), WithE2E(E2EOff), WithWarningHandler(quiet),
		WithRetry(RetryPolicy{MaxAttempts: 3, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}))
	if err != nil {
		t.Fatal(err)
	}
	srv := make(chan *rawServer, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		srv <- newRawServerOn(t, addr, rawKeepAliveThenClose)
	}()
	r, _, err := within(t, "Exec", func() (*ExecResult, error) { return c.Exec(bg, rawDesk, ExecRequest{Command: "deploy"}) })
	if err != nil || r.Exit != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	wantCount(t, <-srv, "POST", 1, 1)
}

func TestRetryDroppedBeforeAnyResponseByte_ADeleteIsSentOnce(t *testing.T) {
	for _, mode := range []rawMode{rawCloseBeforeResponse, rawResetBeforeResponse, rawCloseAfterBody} {
		t.Run(rawModeNames[mode], func(t *testing.T) {
			s := newRawServer(t, mode)
			c := rawClient(t, s, 2, time.Second, 30*time.Second)
			e, _ := fails(t, "KillJob", ClassUnreachable, func() (*Job, error) { return c.KillJob(context.Background(), rawDesk, "nightly") })
			wantKind(t, "KillJob", e, KindNetwork, ReasonNetwork)
			wantCount(t, s, "DELETE", 1, 1)
			fails(t, "DeleteWebhook", ClassUnreachable, func() (any, error) { return nil, c.DeleteWebhook(context.Background(), "wh_1") })
			wantCount(t, s, "DELETE", 2, 2)
		})
	}
}

func TestRetryStatuses(t *testing.T) {
	bg := context.Background()
	stats := func(c *Client) func() (*StatsReport, error) {
		return func() (*StatsReport, error) { return c.Stats(bg, rawDesk) }
	}
	exec := func(c *Client) func() (*ExecResult, error) {
		return func() (*ExecResult, error) { return c.Exec(bg, rawDesk, ExecRequest{Command: "deploy"}) }
	}
	for _, code := range []int{502, 503, 504} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			s := newRawServer(t, rawStatus)
			s.setStatus(code, "", "")
			c := rawClient(t, s, 2, time.Second, 30*time.Second)
			if e, _ := fails(t, "Stats", ClassUnreachable, stats(c)); e.Status != code {
				t.Fatal(e)
			}
			wantCount(t, s, "GET", 3, 3)
			fails(t, "Exec", ClassUnreachable, exec(c))
			wantCount(t, s, "POST", 1, 1)
		})
	}
	t.Run("503 switched off", func(t *testing.T) {
		s := newRawServer(t, rawStatus)
		s.setStatus(503, "", ReasonDeskOpsDisabled)
		fails(t, "Stats", ClassUnreachable, stats(rawClient(t, s, 2, time.Second, 30*time.Second)))
		wantCount(t, s, "GET", 1, 1)
	})
	t.Run("503 Retry-After 1", func(t *testing.T) {
		s := newRawServer(t, rawStatus)
		s.setStatus(503, "1", "")
		_, took := fails(t, "Stats", ClassUnreachable, stats(rawClient(t, s, 1, time.Second, 30*time.Second)))
		wantCount(t, s, "GET", 2, 2)
		if took < 900*time.Millisecond {
			t.Fatalf("Retry-After: 1 not waited (%v)", took)
		}
	})
	t.Run("429 Retry-After 0", func(t *testing.T) {
		s := newRawServer(t, rawStatus)
		s.setStatus(429, "0", ReasonRateLimited)
		if e, _ := fails(t, "Exec", ClassRefused, exec(rawClient(t, s, 2, time.Second, 30*time.Second))); e.Status != 429 {
			t.Fatal(e)
		}
		wantCount(t, s, "POST", 3, 3)
	})
	t.Run("429 Retry-After 120", func(t *testing.T) {
		s := newRawServer(t, rawStatus)
		s.setStatus(429, "120", ReasonRateLimited)
		e, took := fails(t, "Exec", ClassRefused, exec(rawClient(t, s, 2, time.Second, 30*time.Second)))
		if e.RetryAfter != 120*time.Second || took > time.Second {
			t.Fatalf("retry-after %v, took %v", e.RetryAfter, took)
		}
		wantCount(t, s, "POST", 1, 1)
	})
	t.Run("409 idempotency_key_in_flight", func(t *testing.T) {
		s := newRawServer(t, rawStatus)
		s.setStatus(409, "", ReasonIdempotencyInFlight)
		c := rawClient(t, s, 2, time.Second, 30*time.Second)
		fails(t, "Exec", ClassRefused, func() (*ExecResult, error) {
			return c.Exec(bg, rawDesk, ExecRequest{Command: "deploy"}, WithIdempotencyKey("k-1"))
		})
		wantCount(t, s, "POST", 3, 3)
		fails(t, "KillJob", ClassRefused, func() (*Job, error) { return c.KillJob(bg, rawDesk, "nightly") })
		wantCount(t, s, "DELETE", 3, 3)
	})
}

func TestRetryKeepAliveThenClose_ANonGETOnAReusedConnectionIsSentOnce(t *testing.T) {
	s := newRawServer(t, rawKeepAliveThenClose)
	c := rawClient(t, s, 2, time.Second, 30*time.Second) // the SDK's own retries on, too
	bg := context.Background()
	warm := func() {
		t.Helper()
		if _, _, err := within(t, "Stats", func() (*StatsReport, error) { return c.Stats(bg, rawDesk) }); err != nil {
			t.Fatal(err)
		}
	}
	warm()
	e, _ := fails(t, "KillJob", ClassUnreachable, func() (*Job, error) { return c.KillJob(bg, rawDesk, "nightly") })
	wantKind(t, "KillJob", e, KindNetwork, ReasonNetwork)
	wantCount(t, s, "DELETE", 1, 1)
	warm()
	fails(t, "Exec", ClassUnreachable, func() (*ExecResult, error) {
		return c.Exec(bg, rawDesk, ExecRequest{Command: "deploy"}, WithIdempotencyKey("k-1"))
	})
	wantCount(t, s, "POST", 1, 1)
	warm()
	fails(t, "Upload", ClassUnreachable, func() (*CopyResult, error) { return c.Upload(bg, rawDesk, "/tmp/up", bytes.NewReader([]byte("data"))) })
	wantCount(t, s, "PUT", 1, 1)
	warm()
	fails(t, "Upload (empty)", ClassUnreachable, func() (*CopyResult, error) { return c.Upload(bg, rawDesk, "/tmp/empty", bytes.NewReader(nil)) })
	wantCount(t, s, "PUT", 2, 2)
}

func TestRetryOff_EveryModeOnce(t *testing.T) {
	bg := context.Background()
	type mode struct {
		name string
		set  func(*rawServer)
	}
	modes := []mode{
		{"close", func(s *rawServer) { s.setMode(rawCloseBeforeResponse) }},
		{"reset", func(s *rawServer) { s.setMode(rawResetBeforeResponse) }},
		{"close after body", func(s *rawServer) { s.setMode(rawCloseAfterBody) }},
		{"stall mid JSON", func(s *rawServer) { s.setMode(rawStallMidJSON) }},
	}
	for _, code := range []int{429, 409, 502, 503, 504} {
		modes = append(modes, mode{fmt.Sprint(code), func(s *rawServer) { s.setStatus(code, "0", ReasonIdempotencyInFlight) }})
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			s := newRawServer(t, rawSilent)
			m.set(s)
			c := rawClient(t, s, 0, time.Second, 30*time.Second)
			_, _, err := within(t, "Stats", func() (*StatsReport, error) { return c.Stats(bg, rawDesk) })
			errOf(t, err)
			_, _, err = within(t, "Exec", func() (*ExecResult, error) { return c.Exec(bg, rawDesk, ExecRequest{Command: "x"}) })
			errOf(t, err)
			wantCount(t, s, "GET", 1, 1)
			wantCount(t, s, "POST", 1, 1)
		})
	}
}

func TestRetryDelays(t *testing.T) {
	p := DefaultRetry
	if p.MaxAttempts != 3 || p.BaseDelay != 250*time.Millisecond || p.MaxDelay != 8*time.Second || p.MaxRetryWait != 60*time.Second {
		t.Fatalf("defaults %+v", p)
	}
	for n, full := range []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second} {
		if lo, hi := p.backoff(n, 0), p.backoff(n, 0.999999); lo != full/2 || hi < full*99/100 || hi > full {
			t.Fatalf("retry %d: %v..%v, want %v..%v", n, lo, hi, full/2, full)
		}
	}
	if d := p.backoff(200, 1); d != 8*time.Second {
		t.Fatalf("a large n: %v", d)
	}
	for range 1000 {
		if d, ok := p.delay(&Error{}, 1); !ok || d < 250*time.Millisecond || d > 500*time.Millisecond {
			t.Fatalf("jitter out of [0.5, 1.0]: %v", d)
		}
	}
	// Retry-After: 429 and 503 honour it up to MaxRetryWait (60 s).
	for _, status := range []int{429, 503} {
		if d, ok := p.delay(&Error{Status: status, RetryAfter: 60 * time.Second}, 0); !ok || d != 60*time.Second {
			t.Fatalf("%d Retry-After 60: %v %v", status, d, ok)
		}
		if _, ok := p.delay(&Error{Status: status, RetryAfter: 61 * time.Second}, 0); ok {
			t.Fatalf("%d Retry-After 61 waited", status)
		}
	}
	if d, _ := p.delay(&Error{Status: 502, RetryAfter: 30 * time.Second}, 0); d > 250*time.Millisecond {
		t.Fatalf("a 502's Retry-After: %v", d)
	}
	// MaxRetryWait 0 is the default 60 s.
	if d, ok := (RetryPolicy{MaxAttempts: 3}).delay(&Error{Status: 429, RetryAfter: 30 * time.Second}, 0); !ok || d != 30*time.Second {
		t.Fatalf("MaxRetryWait 0: %v %v", d, ok)
	}
}

// recorder is a RoundTripper that keeps every request and answers 200 {}.
type recorder struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
	}
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
}

// Every request but a GET reaches net/http with a body it can neither
// rewind (GetBody) nor treat as absent (nil, NoBody): the two conditions
// under which net/http (HTTP/1 and HTTP/2) re-sends a request by itself.
func TestNonGETRequestsCannotBeReplayedByNetHTTP(t *testing.T) {
	rec := &recorder{}
	c, err := New("ak_t", WithBaseURL("http://127.0.0.1:9/v1"), WithDeskToken("gdagt_t"), WithE2E(E2EOff), WithWarningHandler(quiet), WithHTTPClient(&http.Client{Transport: rec}))
	if err != nil {
		t.Fatal(err)
	}
	bg := context.Background()
	_, _ = c.Exec(bg, rawDesk, ExecRequest{Command: "x"}, WithIdempotencyKey("k-1"))
	_, _ = c.StartJob(bg, rawDesk, JobRequest{Name: "j", Command: "x"}, WithIdempotencyKey("k-2"))
	_, _ = c.Upload(bg, rawDesk, "/tmp/a", bytes.NewReader([]byte("abc")))
	_, _ = c.Upload(bg, rawDesk, "/tmp/empty", bytes.NewReader(nil))
	_, _ = c.KillJob(bg, rawDesk, "j")
	_ = c.DeleteWebhook(bg, "wh_1")
	_, _ = c.Wake(bg, rawDesk, 0)
	_, _ = c.Stats(bg, rawDesk)
	seen := map[string]bool{}
	for _, req := range rec.reqs {
		seen[req.Method] = true
		if req.Method == http.MethodGet {
			continue
		}
		if req.GetBody != nil || req.Body == nil || req.Body == http.NoBody {
			t.Fatalf("%s %s: GetBody set %v, Body %T: net/http could re-send it", req.Method, req.URL.Path, req.GetBody != nil, req.Body)
		}
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "GET"} {
		if !seen[m] {
			t.Fatalf("no %s request seen", m)
		}
	}
}
