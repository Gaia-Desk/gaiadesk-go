package gaiadesk

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// Version is this SDK's version.
const Version = "0.1.0"

// DefaultBaseURL is the hosted GaiaDesk API.
const DefaultBaseURL = "https://api.gaiadesk.net/v1"

// FileLimit is the most one file may be through the API (256 MB); larger
// files go direct, through gaiadesk-cli.
const FileLimit = 256 * 1024 * 1024

// WaitMax is the longest one `GET …/jobs/{name}/wait` holds (870 s);
// [Client.WaitJob] waits again for longer.
const WaitMax = 870 * time.Second

// Transport is how a Client reaches a /v1 API.
type Transport string

// The transports.
const (
	// TransportAPI is the hosted API (New).
	TransportAPI Transport = "api"
	// TransportLocal is the desk's own API over its Unix socket or named
	// pipe, for code running on the desk (NewLocal).
	TransportLocal Transport = "local"
	// TransportLAN is a desk's LAN gateway over pinned TLS (NewLAN).
	TransportLAN Transport = "lan"
)

func (t Transport) label() string {
	if t == TransportAPI {
		return "API"
	}
	return string(t)
}

// Client drives GaiaDesk desks through a /v1 API: the hosted one (New), a
// desk's own (NewLocal), or a desk's LAN gateway (NewLAN). Every method
// takes a context; cancelling it stops the call. A Client is safe for
// concurrent use.
type Client struct {
	transport Transport
	baseURL   string
	where     string
	hc        *http.Client
	creds     func(callToken string) (http.Header, error)
	// mapDialError turns a connection failure into the transport's own error
	// (local: no socket; lan: the fingerprint), or returns nil.
	mapDialError func(err error, op string) error
	e2e          *e2eLayer
	retry        RetryPolicy
	userAgent    string
}

// Transport says which transport the client uses.
func (c *Client) Transport() Transport { return c.transport }

// BaseURL is the API's base URL (`…/v1`).
func (c *Client) BaseURL() string { return c.baseURL }

// Option configures a Client.
type Option func(*config)

type config struct {
	set        map[string]bool
	baseURL    string
	deskToken  string
	httpClient *http.Client
	e2eMode    E2EMode
	e2eKeys    map[string]string
	warn       func(string)
	retry      RetryPolicy
	userAgent  string
	socketPath string
	adminToken string
	env        func(string) string
}

func (c *config) mark(name string) { c.set[name] = true }

// WithBaseURL sets the API's base URL (default DefaultBaseURL), for a local
// signal server or a staging API.
func WithBaseURL(u string) Option { return func(c *config) { c.mark("WithBaseURL"); c.baseURL = u } }

// WithDeskToken sets the scoped agent token (`gdagt_…`) sent as
// X-GaiaDesk-Desk-Token with every desk operation; the desk verifies it.
// A call may give its own with UseDeskToken.
func WithDeskToken(t string) Option {
	return func(c *config) { c.mark("WithDeskToken"); c.deskToken = t }
}

// WithHTTPClient sets the http.Client of the hosted API transport (default:
// one of the SDK's own). Do not give it a Timeout: streams and held waits
// last long; use contexts.
func WithHTTPClient(h *http.Client) Option {
	return func(c *config) { c.mark("WithHTTPClient"); c.httpClient = h }
}

// WithE2E sets end-to-end encryption of desk operations (hosted API only):
// E2EAuto (default), E2ERequire or E2EOff.
func WithE2E(mode E2EMode) Option { return func(c *config) { c.mark("WithE2E"); c.e2eMode = mode } }

// WithE2EKeys pins desk keys, {deskID: e2e_pub (base64url)}: a different
// key from the server is refused, and a pinned key seals while the server
// lists none.
func WithE2EKeys(keys map[string]string) Option {
	return func(c *config) { c.mark("WithE2EKeys"); c.e2eKeys = keys }
}

// WithWarningHandler sets where the SDK's warnings go (default: the
// standard logger's stderr, via os.Stderr).
func WithWarningHandler(f func(message string)) Option {
	return func(c *config) { c.mark("WithWarningHandler"); c.warn = f }
}

// WithRetry sets the retry policy (default DefaultRetry). NoRetry turns
// retries off.
func WithRetry(p RetryPolicy) Option { return func(c *config) { c.mark("WithRetry"); c.retry = p } }

// WithUserAgent sets the User-Agent (default `gaiadesk-go/<Version>`).
func WithUserAgent(ua string) Option {
	return func(c *config) { c.mark("WithUserAgent"); c.userAgent = ua }
}

// WithSocketPath sets the local API's socket path or pipe name (NewLocal;
// default: see LocalSocketPath and LocalPipeName).
func WithSocketPath(p string) Option {
	return func(c *config) { c.mark("WithSocketPath"); c.socketPath = p }
}

// WithAdminToken sets the desk's local admin token (`gdlocal_…`; NewLocal;
// default: read from its file on each request).
func WithAdminToken(t string) Option {
	return func(c *config) { c.mark("WithAdminToken"); c.adminToken = t }
}

// WithEnv sets the environment the local transport's defaults are read
// from (GAIADESK_API_DIR, GAIADESK_API_PIPE, USERNAME; default: this
// process's).
func WithEnv(env map[string]string) Option {
	return func(c *config) {
		c.mark("WithEnv")
		c.env = func(k string) string { return env[k] }
	}
}

func newConfig(opts []Option) *config {
	c := &config{set: map[string]bool{}, retry: DefaultRetry, env: os.Getenv}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	return c
}

// only refuses options that do not belong to this transport.
func (c *config) only(t Transport, allowed ...string) error {
	ok := map[string]bool{"WithRetry": true, "WithUserAgent": true, "WithDeskToken": true}
	for _, a := range allowed {
		ok[a] = true
	}
	for name := range c.set {
		if !ok[name] {
			return usageError("%s does not apply to the %s transport", name, t.label())
		}
	}
	return nil
}

func nonEmpty(v, what string) (string, error) {
	if strings.TrimSpace(v) == "" {
		return "", usageError("%s must be a non-empty string", what)
	}
	return strings.TrimSpace(v), nil
}

func (c *config) base(t Transport) (*Client, error) {
	if c.retry.MaxAttempts < 0 || c.retry.BaseDelay < 0 || c.retry.MaxDelay < 0 {
		return nil, usageError("the retry policy's attempts and delays are >= 0")
	}
	cl := &Client{transport: t, retry: c.retry, userAgent: "gaiadesk-go/" + Version}
	if c.userAgent != "" {
		cl.userAgent = c.userAgent
	}
	return cl, nil
}

var httpURL = regexp.MustCompile(`(?i)^https?://[^/]`)

// New is a client of the hosted GaiaDesk API, authenticated with an Atlas
// API key (`ak_…`) or a signed-in person's session token. Desk operations
// from an API key need an agent token too (WithDeskToken or UseDeskToken).
func New(apiKey string, opts ...Option) (*Client, error) {
	c := newConfig(opts)
	if err := c.only(TransportAPI, "WithBaseURL", "WithHTTPClient", "WithE2E", "WithE2EKeys", "WithWarningHandler"); err != nil {
		return nil, err
	}
	key, err := nonEmpty(apiKey, "the API key")
	if err != nil {
		return nil, err
	}
	deskToken := ""
	if c.set["WithDeskToken"] {
		if deskToken, err = nonEmpty(c.deskToken, "the desk token (a scoped agent token, gdagt_…)"); err != nil {
			return nil, err
		}
	}
	cl, err := c.base(TransportAPI)
	if err != nil {
		return nil, err
	}
	cl.baseURL = strings.TrimRight(DefaultBaseURL, "/")
	if c.set["WithBaseURL"] {
		if !httpURL.MatchString(c.baseURL) {
			return nil, usageError("the base URL must be an http(s) URL: %q", c.baseURL)
		}
		cl.baseURL = strings.TrimRight(c.baseURL, "/")
	}
	cl.where = "the GaiaDesk API (" + cl.baseURL + ")"
	cl.hc = c.httpClient
	if cl.hc == nil {
		cl.hc = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	}
	cl.creds = func(callToken string) (http.Header, error) {
		h := http.Header{}
		h.Set("Authorization", "Bearer "+key)
		if t := firstNonEmpty(callToken, deskToken); t != "" {
			h.Set("X-GaiaDesk-Desk-Token", t)
		}
		return h, nil
	}
	if cl.e2e, err = newE2ELayer(c, cl); err != nil {
		return nil, err
	}
	return cl, nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
