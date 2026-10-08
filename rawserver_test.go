package gaiadesk

// A raw TCP "HTTP server" with no framework in between, for the ways a real
// server or proxy fails: accept a request and close the socket before any
// response byte (FIN or RST, with or without reading the body), answer the
// headers and part of the body and then go silent with the socket open, or
// never answer at all. It proves what the SDK (and net/http under it) does
// on the wire itself, not what a test harness happens to do.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type rawMode int32

const (
	// Read the request's headers, then close (FIN) before any response byte,
	// leaving the body unread.
	rawCloseBeforeResponse rawMode = iota
	// Read the headers, then reset the connection (RST) before any response byte.
	rawResetBeforeResponse
	// Read the whole request (Content-Length or chunked body), then close
	// before any response byte.
	rawCloseAfterBody
	// Send 200 headers and one chunk of a chunked body, then nothing, with
	// the socket left open.
	rawStallMidBody
	// Send 200 headers with a Content-Length larger than what follows, then
	// nothing, the socket open.
	rawStallMidJSON
	// Send 200 text/event-stream headers and one stdout event, then nothing,
	// the socket open.
	rawStallMidEvents
	// Read the request and never answer.
	rawSilent
	// Answer the first request on a connection with a 200 JSON body and keep
	// the connection alive; read the second one whole and close before any
	// response byte: a pooled connection that dies under a request.
	rawKeepAliveThenClose
	// Answer every request with an error envelope (setStatus: its status,
	// Retry-After and reason), keeping the connection alive.
	rawStatus
)

// rawAnswer is the JSON rawKeepAliveThenClose answers with: a finished exec
// (its stdout says which connection answered), a stats report.
const rawAnswer = `{"desk":"123456789","exit":0,"stdout":"conn-%d","stderr":"","timed_out":false}`

type rawServer struct {
	l    net.Listener
	url  string
	mode atomic.Int32

	mu       sync.Mutex
	byMethod map[string]int
	conns    map[net.Conn]bool
	nconn    int
	wg       sync.WaitGroup
	done     chan struct{}
	status   rawStatusAnswer
}

type rawStatusAnswer struct {
	code               int
	retryAfter, reason string
}

func newRawServer(t *testing.T, mode rawMode) *rawServer {
	t.Helper()
	return newRawServerOn(t, "127.0.0.1:0", mode)
}

// newRawServerOn listens on addr (a port that was free a moment ago).
func newRawServerOn(t *testing.T, addr string, mode rawMode) *rawServer {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	s := &rawServer{l: l, url: "http://" + l.Addr().String() + "/v1", byMethod: map[string]int{}, conns: map[net.Conn]bool{}, done: make(chan struct{})}
	s.setMode(mode)
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(s.close)
	return s
}

func (s *rawServer) setMode(m rawMode) { s.mode.Store(int32(m)) }

// setStatus answers every request with this status, Retry-After ("" for
// none) and reason.
func (s *rawServer) setStatus(code int, retryAfter, reason string) {
	s.mu.Lock()
	s.status = rawStatusAnswer{code, retryAfter, reason}
	s.mu.Unlock()
	s.setMode(rawStatus)
}

// count is how many requests with this method arrived.
func (s *rawServer) count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byMethod[method]
}

func (s *rawServer) accept() {
	defer s.wg.Done()
	for {
		c, err := s.l.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[c] = true
		s.nconn++
		id := s.nconn
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(c, id)
		}()
	}
}

// drop closes a connection (FIN, or RST with reset) and forgets it.
func (s *rawServer) drop(c net.Conn, reset bool) {
	if tc, ok := c.(*net.TCPConn); ok && reset {
		_ = tc.SetLinger(0)
	}
	_ = c.Close()
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func (s *rawServer) serve(c net.Conn, id int) {
	br := bufio.NewReader(c)
	for n := 1; ; n++ {
		method, h, err := readHead(br)
		if err != nil {
			s.drop(c, false)
			return
		}
		s.mu.Lock()
		s.byMethod[method]++
		s.mu.Unlock()
		switch rawMode(s.mode.Load()) {
		case rawCloseBeforeResponse:
			s.drop(c, false)
			return
		case rawResetBeforeResponse:
			s.drop(c, true)
			return
		case rawCloseAfterBody:
			_ = readBody(br, h)
			s.drop(c, false)
			return
		case rawStallMidBody:
			_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n")
		case rawStallMidJSON:
			_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"desk\":")
		case rawStallMidEvents:
			ev := "event: stdout\ndata: {\"event\":\"stdout\",\"data\":\"hi\"}\n\n"
			_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(ev), ev)
		case rawSilent:
		case rawKeepAliveThenClose:
			if err := readBody(br, h); err != nil || n > 1 {
				s.drop(c, false)
				return
			}
			body := fmt.Sprintf(rawAnswer, id)
			_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
			continue
		case rawStatus:
			if err := readBody(br, h); err != nil {
				s.drop(c, false)
				return
			}
			s.mu.Lock()
			st := s.status
			s.mu.Unlock()
			kind := "unreachable"
			if st.code == 429 || st.code == 409 {
				kind = "refused"
			}
			body := fmt.Sprintf(`{"error":{"kind":%q,"message":"HTTP %d","reason":%q}}`, kind, st.code, st.reason)
			extra := ""
			if st.retryAfter != "" {
				extra = "Retry-After: " + st.retryAfter + "\r\n"
			}
			_, _ = fmt.Fprintf(c, "HTTP/1.1 %d Error\r\nContent-Type: application/json\r\nContent-Length: %d\r\n%s\r\n%s", st.code, len(body), extra, body)
			continue
		}
		// Held open, silent (any request body left unread), until the
		// server stops.
		<-s.done
		s.drop(c, false)
		return
	}
}

// readHead reads a request's head: its method and headers (lower-case names).
func readHead(br *bufio.Reader) (string, map[string]string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", nil, err
	}
	method, _, _ := strings.Cut(line, " ")
	h := map[string]string{}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			return "", nil, err
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "" {
			return method, h, nil
		}
		k, v, _ := strings.Cut(l, ":")
		h[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
}

// readBody reads a request's body: Content-Length bytes, or chunks.
func readBody(br *bufio.Reader, h map[string]string) error {
	if strings.EqualFold(h["transfer-encoding"], "chunked") {
		for {
			l, err := br.ReadString('\n')
			if err != nil {
				return err
			}
			size, err := strconv.ParseInt(strings.TrimSpace(strings.SplitN(l, ";", 2)[0]), 16, 64)
			if err != nil {
				return err
			}
			if size == 0 {
				_, err = br.ReadString('\n') // the blank line after the last chunk (no trailers sent)
				return err
			}
			if _, err := io.CopyN(io.Discard, br, size+2); err != nil {
				return err
			}
		}
	}
	n, _ := strconv.ParseInt(h["content-length"], 10, 64)
	_, err := io.CopyN(io.Discard, br, n)
	return err
}

func (s *rawServer) close() {
	close(s.done)
	_ = s.l.Close()
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}
