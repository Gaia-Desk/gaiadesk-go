package gaiadesk

// Opening sealed answers into exactly what the plaintext call answers: JSON
// results, error envelopes, SSE events (as the server maps a desk's events:
// protocol/src/desk_op_http.rs) and file bytes; and sealing an upload.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/Gaia-Desk/gaiadesk-go/internal/e2e"
)

// openFailed is the protocol error for a sealed event that did not open.
func openFailed(err error, op string) *Error {
	reason := ReasonE2EMalformed
	var oe *e2e.OpenError
	if errors.As(err, &oe) {
		reason = oe.Reason
	}
	e := newError(ClassProtocol, reason, "the desk's end-to-end encrypted answer did not open: "+err.Error())
	e.Op = op
	return e
}

// sealedEvents are the `e2e.events` of an answer or an error envelope.
func sealedEvents(top map[string]json.RawMessage) ([]json.RawMessage, bool) {
	var holder struct {
		Events []json.RawMessage `json:"events"`
	}
	if raw, ok := top["e2e"]; ok && json.Unmarshal(raw, &holder) == nil && holder.Events != nil {
		return holder.Events, true
	}
	var errObj map[string]json.RawMessage
	if raw, ok := top["error"]; ok && json.Unmarshal(raw, &errObj) == nil {
		if inner, ok := errObj["e2e"]; ok && json.Unmarshal(inner, &holder) == nil && holder.Events != nil {
			return holder.Events, true
		}
	}
	return nil, false
}

// openErrorEnvelope gives an error envelope the desk's real message: a
// desk's error comes with a placeholder `message` and `e2e.events`, whose
// last opens to the `error`. An envelope without events (the server's own
// error) is as it is.
func openErrorEnvelope(text []byte, seal *e2e.CallerSeal) []byte {
	var top map[string]json.RawMessage
	if json.Unmarshal(text, &top) != nil {
		return text
	}
	var errObj map[string]json.RawMessage
	if json.Unmarshal(top["error"], &errObj) != nil || errObj == nil {
		return text
	}
	events, ok := sealedEvents(top)
	if !ok {
		return text
	}
	var last *e2e.DeskEvent
	for _, f := range events {
		ev, err := seal.OpenDeskEvent(f)
		if err != nil {
			last = nil
			break
		}
		last = &ev
	}
	var message string
	if last != nil && last.Event == "error" {
		message = last.Message
	} else {
		var placeholder string
		_ = json.Unmarshal(errObj["message"], &placeholder)
		if placeholder == "" {
			placeholder = "the desk reported an error"
		}
		message = placeholder + " (its end-to-end encrypted message did not open)"
	}
	delete(errObj, "e2e")
	delete(top, "e2e")
	errObj["message"], _ = json.Marshal(message)
	top["error"], _ = json.Marshal(errObj)
	out, _ := json.Marshal(top)
	return out
}

// openAnswer opens a sealed JSON answer (`{"e2e": {"events"}}`) into the
// result the plaintext call answers; a held body's envelope is opened and
// returned as it is (the caller reads it).
func openAnswer(text []byte, seal *e2e.CallerSeal, op string) (json.RawMessage, error) {
	if _, ok := errorEnvelope(text); ok {
		return openErrorEnvelope(text, seal), nil
	}
	var top map[string]json.RawMessage
	_ = json.Unmarshal(text, &top)
	events, _ := sealedEvents(top)
	if len(events) == 0 {
		e := newError(ClassProtocol, ReasonE2EUnsealedAnswer, "the GaiaDesk API answered an end-to-end encrypted operation without sealed events")
		e.Op, e.Body = op, text
		return nil, e
	}
	var last e2e.DeskEvent
	for _, f := range events {
		ev, err := seal.OpenDeskEvent(f)
		if err != nil {
			return nil, openFailed(err, op)
		}
		last = ev
	}
	switch last.Event {
	case "exit":
		if len(last.Result) == 0 {
			return json.RawMessage("null"), nil
		}
		return last.Result, nil
	case "error":
		return nil, deskError(last, seal.Desk, op)
	}
	e := newError(ClassProtocol, ReasonE2EMalformed, "the desk's sealed answer has no result")
	e.Op = op
	return nil, e
}

// deskErrorStatus is the HTTP status /v1 gives a desk's error (protocol
// desk_op_http.rs desk_error_status) and the class it answers.
func deskErrorStatus(kind, reason string) (int, Class) {
	switch kind {
	case "usage":
		return 400, ClassUsage
	case "refused":
		switch reason {
		case ReasonDeskBusy:
			return 429, ClassRefused
		case ReasonE2ERequired:
			return 409, ClassRefused
		}
		return 403, ClassRefused
	case "unreachable":
		return 409, ClassUnreachable
	case "connection_lost":
		return 502, ClassConnectionLost
	case "protocol":
		return 502, ClassProtocol
	}
	return 422, ClassFailed
}

// deskError is a desk's opened `error` event as the error the plaintext
// call gives.
func deskError(ev e2e.DeskEvent, desk, op string) *Error {
	status, class := deskErrorStatus(ev.Kind, ev.Reason)
	reason := firstNonEmpty(ev.Reason, string(class))
	e := newError(class, reason, ev.Message)
	e.Desk, e.Op, e.Status = desk, op, status
	e.Body, _ = json.Marshal(map[string]any{"error": map[string]string{"kind": string(class), "message": ev.Message, "reason": reason, "desk": desk}})
	return e
}

// ───────────────────────────── files ─────────────────────────────

// uploadSealer is an upload's body, sealed: one input frame per line, at
// most 48 KiB of the file each, the last flagged. The file is read as the
// body is sent.
type uploadSealer struct {
	seal *e2e.CallerSeal
	src  io.Reader
	left int64
	buf  bytes.Buffer
	done bool
}

func sealUpload(seal *e2e.CallerSeal, src io.Reader, size int64) io.Reader {
	return &uploadSealer{seal: seal, src: src, left: size}
}

func (u *uploadSealer) Read(p []byte) (int, error) {
	for u.buf.Len() == 0 && !u.done {
		n := min(u.left, e2e.InputChunk)
		chunk := make([]byte, n)
		if _, err := io.ReadFull(u.src, chunk); err != nil {
			return 0, fmt.Errorf("the file ended before its %d bytes were read: %w", u.left, err)
		}
		u.left -= n
		last := u.left == 0
		b, _ := json.Marshal(u.seal.SealInput(last, chunk))
		u.buf.Write(b)
		u.buf.WriteByte('\n')
		u.done = last
	}
	if u.buf.Len() == 0 {
		return 0, io.EOF
	}
	return u.buf.Read(p)
}

// sealedDownload reads a sealed download (`application/x-ndjson`, one
// sealed event per line) as the file's bytes. Missing its last event, the
// download is incomplete.
type sealedDownload struct {
	seal    *e2e.CallerSeal
	br      *bufio.Reader
	body    io.Closer
	op      string
	pending []byte
	result  json.RawMessage
	done    bool
	err     error
}

func (d *sealedDownload) Read(p []byte) (int, error) {
	for {
		if len(d.pending) > 0 {
			n := copy(p, d.pending)
			d.pending = d.pending[n:]
			return n, nil
		}
		if d.done {
			return 0, io.EOF
		}
		if d.err != nil {
			return 0, d.err
		}
		line, rerr := d.br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			d.event(line)
			continue
		}
		if rerr == io.EOF {
			e := newError(ClassConnectionLost, ReasonIncomplete, "the download ended before the desk said it was complete")
			e.Op, e.Desk = d.op, d.seal.Desk
			d.err = e
		} else if rerr != nil {
			d.err = cutOff(d.op, rerr)
		}
	}
}

func (d *sealedDownload) event(line []byte) {
	ev, err := d.seal.OpenDeskEvent(line)
	if err != nil {
		d.err = openFailed(err, d.op)
		return
	}
	switch ev.Event {
	case "stdout":
		b, ok := e2e.B64Decode(ev.Data)
		if !ok {
			e := newError(ClassProtocol, ReasonE2EMalformed, "a sealed download carries bytes that are not base64")
			e.Op = d.op
			d.err = e
			return
		}
		d.pending = b
	case "error":
		d.err = deskError(ev, d.seal.Desk, d.op)
	case "exit":
		d.result = ev.Result
		d.done = true
	}
}

func (d *sealedDownload) Close() error { return d.body.Close() }

// cutOff is the error for a body that broke off mid-read.
func cutOff(op string, err error) *Error {
	if errors.Is(err, errStreamClosed) {
		return interrupted(op, err)
	}
	e := newError(ClassConnectionLost, "", "the transfer broke off: "+err.Error())
	e.Op, e.Err = op, err
	return e
}

// plainDownload reads a plaintext download, a break being ConnectionLost.
type plainDownload struct {
	body io.ReadCloser
	op   string
}

func (d *plainDownload) Read(p []byte) (int, error) {
	n, err := d.body.Read(p)
	if err != nil && err != io.EOF {
		return n, cutOff(d.op, err)
	}
	return n, err
}

func (d *plainDownload) Close() error { return d.body.Close() }

// ───────────────────────────── sealed streams ─────────────────────────────

// utf8Carry decodes a byte stream as text, carrying a character split
// across chunks to the next one (invalid bytes become U+FFFD).
type utf8Carry struct{ carry []byte }

func (u *utf8Carry) decode(b []byte, final bool) string {
	buf := append(u.carry, b...)
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
	u.carry = append([]byte(nil), buf[cut:]...)
	return strings.ToValidUTF8(string(buf[:cut]), "�")
}

// unsealer turns a sealed SSE stream into the plaintext one: each `sealed`
// event opened (in order) and mapped as the API maps a desk's events
// (exec: stdout, stderr, exit, error; logs: output, end, interrupted,
// error), split characters carried. A plaintext `error` (the server's: the
// desk was lost) passes; any other plaintext output is refused.
type unsealer struct {
	src   *sseReader
	seal  *e2e.CallerSeal
	kind  string
	op    string
	queue []sseEvent
	dec   map[string]*utf8Carry
}

func newUnsealer(src *sseReader, seal *e2e.CallerSeal, kind, op string) *unsealer {
	return &unsealer{src: src, seal: seal, kind: kind, op: op, dec: map[string]*utf8Carry{"stdout": {}, "stderr": {}}}
}

func sse(name string, v any) sseEvent {
	b, _ := json.Marshal(v)
	return sseEvent{event: name, data: string(b)}
}

func (u *unsealer) next() (sseEvent, error) {
	for len(u.queue) == 0 {
		ev, err := u.src.next()
		if err != nil {
			return sseEvent{}, err
		}
		if err := u.feed(ev); err != nil {
			return sseEvent{}, err
		}
	}
	ev := u.queue[0]
	u.queue = u.queue[1:]
	return ev, nil
}

func (u *unsealer) feed(ev sseEvent) error {
	if ev.event == "error" {
		u.queue = append(u.queue, ev)
		return nil
	}
	if ev.event != "sealed" {
		switch ev.event {
		case "stdout", "stderr", "exit", "output", "end", "interrupted", "message":
			e := newError(ClassProtocol, ReasonE2EUnsealedAnswer, fmt.Sprintf("the GaiaDesk API sent a plaintext `%s` event in an end-to-end encrypted stream", ev.event))
			e.Op = u.op
			return e
		}
		return nil
	}
	var probe struct {
		Event *string `json:"event"`
	}
	data := []byte(ev.data)
	if json.Unmarshal(data, &probe) == nil && probe.Event != nil && *probe.Event != "sealed" {
		data = []byte("null")
	}
	de, err := u.seal.OpenDeskEvent(data)
	if err != nil {
		return openFailed(err, u.op)
	}
	switch de.Event {
	case "stdout", "stderr":
		b, ok := e2e.B64Decode(de.Data)
		if !ok {
			return nil // not base64: the desk's bug, dropped (as the server does)
		}
		stream := de.Event
		if u.kind == "logs" {
			stream = "stdout"
		}
		if text := u.dec[stream].decode(b, false); text != "" {
			if u.kind == "logs" {
				u.queue = append(u.queue, sse("output", map[string]string{"event": "output", "data": text}))
			} else {
				u.queue = append(u.queue, sse(de.Event, map[string]string{"event": de.Event, "data": text}))
			}
		}
	case "exit":
		var result map[string]json.RawMessage
		_ = json.Unmarshal(de.Result, &result)
		if result == nil {
			result = map[string]json.RawMessage{}
		}
		if u.kind == "exec" {
			for _, s := range []string{"stdout", "stderr"} {
				if t := u.dec[s].decode(nil, true); t != "" {
					u.queue = append(u.queue, sse(s, map[string]string{"event": s, "data": t}))
				}
			}
			delete(result, "stdout")
			delete(result, "stderr")
			delete(result, "truncated")
			result["event"] = json.RawMessage(`"exit"`)
			u.queue = append(u.queue, sse("exit", result))
		} else {
			if t := u.dec["stdout"].decode(nil, true); t != "" {
				u.queue = append(u.queue, sse("output", map[string]string{"event": "output", "data": t}))
			}
			if string(result["interrupted"]) == "true" {
				u.queue = append(u.queue, sse("interrupted", map[string]string{"event": "interrupted"}))
			} else {
				u.queue = append(u.queue, sse("end", map[string]any{"event": "end", "job": result["job"]}))
			}
		}
	case "error":
		errObj := map[string]string{"kind": de.Kind, "message": de.Message, "desk": u.seal.Desk}
		if de.Reason != "" {
			errObj["reason"] = de.Reason
		}
		if u.kind == "exec" {
			exit := 255
			if de.Kind == "refused" {
				exit = 254
			}
			u.queue = append(u.queue, sse("error", map[string]any{"event": "error", "exit": exit, "error": errObj}))
		} else {
			u.queue = append(u.queue, sse("error", map[string]any{"event": "error", "error": errObj}))
		}
	}
	return nil
}
