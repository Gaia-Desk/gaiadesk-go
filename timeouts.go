package gaiadesk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Why a request's context was cancelled by the SDK itself.
var (
	errResponseTimeout = errors.New("gaiadesk: response timeout")
	errIdleTimeout     = errors.New("gaiadesk: idle timeout")
)

// responseTimedOut is the error for an answer that did not begin within
// the response timeout.
func (c *Client) responseTimedOut(op string, cause error) *Error {
	e := newError(ClassUnreachable, "timeout", fmt.Sprintf("%s did not answer %s within %v (WithResponseTimeout)", c.where, op, c.responseTimeout))
	e.Op, e.Err = op, cause
	return e
}

// idleTimedOut is the error for an answer whose body stopped arriving.
func (c *Client) idleTimedOut(op string) *Error {
	e := newError(ClassConnectionLost, "timeout", fmt.Sprintf("%s stopped sending its answer to %s: nothing for %v (WithIdleTimeout)", c.where, op, c.idleTimeout))
	e.Op, e.Err = op, errIdleTimeout
	return e
}

// isTimeout says whether err is one of the SDK's own timeouts (passed
// through as it is, never rewrapped as a plain cut-off).
func isTimeout(err error) bool {
	e := AsError(err)
	return e != nil && e.Kind == KindTimeout
}

// headersTimer cancels a request's context when its answer has not begun
// within d (0: never).
type headersTimer struct {
	t     *time.Timer
	fired atomic.Bool
}

func startHeadersTimer(d time.Duration, cancel context.CancelCauseFunc) *headersTimer {
	h := &headersTimer{}
	if d > 0 {
		h.t = time.AfterFunc(d, func() {
			h.fired.Store(true)
			cancel(errResponseTimeout)
		})
	}
	return h
}

// stop stops the timer; true when it had already fired.
func (h *headersTimer) stop() bool {
	if h.t != nil {
		h.t.Stop()
	}
	return h.fired.Load()
}

// idleBody is a response body whose every read must make progress within
// the idle timeout. Running out cancels the request's context: net/http
// then abandons (closes) the connection rather than pooling it, and the
// read fails with the SDK's ConnectionLost timeout. Closing it releases
// the request's context.
type idleBody struct {
	rc     io.ReadCloser
	idle   time.Duration
	cancel context.CancelCauseFunc
	mkErr  func() *Error

	t     *time.Timer
	fired atomic.Bool
	once  sync.Once
}

func newIdleBody(rc io.ReadCloser, idle time.Duration, cancel context.CancelCauseFunc, mkErr func() *Error) *idleBody {
	b := &idleBody{rc: rc, idle: idle, cancel: cancel, mkErr: mkErr}
	if idle > 0 {
		b.t = time.AfterFunc(time.Hour, func() {
			b.fired.Store(true)
			cancel(errIdleTimeout)
		})
		b.t.Stop()
	}
	return b
}

func (b *idleBody) Read(p []byte) (int, error) {
	if b.fired.Load() {
		return 0, b.mkErr()
	}
	if b.t != nil {
		b.t.Reset(b.idle)
	}
	n, err := b.rc.Read(p)
	if b.t != nil {
		b.t.Stop()
	}
	if err != nil && err != io.EOF && b.fired.Load() {
		return n, b.mkErr()
	}
	return n, err
}

func (b *idleBody) Close() error {
	if b.t != nil {
		b.t.Stop()
	}
	err := b.rc.Close()
	b.once.Do(func() { b.cancel(context.Canceled) })
	return err
}
