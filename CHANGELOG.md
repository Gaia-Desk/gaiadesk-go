# Changelog

## 0.1.1

- **Never hang on a dropped or stalled connection.** `WithResponseTimeout`
  (default 16 minutes, above the API's 15-minute call limit) bounds the wait
  for an answer to begin, sending the request included; `WithIdleTimeout`
  (default 90 s; streams and held waits keep alive every 15 s) bounds every
  read of a body: JSON results, error bodies, downloads (plain and sealed) and
  event streams. Exceeded: `ErrUnreachable` / `ErrConnectionLost`, kind
  `timeout` (a stream ends with it in its `Exit`, exit code 255), never a hang;
  `0` is no limit. On every transport (`New`, `NewLocal`, `NewLAN`) and for a
  client given with `WithHTTPClient`. Before, a server that went silent before
  or during its answer (a half-open socket, a stalled proxy) hung the call.
- **A call that changes something is sent at most once.** `net/http` silently
  re-sent a POST carrying `Idempotency-Key` on a pooled connection that closed
  before any answer (its body rewound with `GetBody`): an `Exec` with a key
  could run twice with retries off. The SDK now takes that rewind away from
  every request that is not a GET, and no longer retries a keyed POST after a
  network failure itself; lost connections and 502/503/504 are retried for
  GETs only, timeouts not at all.
- **One retry policy, the same in every GaiaDesk SDK.** A connection that was
  never made (DNS, refused, TLS handshake, a missing local socket or pipe) is
  now retried for any method (before: GETs only), since nothing was sent; a
  connect that times out is now kind `timeout` and not retried. Lost
  connections and 502/503/504: GETs only (a 503 saying the API or desk
  operations are off: never). 429 and 409 `idempotency_key_in_flight`: any
  method. `Retry-After` is honoured for 429 and 503 only (a 502/504 now uses
  the backoff). Defaults changed: `BaseDelay` 250 ms (was 500 ms), `MaxDelay`
  8 s (was 20 s) and now caps the backoff only; the new
  `RetryPolicy.MaxRetryWait` (60 s; 0 means 60 s) caps the `Retry-After`
  waited (before: `MaxDelay`, 20 s). A DELETE (or an empty upload) now reaches
  `net/http` with an empty body it cannot rewind, so neither HTTP/1 nor HTTP/2
  can re-send it by itself; an empty upload is therefore sent chunked.
- Proven on a raw-socket test server: closed or reset before any response
  byte (reads retried, bodies sent once), stalled mid-body, mid-JSON,
  mid-stream, silent, a pooled connection that dies under a request (GET,
  DELETE, POST, PUT), refused then listening, 429/409/502/503/504 answers
  with and without `Retry-After`, and a 300-request stress run.

## 0.1.0

The first release of the GaiaDesk SDK for Go (`github.com/Gaia-Desk/gaiadesk-go`).

- **Hosted API** (`gaiadesk.New(apiKey, …)`): every `/v1` route of the contract.
  - Desks: `Desks`, `Desk`, `Reach`, `Wake`.
  - Desk operations: `Exec` (with `Check`, `Admin`, env, shell, cwd, stdin, timeout),
    `ExecStream`, `StartJob`, `Jobs`, `JobLogs`, `FollowJobLogs`, `WaitJob` (held
    waits; longer waits asked again), `KillJob`, `Stats`, `Upload` / `UploadFile`,
    `OpenDownload` / `Download` / `DownloadBytes` / `DownloadFile`, `CreateToken`,
    `Tokens`, `RevokeToken`.
  - Audit (`Audit`, `AuditAll` paging), webhooks (`Webhooks`, `CreateWebhook`,
    `DeleteWebhook`, and `VerifyWebhook` / `ParseWebhook` / `SignWebhook` for
    deliveries), support sessions (`CreateSupportSession`, `SupportSessions`,
    `SupportSession`).
- **End-to-end encrypted desk operations**: sealed to the desk's X25519 key
  (`e2e_pub`) whenever it publishes one; `WithE2E(E2EAuto | E2ERequire | E2EOff)`,
  pinned keys (`WithE2EKeys`), one retry each for `e2e_required` and a rotated
  key; reproduces the protocol's fixed test vectors byte for byte.
- **Run as administrator**: `ExecRequest.Admin` and the `admin` token scope
  (`ScopeAdmin`), with the refusal reasons `admin_scope_missing`,
  `admin_not_enabled`, `admin_denied`, `admin_unavailable`.
- **Local and LAN transports**: `NewLocal` (the desk's Unix socket or Windows
  named pipe, its admin token or an agent token) and `NewLAN` (a desk's LAN
  gateway, its certificate pinned by SHA-256 before any request byte is sent).
- Typed errors (`*Error` with `Class`, `Kind`, `Reason`, `Status`, `RequestID`,
  `RetryAfter`; `errors.Is` sentinels), retries with backoff and `Retry-After`
  (`WithRetry`), `Idempotency-Key`, rate-limit headers (`CaptureResponse`),
  `context.Context` on every call.
- One dependency: `golang.org/x/crypto` (XChaCha20-Poly1305); X25519 and HKDF
  come from the standard library. Go 1.25+.
