package mockapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Gaia-Desk/gaiadesk-go/internal/e2e"
)

type obj = map[string]any

// event is a desk's event (what a sealed event opens to).
type event struct {
	Event   string `json:"event"`
	Data    string `json:"data,omitempty"`
	Result  any    `json:"result,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Message string `json:"message,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

func out(b []byte) event    { return event{Event: "stdout", Data: e2e.B64(b)} }
func exit(result any) event { return event{Event: "exit", Result: result} }
func deskErr(kind, reason, msg string) event {
	return event{Event: "error", Kind: kind, Reason: reason, Message: msg}
}

// run is what the desk does: its events for one operation (canned).
func (s *Server) run(d *Desk, desk string, req obj, input []byte, deskToken string) []event {
	switch req["op"] {
	case "exec":
		spec, _ := req["spec"].(obj)
		cmd := str(spec["command"])
		if argv, ok := spec["argv"].([]any); ok {
			parts := make([]string, len(argv))
			for i, a := range argv {
				parts[i] = str(a)
			}
			cmd = strings.Join(parts, " ")
		}
		if cmd == "refuse" {
			return []event{deskErr("refused", "token_refused", "this token (bot) has no exec scope on desk "+desk)}
		}
		result := obj{"desk": desk, "exit": 0, "remote_code": 0, "duration_ms": 7, "notes": []string{}, "stderr": "warn\n", "timed_out": false,
			"truncated": false, "error": nil, "mode": "pipes", "route": "the GaiaDesk server", "shell": spec["shell"]}
		if cmd == "exit 3" {
			result["exit"], result["remote_code"] = 3, 3
		}
		text := "ran: " + cmd + " é\n"
		if env, ok := spec["env"].(obj); ok {
			keys := make([]string, 0, len(env))
			for k := range env {
				keys = append(keys, k)
			}
			sortStrings(keys)
			for _, k := range keys {
				text += fmt.Sprintf("env: %s=%v\n", k, env[k])
			}
		}
		if stdin, ok := spec["stdin"].(string); ok {
			text += "stdin: " + stdin + "\n"
		}
		if cwd, ok := spec["cwd"].(string); ok {
			text += "cwd: " + cwd + "\n"
		}
		if t, ok := spec["timeout_secs"].(float64); ok {
			text += fmt.Sprintf("timeout: %v\n", t)
		}
		result["stdout"] = text
		if req["stream"] != true {
			return []event{exit(result)}
		}
		// The output in pieces, a character split across two of them.
		b := []byte(text)
		cut := strings.Index(text, "é") + 1
		events := []event{out(b[:cut]), {Event: "stderr", Data: e2e.B64([]byte("warn\n"))}, out(b[cut:])}
		if cmd == "lose" {
			return events // the desk goes away (the server says so in the clear)
		}
		return append(events, exit(result))
	case "job_start":
		spec, _ := req["spec"].(obj)
		return []event{exit(obj{"name": spec["name"], "state": "running", "pid": 42, "command": fmt.Sprint(spec["command"]), "started_at_ms": 1791300000000, "limits": spec["limits"]})}
	case "job_list":
		return []event{exit(obj{"jobs": []obj{{"name": "build", "state": "running", "pid": 42, "command": "make", "started_at_ms": 1791300000000}}})}
	case "job_kill":
		return []event{exit(obj{"name": req["name"], "state": "killed", "command": "make", "started_at_ms": 1791300000000})}
	case "job_wait":
		name := str(req["name"])
		switch name {
		case "held-gone", "nope":
			return []event{deskErr("failed", "", fmt.Sprintf("no job named %q", name))}
		case "held-fail":
			return []event{deskErr("connection_lost", "desk_disconnected", "The desk went away during this operation.")}
		case "slow":
			return []event{exit(obj{"job": obj{"name": name, "state": "running", "command": "sleep", "started_at_ms": 1}, "timed_out": true})}
		}
		return []event{exit(obj{"job": obj{"name": name, "state": "exited", "exit_code": 3, "command": "make", "started_at_ms": 1}, "timed_out": false})}
	case "job_logs":
		name := str(req["name"])
		if name == "missing" {
			return []event{deskErr("failed", "", `no job named "missing"`)}
		}
		if req["follow"] != true {
			tail := "all"
			if t, ok := req["tail"].(float64); ok {
				tail = strconv.Itoa(int(t))
			}
			return []event{exit(obj{"job": obj{"name": name, "state": "running", "command": "make", "started_at_ms": 1}, "output": "tail " + tail + "\n"})}
		}
		l2 := []byte("line2 é\n")
		return []event{out([]byte("line1\n")), out(l2[:7]), out(l2[7:]), exit(obj{"job": obj{"name": name, "state": "exited", "exit_code": 0, "command": "make", "started_at_ms": 1}})}
	case "stats":
		return []event{exit(obj{"desk": desk, "hostname": "studio", "os": "macos", "cpu_percent": 5, "cpus": 8, "mem_total_mb": 16384, "mem_free_mb": 1024, "uptime_secs": 100, "jobs_running": 1})}
	case "file_put":
		path := str(req["path"])
		s.mu.Lock()
		s.Files[path] = input
		s.mu.Unlock()
		if path == "fails" {
			return []event{exit(obj{"direction": "upload", "desk": desk, "destination": path, "files": 0, "dirs": 0, "bytes": 0, "resumed_bytes": 0, "failed": []obj{{"path": path, "message": "disk full"}}, "seconds": 0})}
		}
		return []event{exit(obj{"direction": "upload", "desk": desk, "destination": path, "files": 1, "dirs": 0, "bytes": len(input), "resumed_bytes": 0, "failed": []obj{}, "seconds": 0})}
	case "file_get":
		path := str(req["path"])
		if path == "missing" {
			return []event{deskErr("failed", "not_found", "no such file: missing")}
		}
		s.mu.Lock()
		data, ok := s.Files[path]
		s.mu.Unlock()
		if !ok {
			data = []byte("contents of " + path + "\n")
		}
		var ev []event
		for i := 0; i < len(data); i += e2e.InputChunk {
			ev = append(ev, out(data[i:min(len(data), i+e2e.InputChunk)]))
		}
		return append(ev, exit(obj{"direction": "download", "desk": desk, "destination": path, "files": 1, "dirs": 0, "bytes": len(data), "resumed_bytes": 0, "failed": []obj{}, "seconds": 0}))
	case "token_mint":
		spec, _ := req["spec"].(obj)
		return []event{exit(obj{"tokens": []obj{{"desk": desk, "secret": "gdagt_minted_secret", "token": obj{"id": "tok1", "label": spec["name"], "issued_at_ms": 1, "expires_at_ms": 2, "scopes": spec["scopes"]}}}})}
	case "token_list":
		return []event{exit(obj{"tokens": []obj{{"id": "tok1", "label": "bot", "issued_at_ms": 1, "expires_at_ms": 2, "scopes": []string{"exec"}}}})}
	case "token_revoke":
		return []event{exit(obj{"revoked": req["token"], "stopped_sessions": 0})}
	}
	return []event{deskErr("protocol", "unknown_op", "unknown operation")}
}

func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// routeOp is the route's operation and its plaintext request.
func routeOp(method, rest string, q url.Values, body []byte) (op string, req obj, stream string) {
	j := func() obj {
		var o obj
		_ = json.Unmarshal(body, &o)
		if o == nil {
			o = obj{}
		}
		return o
	}
	switch {
	case rest == "/exec" && method == http.MethodPost:
		r := obj{"op": "exec", "spec": j()}
		if q.Get("stream") == "1" {
			r["stream"] = true
			return "exec", r, "exec"
		}
		return "exec", r, ""
	case rest == "/jobs" && method == http.MethodPost:
		return "job_start", obj{"op": "job_start", "spec": j()}, ""
	case rest == "/jobs" && method == http.MethodGet:
		return "job_list", obj{"op": "job_list"}, ""
	case rest == "/stats" && method == http.MethodGet:
		return "stats", obj{"op": "stats"}, ""
	case rest == "/files" && method == http.MethodPut:
		return "file_put", obj{"op": "file_put", "path": q.Get("path"), "size": float64(len(body))}, ""
	case rest == "/files" && method == http.MethodGet:
		return "file_get", obj{"op": "file_get", "path": q.Get("path")}, ""
	case rest == "/tokens" && method == http.MethodPost:
		return "token_mint", obj{"op": "token_mint", "spec": j()}, ""
	case rest == "/tokens" && method == http.MethodGet:
		return "token_list", obj{"op": "token_list"}, ""
	case strings.HasPrefix(rest, "/tokens/") && method == http.MethodDelete:
		t, _ := url.PathUnescape(strings.TrimPrefix(rest, "/tokens/"))
		return "token_revoke", obj{"op": "token_revoke", "token": t}, ""
	case strings.HasPrefix(rest, "/jobs/"):
		name, sub, _ := strings.Cut(strings.TrimPrefix(rest, "/jobs/"), "/")
		name, _ = url.PathUnescape(name)
		switch {
		case sub == "" && method == http.MethodDelete:
			return "job_kill", obj{"op": "job_kill", "name": name}, ""
		case sub == "wait" && method == http.MethodGet:
			r := obj{"op": "job_wait", "name": name}
			if t := q.Get("timeout"); t != "" {
				n, _ := strconv.Atoi(t)
				r["timeout_ms"] = float64(n * 1000)
			}
			return "job_wait", r, ""
		case sub == "logs" && method == http.MethodGet:
			r := obj{"op": "job_logs", "name": name}
			if t := q.Get("tail"); t != "" {
				n, _ := strconv.Atoi(t)
				r["tail"] = float64(n)
			}
			if q.Get("follow") == "1" {
				r["follow"] = true
				return "job_logs", r, "logs"
			}
			return "job_logs", r, ""
		}
	}
	return "", nil, ""
}

// plainMap maps a desk's events to the plaintext SSE events (the server's
// ExecEvents / LogEvents).
type plainMap struct {
	kind, desk string
	carry      map[string][]byte
}

func (p *plainMap) text(stream string, b []byte, final bool) string {
	buf := append(p.carry[stream], b...)
	cut := len(buf)
	if !final {
		for i := len(buf) - 1; i >= 0 && i >= len(buf)-3; i-- {
			if utf8.RuneStart(buf[i]) {
				if !utf8.FullRune(buf[i:]) {
					cut = i
				}
				break
			}
		}
	}
	p.carry[stream] = append([]byte(nil), buf[cut:]...)
	return string(buf[:cut])
}

func (p *plainMap) mapEvent(e event) [][2]any {
	switch e.Event {
	case "stdout", "stderr":
		s := e.Event
		if p.kind == "logs" {
			s = "stdout"
		}
		b, _ := e2e.B64Decode(e.Data)
		t := p.text(s, b, false)
		if t == "" {
			return nil
		}
		if p.kind == "logs" {
			return [][2]any{{"output", obj{"event": "output", "data": t}}}
		}
		return [][2]any{{e.Event, obj{"event": e.Event, "data": t}}}
	case "exit":
		r, _ := e.Result.(obj)
		var evs [][2]any
		if p.kind == "logs" {
			if t := p.text("stdout", nil, true); t != "" {
				evs = append(evs, [2]any{"output", obj{"event": "output", "data": t}})
			}
			if r["interrupted"] == true {
				return append(evs, [2]any{"interrupted", obj{"event": "interrupted"}})
			}
			return append(evs, [2]any{"end", obj{"event": "end", "job": r["job"]}})
		}
		for _, s := range []string{"stdout", "stderr"} {
			if t := p.text(s, nil, true); t != "" {
				evs = append(evs, [2]any{s, obj{"event": s, "data": t}})
			}
		}
		rest := obj{"event": "exit"}
		for k, v := range r {
			if k != "stdout" && k != "stderr" && k != "truncated" {
				rest[k] = v
			}
		}
		return append(evs, [2]any{"exit", rest})
	case "error":
		er := obj{"kind": e.Kind, "message": e.Message, "desk": p.desk}
		if e.Reason != "" {
			er["reason"] = e.Reason
		}
		if p.kind == "exec" {
			code := 255
			if e.Kind == "refused" {
				code = 254
			}
			return [][2]any{{"error", obj{"event": "error", "exit": code, "error": er}}}
		}
		return [][2]any{{"error", obj{"event": "error", "error": er}}}
	}
	return nil
}

// writeSSE writes one event in pieces (split mid-line, "\r\n" across
// writes), a keep-alive comment first.
func writeSSE(w http.ResponseWriter, name string, v any) {
	data, _ := json.Marshal(v)
	text := ": keep-alive\r\nevent: " + name + "\r\ndata: " + string(data) + "\r\n\r\n"
	cuts := []int{3, len(text) / 2, len(text) - 1, len(text)}
	at := 0
	f, _ := w.(http.Flusher)
	for _, c := range cuts {
		_, _ = w.Write([]byte(text[at:c]))
		at = c
		if f != nil {
			f.Flush()
		}
		time.Sleep(time.Millisecond)
	}
}

func (s *Server) deskOp(w http.ResponseWriter, req *http.Request, rec Recorded, d *Desk, id, rest, deskToken string) {
	// A sealed request: the POST body's `e2e`, or the header.
	var sealedReq *e2e.SealedRequest
	if req.Method == http.MethodPost && len(rec.Body) > 0 {
		var j struct {
			E2E *e2e.SealedRequest `json:"e2e"`
		}
		if json.Unmarshal(rec.Body, &j) == nil && j.E2E != nil {
			sealedReq = j.E2E
		}
	}
	if h := req.Header.Get(e2e.Header); h != "" {
		b, _ := e2e.B64Decode(h)
		var r e2e.SealedRequest
		if json.Unmarshal(b, &r) == nil {
			sealedReq = &r
		}
	}
	routeBody := rec.Body
	if sealedReq != nil {
		routeBody = nil
	}
	op, opReq, stream := routeOp(req.Method, rest, rec.Query, routeBody)
	if op == "" {
		s.fail1(w, "usage", "no route", "no_route", "")
		return
	}
	s.mu.Lock()
	if d.Offline && sealedReq == nil {
		d.Offline = false // the API wakes it for the operation
	}
	s.mu.Unlock()
	input := rec.Body
	var seal *e2e.DeskSeal
	if sealedReq != nil {
		if d.Secret == nil {
			s.fail1(w, "protocol", "the desk cannot open end-to-end encrypted operations", "e2e_unsupported", id)
			return
		}
		var plain []byte
		for _, k := range append([][]byte{d.Secret}, d.Previous...) {
			p, ds, err := e2e.OpenRequest(k, id, op, *sealedReq)
			if err == nil {
				plain, seal = p, ds
				break
			}
		}
		if seal == nil {
			s.fail1(w, "refused", "the end-to-end encrypted request did not open: it was altered, or sealed to another key (fetch the desk's e2e_pub again)", "e2e_decrypt_failed", id)
			return
		}
		var inner struct {
			V       int   `json:"v"`
			TS      int64 `json:"ts"`
			Request obj   `json:"request"`
		}
		_ = json.Unmarshal(plain, &inner)
		if inner.V != 1 || math.Abs(float64(inner.TS-time.Now().Unix())) > 600 {
			s.fail1(w, "refused", "stale", "e2e_stale", id)
			return
		}
		if inner.Request["op"] != op {
			s.fail1(w, "refused", "op mismatch", "e2e_op_mismatch", id)
			return
		}
		opReq = inner.Request
		if op == "file_put" {
			var parts bytes.Buffer
			last := false
			for _, line := range strings.Split(string(rec.Body), "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				f, err := e2e.ParseFrame([]byte(line))
				if err != nil {
					s.fail1(w, "usage", "a frame is malformed", "e2e_malformed", id)
					return
				}
				l, data, err := seal.OpenInput(f)
				if err != nil {
					s.fail1(w, "refused", "an input frame did not open", "e2e_decrypt_failed", id)
					return
				}
				parts.Write(data)
				last = l
			}
			if !last {
				s.fail1(w, "usage", "the upload ended early", "body_interrupted", id)
				return
			}
			input = parts.Bytes()
		}
		s.mu.Lock()
		s.sealed = append(s.sealed, op)
		s.mu.Unlock()
	} else {
		if d.Required {
			s.fail1(w, "refused", "This desk requires end-to-end encryption for API commands.", "e2e_required", id)
			return
		}
		s.mu.Lock()
		s.plain = append(s.plain, op)
		s.mu.Unlock()
	}
	if op == "job_wait" {
		s.mu.Lock()
		s.waits = append(s.waits, rec.Query.Get("timeout"))
		s.mu.Unlock()
	}
	s.answer(w, d, id, op, opReq, stream, input, seal, deskToken)
}
