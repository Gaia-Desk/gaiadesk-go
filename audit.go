package gaiadesk

import (
	"context"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// AuditFilter narrows the audit trail.
type AuditFilter struct {
	// Desk: only events about this desk (one of the caller's own).
	Desk string
	// Actor: only events by this actor id (an email, a token id).
	Actor string
	// Action: only this action, or every action under it when it ends in
	// `.*` (`api.*`).
	Action string
	// Token: only events by this agent token id or API key id.
	Token string
	// Since and Until bound when it happened (inclusive; zero: unbounded).
	Since, Until time.Time
	// Limit is the most events per call (default 100, at most 500).
	Limit int
}

func (f *AuditFilter) query() (url.Values, error) {
	q := url.Values{}
	if f == nil {
		return q, nil
	}
	if f.Desk != "" {
		d, err := checkDesk(f.Desk)
		if err != nil {
			return nil, err
		}
		q.Set("desk", d)
	}
	for k, v := range map[string]string{"actor": f.Actor, "action": f.Action, "token": f.Token} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if !f.Since.IsZero() {
		q.Set("since_ms", strconv.FormatInt(f.Since.UnixMilli(), 10))
	}
	if !f.Until.IsZero() {
		q.Set("until_ms", strconv.FormatInt(f.Until.UnixMilli(), 10))
	}
	if f.Limit != 0 {
		if f.Limit < 1 || f.Limit > 500 {
			return nil, usageError("limit is 1 to 500")
		}
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	return q, nil
}

// Audit reads audit events about the caller and their own desks, newest
// first: `GET /audit`. AuditAll pages through all of them.
func (c *Client) Audit(ctx context.Context, f *AuditFilter, opts ...CallOption) ([]AuditEvent, error) {
	if err := c.hostedOnly("Audit"); err != nil {
		return nil, err
	}
	call, err := callOpts(opts)
	if err != nil {
		return nil, err
	}
	q, err := f.query()
	if err != nil {
		return nil, err
	}
	l, err := get[struct {
		Events *[]AuditEvent `json:"events"`
	}](ctx, c, &request{method: http.MethodGet, path: "/audit", query: q, call: call})
	if err != nil {
		return nil, err
	}
	if l.Events == nil {
		return nil, protocolErr("GET /audit", "the GaiaDesk API listed no events")
	}
	return *l.Events, nil
}

// AuditAll iterates over every audit event the filter matches, newest
// first, a page (Limit, default 100) at a time; an error ends the
// iteration with it.
func (c *Client) AuditAll(ctx context.Context, f *AuditFilter, opts ...CallOption) iter.Seq2[AuditEvent, error] {
	return func(yield func(AuditEvent, error) bool) {
		page := AuditFilter{}
		if f != nil {
			page = *f
		}
		limit := page.Limit
		if limit == 0 {
			limit = 100
		}
		seen := map[string]bool{}
		for {
			events, err := c.Audit(ctx, &page, opts...)
			if err != nil {
				yield(AuditEvent{}, err)
				return
			}
			fresh := 0
			var oldest int64
			for _, e := range events {
				oldest = e.OccurredAtMS
				if seen[e.ID] {
					continue
				}
				seen[e.ID] = true
				fresh++
				if !yield(e, nil) {
					return
				}
			}
			if len(events) < limit {
				return
			}
			// until_ms is inclusive: ask again from the oldest time seen
			// (its events already yielded are skipped), or past it when a
			// whole page was that one millisecond.
			until := oldest
			if fresh == 0 {
				until--
			}
			if !page.Since.IsZero() && until < page.Since.UnixMilli() {
				return
			}
			page.Until = time.UnixMilli(until)
		}
	}
}
