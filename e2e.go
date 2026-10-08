package gaiadesk

// End-to-end encryption on the hosted API transport: whether and to which
// key an operation is sealed (the desk's `e2e_pub` from GET /desks/{id},
// cached; pinned keys; the mode; a wake when a desk that must be sealed to
// lists no key), the one retry each for `e2e_required` and
// `e2e_decrypt_failed`, and turning sealed answers back into exactly what
// the plaintext call answers. The crypto is internal/e2e; the answers'
// opening is e2e_open.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/Gaia-Desk/gaiadesk-go/internal/e2e"
)

// E2EMode is the end-to-end encryption of desk operations on the hosted API.
type E2EMode string

// The modes.
const (
	// E2EAuto (default): sealed when the desk lists a key (or requires it);
	// else in the clear, with a warning once per desk.
	E2EAuto E2EMode = "auto"
	// E2ERequire: never in the clear; a desk without a key is woken and
	// asked again, else the call fails (ErrE2E) and nothing is sent.
	E2ERequire E2EMode = "require"
	// E2EOff: never sealed.
	E2EOff E2EMode = "off"
)

const (
	keyTTL      = 5 * time.Minute
	noKeyTTL    = 30 * time.Second
	defaultWake = 30
)

// warned: one warning per API and desk, in this process.
var warned sync.Map

type keyInfo struct {
	pub      []byte
	required bool
	why      string
}

type cached struct {
	at   time.Time
	info keyInfo
}

type e2eLayer struct {
	mode  E2EMode
	pins  map[string][]byte
	warn  func(string)
	c     *Client
	mu    sync.Mutex
	cache map[string]cached
}

func newE2ELayer(cfg *config, c *Client) (*e2eLayer, error) {
	l := &e2eLayer{mode: E2EAuto, pins: map[string][]byte{}, c: c, cache: map[string]cached{}}
	if cfg.set["WithE2E"] {
		switch cfg.e2eMode {
		case E2EAuto, E2ERequire, E2EOff:
			l.mode = cfg.e2eMode
		default:
			return nil, usageError("e2e is auto, require or off (not %q)", cfg.e2eMode)
		}
	}
	for d, k := range cfg.e2eKeys {
		pub := e2e.DeskKey(k)
		if pub == nil {
			return nil, usageError("e2e key for desk %q is not a 32-byte base64url X25519 key", d)
		}
		l.pins[d] = pub
	}
	l.warn = cfg.warn
	if l.warn == nil {
		l.warn = func(m string) { log.Print(m) }
	}
	return l, nil
}

func (l *e2eLayer) forget(desk string) {
	l.mu.Lock()
	delete(l.cache, desk)
	l.mu.Unlock()
}

// info is GET /desks/{id}: the desk's key and whether it requires sealing
// (cached; fresh asks again).
func (l *e2eLayer) info(ctx context.Context, desk string, call callConfig, fresh bool) (keyInfo, error) {
	l.mu.Lock()
	hit, ok := l.cache[desk]
	l.mu.Unlock()
	if !fresh && ok {
		ttl := noKeyTTL
		if hit.info.pub != nil {
			ttl = keyTTL
		}
		if time.Since(hit.at) < ttl {
			return hit.info, nil
		}
	}
	r := &request{method: http.MethodGet, path: deskPath(desk), call: callConfig{deskToken: call.deskToken}}
	res, err := l.c.send(ctx, r, nil)
	if err != nil {
		if errors.Is(err, ErrInterrupted) {
			return keyInfo{}, err
		}
		return keyInfo{why: fmt.Sprintf("its key could not be read (GET /desks/%s: %s)", desk, errMessage(err))}, nil
	}
	defer res.Body.Close()
	var d struct {
		E2EPub      *string `json:"e2e_pub"`
		E2ERequired bool    `json:"e2e_required"`
		Online      *bool   `json:"online"`
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	_ = json.Unmarshal(body, &d)
	info := keyInfo{required: d.E2ERequired}
	if d.E2EPub != nil {
		info.pub = e2e.DeskKey(*d.E2EPub)
	}
	if info.pub == nil {
		if d.Online != nil && !*d.Online {
			info.why = "it is offline, and lists its key only while online"
		} else {
			info.why = "it lists no end-to-end key (a GaiaDesk from before end-to-end encryption?)"
		}
	}
	l.mu.Lock()
	l.cache[desk] = cached{at: time.Now(), info: info}
	l.mu.Unlock()
	return info, nil
}

func errMessage(err error) string {
	if e := AsError(err); e != nil {
		return e.Message
	}
	return err.Error()
}

func e2eError(reason, desk, msg string) *Error {
	e := newError(ClassRefused, reason, msg)
	e.Desk = desk
	e.e2e = true
	return e
}

// checked is the server's key for desk, refused when a pinned key differs.
func (l *e2eLayer) checked(desk string, info keyInfo) ([]byte, error) {
	pin := l.pins[desk]
	if info.pub != nil && pin != nil && string(info.pub) != string(pin) {
		l.forget(desk)
		return nil, e2eError(ReasonE2EKeyMismatch, desk, fmt.Sprintf("the GaiaDesk API lists a different end-to-end key for desk %s than the pinned one; nothing was sent", desk))
	}
	if info.pub != nil {
		return info.pub, nil
	}
	return pin, nil
}

// key is the key to seal desk's next operation to, or nil to send it in
// the clear (auto, no key: warned once). insist: it must be sealed (the API
// said `e2e_required`). A desk that must be sealed to and lists no key is
// woken and asked again; still none is an ErrE2E.
func (l *e2eLayer) key(ctx context.Context, desk string, call callConfig, insist bool) ([]byte, error) {
	info, err := l.info(ctx, desk, call, insist)
	if err != nil {
		return nil, err
	}
	pub, err := l.checked(desk, info)
	if err != nil || pub != nil {
		return pub, err
	}
	if l.mode != E2ERequire && !info.required && !insist {
		id := l.c.baseURL + " " + desk
		if _, dup := warned.LoadOrStore(id, true); !dup {
			l.warn(fmt.Sprintf("GaiaDesk: operations on desk %s are not end-to-end encrypted: %s. The API relays them in the clear (use WithE2E(gaiadesk.E2ERequire) to refuse that).", desk, info.why))
		}
		return nil, nil
	}
	wait := defaultWake
	if call.wake != nil && *call.wake < wait {
		wait = *call.wake
	}
	if wait > 90 {
		wait = 90
	}
	wr := &request{method: http.MethodPost, path: deskPath(desk) + "/wake", json: map[string]int{"wait_s": wait}, call: callConfig{deskToken: call.deskToken}}
	if res, err := l.c.send(ctx, wr, nil); err != nil {
		if errors.Is(err, ErrInterrupted) {
			return nil, err
		}
	} else {
		res.Body.Close()
	}
	if info, err = l.info(ctx, desk, call, true); err != nil {
		return nil, err
	}
	if pub, err = l.checked(desk, info); err != nil || pub != nil {
		return pub, err
	}
	return nil, e2eError(ReasonE2EUnavailable, desk, fmt.Sprintf("desk %s must be reached end-to-end encrypted, but %s; nothing was sent", desk, info.why))
}

// call runs one desk operation: attempt sends it (sealed, or in the clear
// when given nil). A plaintext call refused `e2e_required` is sealed and
// sent again; a sealed one the desk could not open (`e2e_decrypt_failed`:
// its key rotated) is sealed to the key asked for again, once.
func (l *e2eLayer) call(ctx context.Context, r *request, attempt func(*sealedReq) (*http.Response, error)) (*http.Response, *e2e.CallerSeal, error) {
	if l.mode == E2EOff {
		res, err := attempt(nil)
		return res, nil, err
	}
	op := r.e2e
	seal := func(pub []byte) (*sealedReq, error) {
		req, s, err := e2e.SealRequest(pub, op.desk, op.op, op.request, time.Now())
		if err != nil {
			return nil, newError(ClassProtocol, ReasonE2EMalformed, "cannot seal the request: "+err.Error())
		}
		return &sealedReq{req: req, seal: s}, nil
	}
	send := func(pub []byte) (*http.Response, *e2e.CallerSeal, error) {
		if pub == nil {
			res, err := attempt(nil)
			return res, nil, err
		}
		s, err := seal(pub)
		if err != nil {
			return nil, nil, err
		}
		res, err := attempt(s)
		return res, s.seal, err
	}
	pub, err := l.key(ctx, op.desk, r.call, false)
	if err != nil {
		return nil, nil, err
	}
	res, s, err := send(pub)
	e := AsError(err)
	if e == nil || e.Class != ClassRefused {
		return res, s, err
	}
	if pub == nil && e.Reason == ReasonE2ERequired {
		l.forget(op.desk)
		again, kerr := l.key(ctx, op.desk, r.call, true)
		if kerr != nil {
			return nil, nil, kerr
		}
		return send(again)
	}
	if pub != nil && !e.e2e && e.Reason == ReasonE2EDecryptFailed {
		l.forget(op.desk)
		again, kerr := l.key(ctx, op.desk, r.call, false)
		if kerr != nil {
			return nil, nil, kerr
		}
		if again == nil {
			return nil, nil, err
		}
		return send(again)
	}
	return res, s, err
}
