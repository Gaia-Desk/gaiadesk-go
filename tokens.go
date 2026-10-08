package gaiadesk

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Agent token scopes (what a token may do on the desk).
const (
	ScopeScreen  = "screen"
	ScopeExec    = "exec"
	ScopeShell   = "shell"
	ScopeCp      = "cp"
	ScopeForward = "forward"
	ScopeJobs    = "jobs"
	// ScopeAdmin lets a token ASK to run as administrator; never implied.
	// The desk owner's Admin access switch (turned on at the desk) and its
	// mode still decide. A confined token (Cwd, LowPriv) cannot have it.
	ScopeAdmin = "admin"
)

// DefaultScopes are a new token's scopes when none are given.
var DefaultScopes = []string{ScopeExec, ScopeCp, ScopeJobs}

// TokenRequest mints scoped agent tokens, one per desk, under one name.
type TokenRequest struct {
	// Desks to mint a token on.
	Desks []string
	// Name names the token (required).
	Name string
	// Expires is its lifetime (rounded up to seconds; default 7 days).
	Expires time.Duration
	// Scopes it holds (default DefaultScopes).
	Scopes []string
	// Cwd confines its work to this directory on the desk.
	Cwd string
	// LowPriv runs its work as the desk's low-privilege agent user, or refuses.
	LowPriv bool
}

// CreateToken mints a scoped agent token on each desk:
// `POST /desks/{id}/tokens` once per desk. Token administration is the desk
// OWNER's (a signed-in person's session, not an API key's agent token). If
// a later desk fails, the result holds the tokens already minted alongside
// the error: their secrets are shown once.
func (c *Client) CreateToken(ctx context.Context, t TokenRequest, opts ...CallOption) (*MintResult, error) {
	call, err := callOpts(opts)
	if err != nil {
		return nil, err
	}
	if len(t.Desks) == 0 {
		return nil, usageError("at least one desk is required")
	}
	desks := make([]string, len(t.Desks))
	for i, d := range t.Desks {
		if desks[i], err = checkDesk(d); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(t.Name) == "" {
		return nil, usageError("a token needs a name")
	}
	if t.Scopes != nil && len(t.Scopes) == 0 {
		return nil, usageError("scopes must not be empty")
	}
	scopes := t.Scopes
	if scopes == nil {
		scopes = DefaultScopes
	}
	if t.Expires < 0 {
		return nil, usageError("expires must be > 0")
	}
	expires := 7 * 24 * time.Hour
	if t.Expires > 0 {
		expires = t.Expires
	}
	for _, s := range scopes {
		if s == ScopeAdmin && (t.Cwd != "" || t.LowPriv) {
			return nil, usageError("a confined token (Cwd, LowPriv) cannot have the admin scope")
		}
	}
	spec := map[string]any{"name": t.Name, "expires_secs": wholeSeconds(expires), "scopes": scopes}
	if t.Cwd != "" {
		spec["cwd"] = t.Cwd
	}
	if t.LowPriv {
		spec["low_priv"] = true
	}
	out := &MintResult{Tokens: []MintedToken{}}
	for _, d := range desks {
		r := &request{method: http.MethodPost, path: deskPath(d) + "/tokens", json: spec, call: call,
			e2e: &e2eOp{desk: d, op: "token_mint", request: map[string]any{"op": "token_mint", "spec": spec}}}
		res, err := get[struct {
			Tokens *[]MintedToken `json:"tokens"`
		}](ctx, c, r)
		if err == nil && res.Tokens == nil {
			err = protocolErr(r.op(), "the GaiaDesk API minted no tokens")
		}
		if err != nil {
			if len(out.Tokens) > 0 {
				return out, err
			}
			return nil, err
		}
		out.Tokens = append(out.Tokens, *res.Tokens...)
	}
	return out, nil
}

// Tokens lists the desk's agent tokens (never their secrets):
// `GET /desks/{id}/tokens`.
func (c *Client) Tokens(ctx context.Context, desk string, opts ...CallOption) ([]TokenInfo, error) {
	d, call, err := prep(desk, opts)
	if err != nil {
		return nil, err
	}
	r := &request{method: http.MethodGet, path: deskPath(d) + "/tokens", call: call,
		e2e: &e2eOp{desk: d, op: "token_list", request: map[string]any{"op": "token_list"}}}
	l, err := get[struct {
		Tokens *[]TokenInfo `json:"tokens"`
	}](ctx, c, r)
	if err != nil {
		return nil, err
	}
	if l.Tokens == nil {
		return nil, protocolErr(r.op(), "the GaiaDesk API listed no tokens")
	}
	return *l.Tokens, nil
}

// RevokeToken revokes an agent token by id or name; its live sessions and
// jobs end: `DELETE /desks/{id}/tokens/{token_id}`.
func (c *Client) RevokeToken(ctx context.Context, desk, token string, opts ...CallOption) (*Revoked, error) {
	d, call, err := prep(desk, opts)
	if err != nil {
		return nil, err
	}
	if token == "" || strings.HasPrefix(token, "-") {
		return nil, usageError("a token name or id is required")
	}
	return get[Revoked](ctx, c, &request{method: http.MethodDelete, path: deskPath(d) + "/tokens/" + url.PathEscape(token), call: call,
		e2e: &e2eOp{desk: d, op: "token_revoke", request: map[string]any{"op": "token_revoke", "token": token}}})
}

// Stats reads the desk's figures (CPU, memory, disks, running jobs):
// `GET /desks/{id}/stats`.
func (c *Client) Stats(ctx context.Context, desk string, opts ...CallOption) (*StatsReport, error) {
	d, call, err := prep(desk, opts)
	if err != nil {
		return nil, err
	}
	return get[StatsReport](ctx, c, &request{method: http.MethodGet, path: deskPath(d) + "/stats", call: call,
		e2e: &e2eOp{desk: d, op: "stats", request: map[string]any{"op": "stats"}}})
}
