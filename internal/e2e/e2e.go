// Package e2e is the crypto of end-to-end encrypted desk operations, v1:
// the caller's side (seal a request, seal input, open events) and, for
// tests and mocks, the desk's side (open a request, seal events, open
// input). The reference is the protocol crate's e2e.rs, whose fixed test
// vectors this package reproduces byte for byte.
//
// Per operation: an ephemeral X25519 key pair; shared = X25519(eph, desk);
// prk = HKDF-SHA256-Extract("gaiadesk desk-op e2e v1", shared); one key per
// use (request, input, event) = HKDF-Expand(prk, label 0x00 eph_pub
// desk_pub, 32). Every message is XChaCha20-Poly1305 with a random 24-byte
// nonce and associated data naming the use, the desk, the operation and
// (for input and events) the message's place.
//
// X25519 and HKDF come from the standard library (crypto/ecdh,
// crypto/hkdf), XChaCha20-Poly1305 from golang.org/x/crypto.
package e2e

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	// Version is the envelope version, `v` in every sealed request.
	Version = 1
	// Header carries a sealed request on a call without a JSON body.
	Header = "GaiaDesk-E2E"
	// FramesContentType is the content type of sealed uploads and downloads.
	FramesContentType = "application/x-ndjson"
	// HKDFSalt is the HKDF-SHA256 salt.
	HKDFSalt = "gaiadesk desk-op e2e v1"
	// InputChunk is the most file bytes one sealed input frame carries.
	InputChunk = 48 * 1024
)

// The three uses of an operation's keys.
const (
	UseRequest = "request"
	UseInput   = "input"
	UseEvent   = "event"
)

// Reasons a sealed message does not open (the protocol's).
const (
	ReasonMalformed     = "e2e_malformed"
	ReasonDecryptFailed = "e2e_decrypt_failed"
	ReasonWeakKey       = "e2e_weak_key"
)

// SealedRequest is a sealed request: `{"e2e": …}` of a POST body, or the
// GaiaDesk-E2E header's JSON. Field order is the wire order.
type SealedRequest struct {
	V          int    `json:"v"`
	Pub        string `json:"pub"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// Frame is a sealed frame after the request: an event coming back or a
// piece of input going up.
type Frame struct {
	Seq        uint64 `json:"seq"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// OpenError says why a sealed message did not open.
type OpenError struct {
	Reason  string
	Message string
}

func (e *OpenError) Error() string { return e.Message }

func malformed(m string) error { return &OpenError{Reason: ReasonMalformed, Message: m} }

// ───────────────────────────── base64 ─────────────────────────────

// B64 is standard base64 with padding (a desk event's `data`).
func B64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// B64URL is base64url without padding: every binary field of the envelope.
func B64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// B64Decode reads standard or url-safe base64, padded or not; ok is false
// when it is not base64.
func B64Decode(s string) ([]byte, bool) {
	t := strings.TrimSpace(s)
	t = strings.NewReplacer("-", "+", "_", "/").Replace(t)
	t = strings.TrimRight(t, "=")
	if len(t)%4 == 1 {
		return nil, false
	}
	b, err := base64.RawStdEncoding.DecodeString(t)
	if err != nil {
		return nil, false
	}
	return b, true
}

// DeskKey reads a desk key as given (`e2e_pub`, base64url): its 32 bytes,
// or nil.
func DeskKey(s string) []byte {
	k, ok := B64Decode(s)
	if !ok || len(k) != 32 {
		return nil
	}
	return k
}

// ───────────────────────────── primitives ─────────────────────────────

// PublicKey is the X25519 public key of a 32-byte secret.
func PublicKey(secret []byte) ([]byte, error) {
	k, err := ecdh.X25519().NewPrivateKey(secret)
	if err != nil {
		return nil, err
	}
	return k.PublicKey().Bytes(), nil
}

// X25519 is the key exchange, refusing a non-contributory (all-zero) result.
func X25519(secret, pub []byte) ([]byte, error) {
	weak := &OpenError{Reason: ReasonWeakKey, Message: "the key exchange gave no shared secret (a low-order key)"}
	k, err := ecdh.X25519().NewPrivateKey(secret)
	if err != nil {
		return nil, malformed("not an X25519 secret")
	}
	p, err := ecdh.X25519().NewPublicKey(pub)
	if err != nil {
		return nil, malformed("not an X25519 public key")
	}
	shared, err := k.ECDH(p)
	if err != nil {
		return nil, weak
	}
	if bytes.Equal(shared, make([]byte, len(shared))) {
		return nil, weak
	}
	return shared, nil
}

// AssociatedData is `"gaiadesk-e2e/v1 <use>" 0 desk 0 op`.
func AssociatedData(use, desk, op string) []byte {
	var b bytes.Buffer
	b.WriteString("gaiadesk-e2e/v1 " + use)
	b.WriteByte(0)
	b.WriteString(desk)
	b.WriteByte(0)
	b.WriteString(op)
	return b.Bytes()
}

// AssociatedDataSeq is AssociatedData then `0 seq` (u64 big-endian): input
// and events.
func AssociatedDataSeq(use, desk, op string, seq uint64) []byte {
	b := AssociatedData(use, desk, op)
	b = append(b, 0)
	return binary.BigEndian.AppendUint64(b, seq)
}

// Keys are one operation's three keys.
type Keys struct {
	Request, Input, Event []byte
}

// DeriveKeys derives the keys from the exchange's shared secret and both
// public keys.
func DeriveKeys(shared, ephPub, deskPub []byte) (Keys, error) {
	prk, err := hkdf.Extract(sha256.New, shared, []byte(HKDFSalt))
	if err != nil {
		return Keys{}, err
	}
	key := func(label string) ([]byte, error) {
		info := append(append(append([]byte(label), 0), ephPub...), deskPub...)
		return hkdf.Expand(sha256.New, prk, string(info), 32)
	}
	var k Keys
	if k.Request, err = key(UseRequest); err != nil {
		return Keys{}, err
	}
	if k.Input, err = key(UseInput); err != nil {
		return Keys{}, err
	}
	if k.Event, err = key(UseEvent); err != nil {
		return Keys{}, err
	}
	return k, nil
}

// Seal is XChaCha20-Poly1305 encryption: the ciphertext and its tag.
func Seal(key, nonce, aad, plaintext []byte) []byte {
	a, err := chacha20poly1305.NewX(key)
	if err != nil {
		panic(err) // keys are always 32 bytes here
	}
	return a.Seal(nil, nonce, plaintext, aad)
}

// Open decrypts base64url fields; an *OpenError when it does not
// authenticate or is malformed.
func Open(key []byte, nonce, ciphertext string, aad []byte) ([]byte, error) {
	n, ok1 := B64Decode(nonce)
	c, ok2 := B64Decode(ciphertext)
	if !ok1 || len(n) != chacha20poly1305.NonceSizeX || !ok2 || len(c) < chacha20poly1305.Overhead {
		return nil, malformed("a sealed message is malformed")
	}
	a, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, malformed("a sealed message is malformed")
	}
	p, err := a.Open(nil, n, c, aad)
	if err != nil {
		return nil, &OpenError{Reason: ReasonDecryptFailed, Message: "a sealed message did not open: it was altered, reordered, or sealed for another desk or operation"}
	}
	return p, nil
}

// Random returns n random bytes.
func Random(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return b
}

// ───────────────────────────── the caller ─────────────────────────────

// CallerSeal is the caller's side of one operation after its request is
// sealed: its input going up, the desk's events coming back.
type CallerSeal struct {
	keys      Keys
	Desk, Op  string
	nextInput uint64
	nextEvent uint64
}

// SealInputWith seals the next piece of input (`last` on the final one,
// which may be empty) with a given nonce.
func (s *CallerSeal) SealInputWith(nonce []byte, last bool, data []byte) Frame {
	seq := s.nextInput
	s.nextInput++
	flag := byte(0)
	if last {
		flag = 1
	}
	pt := append([]byte{flag}, data...)
	ct := Seal(s.keys.Input, nonce, AssociatedDataSeq(UseInput, s.Desk, s.Op, seq), pt)
	return Frame{Seq: seq, Nonce: B64URL(nonce), Ciphertext: B64URL(ct)}
}

// SealInput seals the next piece of input with a fresh nonce.
func (s *CallerSeal) SealInput(last bool, data []byte) Frame {
	return s.SealInputWith(Random(24), last, data)
}

// ParseFrame reads a sealed frame's JSON strictly: `seq` a whole number,
// `nonce` and `ciphertext` strings (other fields, such as an SSE event's
// `"event": "sealed"`, are ignored).
func ParseFrame(raw []byte) (Frame, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return Frame{}, malformed("a sealed event is malformed")
	}
	var f Frame
	var seq json.Number
	d := json.NewDecoder(bytes.NewReader(m["seq"]))
	d.UseNumber()
	if err := d.Decode(&seq); err != nil {
		return Frame{}, malformed("a sealed event is malformed")
	}
	n, err := parseUint(string(seq))
	if err != nil {
		return Frame{}, malformed("a sealed event is malformed")
	}
	f.Seq = n
	if json.Unmarshal(m["nonce"], &f.Nonce) != nil || json.Unmarshal(m["ciphertext"], &f.Ciphertext) != nil || m["nonce"] == nil || m["ciphertext"] == nil {
		return Frame{}, malformed("a sealed event is malformed")
	}
	return f, nil
}

func parseUint(s string) (uint64, error) {
	var n uint64
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a whole number")
		}
		n = n*10 + uint64(c-'0')
	}
	return n, nil
}

// OpenEvent opens the desk's next event (it must be the next in order): its
// plaintext.
func (s *CallerSeal) OpenEvent(f Frame) ([]byte, error) {
	if f.Seq != s.nextEvent {
		return nil, &OpenError{Reason: ReasonDecryptFailed, Message: fmt.Sprintf("a sealed event is out of order (got %d, expected %d)", f.Seq, s.nextEvent)}
	}
	p, err := Open(s.keys.Event, f.Nonce, f.Ciphertext, AssociatedDataSeq(UseEvent, s.Desk, s.Op, f.Seq))
	if err != nil {
		return nil, err
	}
	s.nextEvent++
	return p, nil
}

// DeskEvent is what a sealed event opens to: the desk's own event.
type DeskEvent struct {
	Event   string          `json:"event"`
	Data    string          `json:"data,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Kind    string          `json:"kind,omitempty"`
	Message string          `json:"message,omitempty"`
	Reason  string          `json:"reason,omitempty"`
}

// OpenDeskEvent opens the next event as the desk event it carries
// (`{"event": "stdout" | "stderr" | "exit" | "error", …}`).
func (s *CallerSeal) OpenDeskEvent(raw []byte) (DeskEvent, error) {
	f, err := ParseFrame(raw)
	if err != nil {
		return DeskEvent{}, err
	}
	return s.OpenDeskFrame(f)
}

// OpenDeskFrame is OpenDeskEvent for a parsed frame.
func (s *CallerSeal) OpenDeskFrame(f Frame) (DeskEvent, error) {
	plain, err := s.OpenEvent(f)
	if err != nil {
		return DeskEvent{}, err
	}
	if !utf8.Valid(plain) {
		return DeskEvent{}, malformed("a sealed event is not JSON")
	}
	var e DeskEvent
	if err := json.Unmarshal(plain, &e); err != nil {
		return DeskEvent{}, malformed("a sealed event is not JSON")
	}
	switch e.Event {
	case "stdout", "stderr", "exit", "error":
	default:
		return DeskEvent{}, malformed("a sealed event is not a desk event")
	}
	return e, nil
}

// SealRequestWith seals plaintext (the inner request's JSON) for desk `desk`
// (its key deskPub) as operation op, with a given ephemeral secret and
// nonce: the test vectors' entry point. Never reuse either.
func SealRequestWith(eph, nonce, deskPub []byte, desk, op string, plaintext []byte) (SealedRequest, *CallerSeal, error) {
	ephPub, err := PublicKey(eph)
	if err != nil {
		return SealedRequest{}, nil, err
	}
	shared, err := X25519(eph, deskPub)
	if err != nil {
		return SealedRequest{}, nil, err
	}
	keys, err := DeriveKeys(shared, ephPub, deskPub)
	if err != nil {
		return SealedRequest{}, nil, err
	}
	ct := Seal(keys.Request, nonce, AssociatedData(UseRequest, desk, op), plaintext)
	r := SealedRequest{V: Version, Pub: B64URL(ephPub), Nonce: B64URL(nonce), Ciphertext: B64URL(ct)}
	return r, &CallerSeal{keys: keys, Desk: desk, Op: op}, nil
}

// SealRequest seals a desk operation's request (`{"op": …}`) now:
// `{"v":1,"ts":<now>,"request":…}` under a fresh ephemeral key.
func SealRequest(deskPub []byte, desk, op string, request any, now time.Time) (SealedRequest, *CallerSeal, error) {
	inner, err := json.Marshal(struct {
		V       int   `json:"v"`
		TS      int64 `json:"ts"`
		Request any   `json:"request"`
	}{Version, now.Unix(), request})
	if err != nil {
		return SealedRequest{}, nil, err
	}
	return SealRequestWith(Random(32), Random(24), deskPub, desk, op, inner)
}

// RequestHeader is the GaiaDesk-E2E header value of a sealed request:
// base64url of its JSON.
func RequestHeader(r SealedRequest) string {
	b, _ := json.Marshal(r)
	return B64URL(b)
}
