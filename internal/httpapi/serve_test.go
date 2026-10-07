package httpapi

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/fl4metf/vm-harness/harness"
	"github.com/fl4metf/vm-harness/internal/memprovider"
	"github.com/fl4metf/vm-harness/vm"
)

func TestServeFinishesRunningRequestsOnCancel(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, "finished")
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, l, h, "") }()

	replies := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + l.Addr().String() + "/slow")
		if err != nil {
			replies <- err.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		replies <- string(body)
	}()

	<-entered
	cancel()
	select {
	case err := <-served:
		t.Fatalf("serve returned %v while a request was still running", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if got := <-replies; got != "finished" {
		t.Fatalf("reply = %q, want finished", got)
	}
	if err := <-served; err != nil {
		t.Fatalf("serve = %v", err)
	}
	if conn, err := net.Dial("tcp", l.Addr().String()); err == nil {
		conn.Close()
		t.Fatal("server still accepts connections after shutdown")
	}
}

func TestListenAndServe(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	m := harness.New(harness.Config{Root: t.TempDir()}, memprovider.New(vm.VirtualBox))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- ListenAndServe(ctx, addr, New(m, "s3cret")) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("healthz status = %d", resp.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not come up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	resp, err := http.Get("http://" + addr + "/v1/vms")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list status = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("ListenAndServe = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after cancel")
	}
}

func TestListenAndServeAddressInUse(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := ListenAndServe(ctx, l.Addr().String(), http.NotFoundHandler()); err == nil {
		t.Fatal("ListenAndServe on a busy address returned nil")
	}
}

func TestServeAcceptsListenHostName(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	m := harness.New(harness.Config{Root: t.TempDir()}, memprovider.New(vm.VirtualBox))
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, l, New(m, ""), "vmh.example") }()
	defer func() {
		cancel()
		<-served
	}()

	cases := []struct {
		host   string
		status int
	}{
		{"vmh.example:" + port, http.StatusOK},
		{"VMH.example", http.StatusOK},
		{l.Addr().String(), http.StatusOK},
		{"other.example:" + port, http.StatusForbidden},
	}
	for _, c := range cases {
		req, err := http.NewRequest(http.MethodGet, "http://"+l.Addr().String()+"/v1/providers", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = c.host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.status {
			t.Errorf("host %q: status = %d, want %d", c.host, resp.StatusCode, c.status)
		}
	}
}
