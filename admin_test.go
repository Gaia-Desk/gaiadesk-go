package gaiadesk

// Administrator work is not available over the API: the API refuses an exec
// asking for it (200, exit 254, a refused error) and a token minted with the
// `admin` scope (403), both with reason admin_not_via_api. The SDK maps both
// to its refused error, as it maps other refusals there.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// canned answers every request with the answer for its path.
type canned map[string]struct {
	status int
	body   string
}

func (c canned) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
	}
	a, ok := c[req.Method+" "+req.URL.Path]
	if !ok {
		a.status, a.body = 404, `{"error":{"kind":"usage","message":"no such route","reason":"no_such_route"}}`
	}
	return &http.Response{StatusCode: a.status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(a.body)), Request: req}, nil
}

func TestAdminNotViaAPI_IsARefusal(t *testing.T) {
	const refused = `{"kind":"refused","reason":"admin_not_via_api","message":"administrator work is not available over the API; run it with gaiadesk-cli exec --admin","desk":"123456789"}`
	api := canned{
		"POST /v1/desks/123456789/exec":   {200, `{"desk":"123456789","exit":254,"remote_code":null,"stdout":"","stderr":"","timed_out":false,"truncated":false,"error":` + refused + `}`},
		"POST /v1/desks/123456789/tokens": {403, `{"error":` + refused + `}`},
	}
	hc := &http.Client{Transport: api}
	bg := context.Background()
	check := func(what string, err error, status int) {
		t.Helper()
		e := errOf(t, err)
		if !errors.Is(err, ErrRefused) || e.Class != ClassRefused || e.Kind != KindRefused || e.Reason != ReasonAdminNotViaAPI ||
			e.ExitCode != 254 || e.Status != status || e.Desk != rawDesk {
			t.Fatalf("%s: %+v", what, e)
		}
	}
	c, err := New("ak_t", WithBaseURL("http://127.0.0.1:9/v1"), WithDeskToken("gdagt_t"), WithE2E(E2EOff), WithWarningHandler(quiet), WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	// An exec the API refused before running anything: the refused error,
	// not a result (exit 254, as gaiadesk-cli exits).
	r, err := c.Exec(bg, rawDesk, ExecRequest{Command: "id -u"})
	if r != nil {
		t.Fatalf("a refused exec returned a result: %+v", r)
	}
	check("Exec", err, 0) // answered 200: the refusal is in the result, no HTTP error status
	// Minting a token with the admin scope (the SDK has no constant for it;
	// the API refuses the raw scope).
	o, err := New("session-person", WithBaseURL("http://127.0.0.1:9/v1"), WithE2E(E2EOff), WithWarningHandler(quiet), WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	_, err = o.CreateToken(bg, TokenRequest{Desks: []string{rawDesk}, Name: "ops", Scopes: []string{"admin"}})
	check("CreateToken", err, 403)
	// A stream that ends with the refusal: its Exit carries it.
	x := &Exit{ExitCode: 254, Error: &CliError{Kind: "refused", Reason: ReasonAdminNotViaAPI, Message: "no", Desk: rawDesk}}
	if e := AsError(x.Err()); !errors.Is(x.Err(), ErrRefused) || e.Reason != ReasonAdminNotViaAPI || e.ExitCode != 254 {
		t.Fatal(x.Err())
	}
}
