package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ZetGames/vm-harness/harness"
	"github.com/ZetGames/vm-harness/vm"
)

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
	shutdownTimeout   = 30 * time.Second
)

type server struct {
	mux   *http.ServeMux
	token string
}

type listenHostKey struct{}

func New(m *harness.Manager, token string) http.Handler {
	return &server{mux: routes(&api{m: m}), token: token}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := checkCaller(r); err != nil {
		writeError(w, err)
		return
	}
	if r.URL.Path != "/healthz" && !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="vmh"`)
		fail(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
		return
	}
	s.mux.ServeHTTP(w, r)
}

func checkCaller(r *http.Request) error {
	if !hostAllowed(r) {
		return fmt.Errorf("host %q is not served, connect through localhost, a loopback address or the address vmh listens on: %w", r.Host, vm.ErrForbidden)
	}
	if _, ok := r.Header["Origin"]; ok {
		return fmt.Errorf("requests from web pages are not accepted: %w", vm.ErrForbidden)
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site":
		return fmt.Errorf("cross-site requests are not accepted: %w", vm.ErrForbidden)
	}
	return nil
}

func hostAllowed(r *http.Request) bool {
	host := (&url.URL{Host: r.Host}).Hostname()
	listen, _ := r.Context().Value(listenHostKey{}).(string)
	switch {
	case host == "":
		return false
	case strings.EqualFold(host, "localhost"), strings.EqualFold(host, listen):
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	return ip.IsLoopback() || ok && ip.Equal(local.IP)
}

func (s *server) authorized(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(token)))
	want := sha256.Sum256([]byte(s.token))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

func routes(a *api) *http.ServeMux {
	mux := http.NewServeMux()
	allowed := make(map[string][]string)
	handle := func(pattern string, h handler) {
		mux.Handle(pattern, h)
		method, path, _ := strings.Cut(pattern, " ")
		allowed[path] = append(allowed[path], method)
	}

	handle("GET /healthz", a.health)
	handle("GET /v1/providers", a.providers)
	handle("GET /v1/vms", a.list)
	handle("POST /v1/vms", a.create)
	handle("GET /v1/vms/{vm}", machineOp(a.m.Get))
	handle("PATCH /v1/vms/{vm}", a.update)
	handle("DELETE /v1/vms/{vm}", a.delete)
	handle("POST /v1/vms/{vm}/start", a.start)
	handle("POST /v1/vms/{vm}/stop", a.stop)
	handle("POST /v1/vms/{vm}/pause", machineOp(a.m.Pause))
	handle("POST /v1/vms/{vm}/resume", machineOp(a.m.Resume))
	handle("POST /v1/vms/{vm}/reset", machineOp(a.m.Reset))
	handle("POST /v1/vms/{vm}/suspend", machineOp(a.m.Suspend))
	handle("POST /v1/vms/{vm}/clone", a.clone)
	handle("GET /v1/vms/{vm}/snapshots", a.snapshots)
	handle("POST /v1/vms/{vm}/snapshots", a.takeSnapshot)
	handle("POST /v1/vms/{vm}/snapshots/{snapshot}/restore", a.restoreSnapshot)
	handle("DELETE /v1/vms/{vm}/snapshots/{snapshot}", a.deleteSnapshot)
	handle("POST /v1/vms/{vm}/exec", a.exec)
	handle("POST /v1/vms/{vm}/copy-to", copyOp(a.m.CopyTo))
	handle("POST /v1/vms/{vm}/copy-from", copyOp(a.m.CopyFrom))
	handle("PUT /v1/vms/{vm}/files", a.writeFile)
	handle("GET /v1/vms/{vm}/files", a.readFile)
	handle("GET /v1/vms/{vm}/ip", a.ip)
	handle("POST /v1/vms/{vm}/wait", a.wait)
	handle("GET /v1/vms/{vm}/screenshot", a.screenshot)
	handle("GET /v1/vms/{vm}/ports", a.ports)
	handle("POST /v1/vms/{vm}/ports", a.addPort)
	handle("DELETE /v1/vms/{vm}/ports/{name}", a.removePort)

	for path, methods := range allowed {
		mux.Handle(path, methodNotAllowed(methods))
	}
	mux.Handle("/", handler(noRoute))
	return mux
}

func methodNotAllowed(methods []string) http.Handler {
	allow := strings.Join(methods, ", ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		fail(w, http.StatusMethodNotAllowed, vm.CodeInvalid, fmt.Sprintf("%s does not accept %s, use %s", r.URL.Path, r.Method, allow))
	})
}

func noRoute(_ http.ResponseWriter, r *http.Request) error {
	return fmt.Errorf("no route for %s %s: %w", r.Method, r.URL.Path, vm.ErrNotFound)
}

func ListenAndServe(ctx context.Context, addr string, h http.Handler) error {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(addr)
	return serve(ctx, l, h, host)
}

func serve(ctx context.Context, l net.Listener, h http.Handler, host string) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		BaseContext: func(net.Listener) context.Context {
			return context.WithValue(context.Background(), listenHostKey{}, host)
		},
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		srv.Close()
		return fmt.Errorf("shut down http server: %w", err)
	}
	return nil
}
