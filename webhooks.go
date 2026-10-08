package gaiadesk

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Webhook event types.
const (
	EventDeskOnline           = "desk.online"
	EventDeskOffline          = "desk.offline"
	EventDeskWoke             = "desk.woke"
	EventJobFinished          = "job.finished"
	EventSupportSessionJoined = "support.session.joined"
	EventSupportSessionEnded  = "support.session.ended"
	webhookTolerance          = 5 * time.Minute
)

// WebhookRequest subscribes an HTTPS endpoint to events.
type WebhookRequest struct {
	// URL is an `https://` URL on the public internet.
	URL string
	// Events to deliver (at least one): EventDeskOnline, ….
	Events []string
	// Description, at most 200 characters.
	Description string
}

// Webhooks lists the account's webhook subscriptions (never their secrets):
// `GET /webhooks`.
func (c *Client) Webhooks(ctx context.Context, opts ...CallOption) ([]Webhook, error) {
	if err := c.hostedOnly("Webhooks"); err != nil {
		return nil, err
	}
	call, err := callOpts(opts)
	if err != nil {
		return nil, err
	}
	l, err := get[struct {
		Webhooks *[]Webhook `json:"webhooks"`
	}](ctx, c, &request{method: http.MethodGet, path: "/webhooks", call: call})
	if err != nil {
		return nil, err
	}
	if l.Webhooks == nil {
		return nil, protocolErr("GET /webhooks", "the GaiaDesk API listed no webhooks")
	}
	return *l.Webhooks, nil
}

// CreateWebhook subscribes an endpoint: `POST /webhooks`. Keep the
// result's Secret (it is never shown again) to verify deliveries with
// VerifyWebhook.
func (c *Client) CreateWebhook(ctx context.Context, w WebhookRequest, opts ...CallOption) (*WebhookCreated, error) {
	if err := c.hostedOnly("CreateWebhook"); err != nil {
		return nil, err
	}
	call, err := callOpts(opts)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(strings.ToLower(w.URL), "https://") {
		return nil, usageError("a webhook URL is https://")
	}
	if len(w.Events) == 0 {
		return nil, usageError("a webhook needs at least one event")
	}
	if len(w.Description) > 200 {
		return nil, usageError("a webhook's description is at most 200 characters")
	}
	body := map[string]any{"url": w.URL, "events": w.Events}
	if w.Description != "" {
		body["description"] = w.Description
	}
	return get[WebhookCreated](ctx, c, &request{method: http.MethodPost, path: "/webhooks", json: body, call: call})
}

// DeleteWebhook unsubscribes (deliveries still queued are dropped):
// `DELETE /webhooks/{webhook_id}`.
func (c *Client) DeleteWebhook(ctx context.Context, id string, opts ...CallOption) error {
	if err := c.hostedOnly("DeleteWebhook"); err != nil {
		return err
	}
	call, err := callOpts(opts)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(id, "wh_") {
		return usageError("a webhook id is wh_…: %q", id)
	}
	_, err = c.call(ctx, &request{method: http.MethodDelete, path: "/webhooks/" + url.PathEscape(id), call: call})
	return err
}

// WebhookEvent is one delivery. ID is stable across retries: de-duplicate
// by it. Data's shape depends on Type: DeskEventData for desk.*,
// JobFinishedData for job.finished, SupportSessionEventData for
// support.session.*.
type WebhookEvent struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Created int64           `json:"created"`
	Data    json.RawMessage `json:"data"`
}

// DeskEventData is the data of desk.online, desk.offline and desk.woke.
type DeskEventData struct {
	Desk struct {
		DeskID     string `json:"desk_id"`
		Owner      string `json:"owner"`
		Reason     string `json:"reason"`
		ReasonText string `json:"reason_text"`
		Version    string `json:"version,omitempty"`
		// WokeAfterMS is desk.woke's: how long after the ring it came online.
		WokeAfterMS *int64 `json:"woke_after_ms,omitempty"`
	} `json:"desk"`
}

// JobFinishedData is the data of job.finished.
type JobFinishedData struct {
	Desk struct {
		DeskID string `json:"desk_id"`
		Owner  string `json:"owner"`
	} `json:"desk"`
	Job Job `json:"job"`
}

// SupportSessionEventData is the data of support.session.joined and
// support.session.ended (never the join code, the desk or the embed token).
type SupportSessionEventData struct {
	SupportSession SupportSession `json:"support_session"`
}

// DecodeData unmarshals the event's data into v (one of the *Data types).
func (e *WebhookEvent) DecodeData(v any) error { return json.Unmarshal(e.Data, v) }

// Webhook verification failures.
var (
	ErrWebhookSignature = errors.New("gaiadesk: webhook signature does not verify")
	ErrWebhookTimestamp = errors.New("gaiadesk: webhook timestamp is more than five minutes off")
)

// VerifyWebhook checks a delivery's GaiaDesk-Signature header
// (`t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<raw body>">`) against the
// webhook's secret over the raw body, in constant time, and that t is
// within five minutes of now.
func VerifyWebhook(secret, signature string, body []byte, now time.Time) error {
	var t int64 = -1
	var sigs [][]byte
	for _, part := range strings.Split(signature, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return ErrWebhookSignature
			}
			t = n
		case "v1":
			if b, err := hex.DecodeString(v); err == nil {
				sigs = append(sigs, b)
			}
		}
	}
	if t < 0 || len(sigs) == 0 {
		return ErrWebhookSignature
	}
	if d := now.Sub(time.Unix(t, 0)); d > webhookTolerance || d < -webhookTolerance {
		return ErrWebhookTimestamp
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(t, 10) + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, s := range sigs {
		if hmac.Equal(s, want) {
			return nil
		}
	}
	return ErrWebhookSignature
}

// ParseWebhook verifies a delivery (VerifyWebhook, against now) and reads it.
func ParseWebhook(secret, signature string, body []byte) (*WebhookEvent, error) {
	if err := VerifyWebhook(secret, signature, body, time.Now()); err != nil {
		return nil, err
	}
	var e WebhookEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// SignWebhook makes a GaiaDesk-Signature header for body at t, as the API
// does: for testing a webhook handler.
func SignWebhook(secret string, body []byte, t time.Time) string {
	mac := hmac.New(sha256.New, []byte(secret))
	ts := strconv.FormatInt(t.Unix(), 10)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
