package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/fl4metf/vm-harness/harness"
	"github.com/fl4metf/vm-harness/internal/memprovider"
	"github.com/fl4metf/vm-harness/vm"
)

var vmwareFeatures = []string{
	vm.FeatureGuestExec, vm.FeatureGuestCopy, vm.FeatureScreenshot, vm.FeatureSnapshots, vm.FeatureLinkedClone,
}

type recorder struct {
	*memprovider.Provider
	gui  bool
	copy vm.CopyRequest
	cred vm.Credentials
}

func (p *recorder) Start(ctx context.Context, ref string, gui bool) error {
	p.gui = gui
	return p.Provider.Start(ctx, ref, gui)
}

func (p *recorder) CopyTo(ctx context.Context, ref string, req vm.CopyRequest) error {
	p.copy = req
	return p.Provider.CopyTo(ctx, ref, req)
}

func (p *recorder) CopyFrom(ctx context.Context, ref string, req vm.CopyRequest) error {
	p.copy = req
	return p.Provider.CopyFrom(ctx, ref, req)
}

func (p *recorder) Screenshot(ctx context.Context, ref string, cred vm.Credentials) ([]byte, error) {
	p.cred = cred
	return p.Provider.Screenshot(ctx, ref, cred)
}

type offline struct {
	*memprovider.Provider
}

func (offline) Info(context.Context) (vm.HostInfo, error) {
	return vm.HostInfo{}, fmt.Errorf("hypervisor binary not found: %w", vm.ErrUnavailable)
}

type testEnv struct {
	h    http.Handler
	vbox *recorder
	vmw  *memprovider.Provider
}

func newEnv(t *testing.T) *testEnv {
	return newEnvWith(t, harness.Config{})
}

func newEnvWith(t *testing.T, cfg harness.Config) *testEnv {
	vbox := &recorder{Provider: memprovider.New(vm.VirtualBox)}
	vmw := memprovider.New(vm.VMware)
	vmw.Features = vmwareFeatures
	cfg.Root = t.TempDir()
	return &testEnv{h: New(harness.New(cfg, vbox, vmw), ""), vbox: vbox, vmw: vmw}
}

const testHost = "127.0.0.1:8070"

func request(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Host = testHost
	return r
}

func (e *testEnv) do(method, target, body string) *httptest.ResponseRecorder {
	r := request(method, target, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return e.send(r)
}

func (e *testEnv) send(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func managed(name string, state vm.State) vm.Machine {
	return vm.Machine{Name: name, State: state, Managed: true, Meta: map[string]string{}}
}

func wantStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, status, w.Body)
	}
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder, status int) T {
	t.Helper()
	wantStatus(t, w, status)
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q, want application/json", ct)
	}
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
	return v
}

func wantError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) errorDetail {
	t.Helper()
	got := decode[errorResponse](t, w, status).Error
	if got.Code != code || got.Message == "" {
		t.Fatalf("error = %+v, want code %q with a message", got, code)
	}
	return got
}

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBearerToken(t *testing.T) {
	m := harness.New(harness.Config{Root: t.TempDir()}, memprovider.New(vm.VirtualBox))
	h := New(m, "s3cret")
	cases := []struct {
		name   string
		target string
		auth   string
		status int
	}{
		{"no header", "/v1/providers", "", http.StatusUnauthorized},
		{"wrong token", "/v1/providers", "Bearer guess", http.StatusUnauthorized},
		{"token prefix", "/v1/providers", "Bearer s3c", http.StatusUnauthorized},
		{"longer token", "/v1/providers", "Bearer s3cret2", http.StatusUnauthorized},
		{"basic scheme", "/v1/providers", "Basic czNjcmV0", http.StatusUnauthorized},
		{"bare token", "/v1/providers", "s3cret", http.StatusUnauthorized},
		{"valid", "/v1/providers", "Bearer s3cret", http.StatusOK},
		{"scheme in lower case", "/v1/vms", "bearer s3cret", http.StatusOK},
		{"health is public", "/healthz", "", http.StatusOK},
		{"unknown route needs the token", "/v1/nope", "", http.StatusUnauthorized},
		{"unknown route with the token", "/v1/nope", "Bearer s3cret", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := request(http.MethodGet, c.target, nil)
			if c.auth != "" {
				r.Header.Set("Authorization", c.auth)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if c.status != http.StatusUnauthorized {
				wantStatus(t, w, c.status)
				return
			}
			wantError(t, w, http.StatusUnauthorized, "unauthorized")
			if !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Bearer") {
				t.Fatalf("WWW-Authenticate = %q", w.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

func TestEmptyTokenDisablesAuth(t *testing.T) {
	e := newEnv(t)
	wantStatus(t, e.do(http.MethodGet, "/v1/providers", ""), http.StatusOK)
	wantStatus(t, e.do(http.MethodGet, "/v1/vms", ""), http.StatusOK)
}

func TestUnknownRoutes(t *testing.T) {
	e := newEnv(t)
	wantError(t, e.do(http.MethodGet, "/v1/nope", ""), http.StatusNotFound, "not_found")
	wantError(t, e.do(http.MethodGet, "/v1/vms/web/", ""), http.StatusNotFound, "not_found")

	cases := []struct {
		method, target, allow string
	}{
		{http.MethodPut, "/v1/vms", "GET, POST"},
		{http.MethodGet, "/v1/vms/web/start", "POST"},
		{http.MethodPost, "/v1/vms/web", "GET, PATCH, DELETE"},
		{http.MethodGet, "/v1/vms/web/ports/ssh", "DELETE"},
		{http.MethodDelete, "/healthz", "GET"},
	}
	for _, c := range cases {
		w := e.do(c.method, c.target, "")
		wantError(t, w, http.StatusMethodNotAllowed, "invalid_argument")
		if got := w.Header().Get("Allow"); got != c.allow {
			t.Errorf("%s %s: Allow = %q, want %q", c.method, c.target, got, c.allow)
		}
	}
}

func TestErrorStatus(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managed("web", vm.StateRunning))
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("guest program: %w", vm.ErrNotFound), http.StatusNotFound, "not_found"},
		{vm.ErrExists, http.StatusConflict, "already_exists"},
		{vm.ErrInvalidState, http.StatusConflict, "invalid_state"},
		{vm.ErrLimit, http.StatusConflict, "limit_exceeded"},
		{vm.ErrInvalid, http.StatusBadRequest, "invalid_argument"},
		{vm.ErrForbidden, http.StatusForbidden, "forbidden"},
		{vm.ErrUnsupported, http.StatusNotImplemented, "unsupported"},
		{&vm.CommandError{Path: "VBoxManage", Args: []string{"guestcontrol"}, ExitCode: 1, Stderr: "additions not running", Kind: vm.ErrNotReady}, http.StatusServiceUnavailable, "not_ready"},
		{vm.ErrUnavailable, http.StatusServiceUnavailable, "unavailable"},
		{fmt.Errorf("guest exec: %w", context.DeadlineExceeded), http.StatusGatewayTimeout, "timeout"},
		{context.Canceled, http.StatusRequestTimeout, "canceled"},
		{errors.New("disk on fire"), http.StatusInternalServerError, "internal"},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			e.vbox.ExecFunc = func(vm.Machine, vm.ExecRequest) (vm.ExecResult, error) {
				return vm.ExecResult{}, c.err
			}
			got := wantError(t, e.do(http.MethodPost, "/v1/vms/web/exec", `{"command":["true"]}`), c.status, c.code)
			if got.Message != c.err.Error() {
				t.Fatalf("message = %q, want %q", got.Message, c.err.Error())
			}
		})
	}
}

func TestForbiddenForUnmanagedVM(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(vm.Machine{Name: "legacy", State: vm.StateStopped})
	got := wantError(t, e.do(http.MethodPost, "/v1/vms/legacy/start", ""), http.StatusForbidden, "forbidden")
	if !strings.Contains(got.Message, "read-only") || strings.Contains(got.Message, "vmh adopt") {
		t.Fatalf("message = %q, want a read-only explanation without a command to run", got.Message)
	}
	if slices.Contains(e.vbox.Calls(), "start virtualbox-1") {
		t.Fatal("unmanaged vm was started")
	}
	wantStatus(t, e.do(http.MethodGet, "/v1/vms/legacy", ""), http.StatusOK)
}

func TestUnavailableProvider(t *testing.T) {
	vbox := memprovider.New(vm.VirtualBox)
	vbox.Put(managed("web", vm.StateStopped))
	m := harness.New(harness.Config{Root: t.TempDir()}, vbox, offline{memprovider.New(vm.VMware)})
	e := &testEnv{h: New(m, "")}

	wantError(t, e.do(http.MethodGet, "/v1/vms/web?provider=vmware", ""), http.StatusServiceUnavailable, "unavailable")
	wantStatus(t, e.do(http.MethodGet, "/v1/vms/web", ""), http.StatusOK)
	providers := decode[[]harness.ProviderStatus](t, e.do(http.MethodGet, "/v1/providers", ""), http.StatusOK)
	if len(providers) != 2 || providers[1].Name != vm.VMware || providers[1].Available || providers[1].Error == "" {
		t.Fatalf("providers = %+v", providers)
	}

	none := &testEnv{h: New(harness.New(harness.Config{Root: t.TempDir()}, offline{memprovider.New(vm.VirtualBox)}), "")}
	wantError(t, none.do(http.MethodPost, "/v1/vms", `{"name":"web"}`), http.StatusServiceUnavailable, "unavailable")
}

func TestStrictJSON(t *testing.T) {
	cases := []struct {
		name string
		body string
		msg  string
	}{
		{"unknown field", `{"name":"web","colour":"red"}`, `unknown field "colour"`},
		{"unknown nested field", `{"name":"web","cloud_init":{"usr":"me"}}`, `unknown field "usr"`},
		{"truncated", `{"name":`, "unexpected EOF"},
		{"wrong type", `{"name":"web","cpus":"two"}`, "cpus"},
		{"second value", `{"name":"web"} {"name":"db"}`, "unexpected data after the JSON value"},
		{"trailing garbage", `{"name":"web"} ]`, "unexpected data after the JSON value"},
		{"stray brace", `{"name":"web"}}`, "unexpected data after the JSON value"},
		{"stray bracket", `{"name":"web"}]`, "unexpected data after the JSON value"},
		{"array", `[]`, "cannot unmarshal array"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			got := wantError(t, e.do(http.MethodPost, "/v1/vms", c.body), http.StatusBadRequest, "invalid_argument")
			if !strings.Contains(got.Message, c.msg) {
				t.Fatalf("message %q does not mention %q", got.Message, c.msg)
			}
			if len(e.vbox.Calls()) > 0 {
				t.Fatalf("provider was called: %v", e.vbox.Calls())
			}
		})
	}
}

func TestBodyTooLarge(t *testing.T) {
	e := newEnv(t)
	body := `{"name":"` + strings.Repeat("a", maxJSONBody) + `"}`
	wantError(t, e.do(http.MethodPost, "/v1/vms", body), http.StatusRequestEntityTooLarge, "limit_exceeded")

	e.vbox.Put(managed("web", vm.StateRunning))
	r := request(http.MethodPut, "/v1/vms/web/files?path=/tmp/big", strings.NewReader("tiny"))
	r.ContentLength = maxUpload + 1
	wantError(t, e.send(r), http.StatusRequestEntityTooLarge, "limit_exceeded")
	if _, ok := e.vbox.GuestFile("web", "/tmp/big"); ok {
		t.Fatal("oversized upload reached the guest")
	}
}

func TestRejectsBrowserRequests(t *testing.T) {
	cases := []struct {
		name   string
		host   string
		header string
		value  string
	}{
		{"rebound host", "rebind.attacker.example:8070", "", ""},
		{"rebound host without port", "rebind.attacker.example", "", ""},
		{"localhost prefix", "localhost.attacker.example:8070", "", ""},
		{"other ip", "192.0.2.10:8070", "", ""},
		{"no host", "", "", ""},
		{"foreign origin", testHost, "Origin", "https://attacker.example"},
		{"opaque origin", testHost, "Origin", "null"},
		{"own origin", testHost, "Origin", "http://" + testHost},
		{"cross-site fetch", testHost, "Sec-Fetch-Site", "cross-site"},
		{"same-site fetch", testHost, "Sec-Fetch-Site", "same-site"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.vbox.Put(managed("web", vm.StateRunning))
			ran := false
			e.vbox.ExecFunc = func(vm.Machine, vm.ExecRequest) (vm.ExecResult, error) {
				ran = true
				return vm.ExecResult{}, nil
			}
			for _, r := range []*http.Request{
				request(http.MethodPost, "/v1/vms/web/exec", strings.NewReader(`{"command":["id"]}`)),
				request(http.MethodPost, "/v1/vms/web/reset", nil),
				request(http.MethodGet, "/v1/providers", nil),
				request(http.MethodGet, "/healthz", nil),
			} {
				r.Host = c.host
				r.Header.Set("Content-Type", "application/json")
				if c.header != "" {
					r.Header.Set(c.header, c.value)
				}
				wantError(t, e.send(r), http.StatusForbidden, "forbidden")
			}
			if ran || len(e.vbox.Calls()) > 0 {
				t.Fatalf("a rejected request reached the provider: exec ran %v, calls %v", ran, e.vbox.Calls())
			}
		})
	}
}

func TestBrowserChecksApplyWithToken(t *testing.T) {
	h := New(harness.New(harness.Config{Root: t.TempDir()}, memprovider.New(vm.VirtualBox)), "s3cret")
	r := request(http.MethodGet, "/v1/vms", nil)
	r.Header.Set("Authorization", "Bearer s3cret")
	r.Header.Set("Origin", "https://attacker.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	wantError(t, w, http.StatusForbidden, "forbidden")
}

func TestAllowedHosts(t *testing.T) {
	e := newEnv(t)
	for _, host := range []string{"127.0.0.1:8070", "127.0.0.1", "127.9.9.9:8070", "localhost:8070", "LocalHost", "[::1]:8070", "[::1]"} {
		r := request(http.MethodGet, "/v1/providers", nil)
		r.Host = host
		if w := e.send(r); w.Code != http.StatusOK {
			t.Errorf("host %q: status = %d, body: %s", host, w.Code, w.Body)
		}
	}
	for _, site := range []string{"none", "same-origin"} {
		r := request(http.MethodGet, "/v1/providers", nil)
		r.Header.Set("Sec-Fetch-Site", site)
		wantStatus(t, e.send(r), http.StatusOK)
	}

	local := &net.TCPAddr{IP: net.ParseIP("192.0.2.5"), Port: 8070}
	cases := []struct {
		host   string
		status int
	}{
		{"192.0.2.5:8070", http.StatusOK},
		{"192.0.2.5", http.StatusOK},
		{"192.0.2.6:8070", http.StatusForbidden},
		{"vmh.example:8070", http.StatusForbidden},
	}
	for _, c := range cases {
		r := request(http.MethodGet, "/v1/providers", nil)
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
		r.Host = c.host
		if w := e.send(r); w.Code != c.status {
			t.Errorf("host %q on %s: status = %d, want %d, body: %s", c.host, local, w.Code, c.status, w.Body)
		}
	}
}

func TestJSONBodyNeedsJSONContentType(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managed("web", vm.StateRunning))
	ran := 0
	e.vbox.ExecFunc = func(vm.Machine, vm.ExecRequest) (vm.ExecResult, error) {
		ran++
		return vm.ExecResult{}, nil
	}
	exec := func(contentType string) *httptest.ResponseRecorder {
		r := request(http.MethodPost, "/v1/vms/web/exec", strings.NewReader(`{"command":["id"]}`))
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		return e.send(r)
	}
	for _, ct := range []string{"", "text/plain", "text/plain; charset=utf-8", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "application/jsonx"} {
		got := wantError(t, exec(ct), http.StatusUnsupportedMediaType, "invalid_argument")
		if !strings.Contains(got.Message, "application/json") {
			t.Errorf("content type %q: message %q does not name the expected type", ct, got.Message)
		}
	}
	if ran != 0 {
		t.Fatalf("exec ran %d times for bodies not declared as json", ran)
	}
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON"} {
		wantStatus(t, exec(ct), http.StatusOK)
	}

	wantStatus(t, e.send(request(http.MethodPost, "/v1/vms/web/stop", nil)), http.StatusOK)
	wantStatus(t, e.send(request(http.MethodPost, "/v1/vms/web/start", nil)), http.StatusOK)
	upload := request(http.MethodPut, "/v1/vms/web/files?path=/tmp/notes.txt", strings.NewReader("hello"))
	upload.Header.Set("Content-Type", "text/plain")
	wantStatus(t, e.send(upload), http.StatusNoContent)
}
