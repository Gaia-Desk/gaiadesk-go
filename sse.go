package gaiadesk

import (
	"errors"
	"io"
	"strings"
)

// sseEvent is one server-sent event: its `event:` name (default
// `message`) and its `data:` lines joined by "\n".
type sseEvent struct {
	event, data string
}

// sseParser is an incremental `text/event-stream` parser (the WHATWG
// rules: fields, comments, blank-line dispatch). Text may be split
// anywhere, even between "\r" and "\n".
type sseParser struct {
	buf   string
	event string
	data  []string
	has   bool
}

// feed takes text; it returns the events it completed.
func (p *sseParser) feed(text string) []sseEvent {
	p.buf += text
	var out []sseEvent
	for {
		i := strings.IndexAny(p.buf, "\r\n")
		if i < 0 {
			break
		}
		// A trailing "\r" may be the first half of "\r\n": wait for more.
		if p.buf[i] == '\r' && i == len(p.buf)-1 {
			break
		}
		line := p.buf[:i]
		n := 1
		if p.buf[i] == '\r' && p.buf[i+1] == '\n' {
			n = 2
		}
		p.buf = p.buf[i+n:]
		if ev, ok := p.line(line); ok {
			out = append(out, ev)
		}
	}
	return out
}

// end is the end of the stream: an event the server did not finish with a
// blank line is still delivered.
func (p *sseParser) end() []sseEvent {
	var out []sseEvent
	if p.buf != "" {
		line := strings.TrimSuffix(p.buf, "\r")
		p.buf = ""
		if ev, ok := p.line(line); ok {
			out = append(out, ev)
		}
	}
	if ev, ok := p.line(""); ok {
		out = append(out, ev)
	}
	return out
}

func (p *sseParser) line(line string) (sseEvent, bool) {
	if line == "" {
		if !p.has {
			p.event = ""
			return sseEvent{}, false
		}
		ev := sseEvent{event: p.event, data: strings.Join(p.data, "\n")}
		if ev.event == "" {
			ev.event = "message"
		}
		p.event, p.data, p.has = "", nil, false
		return ev, true
	}
	if strings.HasPrefix(line, ":") {
		return sseEvent{}, false // a comment: keep-alive
	}
	field, value, _ := strings.Cut(line, ":")
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "event":
		p.event = value
	case "data":
		p.data = append(p.data, value)
		p.has = true
	}
	return sseEvent{}, false
}

// errStreamClosed is a stream closed by its reader.
var errStreamClosed = errors.New("the stream was closed")

// sseReader reads the events of a `text/event-stream` body as they arrive.
type sseReader struct {
	body  io.Reader
	p     sseParser
	queue []sseEvent
	ended bool
	chunk []byte
	carry utf8Carry
}

func newSSEReader(body io.Reader) *sseReader {
	return &sseReader{body: body, chunk: make([]byte, 32*1024)}
}

// next is the next event; io.EOF at the end of the body.
func (r *sseReader) next() (sseEvent, error) {
	for len(r.queue) == 0 {
		if r.ended {
			return sseEvent{}, io.EOF
		}
		n, err := r.body.Read(r.chunk)
		if n > 0 {
			r.queue = append(r.queue, r.p.feed(r.carry.decode(r.chunk[:n], false))...)
		}
		if err == io.EOF {
			r.queue = append(r.queue, r.p.feed(r.carry.decode(nil, true))...)
			r.queue = append(r.queue, r.p.end()...)
			r.ended = true
		} else if err != nil {
			return sseEvent{}, err
		}
	}
	ev := r.queue[0]
	r.queue = r.queue[1:]
	return ev, nil
}
