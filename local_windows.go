//go:build windows

package gaiadesk

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"time"
)

const (
	errorPipeBusy     syscall.Errno = 231
	errorFileNotFound syscall.Errno = 2
)

// pipeAddr is a named pipe's address.
type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

// pipeConn is a client end of a named pipe as a net.Conn. The handle is
// opened for overlapped I/O, so os.NewFile registers it with the runtime's
// I/O completion port: reads and writes run concurrently (as net/http
// needs) and honour deadlines.
type pipeConn struct {
	*os.File
	name string
}

func (c *pipeConn) LocalAddr() net.Addr  { return pipeAddr(c.name) }
func (c *pipeConn) RemoteAddr() net.Addr { return pipeAddr(c.name) }

// dialLocal connects to the local API's named pipe, waiting while every
// instance is busy (until ctx is done).
func dialLocal(ctx context.Context, name string) (net.Conn, error) {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	for {
		h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OVERLAPPED, 0)
		if err == nil {
			return &pipeConn{File: os.NewFile(uintptr(h), name), name: name}, nil
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
