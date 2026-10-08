//go:build windows

package gaiadesk

// The local transport over a Windows named pipe: a minimal pipe server (one
// HTTP request per connection, as the client sends with keep-alives off)
// answers stats, and a missing pipe is local_api_unavailable.

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func servePipe(t *testing.T, name string, requests int) <-chan http.Header {
	seen := make(chan http.Header, requests)
	p, _ := windows.UTF16PtrFromString(name)
	go func() {
		for i := 0; i < requests; i++ {
			h, err := windows.CreateNamedPipe(p, windows.PIPE_ACCESS_DUPLEX, windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT, windows.PIPE_UNLIMITED_INSTANCES, 64*1024, 64*1024, 0, nil)
			if err != nil {
				t.Errorf("CreateNamedPipe: %v", err)
				return
			}
			if err := windows.ConnectNamedPipe(h, nil); err != nil && err != windows.ERROR_PIPE_CONNECTED {
				t.Errorf("ConnectNamedPipe: %v", err)
				return
			}
			f := os.NewFile(uintptr(h), name)
			req, err := http.ReadRequest(bufio.NewReader(f))
			if err != nil {
				t.Errorf("ReadRequest: %v", err)
				f.Close()
				return
			}
			seen <- req.Header
			body := `{"desk":"123456789","hostname":"pc","os":"windows","cpu_percent":5,"cpus":4,"mem_total_mb":1,"mem_free_mb":1,"uptime_secs":1,"jobs_running":0}`
			fmt.Fprintf(f, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
			_ = windows.FlushFileBuffers(h)
			_ = windows.DisconnectNamedPipe(h)
			f.Close()
		}
	}()
	return seen
}

func TestLocalNamedPipe(t *testing.T) {
	name := fmt.Sprintf(`\\.\pipe\gaiadesk-go-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	seen := servePipe(t, name, 1)
	time.Sleep(50 * time.Millisecond)
	c := must[*Client](t)(NewLocal(WithEnv(map[string]string{"GAIADESK_API_PIPE": name}), WithAdminToken(adminToken)))
	st := must[*StatsReport](t)(c.Stats(ctx(t), okDesk))
	if st.OS != "windows" {
		t.Fatalf("%+v", st)
	}
	if h := <-seen; h.Get("Authorization") != "Bearer "+adminToken {
		t.Fatalf("%v", h)
	}
	missing := must[*Client](t)(NewLocal(WithEnv(map[string]string{"GAIADESK_API_PIPE": name + "-missing"}), WithAdminToken(adminToken), WithRetry(NoRetry)))
	_, err := missing.Stats(ctx(t), okDesk)
	if e := errOf(t, err); e.Reason != ReasonLocalAPIUnavailable || !strings.Contains(e.Message, "-missing") {
		t.Fatalf("%+v", e)
	}
}
