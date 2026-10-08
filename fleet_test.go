package gaiadesk

// The hosted-only routes: desks, reach, wake, audit (and its paging),
// webhooks (and verifying deliveries), support sessions.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDesksReachWake(t *testing.T) {
	s := plainMock(t)
	cx := ctx(t)
	c := newClient(t, s)
	l := must[*DeskList](t)(c.Desks(cx))
	if len(l.Devices) != 4 || l.Identity == nil || l.Identity.Account != "you@example.com" {
		t.Fatalf("%+v", l)
	}
	d := must[*DeskDetail](t)(c.Desk(cx, okDesk))
	if d.DeskID != okDesk || !d.Online || d.Wake == nil || d.Wake.DoorbellSockets != 1 || !d.Wake.LANWake {
		t.Fatalf("%+v", d)
	}
	r := must[*ReachLog](t)(c.Reach(cx, okDesk, &ReachOptions{Since: time.Unix(1790700000, 0), Limit: 20}))
	if q := s.Last().Query; q.Get("since") != "1790700000" || q.Get("limit") != "20" || len(r.Events) != 2 || r.Events[1].Version != "0.10.325" {
		t.Fatalf("%v %+v", q, r)
	}
	_, err := c.Reach(cx, okDesk, &ReachOptions{Limit: 1001})
	isUsage(t, err)
	w := must[*WakeResult](t)(c.Wake(cx, asleepDesk, 30*time.Second, WithIdempotencyKey("wake-1")))
	if !w.Woke || w.AlreadyOnline || w.Rang.Doorbell != 1 {
		t.Fatalf("%+v", w)
	}
	last := s.Last()
	if last.Header.Get("Idempotency-Key") != "wake-1" || string(last.Body) != `{"wait_s":30}` {
		t.Fatalf("%+v %s", last.Header, last.Body)
	}
	if w := must[*WakeResult](t)(c.Wake(cx, okDesk, 0)); !w.AlreadyOnline {
		t.Fatal("already online")
	}
	_, err = c.Wake(cx, offlineDesk, 0)
	if e := errOf(t, err); e.Reason != ReasonNoWakePath || e.Status != 409 {
		t.Fatalf("%+v", e)
	}
	_, err = c.Wake(cx, okDesk, 91*time.Second)
	isUsage(t, err)
}

func TestAudit(t *testing.T) {
	s := plainMock(t)
	cx := ctx(t)
	c := newClient(t, s)
	ev := must[[]AuditEvent](t)(c.Audit(cx, &AuditFilter{Desk: okDesk, Action: "api.*", Since: time.UnixMilli(1790000000000), Limit: 5}))
	q := s.Last().Query
	if len(ev) != 5 || q.Get("desk") != okDesk || q.Get("action") != "api.*" || q.Get("since_ms") != "1790000000000" || q.Get("limit") != "5" {
		t.Fatalf("%v %d", q, len(ev))
	}
	if ev[0].ID != "aud_000" || ev[0].Actor.Type != "api_key" || len(ev[0].Metadata) == 0 {
		t.Fatalf("%+v", ev[0])
	}
	// Paging: every event once, newest first, across shared milliseconds.
	seen := map[string]bool{}
	var prev int64 = 1 << 62
	for e, err := range c.AuditAll(cx, &AuditFilter{Limit: 7}) {
		if err != nil {
			t.Fatal(err)
		}
		if seen[e.ID] || e.OccurredAtMS > prev {
			t.Fatalf("out of order or repeated: %s", e.ID)
		}
		seen[e.ID] = true
		prev = e.OccurredAtMS
	}
	if len(seen) != 250 {
		t.Fatalf("%d events", len(seen))
	}
	n := 0
	for range c.AuditAll(cx, nil) {
		n++
		if n == 3 {
			break
		}
	}
	_, err := c.Audit(cx, &AuditFilter{Limit: 501})
	isUsage(t, err)
}

func TestWebhooks(t *testing.T) {
	s := plainMock(t)
	cx := ctx(t)
	c := newClient(t, s)
	h := must[*WebhookCreated](t)(c.CreateWebhook(cx, WebhookRequest{URL: "https://example.com/hooks", Events: []string{EventDeskOnline, EventJobFinished}, Description: "ops"}))
	if !strings.HasPrefix(h.ID, "wh_") || !strings.HasPrefix(h.Secret, "whsec_") || h.Description != "ops" {
		t.Fatalf("%+v", h)
	}
	l := must[[]Webhook](t)(c.Webhooks(cx))
	if len(l) != 1 || l[0].ID != h.ID {
		t.Fatalf("%+v", l)
	}
	if err := c.DeleteWebhook(cx, h.ID); err != nil {
		t.Fatal(err)
	}
	if l := must[[]Webhook](t)(c.Webhooks(cx)); len(l) != 0 {
		t.Fatal("not deleted")
	}
	if e := errOf(t, c.DeleteWebhook(cx, h.ID)); e.Status != 404 {
		t.Fatal(e)
	}
	for _, bad := range []WebhookRequest{{URL: "http://x", Events: []string{"desk.online"}}, {URL: "https://x"}, {URL: "https://x", Events: []string{"x"}, Description: strings.Repeat("d", 201)}} {
		_, err := c.CreateWebhook(cx, bad)
		isUsage(t, err)
	}
	isUsage(t, c.DeleteWebhook(cx, "x"))
}

func TestVerifyWebhook(t *testing.T) {
	secret := "whsec_" + strings.Repeat("ab", 32)
	body := []byte(`{"id":"evt_1b2c3d4e5f60718293a4b5c6","type":"job.finished","created":1791300300,"data":{"desk":{"desk_id":"123456789","owner":"o@example.com"},"job":{"name":"nightly","command":"make","state":"exited","started_at_ms":1,"exit_code":0}}}`)
	now := time.Unix(1791300300, 0)
	sig := SignWebhook(secret, body, now)
	if !strings.HasPrefix(sig, "t=1791300300,v1=") || len(sig) != len("t=1791300300,v1=")+64 {
		t.Fatal(sig)
	}
	if err := VerifyWebhook(secret, sig, body, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(VerifyWebhook(secret, sig, body, now.Add(6*time.Minute)), ErrWebhookTimestamp) {
		t.Fatal("stale accepted")
	}
	if !errors.Is(VerifyWebhook("whsec_other", sig, body, now), ErrWebhookSignature) {
		t.Fatal("wrong secret accepted")
	}
	tampered := append([]byte(nil), body...)
	tampered[10] ^= 1
	if !errors.Is(VerifyWebhook(secret, sig, tampered, now), ErrWebhookSignature) {
		t.Fatal("tampered body accepted")
	}
	for _, bad := range []string{"", "t=x,v1=00", "v1=" + strings.Repeat("0", 64), "t=1791300300"} {
		if VerifyWebhook(secret, bad, body, now) == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	// A known vector: HMAC-SHA256("k", "1.{}").
	if got := SignWebhook("k", []byte("{}"), time.Unix(1, 0)); got != "t=1,v1=3dd49b2593d0f9a349e9e71c4bde3e2b862c2be4003fe9b4ba81332029310158" {
		t.Fatal(got)
	}
	e, err := ParseWebhook(secret, SignWebhook(secret, body, time.Now()), body)
	if err != nil || e.Type != EventJobFinished {
		t.Fatal(err)
	}
	var data JobFinishedData
	if err := e.DecodeData(&data); err != nil || data.Job.Name != "nightly" || data.Desk.DeskID != "123456789" || *data.Job.ExitCode != 0 {
		t.Fatalf("%v %+v", err, data)
	}
}

func TestSupportSessions(t *testing.T) {
	s := plainMock(t)
	cx := ctx(t)
	c := newClient(t, s)
	ss := must[*SupportSessionCreated](t)(c.CreateSupportSession(cx, SupportSessionRequest{Mode: SupportCobrowse, Customer: map[string]any{"name": "Ada", "plan": "pro"}, ExpiresIn: 30 * time.Minute, Origin: "https://app.example.com"}, WithIdempotencyKey("ss-1")))
	jsonEq(t, body(t, s.Last()), `{"mode":"cobrowse","customer":{"name":"Ada","plan":"pro"},"expires_in":1800,"origin":"https://app.example.com"}`)
	if !strings.HasPrefix(ss.EmbedToken, "gdemb_") || ss.JoinCode != "123456789" || ss.State != "waiting" || ss.Customer["name"] != "Ada" || ss.Origin != "https://app.example.com" {
		t.Fatalf("%+v", ss)
	}
	got := must[*SupportSession](t)(c.SupportSession(cx, ss.ID))
	if got.ID != ss.ID || got.Mode != SupportCobrowse {
		t.Fatalf("%+v", got)
	}
	l := must[[]SupportSession](t)(c.SupportSessions(cx, &SupportSessionFilter{All: true, Limit: 10}))
	if q := s.Last().Query; q.Get("state") != "all" || q.Get("limit") != "10" || len(l) != 1 {
		t.Fatalf("%v %d", q, len(l))
	}
	_, err := c.SupportSession(cx, "ss_0000000000000099")
	if e := errOf(t, err); e.Status != 404 || e.Reason != "unknown_support_session" {
		t.Fatal(err)
	}
	for _, bad := range []SupportSessionRequest{{Mode: "control"}, {ExpiresIn: 10 * time.Second}, {ExpiresIn: 25 * time.Hour}} {
		_, err := c.CreateSupportSession(cx, bad)
		isUsage(t, err)
	}
	_, err = c.SupportSession(cx, "nope")
	isUsage(t, err)
}
