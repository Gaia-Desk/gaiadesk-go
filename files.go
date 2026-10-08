package gaiadesk

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func localError(format string, a ...any) *Error {
	e := newError("", "local", fmt.Sprintf(format, a...))
	e.Kind = KindLocal
	e.ExitCode = 255
	return e
}

// uploadBody is how an upload's bytes are read: its size, and a reader per
// try (replay: a retry may read them again).
func uploadBody(src io.Reader) (size int64, body func() (io.Reader, error), replay bool, err error) {
	if s, ok := src.(io.Seeker); ok {
		start, err1 := s.Seek(0, io.SeekCurrent)
		end, err2 := s.Seek(0, io.SeekEnd)
		if err1 == nil && err2 == nil {
			if _, err := s.Seek(start, io.SeekStart); err != nil {
				return 0, nil, false, localError("cannot read the file: %v", err)
			}
			return end - start, func() (io.Reader, error) {
				if _, err := s.Seek(start, io.SeekStart); err != nil {
					return nil, localError("cannot read the file again: %v", err)
				}
				return io.LimitReader(src, end-start), nil
			}, true, nil
		}
	}
	// Anything else is read into memory first (the size goes in the request).
	b, err := io.ReadAll(io.LimitReader(src, FileLimit+1))
	if err != nil {
		return 0, nil, false, localError("cannot read the file: %v", err)
	}
	return int64(len(b)), func() (io.Reader, error) { return bytes.NewReader(b), nil }, true, nil
}

// Upload writes r's bytes to remote, a file on the desk (relative paths
// start in the desk user's home, or a confined token's folder; a path that
// names an existing folder receives it under that folder's last name):
// `PUT /desks/{id}/files?path=`. At most FileLimit (256 MB). An
// io.ReadSeeker (an *os.File, a *bytes.Reader) is streamed; any other
// reader is read into memory first, as the API needs the size up front.
func (c *Client) Upload(ctx context.Context, desk, remote string, r io.Reader, opts ...CallOption) (*CopyResult, error) {
	d, call, err := prep(desk, opts)
	if err != nil {
		return nil, err
	}
	if remote == "" {
		return nil, usageError("a remote path is required")
	}
	size, body, replay, err := uploadBody(r)
	if err != nil {
		return nil, err
	}
	if size > FileLimit {
		e := usageError("the file is %d bytes or more; the API takes files up to 256 MB (copy larger ones with gaiadesk-cli)", size)
		e.Reason = ReasonTooLarge
		return nil, e
	}
	req := &request{method: http.MethodPut, path: deskPath(d) + "/files", query: url.Values{"path": {remote}}, body: body, size: size, replay: replay, call: call,
		e2e: &e2eOp{desk: d, op: "file_put", request: map[string]any{"op": "file_put", "path": remote, "size": size}}}
	res, err := get[CopyResult](ctx, c, req)
	if err != nil {
		return nil, err
	}
	if len(res.Failed) > 0 {
		e := newError(ClassFailed, "", fmt.Sprintf("%d file(s) failed to copy", len(res.Failed)))
		e.Desk, e.Op = d, req.op()
		return res, e
	}
	return res, nil
}

func basename(p string) string {
	parts := strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func endsWithSep(p string) bool { return strings.HasSuffix(p, "/") || strings.HasSuffix(p, "\\") }

// UploadFile copies a local file to the desk. A remote ending in `/` (or
// "") is a folder: the file keeps its name.
func (c *Client) UploadFile(ctx context.Context, local, desk, remote string, opts ...CallOption) (*CopyResult, error) {
	if local == "" {
		return nil, usageError("a local path is required")
	}
	f, err := os.Open(local)
	if err != nil {
		return nil, localError("cannot read %s: %v", local, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, localError("cannot read %s: %v", local, err)
	}
	if st.IsDir() {
		return nil, notServed("uploading the folder "+local, c.transport, "the API copies single files; copy folders with gaiadesk-cli")
	}
	if st.Size() > FileLimit {
		e := usageError("%s is %d bytes; the API takes files up to 256 MB (copy larger ones with gaiadesk-cli)", local, st.Size())
		e.Reason = ReasonTooLarge
		return nil, e
	}
	target := remote
	if remote == "" || endsWithSep(remote) {
		target = remote + filepath.Base(local)
	}
	return c.Upload(ctx, desk, target, f, opts...)
}

// downloadReader is an open download.
type downloadReader interface {
	io.ReadCloser
}

// OpenDownload opens a file on the desk for reading: `GET
// /desks/{id}/files?path=`. Read it to io.EOF: a transfer that breaks off
// is an error (ErrConnectionLost), never a clean short file. Close it.
func (c *Client) OpenDownload(ctx context.Context, desk, remote string, opts ...CallOption) (io.ReadCloser, error) {
	d, call, err := prep(desk, opts)
	if err != nil {
		return nil, err
	}
	if remote == "" {
		return nil, usageError("a remote path is required")
	}
	r := &request{method: http.MethodGet, path: deskPath(d) + "/files", query: url.Values{"path": {remote}}, accept: "application/octet-stream", call: call,
		e2e: &e2eOp{desk: d, op: "file_get", request: map[string]any{"op": "file_get", "path": remote}}}
	res, seal, err := c.do(ctx, r)
	if err != nil {
		return nil, err
	}
	var out downloadReader = &plainDownload{body: res.Body, op: r.op()}
	if seal != nil {
		out = &sealedDownload{seal: seal, br: bufio.NewReaderSize(res.Body, 128*1024), body: res.Body, op: r.op()}
	}
	return out, nil
}

// Download copies a file on the desk to w, returning the bytes written.
// On an error, w may hold part of the file.
func (c *Client) Download(ctx context.Context, desk, remote string, w io.Writer, opts ...CallOption) (int64, error) {
	rc, err := c.OpenDownload(ctx, desk, remote, opts...)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	n, err := io.Copy(w, rc)
	if err != nil {
		if AsError(err) == nil {
			if ctx.Err() != nil {
				return n, interrupted("GET "+deskPath(desk)+"/files", ctx.Err())
			}
			return n, localError("cannot write the download: %v", err)
		}
		return n, err
	}
	return n, nil
}

// DownloadBytes reads a file on the desk into memory.
func (c *Client) DownloadBytes(ctx context.Context, desk, remote string, opts ...CallOption) ([]byte, error) {
	var b bytes.Buffer
	if _, err := c.Download(ctx, desk, remote, &b, opts...); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// DownloadFile copies a file on the desk to a local file (a local folder,
// or a path ending in a separator, keeps the remote name). The file appears
// only once complete.
func (c *Client) DownloadFile(ctx context.Context, desk, remote, local string, opts ...CallOption) (*CopyResult, error) {
	d, err := checkDesk(desk)
	if err != nil {
		return nil, err
	}
	if local == "" {
		return nil, usageError("a local path is required")
	}
	started := time.Now()
	dest := local
	if st, err := os.Stat(local); endsWithSep(local) || err == nil && st.IsDir() {
		dest = filepath.Join(local, basename(remote))
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".gaiadesk-*")
	if err != nil {
		return nil, localError("cannot write %s: %v", dest, err)
	}
	keep := false
	defer func() {
		if !keep {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	n, err := c.Download(ctx, d, remote, tmp, opts...)
	if err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, localError("cannot write %s: %v", dest, err)
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return nil, localError("cannot write %s: %v", dest, err)
	}
	keep = true
	return &CopyResult{Direction: "download", Desk: d, Destination: dest, Files: 1, Bytes: uint64(n), Failed: []CopyFailure{}, Seconds: time.Since(started).Seconds()}, nil
}
