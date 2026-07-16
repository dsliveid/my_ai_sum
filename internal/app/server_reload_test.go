package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestSwitchHTTPServerPreservesOldListenerWhenNewAddressFails(t *testing.T) {
	a := newTestApp(t)
	defer a.db.Close()
	a.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	port1 := freeTCPPort(t)
	url1, old, err := a.switchHTTPServer("127.0.0.1", port1)
	if err != nil {
		t.Fatalf("start first server: %v", err)
	}
	if old != nil {
		t.Fatal("first start returned an old server")
	}
	assertHTTPReady(t, url1)

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer occupied.Close()
	busyPort := occupied.Addr().(*net.TCPAddr).Port
	if _, _, err := a.switchHTTPServer("127.0.0.1", busyPort); err == nil {
		t.Fatal("switch to occupied port succeeded unexpectedly")
	}
	assertHTTPReady(t, url1)

	port2 := freeTCPPort(t)
	url2, old, err := a.switchHTTPServer("127.0.0.1", port2)
	if err != nil {
		t.Fatalf("switch to second server: %v", err)
	}
	if old == nil {
		t.Fatal("second start did not return old server")
	}
	assertHTTPReady(t, url2)
	shutdownOldServer(old)

	a.serverMu.Lock()
	current := a.server
	a.serverMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = current.Shutdown(ctx)
}

func TestBrowserURLUsesLocalhostForWildcardListenHost(t *testing.T) {
	for _, host := range []string{"", "0.0.0.0", "::", "[::]"} {
		if got := browserURL(host, 8716); got != "http://localhost:8716/" {
			t.Fatalf("browserURL(%q) = %q, want http://localhost:8716/", host, got)
		}
	}
}

func TestBrowserURLKeepsConcreteListenHost(t *testing.T) {
	if got := browserURL("127.0.0.1", 8716); got != "http://127.0.0.1:8716/" {
		t.Fatalf("browserURL concrete IPv4 = %q", got)
	}
	if got := browserURL("::1", 8716); got != "http://[::1]:8716/" {
		t.Fatalf("browserURL concrete IPv6 = %q", got)
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func assertHTTPReady(t *testing.T, url string) {
	t.Helper()
	client := http.Client{Timeout: 2 * time.Second}
	var lastErr error
	for i := 0; i < 20; i++ {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s not ready: %v", url, lastErr)
}
