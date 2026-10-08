//go:build !windows

package gaiadesk

import (
	"context"
	"net"
)

// dialLocal connects to the local API's Unix socket.
func dialLocal(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

func isPipeMissing(error) bool { return false }
