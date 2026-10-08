package gaiadesk

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Gaia-Desk/gaiadesk-go/internal/e2e"
	"github.com/Gaia-Desk/gaiadesk-go/internal/mockapi"
)

const (
	okDesk = "123456789"
	canary = "canary-7f3a9"
)

var (
	key1, _ = hex.DecodeString("0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	key2, _ = hex.DecodeString("2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40")
)

func pubOf(t *testing.T, secret []byte) string {
	t.Helper()
	p, err := e2e.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	return e2e.B64URL(p)
}

// warnings collects the SDK's warnings.
type warnings struct {
	mu  sync.Mutex
	all []string
}

func (w *warnings) add(m string) { w.mu.Lock(); w.all = append(w.all, m); w.mu.Unlock() }
func (w *warnings) about(desk string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, m := range w.all {
		if strings.Contains(m, desk) {
			out = append(out, m)
		}
	}
	return out
}

func quiet(string) {}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return v
	}
}

func newClient(t *testing.T, s *mockapi.Server, opts ...Option) *Client {
	t.Helper()
	all := append([]Option{WithBaseURL(s.URL), WithDeskToken("gdagt_test"), WithWarningHandler(quiet), WithRetry(RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 50 * time.Millisecond})}, opts...)
	c, err := New("ak_test", all...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// owner is a signed-in person's client (token administration).
func owner(t *testing.T, s *mockapi.Server, opts ...Option) *Client {
	t.Helper()
	c, err := New("session-person", append([]Option{WithBaseURL(s.URL), WithWarningHandler(quiet)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// errOf asserts err is an *Error and returns it.
func errOf(t *testing.T, err error) *Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	e := AsError(err)
	if e == nil {
		t.Fatalf("not an *Error: %T %v", err, err)
	}
	return e
}

func isUsage(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("expected a usage error, got %v", err)
	}
}
