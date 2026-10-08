package gaiadesk

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Class is one of the six kinds every GaiaDesk error envelope carries
// (`error.kind`), the same six gaiadesk-cli prints. Branch on it first,
// then on [Error.Reason] where you need finer detail.
type Class string

// The six error classes.
const (
	ClassUsage          Class = "usage"           // bad arguments: fix the call
	ClassRefused        Class = "refused"         // the desk or the server said no
	ClassUnreachable    Class = "unreachable"     // the desk or the server could not be reached
	ClassConnectionLost Class = "connection_lost" // reached, then the connection went away
	ClassFailed         Class = "failed"          // allowed, and it did not succeed
	ClassProtocol       Class = "protocol"        // an answer this client cannot use (often a desk too old)
)

// ErrorKind is the SDK's finest word on an error: the envelope's reason when
// it is one of the kinds below (so `offline` stays `offline`), else its
// [Class]. It matches the TypeScript and Python SDKs' `kind`.
type ErrorKind string

// The SDK's kinds.
const (
	KindUsage          ErrorKind = "usage"
	KindOffline        ErrorKind = "offline"
	KindUnknownDesk    ErrorKind = "unknown_desk"
	KindNotOnline      ErrorKind = "not_online"
	KindRefused        ErrorKind = "refused"
	KindNetwork        ErrorKind = "network"
	KindNotSignedIn    ErrorKind = "not_signed_in"
	KindTimeout        ErrorKind = "timeout"
	KindConnectionLost ErrorKind = "connection_lost"
	KindLocal          ErrorKind = "local"
	KindFailed         ErrorKind = "failed"
	KindInterrupted    ErrorKind = "interrupted"
	KindProtocol       ErrorKind = "protocol"
	KindUnreachable    ErrorKind = "unreachable"
)

var sdkKinds = map[string]bool{
	"usage": true, "offline": true, "unknown_desk": true, "not_online": true, "refused": true, "network": true,
	"not_signed_in": true, "timeout": true, "connection_lost": true, "local": true, "failed": true,
	"interrupted": true, "protocol": true, "unreachable": true,
}

// sdkKind is the SDK kind for an error's class and reason.
func sdkKind(class, reason string) ErrorKind {
	if sdkKinds[reason] {
		return ErrorKind(reason)
	}
	return ErrorKind(class)
}

// Reasons the API, the desk or the SDK give (`error.reason`). Not a closed
// list: compare with the constants you care about.
const (
	ReasonUnauthenticated     = "unauthenticated"
	ReasonMissingScope        = "missing_scope"
	ReasonRateLimited         = "rate_limited"
	ReasonDeskBusy            = "desk_busy"
	ReasonDeskTokenRequired   = "desk_token_required"
	ReasonDeskOptedOut        = "desk_opted_out"
	ReasonDeskTooOld          = "desk_too_old"
	ReasonDeskOpsDisabled     = "desk_ops_disabled"
	ReasonAgentCannotAdmin    = "agent_cannot_admin"
	ReasonUnknownDesk         = "unknown_desk"
	ReasonNoWakePath          = "no_wake_path"
	ReasonNoSuchRoute         = "no_such_route"
	ReasonTooLarge            = "too_large"
	ReasonIsFolder            = "is_folder"
	ReasonIdempotencyReused   = "idempotency_key_reused"
	ReasonIdempotencyInFlight = "idempotency_key_in_flight"
	ReasonIncomplete          = "incomplete"
	ReasonNetwork             = "network"
	ReasonLocalAPIUnavailable = "local_api_unavailable"
	ReasonFingerprintMismatch = "fingerprint_mismatch"

	// End-to-end encryption.
	ReasonE2ERequired       = "e2e_required"
	ReasonE2EUnavailable    = "e2e_unavailable"
	ReasonE2EKeyMismatch    = "e2e_key_mismatch"
	ReasonE2EDecryptFailed  = "e2e_decrypt_failed"
	ReasonE2EMalformed      = "e2e_malformed"
	ReasonE2EUnsealedAnswer = "e2e_unsealed_answer"
	ReasonE2EUnsupported    = "e2e_unsupported"
	ReasonE2EStale          = "e2e_stale"
	ReasonE2EReplayed       = "e2e_replayed"
	ReasonE2EOpMismatch     = "e2e_op_mismatch"

	// Running as administrator (ExecRequest.Admin; docs/admin-access.md).
	ReasonAdminScopeMissing = "admin_scope_missing" // the token has no `admin` scope (or a person asked)
	ReasonAdminNotEnabled   = "admin_not_enabled"   // the desk owner's Admin access switch is off
	ReasonAdminDenied       = "admin_denied"        // said no, no answer, nobody there, or a confined token
	ReasonAdminUnavailable  = "admin_unavailable"   // no privileged process, or a desk too old for `admin`
	ReasonBlockedByOSPolicy = "blocked_by_os_policy"
)

// Sentinel errors for [errors.Is]: every *Error matches the one for its
// class, so `errors.Is(err, gaiadesk.ErrRefused)` is true for any refusal.
var (
	ErrUsage          = errors.New("gaiadesk: usage")
	ErrRefused        = errors.New("gaiadesk: refused")
	ErrUnreachable    = errors.New("gaiadesk: unreachable")
	ErrConnectionLost = errors.New("gaiadesk: connection lost")
	ErrFailed         = errors.New("gaiadesk: failed")
	ErrProtocol       = errors.New("gaiadesk: protocol")
	// ErrInterrupted: the call's context was cancelled (or its stream closed).
	ErrInterrupted = errors.New("gaiadesk: interrupted")
	// ErrE2E: the SDK would not send an operation in the clear, or the server
	// handed out a key other than the pinned one (also an ErrRefused).
	ErrE2E = errors.New("gaiadesk: end-to-end encryption")
	// ErrCommand: Exec with Check and a non-zero exit (a *CommandError).
	ErrCommand = errors.New("gaiadesk: command failed")
	// ErrFingerprintMismatch: the LAN gateway is not the desk you pinned
	// (a *FingerprintMismatchError, also an ErrUnreachable).
	ErrFingerprintMismatch = errors.New("gaiadesk: fingerprint mismatch")
)

// Error is every failure the SDK returns: the API's error envelope
// (`{"error": {"kind", "message", "reason", "desk", "request_id"}}`), or the
// SDK's own in the same terms.
type Error struct {
	// Class is the envelope's kind: one of the six ("" for the SDK's own
	// interrupted and local errors).
	Class Class
	// Kind is the SDK's finest kind (see ErrorKind).
	Kind ErrorKind
	// Message is what went wrong, for a person.
	Message string
	// Reason is the finer cause (`offline`, `rate_limited`, `e2e_required`,
	// `admin_not_enabled`, …), or "".
	Reason string
	// Desk is the desk the error concerned, when the API said.
	Desk string
	// Op is the request: `POST /desks/123456789/exec`.
	Op string
	// Status is the HTTP status of the failed request (0 when none was
	// answered; on a held answer, the status it would have had).
	Status int
	// RequestID is the request's id (`req_…`), to quote to support.
	RequestID string
	// RetryAfter is how long the API asked to wait before retrying (429).
	RetryAfter time.Duration
	// ExitCode is what gaiadesk-cli exits with for this error (254 refused,
	// 1 failed, 130 interrupted, 255 the rest), for CLI parity.
	ExitCode int
	// Body is the raw error body, when there was one.
	Body json.RawMessage
	// Err is the underlying cause (a network error, the context's error), or nil.
	Err error

	e2e bool
	// notSent: the connection was never made, so nothing was sent (any
	// method may be retried).
	notSent bool
}

// Error says what went wrong, with the request and its id.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("gaiadesk: ")
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	b.WriteString(e.Message)
	if e.Reason != "" && e.Reason != string(e.Class) && !strings.Contains(e.Message, e.Reason) {
		fmt.Fprintf(&b, " (%s)", e.Reason)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " [%s]", e.RequestID)
	}
	return b.String()
}

// Unwrap returns the underlying cause.
func (e *Error) Unwrap() error { return e.Err }

// Is matches the sentinel for the error's class (and ErrE2E, ErrInterrupted).
func (e *Error) Is(target error) bool {
	switch target {
	case ErrUsage:
		return e.Class == ClassUsage
	case ErrRefused:
		return e.Class == ClassRefused
	case ErrUnreachable:
		return e.Class == ClassUnreachable
	case ErrConnectionLost:
		return e.Class == ClassConnectionLost
	case ErrFailed:
		return e.Class == ClassFailed
	case ErrProtocol:
		return e.Class == ClassProtocol
	case ErrInterrupted:
		return e.Kind == KindInterrupted
	case ErrE2E:
		return e.e2e
	}
	return false
}

// Temporary reports whether retrying the same call later may succeed: a
// rate limit, a busy desk, a desk that went away, a network failure.
func (e *Error) Temporary() bool {
	switch e.Reason {
	case ReasonRateLimited, ReasonDeskBusy, ReasonIdempotencyInFlight:
		return true
	}
	if e.Kind == KindNetwork || e.Class == ClassConnectionLost {
		return true
	}
	return e.Status == 502 || e.Status == 503 || e.Status == 504
}

// CommandError is [Client.Exec] with Check set when the command exited
// non-zero (or timed out): the result is in Result.
type CommandError struct {
	// Err says how (class failed; ExitCode is the command's exit).
	Err    *Error
	Result *ExecResult
}

// Error is the *Error's message.
func (e *CommandError) Error() string { return e.Err.Error() }

// Is matches ErrCommand and ErrFailed.
func (e *CommandError) Is(target error) bool { return target == ErrCommand || e.Err.Is(target) }

// Unwrap returns the *Error.
func (e *CommandError) Unwrap() error { return e.Err }

// FingerprintMismatchError is the LAN gateway's certificate not matching
// the pinned fingerprint: it is not the desk you pinned. Do not proceed.
type FingerprintMismatchError struct {
	// Err is the error (class unreachable, reason fingerprint_mismatch).
	Err *Error
	// Expected is the pinned fingerprint, Actual the one the server
	// presented (`ab:cd:…`).
	Expected, Actual string
}

// Error is the *Error's message.
func (e *FingerprintMismatchError) Error() string { return e.Err.Error() }

// Is matches ErrFingerprintMismatch and ErrUnreachable.
func (e *FingerprintMismatchError) Is(target error) bool {
	return target == ErrFingerprintMismatch || e.Err.Is(target)
}

// Unwrap returns the *Error.
func (e *FingerprintMismatchError) Unwrap() error { return e.Err }

// AsError returns err's *Error (through CommandError and wrapping), or nil.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// deskOpExit is gaiadesk-cli's exit code for a desk operation that failed
// with this class.
func deskOpExit(class string) int {
	switch class {
	case "refused":
		return 254
	case "failed":
		return 1
	case "interrupted":
		return 130
	}
	return 255
}

// newError builds an *Error from a class and reason.
func newError(class Class, reason, message string) *Error {
	return &Error{Class: class, Kind: sdkKind(string(class), reason), Reason: reason, Message: message, ExitCode: deskOpExit(string(class))}
}

func usageError(format string, a ...any) *Error {
	return newError(ClassUsage, "", fmt.Sprintf(format, a...))
}

// notServed is the UsageError for an operation a transport does not serve.
func notServed(what string, t Transport, hint string) *Error {
	return usageError("%s is not available over the %s transport; %s", what, t.label(), hint)
}

func interrupted(op string, cause error) *Error {
	// No class: the caller stopped it; nothing the API or the desk said.
	e := newError("", "", "interrupted")
	e.Kind = KindInterrupted
	e.ExitCode = 130
	e.Op = op
	e.Err = cause
	return e
}

// envelope is the API's error object.
type envelope struct {
	Kind      string `json:"kind"`
	Message   string `json:"message"`
	Reason    string `json:"reason"`
	Desk      string `json:"desk"`
	RequestID string `json:"request_id"`
	Status    int    `json:"status"`
}

// errorEnvelope reads `{"error": {"kind", …}}`; ok is false when the JSON
// is not an error (including exec's own `"error": null`).
func errorEnvelope(body []byte) (envelope, bool) {
	var v struct {
		Error *json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &v) != nil || v.Error == nil {
		return envelope{}, false
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(*v.Error, &raw) != nil || raw == nil {
		return envelope{}, false
	}
	var env envelope
	if json.Unmarshal(raw["kind"], &env.Kind) != nil || env.Kind == "" {
		return envelope{}, false
	}
	_ = json.Unmarshal(*v.Error, &env)
	return env, true
}

// fromEnvelope is the typed error for an envelope.
func fromEnvelope(env envelope, op string, status int, body []byte) *Error {
	e := newError(Class(env.Kind), env.Reason, env.Message)
	if e.Message == "" {
		e.Message = fmt.Sprintf("HTTP %d", status)
	}
	e.Desk = env.Desk
	e.Op = op
	e.Status = status
	if env.Status != 0 && status == 200 {
		e.Status = env.Status
	}
	e.RequestID = env.RequestID
	e.Body = append(json.RawMessage(nil), body...)
	return e
}
