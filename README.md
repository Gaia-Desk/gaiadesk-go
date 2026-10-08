# GaiaDesk SDK for Go

Drive your GaiaDesk machines ("desks") from Go through GaiaDesk's Platform
API: list them and see why one is offline, wake them, run commands and get
exit codes back, stream output, copy files, run background jobs, read stats,
mint and revoke scoped agent tokens, read the audit trail, manage webhooks,
and create support sessions for the embed SDK. Desk operations are
**end-to-end encrypted** whenever the desk can open them.

- Module: `github.com/Gaia-Desk/gaiadesk-go`, package `gaiadesk` (Go 1.25+)
- One dependency: `golang.org/x/crypto` (XChaCha20-Poly1305); X25519 and
  HKDF come from the standard library
- Three transports, one API: the **hosted API** (`New`), a desk's **own API**
  for code running on it (`NewLocal`), and a desk's **LAN gateway** (`NewLAN`)

Other GaiaDesk developer tools: the
[TypeScript SDK](https://github.com/Gaia-Desk/gaiadesk-typescript),
the [Python SDK](https://github.com/Gaia-Desk/gaiadesk-python) and the
[MCP server](https://github.com/Gaia-Desk/gaiadesk-mcp). This SDK covers
the same API transport as theirs, plus the fleet, audit, webhook and support
routes. The API contract is GaiaDesk's OpenAPI document (`api/openapi.yaml`);
results are the same JSON shapes `gaiadesk-cli --json` prints.

MIT-licensed. GaiaDesk itself is proprietary and not covered by this license.

## Contents

- [Install](#install)
- [Quick start](#quick-start)
- [Credentials](#credentials)
- [Desks](#desks)
- [Commands](#commands)
- [Background jobs](#background-jobs)
- [Files](#files)
- [Agent tokens](#agent-tokens)
- [Audit, webhooks, support sessions](#audit-webhooks-support-sessions)
- [End-to-end encryption](#end-to-end-encryption)
- [Local and LAN](#local-and-lan)
- [Errors](#errors)
- [Retries, idempotency, rate limits](#retries-idempotency-rate-limits)
- [API coverage](#api-coverage)
- [Examples](#examples)
- [Development](#development)

## Install

```sh
go get github.com/Gaia-Desk/gaiadesk-go@v0.1.0
```

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/Gaia-Desk/gaiadesk-go"
)

func main() {
	gd, err := gaiadesk.New(os.Getenv("GAIADESK_API_KEY"), // ak_…
		gaiadesk.WithDeskToken(os.Getenv("GAIADESK_TOKEN"))) // gdagt_…, verified by the desk
	if err != nil {
		log.Fatal(err)
	}
	r, err := gd.Exec(context.Background(), "123456789", gaiadesk.ExecRequest{Command: "uname -a"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("exit %d\n%s", r.Exit, r.Stdout)
}
```

Every method takes a `context.Context`: cancel it (or give it a deadline) to
stop the call. A `Client` is safe for concurrent use.

## Credentials

| Credential | Where | What it does |
|---|---|---|
| Atlas API key, `ak_…` | `New(apiKey)` | the key's scopes: `desks:read`, `desks:write`, `exec`, `files`, `jobs`, `tokens`, `audit:read`, `webhooks`, `support` |
| A signed-in person's session or OAuth token | `New(token)` | every scope, on their own and their team's desks |
| Scoped agent token, `gdagt_…` | `WithDeskToken(t)`, or `UseDeskToken(t)` per call | sent as `X-GaiaDesk-Desk-Token`; **the desk** verifies it (its scopes, `cwd`, low-privilege user) |
| The desk's local admin token, `gdlocal_…` | `NewLocal()` reads it from `~/.gaiadesk/api-token`, or `WithAdminToken` | the desk owner, on the desk only |

From an API key, desk operations need an agent token as well: a key never
speaks for its account to a desk. Token administration (`CreateToken`,
`Tokens`, `RevokeToken`) is the desk owner's: a signed-in person's session.

Client options: `WithBaseURL`, `WithDeskToken`, `WithHTTPClient`, `WithE2E`,
`WithE2EKeys`, `WithWarningHandler`, `WithRetry`, `WithResponseTimeout`,
`WithIdleTimeout`, `WithUserAgent` (and for
`NewLocal`: `WithSocketPath`, `WithAdminToken`, `WithEnv`). Per-call options:
`UseDeskToken`, `WakeFor`, `WithIdempotencyKey`, `CaptureResponse`,
`WithoutRetry`.

## Desks

```go
list, _ := gd.Desks(ctx) // every desk on the account and its team, online first
for _, d := range list.Devices {
	fmt.Println(d.DeskID, d.Name, d.Online, d.OfflineReason)
}
d, _ := gd.Desk(ctx, "123456789")                                    // one, with wake hints and e2e key
log, _ := gd.Reach(ctx, "123456789", &gaiadesk.ReachOptions{Limit: 20}) // its online/offline history
w, err := gd.Wake(ctx, "123456789", 30*time.Second)                   // ring it, wait up to 30 s
```

A sleeping desk is also rung before any desk operation: `WakeFor(d)` sets how
long to wait (the API's `wake_s`, default 60 s, at most 120 s). A desk with
nothing to ring is `ErrUnreachable`, reason `no_wake_path`.

## Commands

```go
r, err := gd.Exec(ctx, desk, gaiadesk.ExecRequest{
	Command: "make test",                 // one command line; or Argv: []string{"make", "test"}
	Shell:   gaiadesk.ShellBash,          // default: the desk's own
	Cwd:     "src/app",
	Env:     map[string]string{"CI": "1"}, // values never logged by the server or the desk
	Stdin:   strings.NewReader("input"),  // read whole, sent up front as text
	Timeout: 10 * time.Minute,            // the API caps every call at 15 minutes
	Check:   true,                        // a non-zero exit is a *CommandError
})
```

A command that ran is a result whatever its exit code (`r.Exit`,
`r.RemoteCode`, `r.Stdout`, `r.Stderr`, `r.TimedOut`, `r.Truncated` past
8 MB). One the desk refused to start is an error (`ErrRefused`).

**Streaming** (`?stream=1`, Server-Sent Events):

```go
st, err := gd.ExecStream(ctx, desk, gaiadesk.ExecRequest{Argv: []string{"make", "release"}})
if err != nil { // refused, unreachable, … before it started
	return err
}
defer st.Close()                         // stops the command if you leave early
exit, err := st.Copy(os.Stdout, os.Stderr) // or: for c := range st.Chunks() { … }, or st.Recv()
fmt.Println(exit.ExitCode, exit.Err())   // exit.Err(): why it broke off (desk lost, …), or nil
```

A character split across chunks arrives whole. The `Exit` says how it ended:
the command's `ExitCode` and `Result`, or an `Error` (`connection_lost` when
the desk went away mid-stream), or 130 when you closed it or cancelled the
context. The API takes stdin up front only (as text); it has no input while
the command runs.

**As administrator.** `ExecRequest{Admin: true}` runs the command as root
(macOS, Linux) or SYSTEM (Windows). It needs a desk token with the `admin`
scope **and** the desk owner's Admin access switch, turned on only at the desk;
in its default mode the person at the desk is asked each time. Otherwise it
is `ErrRefused` with one of these reasons:

| `Reason` | means |
|---|---|
| `admin_scope_missing` | the token has no `admin` scope (or a person asked) |
| `admin_not_enabled` | the desk's Admin access switch is off |
| `admin_denied` | the person said no, nobody answered or was there, or a confined (`Cwd`/`LowPriv`) token |
| `admin_unavailable` | no privileged GaiaDesk process, or a desk too old for `admin` |

## Background jobs

```go
yes := true
job, _ := gd.StartJob(ctx, desk, gaiadesk.JobRequest{
	Name: "nightly", Command: "./build.sh --release", Shell: gaiadesk.ShellBash,
	Env: map[string]string{"CI": "1"}, Priority: "low", CPUPercent: 50, MemMB: 2048, KeepAwake: &yes,
})
jobs, _ := gd.Jobs(ctx, desk)
logs, _ := gd.JobLogs(ctx, desk, "nightly", &gaiadesk.LogsOptions{Tail: 4096}) // the last 4 KiB
follow, _ := gd.FollowJobLogs(ctx, desk, "nightly", nil)                       // a *Stream until it ends
done, _ := gd.WaitJob(ctx, desk, "nightly", gaiadesk.WaitForever)             // or a timeout; 0: now
stopped, _ := gd.KillJob(ctx, desk, "nightly")
```

`WaitJob` answers the job as it ended, or (`TimedOut`) as it stands. The API
holds one wait at most 870 s; longer waits are asked again. A held answer
(`GaiaDesk-Held: 1`) that failed after its 200 began is returned as its typed
error, like any other failure.

## Files

```go
res, err := gd.UploadFile(ctx, "report.csv", desk, "/tmp/")     // a remote ending in / keeps the name
res, err = gd.Upload(ctx, desk, "notes/a.txt", strings.NewReader("hello"))
n, err := gd.Download(ctx, desk, "/var/log/app.log", os.Stdout) // to any io.Writer
b, err := gd.DownloadBytes(ctx, desk, "notes/a.txt")
res, err = gd.DownloadFile(ctx, desk, "/tmp/report.csv", "./") // appears only once complete
rc, err := gd.OpenDownload(ctx, desk, "big.bin")              // an io.ReadCloser
```

At most 256 MB per file through the API (`FileLimit`; larger files go direct
with `gaiadesk-cli cp`). An `io.ReadSeeker` (an `*os.File`, a
`*bytes.Reader`) is streamed and can be resent on a retry; any other reader
is read into memory first, as the API needs the size up front. A download
that breaks off is an error (`ErrConnectionLost`), never a clean short file.

## Agent tokens

```go
owner, _ := gaiadesk.New(sessionToken) // a signed-in person: the desk owner
minted, err := owner.CreateToken(ctx, gaiadesk.TokenRequest{
	Desks: []string{"123456789"}, Name: "ci-bot", Expires: 7 * 24 * time.Hour,
	Scopes: []string{gaiadesk.ScopeExec, gaiadesk.ScopeCp, gaiadesk.ScopeJobs}, Cwd: "/srv/ci",
})
fmt.Println(minted.Tokens[0].Secret) // shown once
tokens, _ := owner.Tokens(ctx, "123456789")
_, _ = owner.RevokeToken(ctx, "123456789", tokens[0].ID)
```

Scopes: `screen`, `exec`, `shell`, `cp`, `forward`, `jobs`, and `admin` (never
implied; a confined token cannot have it). If a later desk fails,
`CreateToken` returns the tokens already minted along with the error.

## Audit, webhooks, support sessions

```go
events, _ := gd.Audit(ctx, &gaiadesk.AuditFilter{Desk: "123456789", Action: "api.*", Limit: 50})
for e, err := range gd.AuditAll(ctx, &gaiadesk.AuditFilter{Since: time.Now().Add(-24 * time.Hour)}) {
	if err != nil { break }
	fmt.Println(e.Action, e.Actor.ID)
}

hook, _ := gd.CreateWebhook(ctx, gaiadesk.WebhookRequest{URL: "https://example.com/hooks/gaiadesk",
	Events: []string{gaiadesk.EventDeskOffline, gaiadesk.EventJobFinished}})
// hook.Secret is shown once. In your handler, over the raw body:
ev, err := gaiadesk.ParseWebhook(secret, r.Header.Get("GaiaDesk-Signature"), body)

s, _ := gd.CreateSupportSession(ctx, gaiadesk.SupportSessionRequest{Mode: gaiadesk.SupportCobrowse,
	Customer: map[string]any{"name": "Ada"}, ExpiresIn: 30 * time.Minute, Origin: "https://app.example.com"})
// hand s.EmbedToken (shown once) to the page; agents join with s.JoinCode
open, _ := gd.SupportSessions(ctx, nil)
```

`VerifyWebhook` checks `t=<unix>,v1=<hex HMAC-SHA256 of "<t>.<raw body>">` in
constant time and refuses a `t` more than five minutes off; de-duplicate
deliveries by `ev.ID`. `SignWebhook` makes the header, for testing handlers.

## End-to-end encryption

On the hosted API, desk operations are **sealed** so GaiaDesk's servers relay
only ciphertext: the command, its `Env`, `Stdin`, file paths and bytes, and
all output and results are readable by you and the desk only. The server still
sees the credentials, the route (the operation and the desk, a job name or
token id in the path), `stream`/`follow`/`wake_s`, sizes, and how the
operation ended (an exit, or an error's kind and reason). The local and LAN
transports never leave the desk or the LAN and are not sealed.

Before an operation the SDK reads the desk's X25519 key (`e2e_pub` from
`GET /desks/{id}`, cached for 5 minutes) and seals the request to a fresh
ephemeral key: X25519, HKDF-SHA256, XChaCha20-Poly1305, every message bound
to the desk, the operation and its place in the stream (`seq`). Results,
streams, errors and file bytes come back exactly as in the clear; a desk's
error carries its own message. The SDK reproduces the protocol's fixed test
vectors byte for byte (`testdata/e2e-vectors.json`).

```go
gd, err := gaiadesk.New(apiKey, gaiadesk.WithDeskToken(token),
	gaiadesk.WithE2E(gaiadesk.E2ERequire),                                 // E2EAuto (default) | E2ERequire | E2EOff
	gaiadesk.WithE2EKeys(map[string]string{"123456789": "B6N8vBQgk8i3…"})) // optional: pin a desk's e2e_pub
```

- `E2EAuto`: sealed when the desk lists a key; otherwise sent in the clear
  with a one-time warning per desk (`WithWarningHandler`, default the `log`
  package), unless the desk **requires** it: then it is woken and asked again,
  and sealed or refused.
- `E2ERequire`: never in the clear. A desk that lists no key (asleep, offline,
  or a GaiaDesk from before end-to-end encryption) is woken and asked again;
  still none is `ErrE2E` (also `ErrRefused`, reason `e2e_unavailable`), and
  nothing is sent.
- `E2EOff`: plaintext.
- Pinned keys seal even while the desk lists none; a different key from the
  server is `ErrE2E` (`e2e_key_mismatch`) and nothing is sent.
- A plaintext call refused `e2e_required` (409) is sealed and sent once more;
  a sealed one the desk could not open (`e2e_decrypt_failed`: its key
  rotated) is sealed to the key read again, once. Answers that do not open
  (altered, reordered, or a plaintext answer to a sealed call) are
  `ErrProtocol` (`e2e_decrypt_failed`, `e2e_malformed`, `e2e_unsealed_answer`).
- Reading a desk's key needs the API key's `desks:read` scope (waking it,
  `desks:write`); in `E2EAuto`, a key that cannot be read means plaintext
  with the warning.

`CaptureResponse(&info)` sets `info.Sealed` to say whether a call went sealed.

## Local and LAN

**On the desk** (no server, no internet):

```go
gd, err := gaiadesk.NewLocal() // finds the socket or pipe and the admin token
// or: gaiadesk.NewLocal(gaiadesk.WithDeskToken(agentToken)) — its scopes, cwd and user apply
```

| | macOS, Linux | Windows |
|---|---|---|
| where | `$GAIADESK_API_DIR/api.sock`, else `~/.gaiadesk/api.sock` | `$GAIADESK_API_PIPE`, else `\\.\pipe\gaiadesk-api-<user>` |
| admin token | `$GAIADESK_API_DIR/api-token`, else `~/.gaiadesk/api-token` | `%USERPROFILE%\.gaiadesk\api-token` |

No socket or pipe (the app is not running, or Settings → GaiaDesk API →
Local API is off) is `ErrUnreachable`, reason `local_api_unavailable`.
`LocalSocketPath`, `LocalTokenPath`, `LocalPipeName` and `PipeUser` give the
defaults. The Windows pipe is opened for overlapped I/O, so reads, writes and
cancellation work as on a socket, with no extra dependency.

**A desk's LAN gateway** (opt-in on the desk, port 7443):

```go
gd, err := gaiadesk.NewLAN("https://gaiadesk-123456789.local:7443/v1",
	"ab:cd:…",                               // the SHA-256 fingerprint the desk's Settings show
	gaiadesk.WithDeskToken(agentToken))      // agent tokens only
```

The certificate is self-signed, so its SHA-256 is pinned instead of the chain,
and checked during the TLS handshake, before any byte of the request is sent.
A different certificate is a `*FingerprintMismatchError` (`ErrFingerprintMismatch`,
also `ErrUnreachable`): do not proceed, it may not be your desk.

Both serve the desk operations and `Desks`; the hosted-only routes (`Desk`,
`Reach`, `Wake`, audit, webhooks, support) are a usage error that sends nothing.

## Errors

Every failure is an `*Error` (or wraps one): the API's error envelope
`{"error": {"kind", "message", "reason", "desk", "request_id"}}`, or the SDK's
own in the same terms.

```go
r, err := gd.Exec(ctx, desk, req)
switch {
case errors.Is(err, gaiadesk.ErrUnreachable): // offline, unknown desk, network, …
case errors.Is(err, gaiadesk.ErrRefused):     // credential, scope, rate limit, desk said no
case errors.Is(err, gaiadesk.ErrInterrupted): // the context was cancelled
}
if e := gaiadesk.AsError(err); e != nil {
	fmt.Println(e.Class, e.Kind, e.Reason, e.Status, e.RequestID, e.RetryAfter)
}
```

| `Class` (sentinel) | HTTP | means |
|---|---|---|
| `usage` (`ErrUsage`) | 400 | fix the call (`bad_body`, `idempotency_key_reused`, `too_large`, …); the SDK's own checks too, which send nothing |
| `refused` (`ErrRefused`) | 401, 403, 409, 429 | `unauthenticated`, `missing_scope`, `desk_token_required`, `rate_limited`, `desk_busy`, `e2e_required`, `admin_*`, … |
| `unreachable` (`ErrUnreachable`) | 404, 409, 503, 504 | `unknown_desk`, `offline`, `no_wake_path`, `network`, `local_api_unavailable`, … |
| `connection_lost` (`ErrConnectionLost`) | 502 | the desk went away mid-operation; a broken download |
| `failed` (`ErrFailed`) | 422 | allowed, and it did not succeed (no such job, a file that failed) |
| `protocol` (`ErrProtocol`) | 409, 502 | an answer this client cannot use (`desk_too_old`, an envelope missing, sealed events that do not open) |

`Kind` is the finest word, matching the TypeScript and Python SDKs' `kind`
(`offline`, `unknown_desk`, `network`, `timeout`, `interrupted`, `local`, …).
`ExitCode` is what `gaiadesk-cli` would exit with (254 refused, 1 failed, 130
interrupted, 255 the rest). Also: `ErrE2E`, `ErrCommand` (`*CommandError`,
with `Result`), `ErrFingerprintMismatch`, and `e.Temporary()`.

## Retries, idempotency, rate limits

**Retries.** A request is sent again only when that cannot run anything twice:

- **The connection was never made** (DNS, refused, TLS handshake): any method — nothing was sent.
- **The connection was lost after sending, or the answer was 502, 503 or 504**: GETs only (reads).
  A 503 that says the API or desk operations are switched off is not retried.
- **429** (`rate_limited`, `desk_busy`) and **409** `idempotency_key_in_flight`: any method — the server refused
  it before acting.

Timeouts are never retried, and nothing is retried once its answer has begun. A call that changes something
(POST, PUT, DELETE) is never sent again after it may have reached the server; an `Idempotency-Key` is sent but
does not make a call retryable. 429 and 503 wait for `Retry-After`; one longer than `RetryPolicy.MaxRetryWait`
(default 60 s) is not waited for — the error carries it. Otherwise the wait is exponential backoff with jitter:
`RetryPolicy.BaseDelay` (default 250 ms) doubling up to `RetryPolicy.MaxDelay` (default 8 s), times a random 0.5–1.0.
`RetryPolicy.MaxAttempts` (default 3: 2 retries, so 3 attempts in all) sets how many times; 1
(`WithRetry(gaiadesk.NoRetry)`, or `WithoutRetry()` per call) turns retries off. Each retry of a sealed operation
is sealed afresh.

Go's `net/http` itself re-sends only a GET (once, when the pooled connection it used was closed before any
answer), or a request it could not write at all. The SDK hands it every other request with a body it can neither
rewind nor treat as absent, the two cases in which it would re-send one (HTTP/1 re-sends any request carrying
`Idempotency-Key` with a rewindable body; HTTP/2 a bodiless one on some stream errors), so a POST, PUT or DELETE
is never re-sent behind your back.

**Timeouts** (`WithResponseTimeout`, `WithIdleTimeout`, on every transport:
`New`, `NewLocal`, `NewLAN`) make a server or proxy that stops answering an
error, never a hang:

- `WithResponseTimeout` (default `DefaultResponseTimeout`, 16 minutes, above
  the API's 15-minute call limit): the longest wait for an answer to begin,
  sending the request included. Exceeded: `ErrUnreachable`, kind `timeout`.
- `WithIdleTimeout` (default `DefaultIdleTimeout`, 90 s; streams and held
  waits send a keep-alive every 15 s): the longest silence while reading a
  body (JSON, a download, an event stream), per read, so a long download that
  keeps flowing never times out. Exceeded mid-answer: `ErrConnectionLost`,
  kind `timeout` (a `Stream` ends with that error in its `Exit`, exit code 255).
- `0` is no limit; a negative value is a usage error. Both hold for a client
  given with `WithHTTPClient` too. A connection that timed out is closed, not
  pooled; cancelling the context still stops a call at once.
- A connection closed or reset before any answer is `ErrUnreachable` (kind
  `network`) at once; a connect that times out is kind `timeout`. What is
  retried, and what `net/http` re-sends by itself: see Retries above.

`CaptureResponse(&info)` fills `ResponseInfo`: `RequestID`, `RateLimitLimit`,
`RateLimitRemaining`, `RateLimitReset`, `IdempotentReplayed`, `Held`, `Sealed`.

## API coverage

| Endpoint | Method |
|---|---|
| `GET /desks` | `Desks` |
| `GET /desks/{id}` | `Desk` |
| `GET /desks/{id}/reach` | `Reach` |
| `POST /desks/{id}/wake` | `Wake` |
| `POST /desks/{id}/exec` | `Exec` |
| `POST /desks/{id}/exec?stream=1` | `ExecStream` |
| `POST /desks/{id}/jobs` | `StartJob` |
| `GET /desks/{id}/jobs` | `Jobs` |
| `DELETE /desks/{id}/jobs/{name}` | `KillJob` |
| `GET /desks/{id}/jobs/{name}/logs` | `JobLogs` |
| `GET /desks/{id}/jobs/{name}/logs?follow=1` | `FollowJobLogs` |
| `GET /desks/{id}/jobs/{name}/wait` | `WaitJob` |
| `GET /desks/{id}/stats` | `Stats` |
| `PUT /desks/{id}/files` | `Upload`, `UploadFile` |
| `GET /desks/{id}/files` | `OpenDownload`, `Download`, `DownloadBytes`, `DownloadFile` |
| `POST /desks/{id}/tokens` | `CreateToken` |
| `GET /desks/{id}/tokens` | `Tokens` |
| `DELETE /desks/{id}/tokens/{token_id}` | `RevokeToken` |
| `GET /audit` | `Audit`, `AuditAll` |
| `GET /webhooks` | `Webhooks` |
| `POST /webhooks` | `CreateWebhook` |
| `DELETE /webhooks/{webhook_id}` | `DeleteWebhook` |
| webhook deliveries (`GaiaDesk-Signature`) | `VerifyWebhook`, `ParseWebhook`, `SignWebhook` |
| `POST /support/sessions` | `CreateSupportSession` |
| `GET /support/sessions` | `SupportSessions` |
| `GET /support/sessions/{session_id}` | `SupportSession` |

Not in this SDK, because the API does not serve them: interactive shells,
stdin written while a command runs, folder copies, port forwarding, screen
tools and MCP. Those are `gaiadesk-cli`'s (or the TypeScript/Python SDKs'
CLI and native backends, which run GaiaDesk's client on your machine).

## Examples

- [`examples/exec`](examples/exec/main.go): run a command, then stream another
- [`examples/jobs`](examples/jobs/main.go): start a job, follow its log, wait
- [`examples/files`](examples/files/main.go): upload and download, never in the clear
- [`examples/webhook`](examples/webhook/main.go): a verified webhook receiver
- [`examples/local`](examples/local/main.go): drive the desk you are on

## Development

```sh
go vet ./...
go test -race ./...
go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 ./...
```

The tests run against `internal/mockapi`, a mock of the API **and** the desks
behind it (each desk opens sealed requests with its own key, runs canned
operations, seals its events back, and records every request raw), so they
prove both that a sealed call answers exactly what the plaintext one does and
that the API saw none of its command, environment, stdin, paths or bytes.
`internal/e2e` holds the crypto and its vector tests.
