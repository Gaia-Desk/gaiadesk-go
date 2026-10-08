package e2e

// The protocol's fixed test vectors byte for byte
// (protocol/src/e2e/vectors.json, copied to testdata/e2e-vectors.json),
// round trips, and every way a message must fail to open.

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

type vectors struct {
	AADEvent1  string `json:"aad_event_1_hex"`
	AADRequest string `json:"aad_request_hex"`
	DeskID     string `json:"desk_id"`
	DeskPub    string `json:"desk_pub"`
	DeskSecret string `json:"desk_secret_hex"`
	EphSecret  string `json:"eph_secret_hex"`
	HKDFSalt   string `json:"hkdf_salt"`
	Op         string `json:"op"`
	Request    SealedRequest
	ReqHeader  string `json:"request_header"`
	ReqPlain   string `json:"request_plaintext"`
	Events     []struct {
		Frame
		Plaintext string `json:"plaintext"`
	} `json:"events"`
	Inputs []struct {
		Frame
		Data string `json:"data"`
		Last bool   `json:"last"`
	} `json:"inputs"`
}

func load(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("../../testdata/e2e-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, ok := B64Decode(s)
	if !ok {
		t.Fatalf("not base64: %q", s)
	}
	return b
}

func vectorSeal(t *testing.T, v vectors) (SealedRequest, *CallerSeal) {
	t.Helper()
	deskPub, err := PublicKey(unhex(t, v.DeskSecret))
	if err != nil {
		t.Fatal(err)
	}
	r, s, err := SealRequestWith(unhex(t, v.EphSecret), b64(t, v.Request.Nonce), deskPub, v.DeskID, v.Op, []byte(v.ReqPlain))
	if err != nil {
		t.Fatal(err)
	}
	return r, s
}

func reason(err error) string {
	var oe *OpenError
	if errors.As(err, &oe) {
		return oe.Reason
	}
	return ""
}

func TestVectorsRequestAndHeader(t *testing.T) {
	v := load(t)
	if v.HKDFSalt != HKDFSalt {
		t.Fatalf("salt %q", v.HKDFSalt)
	}
	pub, _ := PublicKey(unhex(t, v.DeskSecret))
	if B64URL(pub) != v.DeskPub {
		t.Fatalf("desk pub %s, want %s", B64URL(pub), v.DeskPub)
	}
	r, _ := vectorSeal(t, v)
	if r != v.Request {
		t.Fatalf("request %+v, want %+v", r, v.Request)
	}
	if h := RequestHeader(r); h != v.ReqHeader {
		t.Fatalf("header %s, want %s", h, v.ReqHeader)
	}
}

func TestVectorsAssociatedData(t *testing.T) {
	v := load(t)
	if got := hex.EncodeToString(AssociatedData(UseRequest, v.DeskID, v.Op)); got != v.AADRequest {
		t.Fatalf("aad request %s", got)
	}
	if got := hex.EncodeToString(AssociatedDataSeq(UseEvent, v.DeskID, v.Op, 1)); got != v.AADEvent1 {
		t.Fatalf("aad event 1 %s", got)
	}
}

func TestVectorsEventsAndInputs(t *testing.T) {
	v := load(t)
	_, s := vectorSeal(t, v)
	for _, e := range v.Events {
		p, err := s.OpenEvent(e.Frame)
		if err != nil || string(p) != e.Plaintext {
			t.Fatalf("event %d: %q %v", e.Seq, p, err)
		}
	}
	for _, i := range v.Inputs {
		f := s.SealInputWith(b64(t, i.Nonce), i.Last, []byte(i.Data))
		if f != i.Frame {
			t.Fatalf("input %d: %+v, want %+v", i.Seq, f, i.Frame)
		}
	}
	_, again := vectorSeal(t, v)
	raw0, _ := json.Marshal(v.Events[0].Frame)
	e0, err := again.OpenDeskEvent(raw0)
	if err != nil || e0.Event != "stdout" || e0.Data != "dmVjdG9yCg==" {
		t.Fatalf("desk event 0: %+v %v", e0, err)
	}
	raw1, _ := json.Marshal(v.Events[1].Frame)
	e1, err := again.OpenDeskEvent(raw1)
	if err != nil || e1.Event != "exit" || string(e1.Result) != `{"exit":0}` {
		t.Fatalf("desk event 1: %+v %v", e1, err)
	}
}

func TestVectorsDeskSide(t *testing.T) {
	v := load(t)
	plain, ds, err := OpenRequest(unhex(t, v.DeskSecret), v.DeskID, v.Op, v.Request)
	if err != nil || string(plain) != v.ReqPlain {
		t.Fatalf("open request: %q %v", plain, err)
	}
	for _, i := range v.Inputs {
		last, data, err := ds.OpenInput(i.Frame)
		if err != nil || last != i.Last || string(data) != i.Data {
			t.Fatalf("input %d: %v %q %v", i.Seq, last, data, err)
		}
	}
	ev := ds.SealEventWith(b64(t, v.Events[0].Nonce), []byte(v.Events[0].Plaintext))
	if ev != v.Events[0].Frame {
		t.Fatalf("sealed event %+v", ev)
	}
}

func TestRoundTrip(t *testing.T) {
	secret := unhex(t, "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	pub, _ := PublicKey(secret)
	req, seal, err := SealRequest(pub, "123456789", "file_put", map[string]any{"op": "file_put", "path": "/tmp/x", "size": 3}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	plain, desk, err := OpenRequest(secret, "123456789", "file_put", req)
	if err != nil {
		t.Fatal(err)
	}
	var inner struct {
		V       int            `json:"v"`
		TS      int64          `json:"ts"`
		Request map[string]any `json:"request"`
	}
	if err := json.Unmarshal(plain, &inner); err != nil || inner.V != 1 || time.Since(time.Unix(inner.TS, 0)) > 5*time.Second || inner.Request["path"] != "/tmp/x" {
		t.Fatalf("inner %+v %v", inner, err)
	}
	last, data, err := desk.OpenInput(seal.SealInput(true, []byte("abc")))
	if err != nil || !last || string(data) != "abc" {
		t.Fatalf("input %v %q %v", last, data, err)
	}
	for _, e := range []DeskEvent{{Event: "stdout", Data: B64([]byte("é"))}, {Event: "exit", Result: json.RawMessage(`{"ok":1}`)}} {
		f := desk.SealEvent(e)
		raw, _ := json.Marshal(f)
		got, err := seal.OpenDeskEvent(raw)
		if err != nil || got.Event != e.Event || got.Data != e.Data || !bytes.Equal(got.Result, e.Result) {
			t.Fatalf("event %+v: %+v %v", e, got, err)
		}
	}
	other, _, _ := SealRequest(pub, "123456789", "exec", map[string]any{"op": "exec"}, time.Now())
	if other.Pub == req.Pub || other.Nonce == req.Nonce {
		t.Fatal("a fresh ephemeral key and nonce per operation")
	}
}

func flip(t *testing.T, s string, at int) string {
	u := b64(t, s)
	u[at] ^= 1
	return B64URL(u)
}

func TestTampering(t *testing.T) {
	v := load(t)
	secret := unhex(t, v.DeskSecret)
	r, _ := vectorSeal(t, v)
	for _, bad := range []SealedRequest{
		{V: 1, Pub: r.Pub, Nonce: r.Nonce, Ciphertext: flip(t, r.Ciphertext, 5)},
		{V: 1, Pub: r.Pub, Nonce: flip(t, r.Nonce, 0), Ciphertext: r.Ciphertext},
		{V: 1, Pub: flip(t, r.Pub, 3), Nonce: r.Nonce, Ciphertext: r.Ciphertext},
	} {
		if _, _, err := OpenRequest(secret, v.DeskID, v.Op, bad); err == nil {
			t.Fatal("a tampered request opened")
		}
	}
	e0 := v.Events[0].Frame
	for _, bad := range []Frame{
		{Seq: 0, Nonce: e0.Nonce, Ciphertext: flip(t, e0.Ciphertext, 2)},
		{Seq: 0, Nonce: flip(t, e0.Nonce, 23), Ciphertext: e0.Ciphertext},
	} {
		_, s := vectorSeal(t, v)
		if _, err := s.OpenEvent(bad); reason(err) != ReasonDecryptFailed {
			t.Fatalf("tampered event: %v", err)
		}
	}
	_, s := vectorSeal(t, v)
	if _, err := s.OpenEvent(Frame{Seq: 0, Nonce: e0.Nonce, Ciphertext: "AAAA"}); reason(err) != ReasonMalformed {
		t.Fatalf("short ciphertext: %v", err)
	}
	if _, err := s.OpenDeskEvent([]byte(`{"seq":0}`)); reason(err) != ReasonMalformed {
		t.Fatalf("no nonce: %v", err)
	}
	if _, err := s.OpenDeskEvent([]byte(`{"seq":"0","nonce":"a","ciphertext":"b"}`)); reason(err) != ReasonMalformed {
		t.Fatalf("seq a string: %v", err)
	}
	if _, err := s.OpenDeskEvent([]byte(`null`)); reason(err) != ReasonMalformed {
		t.Fatalf("null: %v", err)
	}
}

func TestAssociatedDataBinds(t *testing.T) {
	v := load(t)
	secret := unhex(t, v.DeskSecret)
	r, _ := vectorSeal(t, v)
	if _, _, err := OpenRequest(secret, "481902775", v.Op, r); reason(err) != ReasonDecryptFailed {
		t.Fatalf("another desk: %v", err)
	}
	if _, _, err := OpenRequest(secret, v.DeskID, "job_start", r); reason(err) != ReasonDecryptFailed {
		t.Fatalf("another op: %v", err)
	}
	deskPub, _ := PublicKey(secret)
	_, elsewhere, _ := SealRequestWith(unhex(t, v.EphSecret), b64(t, v.Request.Nonce), deskPub, "481902775", v.Op, []byte(v.ReqPlain))
	if _, err := elsewhere.OpenEvent(v.Events[0].Frame); reason(err) != ReasonDecryptFailed {
		t.Fatalf("events for another desk: %v", err)
	}
	_, otherOp, _ := SealRequestWith(unhex(t, v.EphSecret), b64(t, v.Request.Nonce), deskPub, v.DeskID, "stats", []byte(v.ReqPlain))
	if _, err := otherOp.OpenEvent(v.Events[0].Frame); reason(err) != ReasonDecryptFailed {
		t.Fatalf("events for another op: %v", err)
	}
}

func TestEventOrder(t *testing.T) {
	v := load(t)
	e0, e1 := v.Events[0].Frame, v.Events[1].Frame
	_, s := vectorSeal(t, v)
	if _, err := s.OpenEvent(e1); reason(err) != ReasonDecryptFailed {
		t.Fatal("the second first")
	}
	_, s = vectorSeal(t, v)
	_, _ = s.OpenEvent(e0)
	if _, err := s.OpenEvent(e0); reason(err) != ReasonDecryptFailed {
		t.Fatal("replayed")
	}
	_, s = vectorSeal(t, v)
	if _, err := s.OpenEvent(Frame{Seq: 0, Nonce: e1.Nonce, Ciphertext: e1.Ciphertext}); reason(err) != ReasonDecryptFailed {
		t.Fatal("renumbered into place")
	}
	_, s = vectorSeal(t, v)
	_, _ = s.OpenEvent(e0)
	if _, err := s.OpenEvent(Frame{Seq: 1, Nonce: e0.Nonce, Ciphertext: e0.Ciphertext}); reason(err) != ReasonDecryptFailed {
		t.Fatal("a replay renumbered")
	}
	_, s = vectorSeal(t, v)
	_, _ = s.OpenEvent(e0)
	if p, err := s.OpenEvent(e1); err != nil || string(p) != v.Events[1].Plaintext {
		t.Fatal("in order it opens")
	}
	_, ds, _ := OpenRequest(unhex(t, v.DeskSecret), v.DeskID, v.Op, v.Request)
	if _, _, err := ds.OpenInput(Frame{Seq: 0, Nonce: v.Inputs[1].Nonce, Ciphertext: v.Inputs[1].Ciphertext}); err == nil {
		t.Fatal("input out of place opened")
	}
}

func TestKeys(t *testing.T) {
	v := load(t)
	if _, err := X25519(unhex(t, v.EphSecret), make([]byte, 32)); reason(err) != ReasonWeakKey {
		t.Fatalf("low-order key: %v", err)
	}
	if len(DeskKey(v.DeskPub)) != 32 || len(DeskKey(v.DeskPub+"=")) != 32 {
		t.Fatal("desk key")
	}
	if DeskKey("AAAA") != nil || DeskKey("not base64!") != nil {
		t.Fatal("bad desk keys read")
	}
}

func TestBase64(t *testing.T) {
	for _, s := range []string{"", "f", "fo", "foo", "foob", "fooba", "foobar"} {
		if B64([]byte(s)) != base64.StdEncoding.EncodeToString([]byte(s)) || B64URL([]byte(s)) != base64.RawURLEncoding.EncodeToString([]byte(s)) {
			t.Fatal(s)
		}
		if b, ok := B64Decode(base64.StdEncoding.EncodeToString([]byte(s))); !ok || string(b) != s {
			t.Fatal(s)
		}
		if b, ok := B64Decode(base64.RawURLEncoding.EncodeToString([]byte(s))); !ok || string(b) != s {
			t.Fatal(s)
		}
	}
	if _, ok := B64Decode("a"); ok {
		t.Fatal("a")
	}
}
