# Changelog

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
