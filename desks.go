package gaiadesk

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// hostedOnly refuses a route only the hosted API serves.
func (c *Client) hostedOnly(what string) error {
	if c.transport == TransportAPI {
		return nil
	}
	return notServed(what, c.transport, "it is the hosted API's (New)")
}

// Desks lists the desks on the account and its team, online first:
// `GET /desks` (on the local and lan transports, the desk itself).
func (c *Client) Desks(ctx context.Context, opts ...CallOption) (*DeskList, error) {
	call, err := callOpts(opts)
	if err != nil {
		return nil, err
	}
	l, err := get[DeskList](ctx, c, &request{method: http.MethodGet, path: "/desks", call: call})
	if err != nil {
		return nil, err
	}
	if l.Devices == nil {
		return nil, protocolErr("GET /desks", "the GaiaDesk API listed no devices")
	}
	return l, nil
}

// Desk reads one desk, with its reachability, wake hints and end-to-end
// key: `GET /desks/{id}`.
func (c *Client) Desk(ctx context.Context, desk string, opts ...CallOption) (*DeskDetail, error) {
	if err := c.hostedOnly("Desk"); err != nil {
		return nil, err
	}
	d, call, err := prep(desk, opts)
	if err != nil {
		return nil, err
	}
	return get[DeskDetail](ctx, c, &request{method: http.MethodGet, path: deskPath(d), call: call})
}

// ReachOptions bound a desk's reach log.
type ReachOptions struct {
	// Since is the oldest transition (default seven days ago; the log keeps
	// thirty).
	Since time.Time
	// Limit is the most transitions, newest first (default 200, at most 1000).
	Limit int
}

// Reach reads the desk's online and offline history, newest first:
// `GET /desks/{id}/reach`.
func (c *Client) Reach(ctx context.Context, desk string, o *ReachOptions, opts ...CallOption) (*ReachLog, error) {
	if err := c.hostedOnly("Reach"); err != nil {
		return nil, err
	}
	d, call, err := prep(desk, opts)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if o != nil {
		if !o.Since.IsZero() {
			q.Set("since", strconv.FormatInt(o.Since.Unix(), 10))
		}
		if o.Limit != 0 {
			if o.Limit < 1 || o.Limit > 1000 {
				return nil, usageError("limit is 1 to 1000")
			}
			q.Set("limit", strconv.Itoa(o.Limit))
		}
	}
	return get[ReachLog](ctx, c, &request{method: http.MethodGet, path: deskPath(d) + "/reach", query: q, call: call})
}

// Wake rings the desk's doorbell and asks its LAN siblings to Wake-on-LAN
// it; with wait > 0, waits up to that long (at most 90 s) for it to come
// online and says whether it woke: `POST /desks/{id}/wake`. A desk with
// nothing to ring is ErrUnreachable, reason ReasonNoWakePath.
func (c *Client) Wake(ctx context.Context, desk string, wait time.Duration, opts ...CallOption) (*WakeResult, error) {
	if err := c.hostedOnly("Wake"); err != nil {
		return nil, err
	}
	d, call, err := prep(desk, opts)
	if err != nil {
		return nil, err
	}
	s := wholeSeconds(wait)
	if wait < 0 || s > 90 {
		return nil, usageError("wait is 0 to 90 seconds")
	}
	return get[WakeResult](ctx, c, &request{method: http.MethodPost, path: deskPath(d) + "/wake", json: map[string]int64{"wait_s": s}, call: call})
}
