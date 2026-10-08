package gaiadesk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Gaia-Desk/gaiadesk-go/internal/e2e"
)

// CallOption configures one call.
type CallOption func(*callConfig)

type callConfig struct {
	deskToken string
	wake      *int
	idemKey   string
	info      *ResponseInfo
	noRetry   bool
	err       error
}

// UseDeskToken sends this scoped agent token (`gdagt_…`) with this call,
// instead of the client's.
func UseDeskToken(token string) CallOption {
	return func(c *callConfig) {
		if strings.TrimSpace(token) == "" {
			c.err = usageError("the desk token must be a non-empty string")
		}
		c.deskToken = strings.TrimSpace(token)
	}
}

// WakeFor rings a sleeping desk and waits up to d (0 to 120 s; the API's
// `wake_s`, default 60 s) for it before a desk operation.
func WakeFor(d time.Duration) CallOption {
	return func(c *callConfig) {
		s := wholeSeconds(d)
		if d < 0 || s > 120 {
			c.err = usageError("wake is 0 to 120 seconds")
		}
		n := int(s)
		c.wake = &n
	}
}

// WithIdempotencyKey sends `Idempotency-Key` on a POST: a retry of yours
// with the same key and the same request within 24 hours gets the first
// answer again. (The SDK itself never sends a POST again after it may have
// reached the server, key or not.)
func WithIdempotencyKey(key string) CallOption {
	return func(c *callConfig) {
		if key == "" || len(key) > 255 {
			c.err = usageError("an idempotency key is 1 to 255 printable ASCII characters")
		}
		for _, r := range key {
			if r < 0x20 || r > 0x7e {
				c.err = usageError("an idempotency key is 1 to 255 printable ASCII characters")
			}
		}
		c.idemKey = key
	}
}

// CaptureResponse fills info with what the response's headers said (its
// request id, rate limit, replay) once the call is answered.
func CaptureResponse(info *ResponseInfo) CallOption {
	return func(c *callConfig) { c.info = info }
}

// WithoutRetry makes this call once, whatever the client's retry policy.
func WithoutRetry() CallOption { return func(c *callConfig) { c.noRetry = true } }

func callOpts(opts []CallOption) (callConfig, error) {
	var c callConfig
	for _, o := range opts {
		if o != nil {
			o(&c)
		}
	}
	return c, c.err
}

// ResponseInfo is what a response's headers said.
type ResponseInfo struct {
	// Status is the HTTP status.
	Status int
	// RequestID is the request's id (`req_…`), to quote to support.
	RequestID string
	// RateLimitLimit, RateLimitRemaining and RateLimitReset are the key's
	// budget (requests per window, left, seconds until it resets); -1 when
	// the response did not say.
	RateLimitLimit, RateLimitRemaining, RateLimitReset int
	// IdempotentReplayed: this answer is a stored one, replayed for a
	// repeated Idempotency-Key.
	IdempotentReplayed bool
	// Held: the 200 was sent before its outcome was known (a held wait).
	Held bool
	// Sealed: the operation went end-to-end encrypted.
	Sealed bool
}

func headerInt(h http.Header, k string) int {
	v, err := strconv.Atoi(strings.TrimSpace(h.Get(k)))
	if err != nil {
		return -1
	}
	return v
}

// request is one API call.
type request struct {
	method, path string
	query        url.Values
	json         any
	// body is the raw body (an upload): a fresh reader per try (nil: no
	// more tries possible), and its size.
	body func() (io.Reader, error)
	size int64
	// replay: body gives a fresh reader each time (a retry may resend it).
	replay bool
	accept string
	e2e    *e2eOp
	call   callConfig
}

func (r *request) op() string { return r.method + " " + r.path }

// e2eOp is a desk operation that may be sealed end to end.
type e2eOp struct {
	desk, op string
	request  map[string]any
}

// sealedReq is an operation sealed: its envelope and its seal.
type sealedReq struct {
	req  e2e.SealedRequest
	seal *e2e.CallerSeal
}

// The query parameters a sealed request carries inside instead.
var sealedQuery = []string{"path", "tail", "timeout"}

// do sends a request (sealed end to end when the hosted API and the desk
// do), with retries; the response is 2xx, and seal is non-nil when it was
// sealed.
func (c *Client) do(ctx context.Context, r *request) (*http.Response, *e2e.CallerSeal, error) {
	policy := c.retry
	if r.call.noRetry || policy.MaxAttempts < 1 {
		policy.MaxAttempts = 1
	}
	for attempt := 1; ; attempt++ {
		res, seal, err := c.once(ctx, r)
		if err == nil {
			return res, seal, nil
		}
		wait, ok := c.retryable(err, r, attempt, policy)
		if !ok {
			return nil, nil, err
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, nil, interrupted(r.op(), ctx.Err())
		case <-t.C:
		}
	}
}

func (c *Client) once(ctx context.Context, r *request) (*http.Response, *e2e.CallerSeal, error) {
	if c.e2e == nil || r.e2e == nil {
		res, err := c.send(ctx, r, nil)
		return res, nil, err
	}
	return c.e2e.call(ctx, r, func(s *sealedReq) (*http.Response, error) { return c.send(ctx, r, s) })
}

// url builds the request URL.
func (c *Client) url(path string, q url.Values) string {
	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// send makes one HTTP request; a non-2xx answer is the typed error from
// its envelope (opened, when the request was sealed).
func (c *Client) send(ctx context.Context, r *request, sealed *sealedReq) (*http.Response, error) {
	op := r.op()
	q := url.Values{}
	for k, v := range r.query {
		q[k] = v
	}
	if r.call.wake != nil {
		q.Set("wake_s", strconv.Itoa(*r.call.wake))
	}
	h, err := c.creds(r.call.deskToken)
	if err != nil {
		return nil, err
	}
	h.Set("User-Agent", c.userAgent)
	h.Set("Accept", firstNonEmpty(r.accept, "application/json"))
	if r.call.idemKey != "" && r.method == http.MethodPost {
		h.Set("Idempotency-Key", r.call.idemKey)
	}
	var body io.Reader
	length := int64(-1)
	switch {
	case sealed != nil:
		for _, k := range sealedQuery {
			q.Del(k)
		}
		if r.method == http.MethodPost {
			b, _ := json.Marshal(map[string]any{"e2e": sealed.req})
			h.Set("Content-Type", "application/json")
			body, length = bytes.NewReader(b), int64(len(b))
		} else {
			h.Set(e2e.Header, e2e.RequestHeader(sealed.req))
			if r.body != nil {
				src, err := r.body()
				if err != nil {
					return nil, err
				}
				h.Set("Content-Type", e2e.FramesContentType)
				body = sealUpload(sealed.seal, src, r.size)
			}
		}
	case r.json != nil:
		b, err := json.Marshal(r.json)
		if err != nil {
			return nil, usageError("cannot encode the request: %v", err)
		}
		h.Set("Content-Type", "application/json")
		body, length = bytes.NewReader(b), int64(len(b))
	case r.body != nil:
		src, err := r.body()
		if err != nil {
			return nil, err
		}
		h.Set("Content-Type", "application/octet-stream")
		body, length = src, r.size
	}
	// The request's own context: cancelled when its answer has not begun
	// within the response timeout, when a read of its body waits longer
	// than the idle timeout, or once its body is closed.
	rctx, cancel := context.WithCancelCause(ctx)
	var conn connTrace
	rctx = httptrace.WithClientTrace(rctx, conn.trace())
	req, err := http.NewRequestWithContext(rctx, r.method, c.url(r.path, q), body)
	if err != nil {
		cancel(nil)
		return nil, usageError("%s: %v", op, err)
	}
	if length >= 0 {
		req.ContentLength = length
		if length == 0 {
			req.Body = http.NoBody
		}
	}
	if r.method != http.MethodGet && r.method != http.MethodHead {
		// net/http re-sends a request on a pooled connection that closed
		// before any answer when it deems it idempotent: a GET, or ANY
		// method carrying Idempotency-Key whose body it can rewind
		// (GetBody, set for a *bytes.Reader). A request that changes
		// something may have reached the server: never let it be re-sent.
		req.GetBody = nil
		// A bodiless one (a DELETE) gets an empty body it cannot rewind:
		// net/http (HTTP/2 included) re-sends a request whose Body is nil or
		// NoBody on some connection failures after it was sent. Over HTTP/1
		// a DELETE's empty body is probed and sent as no body at all.
		if req.Body == nil || req.Body == http.NoBody {
			req.Body = noRewindEmpty{}
			req.ContentLength = 0
		}
	}
	req.Header = h
	timer := startHeadersTimer(c.responseTimeout, cancel)
	res, err := c.hc.Do(req)
	timedOut := timer.stop()
	if err != nil {
		cancel(nil)
		if ctx.Err() != nil {
			return nil, interrupted(op, ctx.Err())
		}
		if timedOut {
			return nil, c.responseTimedOut(op, err)
		}
		notSent := conn.neverConnected(err)
		if c.mapDialError != nil {
			if e := c.mapDialError(err, op); e != nil {
				var fp *FingerprintMismatchError
				if own := AsError(e); own != nil && !errors.As(e, &fp) {
					own.notSent = notSent || !conn.got.Load() && own.Reason == ReasonLocalAPIUnavailable
				}
				return nil, e
			}
		}
		if connectTimedOut(err) {
			e := newError(ClassUnreachable, "timeout", fmt.Sprintf("%s could not be reached in time: %v", c.where, err))
			e.Op, e.Err = op, err
			return nil, e
		}
		var own *Error
		if errors.As(err, &own) {
			if own.Op == "" {
				own.Op = op
			}
			return nil, own
		}
		e := newError(ClassUnreachable, ReasonNetwork, fmt.Sprintf("%s could not be reached: %v", c.where, err))
		e.Op = op
		e.Err = err
		e.notSent = notSent
		return nil, e
	}
	if timedOut {
		_ = res.Body.Close()
		cancel(nil)
		return nil, c.responseTimedOut(op, errResponseTimeout)
	}
	res.Body = newIdleBody(res.Body, c.idleTimeout, cancel, func() *Error { return c.idleTimedOut(op) })
	if r.call.info != nil {
		*r.call.info = ResponseInfo{
			Status: res.StatusCode, RequestID: res.Header.Get("X-Request-Id"),
			RateLimitLimit: headerInt(res.Header, "RateLimit-Limit"), RateLimitRemaining: headerInt(res.Header, "RateLimit-Remaining"),
			RateLimitReset: headerInt(res.Header, "RateLimit-Reset"), IdempotentReplayed: res.Header.Get("Idempotent-Replayed") == "true",
			Held: res.Header.Get("GaiaDesk-Held") == "1", Sealed: sealed != nil,
		}
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		defer res.Body.Close()
		var seal *e2e.CallerSeal
		if sealed != nil {
			seal = sealed.seal
		}
		return nil, apiError(res, op, seal)
	}
	return res, nil
}

// apiError is the typed error for a failed HTTP request: its error
// envelope (a sealed operation's desk error opened), else a protocol error.
func apiError(res *http.Response, op string, seal *e2e.CallerSeal) error {
	text, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if seal != nil && json.Valid(text) {
		text = openErrorEnvelope(text, seal)
	}
	var retryAfter time.Duration
	if s, err := strconv.Atoi(strings.TrimSpace(res.Header.Get("Retry-After"))); err == nil && s >= 0 {
		retryAfter = time.Duration(s) * time.Second
	}
	env, ok := errorEnvelope(text)
	if !ok {
		e := newError(ClassProtocol, "", fmt.Sprintf("the GaiaDesk API answered with HTTP %d and no error envelope", res.StatusCode))
		e.Op, e.Status, e.RequestID, e.RetryAfter = op, res.StatusCode, res.Header.Get("X-Request-Id"), retryAfter
		if len(text) > 0 {
			e.Body = json.RawMessage(strconvQuote(text, 4096))
		}
		return e
	}
	e := fromEnvelope(env, op, res.StatusCode, text)
	if e.RequestID == "" {
		e.RequestID = res.Header.Get("X-Request-Id")
	}
	e.RetryAfter = retryAfter
	return e
}

// strconvQuote is the body as a JSON string (it was not JSON), cut short.
func strconvQuote(b []byte, max int) []byte {
	if len(b) > max {
		b = b[:max]
	}
	q, _ := json.Marshal(string(b))
	return q
}

// call makes a request answered with JSON: the result (opened, when
// sealed).
func (c *Client) call(ctx context.Context, r *request) (json.RawMessage, error) {
	res, seal, err := c.do(ctx, r)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	text, err := io.ReadAll(res.Body)
	if err != nil {
		if ctx.Err() != nil {
			return nil, interrupted(r.op(), ctx.Err())
		}
		if isTimeout(err) {
			return nil, err
		}
		e := newError(ClassConnectionLost, "", fmt.Sprintf("the answer was cut off: %v", err))
		e.Op, e.Err = r.op(), err
		return nil, e
	}
	if !json.Valid(text) {
		e := newError(ClassProtocol, "", "the GaiaDesk API answered with something that is not JSON")
		e.Op, e.Status, e.RequestID = r.op(), res.StatusCode, res.Header.Get("X-Request-Id")
		return nil, e
	}
	if seal != nil {
		return openAnswer(text, seal, r.op())
	}
	return text, nil
}

// decode unmarshals a result; a shape that does not fit is a protocol error.
func decode[T any](raw json.RawMessage, op string) (*T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		e := newError(ClassProtocol, "", fmt.Sprintf("the GaiaDesk API answered with an unexpected shape: %v", err))
		e.Op, e.Body = op, raw
		return nil, e
	}
	return &v, nil
}

// get is call + decode.
func get[T any](ctx context.Context, c *Client, r *request) (*T, error) {
	raw, err := c.call(ctx, r)
	if err != nil {
		return nil, err
	}
	return decode[T](raw, r.op())
}

// deskPath is `/desks/{id}`.
func deskPath(desk string) string { return "/desks/" + url.PathEscape(desk) }

// noRewindEmpty is an empty request body net/http cannot rewind, so it
// never re-sends the request on its own.
type noRewindEmpty struct{}

func (noRewindEmpty) Read([]byte) (int, error) { return 0, io.EOF }
func (noRewindEmpty) Close() error             { return nil }
