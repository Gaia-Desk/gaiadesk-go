package gaiadesk

// The local transport: code running ON the desk talks to the GaiaDesk
// app's own /v1 API over a Unix socket (macOS, Linux) or a named pipe
// (Windows). Same operations, results and errors as the hosted API; only
// the connection and the credentials differ:
//
//	socket  $GAIADESK_API_DIR/api.sock, else ~/.gaiadesk/api.sock
//	pipe    $GAIADESK_API_PIPE, else \\.\pipe\gaiadesk-api-<user>
//	token   an agent token (WithDeskToken) as X-GaiaDesk-Desk-Token, else
//	        the desk's local admin token (gdlocal_…, $GAIADESK_API_DIR/api-token,
//	        else ~/.gaiadesk/api-token) as `Authorization: Bearer`.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/user"
	"runtime"
	"strings"
	"sync"
	"syscall"
)

// LocalAPIUnavailable is what a missing socket or pipe means.
const LocalAPIUnavailable = "GaiaDesk is not serving its local API here: is the app running, and is Settings → GaiaDesk API → Local API on?"

// PipeUser is the pipe-name form of a user name: lowercased, [a-z0-9._-]
// kept, the rest `_`, at most 64 characters, `user` if empty.
func PipeUser(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" {
		return "user"
	}
	return s
}

// LocalPipeName is the local API's Windows pipe: $GAIADESK_API_PIPE, else
// `\\.\pipe\gaiadesk-api-<user>` (<user>: $USERNAME, else username).
func LocalPipeName(getenv func(string) string, username string) string {
	if p := getenv("GAIADESK_API_PIPE"); p != "" {
		return p
	}
	return `\\.\pipe\gaiadesk-api-` + PipeUser(firstNonEmpty(getenv("USERNAME"), username))
}

func isAbs(p, goos string) bool {
	if goos == "windows" {
		return len(p) >= 3 && (p[0]|0x20) >= 'a' && (p[0]|0x20) <= 'z' && p[1] == ':' && (p[2] == '\\' || p[2] == '/') ||
			strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//")
	}
	return strings.HasPrefix(p, "/")
}

func joinPath(dir, name, goos string) string {
	sep := "/"
	if goos == "windows" {
		sep = `\`
	}
	return strings.TrimRight(dir, `\/`) + sep + name
}

// LocalAPIDir is the directory of the local API's socket and token:
// $GAIADESK_API_DIR when it is absolute, else <home>/.gaiadesk. goos is
// runtime.GOOS's form.
func LocalAPIDir(getenv func(string) string, home, goos string) string {
	if d := getenv("GAIADESK_API_DIR"); d != "" && isAbs(d, goos) {
		return d
	}
	return joinPath(home, ".gaiadesk", goos)
}

// LocalSocketPath is the local API's Unix socket (macOS, Linux).
func LocalSocketPath(getenv func(string) string, home, goos string) string {
	return joinPath(LocalAPIDir(getenv, home, goos), "api.sock", goos)
}

// LocalTokenPath is the file holding the desk's local admin token
// (`gdlocal_` + 64 hex digits).
func LocalTokenPath(getenv func(string) string, home, goos string) string {
	return joinPath(LocalAPIDir(getenv, home, goos), "api-token", goos)
}

func localUnavailable(detail string) *Error {
	e := newError(ClassUnreachable, ReasonLocalAPIUnavailable, LocalAPIUnavailable+" ("+detail+")")
	e.Kind = KindUnreachable
	return e
}

// NewLocal is a client of the desk's own API, for code running on the
// desk: the same desk operations as the hosted API (and Desks, this desk),
// over its socket or pipe. Options: WithSocketPath, WithAdminToken,
// WithDeskToken, WithEnv, WithRetry, WithUserAgent.
func NewLocal(opts ...Option) (*Client, error) {
	c := newConfig(opts)
	if err := c.only(TransportLocal, "WithSocketPath", "WithAdminToken", "WithEnv"); err != nil {
		return nil, err
	}
	var err error
	explicit, token, deskToken := "", "", ""
	if c.set["WithSocketPath"] {
		if explicit, err = nonEmpty(c.socketPath, "the socket path"); err != nil {
			return nil, err
		}
	}
	if c.set["WithAdminToken"] {
		if token, err = nonEmpty(c.adminToken, "the admin token"); err != nil {
			return nil, err
		}
	}
	if c.set["WithDeskToken"] {
		if deskToken, err = nonEmpty(c.deskToken, "the desk token"); err != nil {
			return nil, err
		}
	}
	cl, err := c.base(TransportLocal)
	if err != nil {
		return nil, err
	}
	getenv := c.env
	home := func() string {
		h, _ := os.UserHomeDir()
		return h
	}
	var once sync.Once
	where := explicit
	target := func() string {
		once.Do(func() {
			if where != "" {
				return
			}
			if runtime.GOOS == "windows" {
				name := ""
				if u, err := user.Current(); err == nil {
					name = u.Username
					if i := strings.LastIndex(name, `\`); i >= 0 {
						name = name[i+1:]
					}
				}
				where = LocalPipeName(getenv, name)
			} else {
				where = LocalSocketPath(getenv, home(), runtime.GOOS)
			}
		})
		return where
	}
	cl.baseURL = "http://localhost/v1"
	cl.where = "the desk's local API"
	cl.hc = &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialLocal(ctx, target())
		},
	}}
	cl.mapDialError = func(err error, op string) error {
		var ne *net.OpError
		if !errors.As(err, &ne) && !errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		path := target()
		var e *Error
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOTSOCK) || isPipeMissing(err) {
			e = localUnavailable(path)
		} else {
			e = newError(ClassUnreachable, ReasonNetwork, fmt.Sprintf("the desk's local API (%s) could not be reached: %v", path, err))
		}
		e.Op, e.Err = op, err
		return e
	}
	cl.creds = func(callToken string) (http.Header, error) {
		h := http.Header{}
		if t := firstNonEmpty(callToken, deskToken); t != "" {
			h.Set("X-GaiaDesk-Desk-Token", t)
			return h, nil
		}
		admin := token
		if admin == "" {
			file := LocalTokenPath(getenv, home(), runtime.GOOS)
			b, err := os.ReadFile(file)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil, localUnavailable("no local admin token at " + file + "; or give an agent token with WithDeskToken")
				}
				return nil, localError("cannot read the local admin token %s: %v", file, err)
			}
			if admin = strings.TrimSpace(string(b)); admin == "" {
				return nil, localError("the local admin token file %s is empty", file)
			}
		}
		h.Set("Authorization", "Bearer "+admin)
		return h, nil
	}
	return cl, nil
}
