package mockapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Gaia-Desk/gaiadesk-go/internal/e2e"
)

// answer runs the operation and answers as the real API does
// (signal/src/api_v1/desk_ops): plaintext JSON / SSE / bytes for a
// plaintext call; `{"e2e": {"events"}}`, the error envelope with a
// placeholder message and `e2e.events`, `sealed` SSE events and NDJSON
// downloads for a sealed one.
func (s *Server) answer(w http.ResponseWriter, d *Desk, id, op string, req obj, stream string, input []byte, seal *e2e.DeskSeal, deskToken string) {
	events := s.run(d, id, req, input, deskToken)
	s.mu.Lock()
	tamper := s.Tamper
	s.mu.Unlock()
	sealOne := func(e event) e2e.Frame {
		f := seal.SealEvent(e)
		if tamper == "flip" {
			c, _ := e2e.B64Decode(f.Ciphertext)
			c[0] ^= 1
			f.Ciphertext = e2e.B64URL(c)
		}
		return f
	}
	sealAll := func() []e2e.Frame {
		fs := make([]e2e.Frame, len(events))
		for i, e := range events {
			fs[i] = sealOne(e)
		}
		return fs
	}
	final := events[len(events)-1]
	var failed *event
	if final.Event == "error" {
		failed = &final
	}
	errorAnswer := func(e event, extra obj) obj {
		kind := e.Kind
		switch kind {
		case "refused", "usage", "protocol", "unreachable", "connection_lost":
		default:
			kind = "failed"
		}
		msg := e.Message
		if seal != nil {
			msg = "The desk reported an error (end-to-end encrypted)."
		}
		env := s.envelope(kind, msg, e.Reason, id)
		for k, v := range extra {
			env["error"].(obj)[k] = v
		}
		if seal != nil {
			env["e2e"] = obj{"v": 1, "events": sealAll()}
		}
		return env
	}

	// Streams.
	if stream != "" {
		if events[0].Event == "error" {
			s.send(w, statusFor(events[0].Kind, events[0].Reason), errorAnswer(events[0], nil))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", s.requestID())
		w.WriteHeader(200)
		pm := &plainMap{kind: stream, desk: id, carry: map[string][]byte{}}
		for _, e := range events {
			if seal != nil && tamper != "plaintext" {
				f := sealOne(e)
				writeSSE(w, "sealed", obj{"event": "sealed", "seq": f.Seq, "nonce": f.Nonce, "ciphertext": f.Ciphertext})
			} else {
				for _, m := range pm.mapEvent(e) {
					writeSSE(w, m[0].(string), m[1])
				}
			}
		}
		if final.Event != "exit" && final.Event != "error" {
			lost := obj{"event": "error", "error": obj{"kind": "connection_lost", "message": "The desk went away during this operation.", "desk": id, "reason": "desk_disconnected"}}
			if stream == "exec" {
				lost["exit"] = 255
			}
			writeSSE(w, "error", lost)
		}
		return
	}

	// A download.
	if op == "file_get" {
		if events[0].Event == "error" {
			s.send(w, statusFor(events[0].Kind, events[0].Reason), errorAnswer(events[0], nil))
			return
		}
		path := str(req["path"])
		if seal == nil {
			var all []byte
			for _, e := range events {
				if e.Event == "stdout" {
					b, _ := e2e.B64Decode(e.Data)
					all = append(all, b...)
				}
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Request-Id", s.requestID())
			if path == "broken" {
				// A failure after the first byte breaks the transfer.
				w.Header().Set("Content-Length", strconv.Itoa(len(all)+100))
			}
			w.WriteHeader(200)
			_, _ = w.Write(all)
			return
		}
		w.Header().Set("Content-Type", e2e.FramesContentType)
		w.Header().Set("X-Request-Id", s.requestID())
		w.WriteHeader(200)
		keep := events
		if path == "truncated" {
			keep = events[:len(events)-1]
		}
		for _, e := range keep {
			b, _ := json.Marshal(sealOne(e))
			_, _ = w.Write(append(b, '\n'))
		}
		return
	}

	okStatus := 200
	if op == "job_start" || op == "token_mint" {
		okStatus = 201
	}
	name := str(req["name"])
	held := op == "job_wait" && (name == "held" || name == "held-gone" || name == "held-fail")
	if held {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", s.requestID())
		w.Header().Set("GaiaDesk-Held", "1")
		w.WriteHeader(200)
		f, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte(" "))
			if f != nil {
				f.Flush()
			}
			time.Sleep(2 * time.Millisecond)
		}
		if failed != nil {
			_ = json.NewEncoder(w).Encode(errorAnswer(*failed, obj{"status": statusFor(failed.Kind, failed.Reason)}))
			return
		}
		if seal != nil {
			_ = json.NewEncoder(w).Encode(obj{"e2e": obj{"v": 1, "events": sealAll()}})
			return
		}
		_ = json.NewEncoder(w).Encode(final.Result)
		return
	}
	if failed != nil {
		s.send(w, statusFor(failed.Kind, failed.Reason), errorAnswer(*failed, nil))
		return
	}
	if seal != nil && tamper != "plaintext" {
		s.send(w, okStatus, obj{"e2e": obj{"v": 1, "events": sealAll()}})
		return
	}
	s.send(w, okStatus, final.Result)
}
