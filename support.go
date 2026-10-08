package gaiadesk

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// Support session modes.
const (
	// SupportView: the agent sees the shared tab.
	SupportView = "view"
	// SupportCobrowse: the agent may also point, highlight, click, scroll
	// and type inside the page (applied by the embed to its own DOM).
	SupportCobrowse = "cobrowse"
)

// SupportSessionRequest creates a support session for the web embed SDK.
type SupportSessionRequest struct {
	// Mode is SupportView (default) or SupportCobrowse.
	Mode string
	// Customer is who the customer is, as the company knows them: at most 16
	// fields of short strings, numbers or booleans (never in the audit trail).
	Customer map[string]any
	// ExpiresIn ends the session after this long (60 s to 24 h; default 1 h).
	ExpiresIn time.Duration
	// Origin is the page origin the embed must run on
	// (`https://app.example.com`; `http://` only for localhost). Leave it
	// out for the native (desktop) embed.
	Origin string
}

// CreateSupportSession creates a support session: `POST /support/sessions`.
// Hand the result's EmbedToken (shown once) to the customer's page or app;
// the support team joins with JoinCode.
func (c *Client) CreateSupportSession(ctx context.Context, s SupportSessionRequest, opts ...CallOption) (*SupportSessionCreated, error) {
	if err := c.hostedOnly("CreateSupportSession"); err != nil {
		return nil, err
	}
	call, err := callOpts(opts)
	if err != nil {
		return nil, err
	}
	body := map[string]any{}
	if s.Mode != "" {
		if s.Mode != SupportView && s.Mode != SupportCobrowse {
			return nil, usageError("mode is view or cobrowse (not %q)", s.Mode)
		}
		body["mode"] = s.Mode
	}
	if s.Customer != nil {
		if len(s.Customer) > 16 {
			return nil, usageError("customer has at most 16 fields")
		}
		body["customer"] = s.Customer
	}
	if s.ExpiresIn != 0 {
		secs := wholeSeconds(s.ExpiresIn)
		if secs < 60 || secs > 86400 {
			return nil, usageError("expires_in is 60 seconds to 24 hours")
		}
		body["expires_in"] = secs
	}
	if s.Origin != "" {
		body["origin"] = s.Origin
	}
	return get[SupportSessionCreated](ctx, c, &request{method: http.MethodPost, path: "/support/sessions", json: body, call: call})
}

// SupportSessionFilter narrows the support sessions listed.
type SupportSessionFilter struct {
	// All adds ended and expired sessions to the open ones.
	All bool
	// Limit is the most sessions (default 50, at most 200).
	Limit int
}

// SupportSessions lists the support sessions of the caller's account and
// team, newest first (the console's queue): `GET /support/sessions`.
func (c *Client) SupportSessions(ctx context.Context, f *SupportSessionFilter, opts ...CallOption) ([]SupportSession, error) {
	if err := c.hostedOnly("SupportSessions"); err != nil {
		return nil, err
	}
	call, err := callOpts(opts)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if f != nil {
		if f.All {
			q.Set("state", "all")
		}
		if f.Limit != 0 {
			if f.Limit < 1 || f.Limit > 200 {
				return nil, usageError("limit is 1 to 200")
			}
			q.Set("limit", strconv.Itoa(f.Limit))
		}
	}
	l, err := get[struct {
		Sessions *[]SupportSession `json:"sessions"`
	}](ctx, c, &request{method: http.MethodGet, path: "/support/sessions", query: q, call: call})
	if err != nil {
		return nil, err
	}
	if l.Sessions == nil {
		return nil, protocolErr("GET /support/sessions", "the GaiaDesk API listed no sessions")
	}
	return *l.Sessions, nil
}

var supportID = regexp.MustCompile(`^ss_[0-9a-f]{16}$`)

// SupportSession reads one support session's state:
// `GET /support/sessions/{session_id}`.
func (c *Client) SupportSession(ctx context.Context, id string, opts ...CallOption) (*SupportSession, error) {
	if err := c.hostedOnly("SupportSession"); err != nil {
		return nil, err
	}
	call, err := callOpts(opts)
	if err != nil {
		return nil, err
	}
	if !supportID.MatchString(id) {
		return nil, usageError("a support session id is ss_ and 16 hex digits: %q", id)
	}
	return get[SupportSession](ctx, c, &request{method: http.MethodGet, path: "/support/sessions/" + id, call: call})
}
