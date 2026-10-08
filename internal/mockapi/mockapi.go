// Package mockapi is a mock of GaiaDesk's /v1 API AND the desks behind it,
// for the SDK's tests: every route of the contract, plaintext and end-to-end
// encrypted desk operations (each desk holds an X25519 key, opens sealed
// requests, runs canned operations and seals its events back), held waits,
// SSE written in pieces, error envelopes, rate limits, and the desk-served
// variants (local: the admin token; lan: agent tokens only). Every request
// is recorded raw, so a test can prove what the API saw.
//
// It is a port of the TypeScript SDK's test/fixtures/mock-api.ts and
// mock-e2e.ts.
package mockapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Gaia-Desk/gaiadesk-go/internal/e2e"
)

// Special desk ids.
const (
	HTMLDesk    = "999999990" // answers HTML (no envelope)
	LimitedDesk = "999999991" // 429 rate_limited, Retry-After: 7
)

// Desk is one desk behind the mock.
type Desk struct {
	// Secret is its X25519 secret; nil: a desk from before end-to-end encryption.
	Secret []byte
	// Previous keys it still opens with after a rotation.
	Previous [][]byte
	// Offline until woken (Wakeable) or until a plaintext operation wakes it.
	Offline bool
	// Unreachable: offline, and desk operations answer 409 `offline`.
	Unreachable bool
	// Required: "Require end-to-end encryption for API commands".
	Required bool
	// Wakeable: POST …/wake brings it online.
	Wakeable bool
	// HideKeyLookups: this many lookups list neither its key nor that it requires one.
	HideKeyLookups int
	// AdminEnabled: the owner's Admin access switch is on.
	AdminEnabled bool
	// Usage: every operation answers 400 usage.
	Usage bool
}

// Recorded is one request as the mock saw it.
type Recorded struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

// Mode is which API the mock serves.
type Mode string

// The modes.
const (
	Hosted Mode = ""
	Local  Mode = "local"
	LAN    Mode = "lan"
)

// Server is the running mock.
type Server struct {
	// URL is the base URL, `http://127.0.0.1:<port>/v1` (https for lan).
	URL  string
	Mode Mode
	srv  *httptest.Server

	mu       sync.Mutex
	Desks    map[string]*Desk
	requests []Recorded
	sealed   []string
	plain    []string
	wakes    []string
	waits    []string
	// Tamper makes it a hostile server: "flip" a bit of each sealed event,
	// or answer a sealed call in the clear ("plaintext").
	Tamper   string
	Files    map[string][]byte
	webhooks []map[string]any
	sessions []map[string]any
	audit    []map[string]any
	fail     []failure
	conns    atomic.Int64
	rid      atomic.Int64
}

type failure struct {
	status     int
	kind       string
	reason     string
	retryAfter string
}

// New starts a hosted-API mock on loopback.
func New(desks map[string]*Desk) *Server {
	s := newServer(desks, Hosted)
	s.srv = httptest.NewServer(s)
	s.URL = s.srv.URL + "/v1"
	return s
}

// NewOn serves a desk's own API (Local) on a listener (a Unix socket).
func NewOn(l net.Listener, desks map[string]*Desk, mode Mode) *Server {
	s := newServer(desks, mode)
	s.srv = httptest.NewUnstartedServer(s)
	s.srv.Listener.Close()
	s.srv.Listener = l
	s.srv.Start()
	s.URL = "http://localhost/v1"
	return s
}

// NewTLS serves a desk's LAN gateway over TLS (httptest's certificate).
func NewTLS(desks map[string]*Desk) *Server {
	s := newServer(desks, LAN)
	s.srv = httptest.NewUnstartedServer(s)
	s.srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			s.conns.Add(1)
		}
	}
	s.srv.StartTLS()
	s.URL = s.srv.URL + "/v1"
	return s
}

func newServer(desks map[string]*Desk, mode Mode) *Server {
	s := &Server{Mode: mode, Desks: desks, Files: map[string][]byte{}}
	for i := 0; i < 250; i++ {
		s.audit = append(s.audit, map[string]any{
			"id": fmt.Sprintf("aud_%03d", i), "action": "api.execOnDesk", "stream": "api",
			// Two events share each millisecond, newest first.
			"occurred_at_ms": 1_791_000_000_000 - int64(i/2), "actor": map[string]any{"type": "api_key", "id": "ak_test"},
			"metadata": map[string]any{"desk": "123456789"},
		})
	}
	return s
}

// Close stops the server.
func (s *Server) Close() { s.srv.Close() }

// Certificate is the TLS server's leaf certificate (DER), for pinning.
func (s *Server) Certificate() []byte { return s.srv.Certificate().Raw }

// Connections counts accepted connections (lan).
func (s *Server) Connections() int64 { return s.conns.Load() }

// Requests is every request so far.
func (s *Server) Requests() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Recorded(nil), s.requests...)
}

// Last is the last request.
func (s *Server) Last() Recorded {
	r := s.Requests()
	if len(r) == 0 {
		return Recorded{}
	}
	return r[len(r)-1]
}

// Sealed and Plain are the operations the desks ran sealed / in the clear.
func (s *Server) Sealed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sealed...)
}

// Plain: see Sealed.
func (s *Server) Plain() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.plain...)
}

// Wakes are the desks woken.
func (s *Server) Wakes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.wakes...)
}

// Waits are the `timeout` every wait was asked with.
func (s *Server) Waits() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.waits...)
}

// SetTamper sets Tamper.
func (s *Server) SetTamper(t string) { s.mu.Lock(); s.Tamper = t; s.mu.Unlock() }

// Desk returns a desk (to change it).
func (s *Server) Desk(id string) *Desk { s.mu.Lock(); defer s.mu.Unlock(); return s.Desks[id] }

// FailNext makes the next n requests (after authentication) fail with this
// status and envelope (retryAfter "" for none).
func (s *Server) FailNext(n, status int, kind, reason, retryAfter string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < n; i++ {
		s.fail = append(s.fail, failure{status, kind, reason, retryAfter})
	}
}

func (s *Server) requestID() string { return fmt.Sprintf("req_%024x", s.rid.Add(1)) }

func (s *Server) send(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", s.requestID())
	w.Header().Set("RateLimit-Limit", "600")
	w.Header().Set("RateLimit-Remaining", "597")
	w.Header().Set("RateLimit-Reset", "41")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) envelope(kind, message, reason, desk string) map[string]any {
	e := map[string]any{"kind": kind, "message": message, "reason": firstOf(reason, kind), "request_id": s.requestID()}
	if desk != "" {
		e["desk"] = desk
	}
	return map[string]any{"error": e}
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func statusFor(kind, reason string) int {
	switch kind {
	case "usage":
		return 400
	case "refused":
		switch reason {
		case "unauthenticated", "admin_token_local_only":
			return 401
		case "rate_limited", "desk_busy":
			return 429
		case "e2e_required":
			return 409
		}
		return 403
	case "unreachable":
		switch reason {
		case "timeout":
			return 504
		case "unknown_desk", "no_such_route", "unknown_support_session":
			return 404
		}
		return 409
	case "failed":
		return 422
	}
	return 502
}

func (s *Server) fail1(w http.ResponseWriter, kind, message, reason, desk string) {
	s.send(w, statusFor(kind, reason), s.envelope(kind, message, reason, desk))
}

// ServeHTTP answers one request.
func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	rec := Recorded{Method: req.Method, Path: req.URL.Path, Query: req.URL.Query(), Header: req.Header.Clone(), Body: body}
	s.mu.Lock()
	s.requests = append(s.requests, rec)
	s.mu.Unlock()

	// Credentials.
	auth := req.Header.Get("Authorization")
	deskToken := req.Header.Get("X-GaiaDesk-Desk-Token")
	key := strings.TrimPrefix(auth, "Bearer ")
	if !strings.HasPrefix(auth, "Bearer ") {
		key = ""
	}
	if s.Mode != Hosted {
		switch {
		case strings.HasPrefix(key, "gdlocal_") && s.Mode == LAN:
			s.fail1(w, "refused", "the desk's admin token works only on the desk itself; send an agent token in X-GaiaDesk-Desk-Token", "admin_token_local_only", "")
			return
		case key != "" && !strings.HasPrefix(key, "gdlocal_"):
			s.fail1(w, "refused", "not this desk's admin token", "unauthenticated", "")
			return
		case key != "":
			key = "session-admin"
		case deskToken != "":
			key = "ak_desk-token"
		}
	}
	if key == "" {
		s.fail1(w, "refused", "Sign in, or send an API key as `Authorization: Bearer ak_…`.", "unauthenticated", "")
		return
	}
	s.mu.Lock()
	var f *failure
	if len(s.fail) > 0 {
		f = &s.fail[0]
		s.fail = s.fail[1:]
	}
	s.mu.Unlock()
	if f != nil {
		if f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		s.send(w, f.status, s.envelope(f.kind, "injected failure", f.reason, ""))
		return
	}
	path := strings.TrimPrefix(req.URL.Path, "/v1")
	if path == req.URL.Path {
		s.fail1(w, "unreachable", "no route", "no_such_route", "")
		return
	}
	if s.Mode != Hosted && !strings.HasPrefix(path, "/desks") {
		s.fail1(w, "unreachable", "This route is the hosted API's.", "no_such_route", "")
		return
	}
	switch {
	case path == "/desks" && req.Method == http.MethodGet:
		s.listDesks(w)
	case strings.HasPrefix(path, "/desks/"):
		rest := strings.TrimPrefix(path, "/desks/")
		id, sub, _ := strings.Cut(rest, "/")
		if sub != "" {
			sub = "/" + sub
		}
		s.desk(w, req, rec, key, deskToken, id, sub)
	case path == "/audit" && req.Method == http.MethodGet:
		s.listAudit(w, rec.Query)
	case strings.HasPrefix(path, "/webhooks"):
		s.webhook(w, req.Method, strings.TrimPrefix(path, "/webhooks"), body)
	case strings.HasPrefix(path, "/support/sessions"):
		s.support(w, req.Method, strings.TrimPrefix(path, "/support/sessions"), rec.Query, body)
	default:
		s.fail1(w, "unreachable", "no route "+req.Method+" "+path, "no_such_route", "")
	}
}

func (s *Server) listDesks(w http.ResponseWriter) {
	s.mu.Lock()
	var devices []map[string]any
	for id, d := range s.Desks {
		devices = append(devices, map[string]any{"desk_id": id, "name": "desk " + id, "online": !d.Offline, "os": "macos", "owner": "you", "sources": []string{"account"}})
	}
	s.mu.Unlock()
	s.send(w, 200, map[string]any{"devices": devices, "sources": []string{"server"}, "notes": []string{}, "identity": map[string]any{"account": "you@example.com", "source": "api_key"}})
}

func (s *Server) desk(w http.ResponseWriter, req *http.Request, rec Recorded, key, deskToken, id, rest string) {
	if id == HTMLDesk {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "<html><body>Internal Server Error</body></html>")
		return
	}
	if id == LimitedDesk {
		w.Header().Set("Retry-After", "7")
		s.fail1(w, "refused", "Too many requests for this key; try again in 7 s.", "rate_limited", "")
		return
	}
	s.mu.Lock()
	d := s.Desks[id]
	s.mu.Unlock()
	if d == nil {
		s.fail1(w, "unreachable", "No desk with this id on your account or team.", "unknown_desk", id)
		return
	}
	if s.Mode != Hosted && (rest == "" || rest == "/reach" || rest == "/wake") {
		s.fail1(w, "unreachable", "This route is the hosted API's.", "no_such_route", "")
		return
	}
	switch {
	case rest == "" && req.Method == http.MethodGet:
		s.mu.Lock()
		stale := d.HideKeyLookups > 0
		if stale {
			d.HideKeyLookups--
		}
		o := map[string]any{"desk_id": id, "online": !d.Offline, "sources": []string{"account"}, "owner": "you",
			"e2e_required": d.Required && !stale, "wake": map[string]any{"doorbell_sockets": 1, "lan_wake": true}}
		if d.Secret != nil {
			o["features"] = []string{"desk_op", "desk_op_e2e"}
		} else {
			o["features"] = []string{"desk_op"}
		}
		if !d.Offline && d.Secret != nil && !stale {
			pub, _ := e2e.PublicKey(d.Secret)
			o["e2e_pub"] = e2e.B64URL(pub)
		}
		if d.Offline {
			o["offline_since"] = 1791290000
			o["offline_reason"] = "silent"
		}
		s.mu.Unlock()
		s.send(w, 200, o)
		return
	case rest == "/reach" && req.Method == http.MethodGet:
		s.send(w, 200, map[string]any{"desk_id": id, "since": 1790700000, "events": []map[string]any{
			{"at": 1791290000, "online": false, "reason": "silent", "reason_text": "nothing heard from the host"},
			{"at": 1791200000, "online": true, "reason": "registered", "reason_text": "connected", "version": "0.10.325"},
		}})
		return
	case rest == "/wake" && req.Method == http.MethodPost:
		s.mu.Lock()
		s.wakes = append(s.wakes, id)
		was := !d.Offline
		if d.Wakeable {
			d.Offline = false
		}
		online := !d.Offline
		s.mu.Unlock()
		if !d.Wakeable && !was {
			s.fail1(w, "unreachable", "Nothing can wake this desk.", "no_wake_path", id)
			return
		}
		s.send(w, 200, map[string]any{"desk_id": id, "online": online, "woke": d.Wakeable && !was, "already_online": was,
			"rang": map[string]int{"doorbell": 1, "lan_helpers": 0}, "waited_ms": 120})
		return
	}
	// Desk operations.
	if d.Usage {
		s.fail1(w, "usage", "the desk said: bad request", "bad_body", id)
		return
	}
	if d.Unreachable {
		s.fail1(w, "unreachable", "the desk is offline", "offline", id)
		return
	}
	isKey := strings.HasPrefix(key, "ak_")
	tokens := strings.HasPrefix(rest, "/tokens")
	if tokens && isKey {
		s.fail1(w, "refused", "token administration over the API works only for a signed-in person's own desk", "session_required", id)
		return
	}
	if !tokens && isKey && deskToken == "" {
		s.fail1(w, "refused", "from an API key, desk operations need a scoped agent token in X-GaiaDesk-Desk-Token", "desk_token_required", id)
		return
	}
	s.deskOp(w, req, rec, d, id, rest, deskToken)
}

func (s *Server) listAudit(w http.ResponseWriter, q url.Values) {
	limit := 100
	if v := q.Get("limit"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	var until int64 = 1 << 62
	if v := q.Get("until_ms"); v != "" {
		until, _ = strconv.ParseInt(v, 10, 64)
	}
	out := []map[string]any{}
	for _, e := range s.audit {
		if e["occurred_at_ms"].(int64) <= until && len(out) < limit {
			if a := q.Get("action"); a != "" && a != "api.*" && a != e["action"] {
				continue
			}
			out = append(out, e)
		}
	}
	s.send(w, 200, map[string]any{"events": out})
}

func (s *Server) webhook(w http.ResponseWriter, method, rest string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case rest == "" && method == http.MethodGet:
		s.unlockedSend(w, 200, map[string]any{"webhooks": s.webhooks})
	case rest == "" && method == http.MethodPost:
		var in map[string]any
		if json.Unmarshal(body, &in) != nil || !strings.HasPrefix(fmt.Sprint(in["url"]), "https://") {
			s.unlockedSend(w, 400, s.envelope("usage", "a webhook URL is https://", "bad_url", ""))
			return
		}
		h := map[string]any{"id": fmt.Sprintf("wh_%016x", len(s.webhooks)+1), "url": in["url"], "events": in["events"], "description": firstOf(fmt.Sprint(in["description"]), ""), "created_at": time.Now().Unix()}
		if in["description"] == nil {
			h["description"] = ""
		}
		s.webhooks = append(s.webhooks, h)
		created := map[string]any{"secret": "whsec_" + strings.Repeat("ab", 32)}
		for k, v := range h {
			created[k] = v
		}
		s.unlockedSend(w, 201, created)
	case method == http.MethodDelete:
		id := strings.TrimPrefix(rest, "/")
		for i, h := range s.webhooks {
			if h["id"] == id {
				s.webhooks = append(s.webhooks[:i], s.webhooks[i+1:]...)
				s.unlockedSend(w, 200, map[string]any{"deleted": id})
				return
			}
		}
		s.unlockedSend(w, 404, s.envelope("unreachable", "no such webhook", "unknown_webhook", ""))
	default:
		s.unlockedSend(w, 404, s.envelope("unreachable", "no route", "no_such_route", ""))
	}
}

// unlockedSend is send while s.mu is held (send takes no lock itself).
func (s *Server) unlockedSend(w http.ResponseWriter, status int, v any) { s.send(w, status, v) }

func (s *Server) support(w http.ResponseWriter, method, rest string, q url.Values, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case rest == "" && method == http.MethodPost:
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		id := fmt.Sprintf("ss_%016x", len(s.sessions)+1)
		ss := map[string]any{"id": id, "state": "waiting", "mode": firstOf(str(in["mode"]), "view"), "customer": in["customer"], "customer_present": false,
			"customer_verified": true, "join_code": "123456789", "join_url": "https://gaiadesk.net/app/support.html#session=" + id, "owner": "you@example.com",
			"created_at": 1791300000, "expires_at": 1791300000 + 3600, "origin": in["origin"]}
		if ss["customer"] == nil {
			ss["customer"] = map[string]any{}
		}
		s.sessions = append(s.sessions, ss)
		created := map[string]any{"embed_token": "gdemb_" + strings.Repeat("cd", 32)}
		for k, v := range ss {
			created[k] = v
		}
		s.unlockedSend(w, 201, created)
	case rest == "" && method == http.MethodGet:
		out := []map[string]any{}
		for _, ss := range s.sessions {
			if q.Get("state") == "all" || ss["state"] == "waiting" || ss["state"] == "joined" {
				out = append(out, ss)
			}
		}
		s.unlockedSend(w, 200, map[string]any{"sessions": out})
	case method == http.MethodGet:
		id := strings.TrimPrefix(rest, "/")
		for _, ss := range s.sessions {
			if ss["id"] == id {
				s.unlockedSend(w, 200, ss)
				return
			}
		}
		s.unlockedSend(w, 404, s.envelope("unreachable", "no such support session", "unknown_support_session", ""))
	default:
		s.unlockedSend(w, 404, s.envelope("unreachable", "no route", "no_such_route", ""))
	}
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
