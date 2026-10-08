package gaiadesk

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

// RetryPolicy says which failed calls the SDK tries again, and when. The
// rule (the same in every GaiaDesk SDK): a request is sent again only when
// that cannot run anything twice.
//
//   - The connection was never made (DNS, refused, the TLS handshake, a
//     missing socket or pipe): any method; nothing was sent. A connect that
//     times out, or a certificate that does not verify, is not retried.
//   - The connection was lost after sending, or the answer was 502, 503 or
//     504: GETs only. A 503 saying the API or desk operations are switched
//     off is not retried.
//   - 429 (`rate_limited`, `desk_busy`) and 409 `idempotency_key_in_flight`:
//     any method; the server refused it before acting.
//
// Timeouts are never retried, nor anything whose answer has begun; an
// Idempotency-Key does not make a call retryable. 429 and 503 wait for
// Retry-After (one longer than MaxRetryWait is not waited: the error
// carries it); otherwise the wait is min(MaxDelay, BaseDelay·2ⁿ) times a
// random 0.5–1.0. Desk operations sealed end to end are sealed afresh for
// each try.
type RetryPolicy struct {
	// MaxAttempts is the most tries in all: 3 (two retries) by default; 1
	// (or 0) turns retries off.
	MaxAttempts int
	// BaseDelay is the first backoff (250 ms by default); each next one
	// doubles, up to MaxDelay.
	BaseDelay time.Duration
	// MaxDelay caps a backoff (8 s by default).
	MaxDelay time.Duration
	// MaxRetryWait is the longest Retry-After waited (60 s by default; 0
	// also means 60 s). A longer one is returned as the error at once.
	MaxRetryWait time.Duration
}

// DefaultRetry is the default policy: three tries, backoff from 250 ms up
// to 8 s, a Retry-After of up to 60 s waited.
var DefaultRetry = RetryPolicy{MaxAttempts: 3, BaseDelay: 250 * time.Millisecond, MaxDelay: 8 * time.Second, MaxRetryWait: 60 * time.Second}

// NoRetry makes every call once.
var NoRetry = RetryPolicy{MaxAttempts: 1}

// The 503 reasons that say a service is switched off: not retried.
var permanentUnavailable = map[string]bool{ReasonDeskOpsDisabled: true, "api_disabled": true, "local_api_off": true}

// retryable says whether and when to try a failed request again.
func (c *Client) retryable(err error, r *request, attempt int, p RetryPolicy) (time.Duration, bool) {
	if attempt >= p.MaxAttempts {
		return 0, false
	}
	e := AsError(err)
	if e == nil || e.Kind == KindInterrupted || e.Kind == KindTimeout || e.e2e {
		return 0, false
	}
	get := r.method == http.MethodGet
	ok := false
	switch {
	case e.notSent:
		ok = true
	case e.Status == 429 || e.Reason == ReasonRateLimited || e.Reason == ReasonDeskBusy || e.Reason == ReasonIdempotencyInFlight:
		ok = true
	case e.Kind == KindNetwork && e.Status == 0:
		ok = get
	case e.Status == 502 || e.Status == 504:
		ok = get
	case e.Status == 503:
		ok = get && !permanentUnavailable[e.Reason]
	}
	if !ok || (r.body != nil && !r.replay) {
		return 0, false
	}
	return p.delay(e, attempt-1)
}

// delay is the wait before retry n (0: the first), or false when the
// Retry-After asked for is longer than the policy waits.
func (p RetryPolicy) delay(e *Error, n int) (time.Duration, bool) {
	if (e.Status == 429 || e.Status == 503) && e.RetryAfter > 0 {
		limit := p.MaxRetryWait
		if limit <= 0 {
			limit = DefaultRetry.MaxRetryWait
		}
		if e.RetryAfter > limit {
			return 0, false
		}
		return e.RetryAfter, true
	}
	return p.backoff(n, rand.Float64()), true
}

// backoff is min(MaxDelay, BaseDelay·2ⁿ) times a jitter in [0.5, 1.0]
// (u is uniform in [0, 1)).
func (p RetryPolicy) backoff(n int, u float64) time.Duration {
	d := math.Min(float64(p.MaxDelay), float64(p.BaseDelay)*math.Pow(2, float64(n)))
	if d <= 0 {
		return 0
	}
	return time.Duration(d * (0.5 + 0.5*u))
}

// connTrace learns whether a request ever had a connection: one that never
// did was never sent (a dial, DNS or TLS handshake failure).
type connTrace struct {
	got       atomic.Bool
	tlsFailed atomic.Bool
}

func (t *connTrace) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { t.got.Store(true) },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err != nil {
				t.tlsFailed.Store(true)
			}
		},
	}
}

// neverConnected says err is a failure to make the connection (no request
// byte written): not a timeout, not a certificate that does not verify.
func (t *connTrace) neverConnected(err error) bool {
	if t.got.Load() || connectTimedOut(err) || badCertificate(err) {
		return false
	}
	var op *net.OpError
	var dns *net.DNSError
	return t.tlsFailed.Load() || errors.As(err, &dns) || (errors.As(err, &op) && op.Op == "dial")
}

// connectTimedOut: a connect (or TLS handshake) that timed out.
func connectTimedOut(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// badCertificate: the server's certificate did not verify (permanent).
func badCertificate(err error) bool {
	var v *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var hn x509.HostnameError
	var ci x509.CertificateInvalidError
	return errors.As(err, &v) || errors.As(err, &ua) || errors.As(err, &hn) || errors.As(err, &ci)
}
