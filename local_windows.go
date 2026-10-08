//go:build windows

package gaiadesk

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
)

const (
	errorPipeBusy     windows.Errno = 231
	errorFileNotFound windows.Errno = 2
)

// pipeAddr is a named pipe's address.
type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

// pipeConn is a client end of a named pipe as a net.Conn, on an overlapped
// handle with one event per direction, so a read and a write run at once
// (as net/http needs). It does not go through os.File: the runtime's
// poll.FD keeps a file offset that a concurrent Read and Write both update
// unsynchronised (a data race on a pipe, where the offset means nothing).
type pipeConn struct {
	h      windows.Handle
	name   string
	closed atomic.Bool
	r, w   pipeDir
}

// pipeDir is one direction: one operation at a time, its OVERLAPPED on the
// heap (the kernel writes to it after the call returns), its deadline.
type pipeDir struct {
	mu       sync.Mutex
	ov       *windows.Overlapped
	deadline atomic.Int64 // UnixNano; 0: none
}

func newPipeConn(h windows.Handle, name string) (*pipeConn, error) {
	c := &pipeConn{h: h, name: name}
	for _, d := range []*pipeDir{&c.r, &c.w} {
		ev, err := windows.CreateEvent(nil, 1, 0, nil)
		if err != nil {
			c.release()
			return nil, os.NewSyscallError("CreateEvent", err)
		}
		d.ov = &windows.Overlapped{HEvent: ev}
	}
	return c, nil
}

func (c *pipeConn) release() {
	for _, d := range []*pipeDir{&c.r, &c.w} {
		if d.ov != nil {
			_ = windows.CloseHandle(d.ov.HEvent)
		}
	}
	_ = windows.CloseHandle(c.h)
}

func (c *pipeConn) Read(b []byte) (int, error) {
	n, err := c.do(&c.r, b, false)
	if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) || (err == nil && n == 0 && len(b) > 0) {
		return n, io.EOF
	}
	return n, c.opError("read", err)
}

func (c *pipeConn) Write(b []byte) (int, error) {
	total := 0
	for total < len(b) {
		n, err := c.do(&c.w, b[total:], true)
		total += n
		if err != nil {
			return total, c.opError("write", err)
		}
	}
	return total, nil
}

func (c *pipeConn) opError(op string, err error) error {
	if err == nil {
		return nil
	}
	return &net.OpError{Op: op, Net: "pipe", Addr: pipeAddr(c.name), Err: err}
}

// do runs one overlapped read or write to completion, its deadline or Close.
func (c *pipeConn) do(d *pipeDir, b []byte, write bool) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	wait := uint32(windows.INFINITE)
	if dl := d.deadline.Load(); dl != 0 {
		left := time.Until(time.Unix(0, dl))
		if left <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		wait = uint32(min(left.Milliseconds()+1, int64(windows.INFINITE-1)))
	}
	var n uint32
	var err error
	if write {
		err = windows.WriteFile(c.h, b, &n, d.ov)
	} else {
		err = windows.ReadFile(c.h, b, &n, d.ov)
	}
	if err != nil && err != windows.ERROR_IO_PENDING {
		return 0, err
	}
	// Close sets closed before cancelling: an operation issued after its
	// CancelIoEx sees the flag here and cancels itself.
	if c.closed.Load() {
		_ = windows.CancelIoEx(c.h, d.ov)
	}
	timedOut := false
	if ev, _ := windows.WaitForSingleObject(d.ov.HEvent, wait); ev == uint32(windows.WAIT_TIMEOUT) {
		timedOut = true
		_ = windows.CancelIoEx(c.h, d.ov)
	}
	err = windows.GetOverlappedResult(c.h, d.ov, &n, true)
	if errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
		if c.closed.Load() {
			return int(n), net.ErrClosed
		}
		if timedOut {
			return int(n), os.ErrDeadlineExceeded
		}
	}
	return int(n), err
}

// Close cancels any operation in flight, waits for it, and closes the pipe.
func (c *pipeConn) Close() error {
	if c.closed.Swap(true) {
		return net.ErrClosed
	}
	_ = windows.CancelIoEx(c.h, nil)
	c.r.mu.Lock()
	c.w.mu.Lock()
	defer c.r.mu.Unlock()
	defer c.w.mu.Unlock()
	c.release()
	return nil
}

func (c *pipeConn) LocalAddr() net.Addr  { return pipeAddr(c.name) }
func (c *pipeConn) RemoteAddr() net.Addr { return pipeAddr(c.name) }

func setDeadline(d *pipeDir, t time.Time) {
	if t.IsZero() {
		d.deadline.Store(0)
	} else {
		d.deadline.Store(t.UnixNano())
	}
}

// SetDeadline sets both deadlines; one set while an operation waits applies
// from the next operation (net/http's client does not use them on this
// transport; contexts bound its calls).
func (c *pipeConn) SetDeadline(t time.Time) error {
	setDeadline(&c.r, t)
	setDeadline(&c.w, t)
	return nil
}

func (c *pipeConn) SetReadDeadline(t time.Time) error  { setDeadline(&c.r, t); return nil }
func (c *pipeConn) SetWriteDeadline(t time.Time) error { setDeadline(&c.w, t); return nil }

// dialLocal connects to the local API's named pipe, waiting while every
// instance is busy (until ctx is done).
func dialLocal(ctx context.Context, name string) (net.Conn, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	for {
		h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
		if err == nil {
			c, err := newPipeConn(h, name)
			if err != nil {
				return nil, &net.OpError{Op: "dial", Net: "pipe", Addr: pipeAddr(name), Err: err}
			}
			return c, nil
		}
		if !errors.Is(err, errorPipeBusy) {
			return nil, &net.OpError{Op: "dial", Net: "pipe", Addr: pipeAddr(name), Err: os.NewSyscallError("CreateFile", err)}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func isPipeMissing(err error) bool { return errors.Is(err, errorFileNotFound) }
