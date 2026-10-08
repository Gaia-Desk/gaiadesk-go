package gaiadesk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"sync"
)

// Chunk is a piece of a stream's output.
type Chunk struct {
	// Stream is "stdout" or "stderr" (a job's log is all "stdout").
	Stream string
	// Data is the text, as bytes (a character split across chunks arrives
	// whole in the later one).
	Data []byte
}

// Text is the chunk's data as a string.
func (c Chunk) Text() string { return string(c.Data) }

// Exit is how a stream ended.
type Exit struct {
	// ExitCode is what gaiadesk-cli would exit with: the command's own code
	// (exec), 0 for a log follow that ended or was stopped, 130 when the
	// stream was closed or its context cancelled, 254 refused, 255 the rest.
	ExitCode int
	// StderrTail is the reason, when it failed (or what ended a log follow).
	StderrTail string
	// Result is how the command ended, without its output (exec streams).
	Result *ExecExit
	// Job is the job as it ended (a log follow's `end`).
	Job *Job
	// Error is why it never ran, was stopped or broke off, or nil.
	Error *CliError
}

// Err is the exit's Error as a typed *Error, or nil. (A command that ran
// and exited non-zero is not an error: see ExitCode.)
func (e *Exit) Err() error {
	if e == nil || e.Error == nil {
		return nil
	}
	err := newError(Class(e.Error.Kind), e.Error.Reason, e.Error.Message)
	err.Desk = e.Error.Desk
	err.ExitCode = e.ExitCode
	return err
}

// eventSource gives a stream's events: plain SSE, or opened sealed ones.
type eventSource interface {
	next() (sseEvent, error)
}

// Stream is a running command (ExecStream) or a log follow (FollowJobLogs):
// its output as it comes (Recv, Chunks, Copy), then its Exit. Recv is not
// safe for concurrent use; Close may be called from any goroutine.
type Stream struct {
	op, kind, jobName string
	src               eventSource
	body              io.Closer
	ctx               context.Context
	cancel            context.CancelFunc

	mu     sync.Mutex
	closed bool
	exit   *Exit
	last   map[string]json.RawMessage
}

func newStream(ctx context.Context, cancel context.CancelFunc, op, kind, job string, src eventSource, body io.Closer) *Stream {
	return &Stream{op: op, kind: kind, jobName: job, src: src, body: body, ctx: ctx, cancel: cancel}
}

// Recv returns the next chunk of output; io.EOF once the stream has ended
// (Exit then says how).
func (s *Stream) Recv() (Chunk, error) {
	for {
		if s.exit != nil {
			return Chunk{}, io.EOF
		}
		ev, err := s.src.next()
		if err != nil {
			if err == io.EOF {
				s.end(s.endOfStream())
			} else {
				s.end(s.failed(err))
			}
			continue
		}
		var o map[string]json.RawMessage
		if json.Unmarshal([]byte(ev.data), &o) != nil || o == nil {
			continue
		}
		name := str(o["event"])
		if _, ok := o["event"]; !ok {
			name = ev.event
		}
		if c, ok := s.handle(name, o); ok {
			return c, nil
		}
	}
}

func str(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func (s *Stream) handle(name string, o map[string]json.RawMessage) (Chunk, bool) {
	var data *string
	if raw, ok := o["data"]; ok {
		var d string
		if json.Unmarshal(raw, &d) == nil {
			data = &d
		}
	}
	if s.kind == "exec" {
		switch name {
		case "stdout", "stderr":
			if data != nil {
				return Chunk{Stream: name, Data: []byte(*data)}, true
			}
		case "exit", "error":
			o["event"] = json.RawMessage(fmt.Sprintf("%q", name))
			s.last = o
		}
		return Chunk{}, false
	}
	switch name {
	case "output":
		if data != nil {
			return Chunk{Stream: "stdout", Data: []byte(*data)}, true
		}
	case "end":
		var job Job
		x := &Exit{ExitCode: 0}
		if json.Unmarshal(o["job"], &job) == nil && job.Name != "" {
			x.Job = &job
		}
		n := firstNonEmpty(job.Name, s.jobName)
		if job.ExitCode != nil {
			x.StderrTail = fmt.Sprintf("job %s exited (exit %d)", n, *job.ExitCode)
		} else {
			x.StderrTail = fmt.Sprintf("job %s %s", n, firstNonEmpty(job.State, "ended"))
		}
		s.end(x)
	case "interrupted":
		s.end(&Exit{ExitCode: 0, StderrTail: "stopped following; the job goes on"})
	case "error":
		ce := cliError(o["error"])
		if ce == nil {
			ce = &CliError{Kind: "protocol", Message: "the desk reported an error"}
		}
		s.end(&Exit{ExitCode: deskOpExit(ce.Kind), StderrTail: ce.Message, Error: ce})
	}
	return Chunk{}, false
}

// cliError reads an error object; nil when it is not one.
func cliError(raw json.RawMessage) *CliError {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil
	}
	var e CliError
	_ = json.Unmarshal(raw, &e)
	if e.Kind == "" {
		e.Kind = "failed"
	}
	return &e
}

func lostExit(message string) *Exit {
	return &Exit{ExitCode: 255, StderrTail: message, Error: &CliError{Kind: "connection_lost", Message: message}}
}

// endOfStream is the Exit when the body ended.
func (s *Stream) endOfStream() *Exit {
	if s.kind != "exec" {
		return lostExit("the event stream ended before the job did")
	}
	if s.last == nil {
		return lostExit("the event stream ended before the command did")
	}
	if str(s.last["event"]) == "exit" {
		var r ExecExit
		raw, _ := json.Marshal(s.last)
		_ = json.Unmarshal(raw, &r)
		x := &Exit{ExitCode: r.Exit, Result: &r, Error: r.Error}
		if r.Error != nil {
			x.StderrTail = r.Error.Message
		}
		return x
	}
	var code int
	_ = json.Unmarshal(s.last["exit"], &code)
	ce := cliError(s.last["error"])
	x := &Exit{ExitCode: code, Error: ce}
	if ce != nil {
		x.StderrTail = ce.Message
	}
	return x
}

// failed is the Exit for an error that broke the stream off.
func (s *Stream) failed(err error) *Exit {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed || s.ctx.Err() != nil {
		return &Exit{ExitCode: 130, StderrTail: "interrupted"}
	}
	e := AsError(err)
	if e == nil {
		e = newError(ClassUnreachable, ReasonNetwork, "the GaiaDesk API could not be reached: "+err.Error())
	}
	return exitForError(e)
}

// exitForError is the Exit for an error that ended (or prevented) a stream.
func exitForError(e *Error) *Exit {
	kind := string(e.Class)
	if kind == "" {
		kind = string(e.Kind)
	}
	return &Exit{ExitCode: e.ExitCode, StderrTail: e.Message, Error: &CliError{Kind: kind, Message: e.Message, Reason: e.Reason, Desk: e.Desk}}
}

func (s *Stream) end(x *Exit) {
	s.exit = x
	s.cancel()
	_ = s.body.Close()
}

// Exit is how the stream ended, or nil while it runs.
func (s *Stream) Exit() *Exit { return s.exit }

// Chunks iterates over the output; the iteration ends with the stream
// (Exit then says how).
func (s *Stream) Chunks() iter.Seq[Chunk] {
	return func(yield func(Chunk) bool) {
		for {
			c, err := s.Recv()
			if err != nil || !yield(c) {
				return
			}
		}
	}
}

// Copy writes the output to stdout and stderr (either may be nil to
// discard) until the stream ends, and returns its Exit. An error writing
// stops the stream and is returned.
func (s *Stream) Copy(stdout, stderr io.Writer) (*Exit, error) {
	for {
		c, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return s.exit, nil
		}
		w := stdout
		if c.Stream == "stderr" {
			w = stderr
		}
		if w == nil {
			continue
		}
		if _, err := w.Write(c.Data); err != nil {
			_ = s.Close()
			return s.Wait(), err
		}
	}
}

// Wait reads (and discards) the rest of the output and returns the Exit.
func (s *Stream) Wait() *Exit {
	for {
		if _, err := s.Recv(); err != nil {
			return s.exit
		}
	}
}

// Close stops the stream: the request is closed (the server stops the
// command, or stops following the job). The Exit is 130 interrupted unless
// it had already ended.
func (s *Stream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	return nil
}
