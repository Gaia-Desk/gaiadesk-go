// Package gaiadesk is the official Go SDK for GaiaDesk's Platform API
// (https://api.gaiadesk.net/v1): your fleet of desks, their reachability,
// waking them, the audit trail, webhooks, support sessions, and desk
// operations (commands, background jobs, files, tokens, stats) relayed to
// your desks, end-to-end encrypted when the desk can open them.
//
// Three transports speak the same /v1 API, with the same methods, results
// and errors:
//
//   - New: the hosted API, with an Atlas API key (`ak_…`) or a signed-in
//     person's session token. Desk operations from an API key also need a
//     scoped agent token (WithDeskToken, `gdagt_…`), which the desk verifies.
//   - NewLocal: code running ON a desk, through the GaiaDesk app's own API
//     over its Unix socket (~/.gaiadesk/api.sock) or Windows named pipe
//     (\\.\pipe\gaiadesk-api-<user>), with the desk's local admin token or an
//     agent token.
//   - NewLAN: a desk's opt-in LAN gateway (https://<desk>:7443/v1), its
//     self-signed certificate pinned by SHA-256 fingerprint; agent tokens only.
//
// Every method takes a context.Context; cancelling it stops the call (a
// stream ends with exit 130). Failures are *Error values carrying the API's
// error envelope (Class, Reason, Desk, Status, RequestID, RetryAfter); match
// them with errors.Is against ErrRefused, ErrUnreachable, ErrUsage, … or read
// them with AsError.
//
// End-to-end encryption: on the hosted API, a desk operation is sealed to the
// desk's X25519 key (X25519, HKDF-SHA256, XChaCha20-Poly1305) whenever the
// desk publishes one, so the API relays only ciphertext; every result, stream
// event and error is what the plaintext call gives. See WithE2E and
// WithE2EKeys.
package gaiadesk
