package gaiadesk

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorClassesAndKinds(t *testing.T) {
	for _, c := range []struct {
		class, reason string
		kind          ErrorKind
		sentinel      error
		exit          int
	}{
		{"usage", "bad_body", KindUsage, ErrUsage, 255},
		{"refused", "rate_limited", KindRefused, ErrRefused, 254},
		{"unreachable", "offline", KindOffline, ErrUnreachable, 255},
		{"unreachable", "unknown_desk", KindUnknownDesk, ErrUnreachable, 255},
		{"unreachable", "timeout", KindTimeout, ErrUnreachable, 255},
		{"connection_lost", "desk_disconnected", KindConnectionLost, ErrConnectionLost, 255},
		{"failed", "", KindFailed, ErrFailed, 1},
		{"protocol", "desk_too_old", KindProtocol, ErrProtocol, 255},
	} {
		env, ok := errorEnvelope([]byte(fmt.Sprintf(`{"error":{"kind":%q,"message":"m","reason":%q,"desk":"123456789","request_id":"req_1"}}`, c.class, c.reason)))
		if !ok {
			t.Fatal(c)
		}
		e := fromEnvelope(env, "GET /x", 409, nil)
		if e.Kind != c.kind || !errors.Is(e, c.sentinel) || e.ExitCode != c.exit || e.Desk != "123456789" || e.RequestID != "req_1" {
			t.Fatalf("%+v", e)
		}
		wrapped := fmt.Errorf("outer: %w", e)
		if AsError(wrapped) != e || !errors.Is(wrapped, c.sentinel) {
			t.Fatal("through wrapping")
		}
		if errors.Is(e, ErrE2E) || errors.Is(e, ErrInterrupted) {
			t.Fatal("not an e2e or interrupted error")
		}
	}
	for _, notErr := range []string{`{"error":null}`, `{"exit":0}`, `[]`, `{"error":{"message":"no kind"}}`, `not json`} {
		if _, ok := errorEnvelope([]byte(notErr)); ok {
			t.Fatal(notErr)
		}
	}
	i := interrupted("GET /x", nil)
	if !errors.Is(i, ErrInterrupted) || i.ExitCode != 130 || errors.Is(i, ErrUnreachable) {
		t.Fatalf("%+v", i)
	}
	e2 := e2eError(ReasonE2EKeyMismatch, "1", "m")
	if !errors.Is(e2, ErrE2E) || !errors.Is(e2, ErrRefused) {
		t.Fatal("e2e is a refusal")
	}
	ce := &CommandError{Err: newError(ClassFailed, "", "exited 3")}
	if !errors.Is(ce, ErrCommand) || !errors.Is(ce, ErrFailed) || AsError(ce) != ce.Err {
		t.Fatal("command error")
	}
	x := &Exit{ExitCode: 254, Error: &CliError{Kind: "refused", Reason: ReasonAdminNotViaAPI, Message: "no"}}
	if !errors.Is(x.Err(), ErrRefused) || AsError(x.Err()).Reason != ReasonAdminNotViaAPI {
		t.Fatal(x.Err())
	}
	if (&Exit{ExitCode: 3}).Err() != nil {
		t.Fatal("a non-zero exit is not an error")
	}
	if !newError(ClassRefused, ReasonDeskBusy, "busy").Temporary() || newError(ClassRefused, ReasonMissingScope, "no").Temporary() {
		t.Fatal("temporary")
	}
}
