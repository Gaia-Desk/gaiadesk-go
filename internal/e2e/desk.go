package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
)

// DeskSeal is a desk's side of one operation, for tests and mocks: it opens
// the caller's input and seals events back, in the order the protocol's
// crypto.rs gives.
type DeskSeal struct {
	keys      Keys
	Desk, Op  string
	nextInput uint64
	nextEvent uint64
}

// OpenRequest opens a sealed request to desk as op with the desk's secret:
// its plaintext and the seal for the rest.
func OpenRequest(deskSecret []byte, desk, op string, req SealedRequest) ([]byte, *DeskSeal, error) {
	if req.V != Version {
		return nil, nil, malformed("bad version")
	}
	ephPub, ok := B64Decode(req.Pub)
	if !ok || len(ephPub) != 32 {
		return nil, nil, malformed("bad pub")
	}
	deskPub, err := PublicKey(deskSecret)
	if err != nil {
		return nil, nil, err
	}
	shared, err := X25519(deskSecret, ephPub)
	if err != nil {
		return nil, nil, err
	}
	keys, err := DeriveKeys(shared, ephPub, deskPub)
	if err != nil {
		return nil, nil, err
	}
	plain, err := Open(keys.Request, req.Nonce, req.Ciphertext, AssociatedData(UseRequest, desk, op))
	if err != nil {
		return nil, nil, err
	}
	return plain, &DeskSeal{keys: keys, Desk: desk, Op: op}, nil
}

// SealEventWith seals an event's plaintext with a given nonce.
func (s *DeskSeal) SealEventWith(nonce, plaintext []byte) Frame {
	seq := s.nextEvent
	s.nextEvent++
	ct := Seal(s.keys.Event, nonce, AssociatedDataSeq(UseEvent, s.Desk, s.Op, seq), plaintext)
	return Frame{Seq: seq, Nonce: B64URL(nonce), Ciphertext: B64URL(ct)}
}

// SealEvent seals a desk event as JSON.
func (s *DeskSeal) SealEvent(event any) Frame {
	b, err := json.Marshal(event)
	if err != nil {
		panic(err)
	}
	return s.SealEventWith(Random(24), b)
}

// OpenInput opens the caller's next input frame: its flag and bytes.
func (s *DeskSeal) OpenInput(f Frame) (last bool, data []byte, err error) {
	if f.Seq != s.nextInput {
		return false, nil, errors.New("input out of order")
	}
	p, err := Open(s.keys.Input, f.Nonce, f.Ciphertext, AssociatedDataSeq(UseInput, s.Desk, s.Op, f.Seq))
	if err != nil {
		return false, nil, err
	}
	s.nextInput++
	if len(p) == 0 || (p[0] != 0 && p[0] != 1) {
		return false, nil, fmt.Errorf("bad input flag")
	}
	return p[0] == 1, p[1:], nil
}
