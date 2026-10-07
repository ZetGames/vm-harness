package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fl4metf/vm-harness/harness"
	"github.com/fl4metf/vm-harness/internal/secfile/secfiletest"
	"github.com/fl4metf/vm-harness/vm"
)

func TestServeRefusesPublicAddressWithoutToken(t *testing.T) {
	e := newEnv(t)
	for _, addr := range []string{"0.0.0.0:8070", ":8070", "[::]:8070", "192.168.1.5:8070", "example.com:80"} {
		msg := e.fail(9, "invalid_argument", "serve", "--addr", addr)
		if !strings.Contains(msg.Message, "token") || !strings.Contains(msg.Message, addr) {
			t.Errorf("serve --addr %s: message %q", addr, msg.Message)
		}
	}
	r := e.run("-o", "text", "serve", "--addr", "0.0.0.0:8070")
	if r.code != 9 || !strings.HasPrefix(r.stderr, "vmh: refusing to serve") {
		t.Errorf("text refusal: exit %d, stderr %q", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.root, "serve.token")); !os.IsNotExist(err) {
		t.Errorf("refused serve left a token file: %v", err)
	}
}

type serving struct {
	addr string
	url  string
	stop func() result
}

func (e *testEnv) serve(args ...string) serving {
	t := e.t
	t.Helper()
	type call struct {
		addr string
		h    http.Handler
	}
	calls := make(chan call, 1)
	saved := serveHTTP
	serveHTTP = func(ctx context.Context, addr string, h http.Handler) error {
		calls <- call{addr, h}
		<-ctx.Done()
		return nil
	}
	t.Cleanup(func() { serveHTTP = saved })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan result, 1)
	go func() { done <- e.runContext(ctx, append([]string{"serve"}, args...)...) }()
	select {
	case c := <-calls:
		srv := httptest.NewServer(c.h)
		t.Cleanup(srv.Close)
		stop := func() result {
			cancel()
			select {
			case r := <-done:
				return r
			case <-time.After(10 * time.Second):
				t.Fatal("serve did not stop after cancellation")
				return result{}
			}
		}
		return serving{addr: c.addr, url: srv.URL, stop: stop}
	case r := <-done:
		t.Fatalf("serve exited early: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
		return serving{}
	}
}

func TestServeGeneratesToken(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)
	s := e.serve()
	if s.addr != "127.0.0.1:8070" {
		t.Errorf("listen address = %q", s.addr)
	}

	path := filepath.Join(e.root, "serve.token")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	token := string(data)
	if key, err := hex.DecodeString(token); err != nil || len(key) != 32 {
		t.Errorf("token %q is not 32 random bytes in hex", token)
	}
	if err := secfiletest.CheckRestricted(path); err != nil {
		t.Error(err)
	}

	if status, _ := call(t, http.MethodGet, s.url+"/v1/vms", "", ""); status != http.StatusUnauthorized {
		t.Errorf("request without token: status %d", status)
	}
	if status, body := call(t, http.MethodGet, s.url+"/v1/vms", token, ""); status != http.StatusOK || !strings.Contains(body, `"web"`) {
		t.Errorf("request with the generated token: status %d, body %s", status, body)
	}

	r := s.stop()
	if r.code != 0 || r.stdout != "" {
		t.Errorf("serve ended with exit %d, stdout %q", r.code, r.stdout)
	}
	if !strings.Contains(r.stderr, "serve.token") || strings.Contains(r.stderr, token) {
		t.Errorf("stderr should name the token file but not the token: %q", r.stderr)
	}

	again := e.serve()
	defer again.stop()
	if next, err := os.ReadFile(path); err != nil || string(next) == token {
		t.Errorf("second start reused the token: %q, %v", next, err)
	}
}

func TestServeWithToken(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)
	t.Setenv("VMH_TOKEN", "s3cret")
	s := e.serve("--addr", "127.0.0.1:9999")
	if s.addr != "127.0.0.1:9999" {
		t.Errorf("listen address = %q", s.addr)
	}

	if status, _ := call(t, http.MethodGet, s.url+"/v1/vms", "", ""); status != http.StatusUnauthorized {
		t.Errorf("request without token: status %d", status)
	}
	if status, body := call(t, http.MethodGet, s.url+"/v1/vms", "s3cret", ""); status != http.StatusOK || !strings.Contains(body, `"web"`) {
		t.Errorf("request with token: status %d, body %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(e.root, "serve.token")); !os.IsNotExist(err) {
		t.Errorf("serve with a token wrote a token file: %v", err)
	}

	r := s.stop()
	if r.code != 0 || r.stdout != "" || !strings.Contains(r.stderr, "serving the vmh api") {
		t.Errorf("serve ended with exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestServeLimitsHostPathsToFilesDir(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)
	var hostDirs []string
	useManager(t, func(cfg harness.Config) *harness.Manager {
		hostDirs = cfg.HostDirs
		return harness.New(cfg, e.vbox, e.vmw)
	})
	t.Setenv("VMH_TOKEN", "s3cret")
	s := e.serve()
	defer s.stop()

	files := filepath.Join(e.root, "files")
	if !slices.Equal(hostDirs, []string{files}) {
		t.Fatalf("host dirs = %q, want %q", hostDirs, files)
	}
	inside := writeFile(t, filepath.Join(files, "in.txt"), "in")
	outside := writeFile(t, filepath.Join(t.TempDir(), "out.txt"), "out")

	copyTo := func(host string) int {
		body, err := json.Marshal(harness.CopyRequest{CopyRequest: vm.CopyRequest{HostPath: host, GuestPath: "/tmp/x"}})
		if err != nil {
			t.Fatal(err)
		}
		status, _ := call(t, http.MethodPost, s.url+"/v1/vms/web/copy-to", "s3cret", string(body))
		return status
	}
	if status := copyTo(outside); status != http.StatusForbidden {
		t.Errorf("copy from outside %s: status %d, want 403", files, status)
	}
	if status := copyTo(inside); status != http.StatusNoContent {
		t.Errorf("copy from inside %s: status %d, want 204", files, status)
	}
}

func TestServeKeepsConfiguredHostDirs(t *testing.T) {
	e := newEnv(t)
	shared := t.TempDir()
	cfg, err := json.Marshal(map[string][]string{"host_dirs": {shared}})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(e.root, "config.json"), string(cfg))
	var hostDirs []string
	useManager(t, func(cfg harness.Config) *harness.Manager {
		hostDirs = cfg.HostDirs
		return harness.New(cfg, e.vbox, e.vmw)
	})
	t.Setenv("VMH_TOKEN", "s3cret")
	e.serve().stop()

	if !slices.Equal(hostDirs, []string{shared}) {
		t.Errorf("host dirs = %q, want %q", hostDirs, shared)
	}
	if _, err := os.Stat(filepath.Join(e.root, "files")); !os.IsNotExist(err) {
		t.Errorf("files dir was created although host_dirs is set: %v", err)
	}
}

func call(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(data)
}

func TestLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8070": true,
		"127.0.0.2:80":   true,
		"[::1]:8070":     true,
		"localhost:8070": true,
		"LOCALHOST:1":    true,
		"0.0.0.0:8070":   false,
		":8070":          false,
		"[::]:8070":      false,
		"10.0.0.1:8070":  false,
		"myhost:8070":    false,
		"127.0.0.1":      false,
	}
	for addr, want := range cases {
		if got := loopback(addr); got != want {
			t.Errorf("loopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestMCPExitsZeroWhenTheClientDisconnects(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)
	e.stdin = strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"vm_get","arguments":{"vm":"web"}}}`,
	}, "\n") + "\n"
	for range 20 {
		if r := e.run("mcp"); r.code != 0 {
			t.Fatalf("vmh mcp: exit %d, stderr %q", r.code, r.stderr)
		}
	}
}

func TestMCPFailsOnBrokenInput(t *testing.T) {
	e := newEnv(t)
	e.stdin = "hello\n"
	r := e.run("mcp")
	if r.code != 1 || r.stdout != "" || !strings.HasPrefix(r.stderr, "vmh: ") {
		t.Errorf("vmh mcp: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}
