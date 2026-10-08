package gaiadesk

// The lan transport: a desk's opt-in LAN gateway, `https://<desk>:7443/v1`,
// serving the same /v1 desk operations as the hosted API. Its certificate
// is self-signed, so the chain and the host name cannot be checked: the
// SHA-256 of the certificate is pinned instead (the fingerprint the desk
// shows in Settings), and checked during the TLS handshake, BEFORE any
// byte of the request is written. The gateway takes agent tokens only.

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var (
	fpPrefix = regexp.MustCompile(`(?i)^sha-?256[:=\s]*`)
	fpHex    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// NormalizeFingerprint reads a SHA-256 certificate fingerprint as the desk
// shows it (with or without colons or spaces, any case) and writes it as
// 32 lowercase hex pairs joined by `:`.
func NormalizeFingerprint(fp string) (string, error) {
	h := strings.ToLower(strings.NewReplacer(":", "", " ", "", "\t", "").Replace(fpPrefix.ReplaceAllString(strings.TrimSpace(fp), "")))
	if !fpHex.MatchString(h) {
		return "", usageError("the fingerprint must be the certificate's SHA-256: 32 hex pairs (ab:cd:…), not %q", fp)
	}
	pairs := make([]string, 32)
	for i := range pairs {
		pairs[i] = h[i*2 : i*2+2]
	}
	return strings.Join(pairs, ":"), nil
}

var httpsURL = regexp.MustCompile(`(?i)^https://[^/]`)

// NewLAN is a client of a desk's LAN gateway (`https://<desk>:7443/v1`, or
// `https://gaiadesk-<desk id>.local:7443/v1`), its certificate pinned by the
// SHA-256 fingerprint its Settings show. It takes agent tokens only
// (WithDeskToken, or UseDeskToken per call). Options: WithDeskToken,
// WithRetry, WithUserAgent.
func NewLAN(baseURL, fingerprint string, opts ...Option) (*Client, error) {
	c := newConfig(opts)
	if err := c.only(TransportLAN); err != nil {
		return nil, err
	}
	if !httpsURL.MatchString(baseURL) {
		return nil, usageError("the lan transport needs an https:// base URL (https://<desk>:7443/v1), not %q", baseURL)
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, usageError("not a URL: %q", baseURL)
	}
	if fingerprint == "" {
		return nil, usageError("the lan transport needs the gateway certificate's fingerprint (Settings → GaiaDesk API → LAN gateway)")
	}
	pinned, err := NormalizeFingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	deskToken := ""
	if c.set["WithDeskToken"] {
		if deskToken, err = nonEmpty(c.deskToken, "the desk token (a scoped agent token, gdagt_…)"); err != nil {
			return nil, err
		}
	}
	cl, err := c.base(TransportLAN)
	if err != nil {
		return nil, err
	}
	cl.baseURL = strings.TrimRight(baseURL, "/")
	cl.where = "the desk's LAN gateway (" + u.Host + ")"
	tlsConf := &tls.Config{
		// The chain and name cannot be checked (self-signed); the pin is,
		// in VerifyConnection, before the handshake completes.
		InsecureSkipVerify: true, //nolint:gosec // pinned below
		NextProtos:         []string{"http/1.1"},
		VerifyConnection: func(cs tls.ConnectionState) error {
			actual := ""
			if len(cs.PeerCertificates) > 0 {
				sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
				actual, _ = NormalizeFingerprint(hex.EncodeToString(sum[:]))
			}
			if actual == pinned {
				return nil
			}
			e := newError(ClassUnreachable, ReasonFingerprintMismatch, fmt.Sprintf(
				"the desk at %s did not prove the pinned identity: its certificate's SHA-256 is %s, not %s. Do not proceed: this may not be your desk. Check the fingerprint in its Settings → GaiaDesk API.",
				u.Host, firstNonEmpty(actual, "(none)"), pinned))
			e.Kind = KindUnreachable
			return &FingerprintMismatchError{Err: e, Expected: pinned, Actual: actual}
		},
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsConf
	tr.ForceAttemptHTTP2 = false
	cl.hc = &http.Client{Transport: tr}
	cl.mapDialError = func(err error, op string) error {
		var fp *FingerprintMismatchError
		if errors.As(err, &fp) {
			fp.Err.Op = op
			return fp
		}
		return nil
	}
	cl.creds = func(callToken string) (http.Header, error) {
		t := firstNonEmpty(callToken, deskToken)
		if t == "" {
			return nil, usageError("the lan transport needs an agent token (WithDeskToken, gdagt_…): a desk's LAN gateway does not take its admin token")
		}
		h := http.Header{}
		h.Set("X-GaiaDesk-Desk-Token", t)
		return h, nil
	}
	return cl, nil
}
