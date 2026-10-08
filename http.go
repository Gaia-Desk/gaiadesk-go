package gaiadesk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
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

// RetryPolicy says which failed calls the SDK tries again, and when.
//
// It retries a call refused for a rate limit or a busy desk (429
// `rate_limited`, `desk_busy`; 409 `idempotency_key_in_flight`) after the
// API's Retry-After (or the backoff); a GET also after a network failure
// (the connection closed or reset before any answer) or a 502, 503 or 504.
// A call that changes something is never sent again after it may have
// reached the server, and a timeout (WithResponseTimeout, WithIdleTimeout)
// is not retried. Desk operations sealed end to end are sealed afresh for each
// try. A wait longer than MaxDelay is not waited: the error is returned.
type RetryPolicy struct {
	// MaxAttempts is the most tries in all (1: no retry).
	MaxAttempts int
	// BaseDelay is the first backoff; each next one doubles, with jitter.
	BaseDelay time.Duration
	// MaxDelay caps a backoff, and is the longest Retry-After waited.
	MaxDelay time.Duration
}

// DefaultRetry is the default policy: three tries.
var DefaultRetry = RetryPolicy{MaxAttempts: 3, BaseDelay: 500 * time.Millisecond, MaxDelay: 20 * time.Second}

// NoRetry makes every call once.
var NoRetry = RetryPolicy{MaxAttempts: 1}

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

// retryable says whether and when to try a failed request again.
func (c *Client) retryable(err error, r *request, attempt int, p RetryPolicy) (time.Duration, bool) {
	if attempt >= p.MaxAttempts {
		return 0, false
	}
	e := AsError(err)
	if e == nil || e.Kind == KindInterrupted || e.e2e {
		return 0, false
	}
	safe := r.method == http.MethodGet || r.method == http.MethodHead
	replayable := r.body == nil || r.replay
	ok := false
	switch {
	case e.Reason == ReasonRateLimited || e.Reason == ReasonDeskBusy || e.Reason == ReasonIdempotencyInFlight:
		ok = true
	case e.Kind == KindNetwork && e.Status == 0:
		ok = safe
	case e.Status == 502 || e.Status == 503 || e.Status == 504:
		ok = safe && e.Reason != ReasonDeskOpsDisabled && e.Reason != "api_disabled" && e.Reason != "local_api_off"
	}
	if !ok || !replayable {
		return 0, false
	}
	if e.RetryAfter > 0 {
		if e.RetryAfter > p.MaxDelay {
			return 0, false
		}
		return e.RetryAfter, true
	}
	d := p.BaseDelay << (attempt - 1)
	if d <= 0 || d > p.MaxDelay {
		d = p.MaxDelay
	}
	// Full jitter in [d/2, d].
	if d > 1 {
		d = d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
	}
	return d, true
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
	req, err := http.NewRequestWithContext(rctx, r.method, c.url(r.path, q), body)
	if err != nil {
		cancel(nil)
		return nil, usageError("%s: %v", op, err)
	}
	if r.method != http.MethodGet && r.method != http.MethodHead {
		// net/http re-sends a request on a pooled connection that closed
		// before any answer when it deems it idempotent: a GET, or ANY
		// method carrying Idempotency-Key whose body it can rewind
		// (GetBody, set for a *bytes.Reader). A request that changes
		// something may have reached the server: never let it be re-sent.
		req.GetBody = nil
	}
	if length >= 0 {
		req.ContentLength = length
		if length == 0 {
			req.Body = http.NoBody
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
		if c.mapDialError != nil {
			if e := c.mapDialError(err, op); e != nil {
				return nil, e
			}
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
