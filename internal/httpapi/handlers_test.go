package httpapi

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/fl4metf/vm-harness/harness"
	"github.com/fl4metf/vm-harness/internal/memprovider"
	"github.com/fl4metf/vm-harness/vm"
)

func names(machines []vm.Machine) []string {
	out := make([]string, len(machines))
	for i, m := range machines {
		out[i] = m.Name
	}
	return out
}

func wantEmptyList(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	wantStatus(t, w, http.StatusOK)
	if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Fatalf("body = %s, want []", got)
	}
}

func wantNoContent(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	wantStatus(t, w, http.StatusNoContent)
	if w.Body.Len() != 0 {
		t.Fatalf("unexpected body: %s", w.Body)
	}
}

func TestHealthz(t *testing.T) {
	got := decode[map[string]string](t, newEnv(t).do(http.MethodGet, "/healthz", ""), http.StatusOK)
	if got["status"] != "ok" {
		t.Fatalf("health = %v", got)
	}
}

func TestProviders(t *testing.T) {
	got := decode[[]harness.ProviderStatus](t, newEnv(t).do(http.MethodGet, "/v1/providers", ""), http.StatusOK)
	if len(got) != 2 {
		t.Fatalf("providers = %+v", got)
	}
	if got[0].Name != vm.VirtualBox || !got[0].Default || !got[0].Available {
		t.Errorf("first provider = %+v", got[0])
	}
	if got[1].Name != vm.VMware || got[1].Default || !got[1].Available || slices.Contains(got[1].Info.Features, vm.FeaturePortForward) {
		t.Errorf("second provider = %+v", got[1])
	}
}

func TestListVMs(t *testing.T) {
	wantEmptyList(t, newEnv(t).do(http.MethodGet, "/v1/vms", ""))

	e := newEnv(t)
	e.vbox.Put(managed("web", vm.StateRunning))
	e.vbox.Put(vm.Machine{Name: "legacy"})
	e.vmw.Put(managed("db", vm.StateStopped))
	cases := []struct {
		target string
		want   []string
	}{
		{"/v1/vms", []string{"legacy", "web", "db"}},
		{"/v1/vms?managed=1", []string{"web", "db"}},
		{"/v1/vms?managed=false", []string{"legacy", "web", "db"}},
		{"/v1/vms?provider=vmware", []string{"db"}},
		{"/v1/vms?provider=virtualbox&managed=true", []string{"web"}},
	}
	for _, c := range cases {
		got := decode[[]vm.Machine](t, e.do(http.MethodGet, c.target, ""), http.StatusOK)
		if !slices.Equal(names(got), c.want) {
			t.Errorf("%s = %v, want %v", c.target, names(got), c.want)
		}
	}
	wantError(t, e.do(http.MethodGet, "/v1/vms?managed=maybe", ""), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodGet, "/v1/vms?provider=xen", ""), http.StatusBadRequest, "invalid_argument")
}

func TestCreateVM(t *testing.T) {
	e := newEnv(t)
	body := `{"name":"web","os_type":"ubuntu","cpus":1,"memory_mb":512,"labels":{"team":"red"}}`
	got := decode[vm.Machine](t, e.do(http.MethodPost, "/v1/vms", body), http.StatusCreated)
	want := vm.Machine{
		ID:       "virtualbox-1",
		Name:     "web",
		Provider: vm.VirtualBox,
		State:    vm.StateStopped,
		OSType:   "Ubuntu_64",
		CPUs:     1,
		MemoryMB: 512,
		Managed:  true,
		Labels:   map[string]string{"team": "red"},
		NICs:     []vm.NIC{{Mode: vm.NetNAT}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("created = %+v\nwant      %+v", got, want)
	}

	got = decode[vm.Machine](t, e.do(http.MethodPost, "/v1/vms?provider=vmware", `{"name":"db","start":true}`), http.StatusCreated)
	if got.Provider != vm.VMware || got.State != vm.StateRunning {
		t.Fatalf("created = %+v, want a running vmware vm", got)
	}

	wantError(t, e.do(http.MethodPost, "/v1/vms?provider=vmware", `{"name":"x","provider":"virtualbox"}`), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodPost, "/v1/vms", `{"name":"web"}`), http.StatusConflict, "already_exists")
	wantError(t, e.do(http.MethodPost, "/v1/vms", `{"name":"bad name"}`), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodPost, "/v1/vms", ""), http.StatusBadRequest, "invalid_argument")
}

func TestCreateVMLimit(t *testing.T) {
	e := newEnvWith(t, harness.Config{Limits: harness.Limits{MaxVMs: 1, MaxCPUs: 4}})
	wantError(t, e.do(http.MethodPost, "/v1/vms", `{"name":"big","cpus":8}`), http.StatusConflict, "limit_exceeded")
	wantStatus(t, e.do(http.MethodPost, "/v1/vms", `{"name":"a"}`), http.StatusCreated)
	wantError(t, e.do(http.MethodPost, "/v1/vms", `{"name":"b"}`), http.StatusConflict, "limit_exceeded")
}

func TestGetVM(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managed("web", vm.StateRunning))
	e.vbox.Put(managed("twin", vm.StateStopped))
	e.vmw.Put(managed("twin", vm.StateStopped))
	e.vmw.Put(vm.Machine{ID: "/vms/db/db.vmx", Name: "db"})
	e.vmw.Put(vm.Machine{ID: `E:\vms\my app\my app.vmx`, Name: "app"})
	cases := []struct {
		target   string
		name     string
		provider string
	}{
		{"/v1/vms/web", "web", vm.VirtualBox},
		{"/v1/vms/virtualbox-1", "web", vm.VirtualBox},
		{"/v1/vms/%2Fvms%2Fdb%2Fdb.vmx", "db", vm.VMware},
		{"/v1/vms/" + url.PathEscape(`E:\vms\my app\my app.vmx`), "app", vm.VMware},
		{"/v1/vms/twin?provider=vmware", "twin", vm.VMware},
		{"/v1/vms/twin?provider=virtualbox", "twin", vm.VirtualBox},
	}
	for _, c := range cases {
		got := decode[vm.Machine](t, e.do(http.MethodGet, c.target, ""), http.StatusOK)
		if got.Name != c.name || got.Provider != c.provider {
			t.Errorf("%s = %s/%s, want %s/%s", c.target, got.Provider, got.Name, c.provider, c.name)
		}
	}
	got := wantError(t, e.do(http.MethodGet, "/v1/vms/twin", ""), http.StatusBadRequest, "invalid_argument")
	if !strings.Contains(got.Message, "ambiguous") {
		t.Errorf("ambiguity message = %q", got.Message)
	}
	wantError(t, e.do(http.MethodGet, "/v1/vms/ghost", ""), http.StatusNotFound, "not_found")
	wantError(t, e.do(http.MethodGet, "/v1/vms/web?provider=xen", ""), http.StatusBadRequest, "invalid_argument")
}

func TestUpdateVM(t *testing.T) {
	e := newEnv(t)
	web := managed("web", vm.StateStopped)
	web.Meta[vm.LabelKey("team")] = "red"
	web.Meta[vm.LabelKey("tier")] = "front"
	e.vbox.Put(web)
	e.vbox.Put(managed("api", vm.StateRunning))

	body := `{"cpus":4,"memory_mb":4096,"labels":{"team":"blue","tier":""}}`
	got := decode[vm.Machine](t, e.do(http.MethodPatch, "/v1/vms/web", body), http.StatusOK)
	if got.CPUs != 4 || got.MemoryMB != 4096 || !reflect.DeepEqual(got.Labels, map[string]string{"team": "blue"}) {
		t.Fatalf("updated = %+v", got)
	}
	got = decode[vm.Machine](t, e.do(http.MethodPatch, "/v1/vms/api", `{"labels":{"team":"green"}}`), http.StatusOK)
	if got.Labels["team"] != "green" {
		t.Fatalf("labels of running vm = %v", got.Labels)
	}
	wantError(t, e.do(http.MethodPatch, "/v1/vms/api", `{"cpus":2}`), http.StatusConflict, "invalid_state")
	wantError(t, e.do(http.MethodPatch, "/v1/vms/web", `{"cpus":-1}`), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodPatch, "/v1/vms/web", `{"name":"other"}`), http.StatusBadRequest, "invalid_argument")
}

func TestDeleteVM(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managed("web", vm.StateRunning))
	wantError(t, e.do(http.MethodDelete, "/v1/vms/web", ""), http.StatusConflict, "invalid_state")
	wantError(t, e.do(http.MethodDelete, "/v1/vms/web?force=maybe", ""), http.StatusBadRequest, "invalid_argument")
	wantNoContent(t, e.do(http.MethodDelete, "/v1/vms/web?force=1", ""))
	wantError(t, e.do(http.MethodGet, "/v1/vms/web", ""), http.StatusNotFound, "not_found")
	wantError(t, e.do(http.MethodDelete, "/v1/vms/web", ""), http.StatusNotFound, "not_found")
}

func TestPowerActions(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managed("web", vm.StateStopped))
	steps := []struct {
		action string
		body   string
		want   vm.State
	}{
		{"start", `{"gui":true}`, vm.StateRunning},
		{"pause", "", vm.StatePaused},
		{"resume", "", vm.StateRunning},
		{"reset", "", vm.StateRunning},
		{"suspend", "", vm.StateSaved},
		{"start", "", vm.StateRunning},
		{"stop", `{"timeout_sec":5}`, vm.StateStopped},
		{"start", "{}", vm.StateRunning},
		{"stop", `{"force":true}`, vm.StateStopped},
	}
	for i, s := range steps {
		got := decode[vm.Machine](t, e.do(http.MethodPost, "/v1/vms/web/"+s.action, s.body), http.StatusOK)
		if got.State != s.want {
			t.Fatalf("step %d %s: state = %s, want %s", i, s.action, got.State, s.want)
		}
	}
	want := []string{"start", "pause", "resume", "reset", "suspend", "start", "stop", "start", "kill"}
	var ops []string
	for _, call := range e.vbox.Calls() {
		op, _, _ := strings.Cut(call, " ")
		ops = append(ops, op)
	}
	if !slices.Equal(ops, want) {
		t.Fatalf("provider calls = %v, want %v", ops, want)
	}
}

func TestStartPassesGUI(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managed("web", vm.StateStopped))
	wantStatus(t, e.do(http.MethodPost, "/v1/vms/web/start", `{"gui":true}`), http.StatusOK)
	if !e.vbox.gui {
		t.Fatal("gui flag did not reach the provider")
	}
}

func TestPowerErrors(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managed("web", vm.StateStopped))
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/pause", ""), http.StatusConflict, "invalid_state")
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/resume", ""), http.StatusConflict, "invalid_state")
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/stop", `{"timeout_sec":-1}`), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/stop", `{"forced":true}`), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/start", `{"gui":"yes"}`), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodPost, "/v1/vms/ghost/reset", ""), http.StatusNotFound, "not_found")
	wantError(t, e.do(http.MethodPost, "/v1/vms/ghost/stop", ""), http.StatusNotFound, "not_found")
}

func TestCloneVM(t *testing.T) {
	e := newEnv(t)
	web := managed("web", vm.StateStopped)
	web.Meta[vm.LabelKey("team")] = "red"
	e.vbox.Put(web)

	got := decode[vm.Machine](t, e.do(http.MethodPost, "/v1/vms/web/clone", `{"name":"web-2","linked":true}`), http.StatusCreated)
	if got.Name != "web-2" || got.Provider != vm.VirtualBox || !got.Managed || got.Labels["team"] != "red" {
		t.Fatalf("linked clone = %+v", got)
	}
	got = decode[vm.Machine](t, e.do(http.MethodPost, "/v1/vms/web/clone?provider=virtualbox", `{"name":"web-3"}`), http.StatusCreated)
	if got.Name != "web-3" || !got.Managed {
		t.Fatalf("full clone = %+v", got)
	}
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/clone", `{"name":"web-2"}`), http.StatusConflict, "already_exists")
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/clone", `{"name":"bad name"}`), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/clone", `{"name":"x","full":true}`), http.StatusBadRequest, "invalid_argument")
}

func TestSnapshots(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managed("web", vm.StateRunning))
	wantEmptyList(t, e.do(http.MethodGet, "/v1/vms/web/snapshots", ""))

	snap := decode[vm.Snapshot](t, e.do(http.MethodPost, "/v1/vms/web/snapshots", `{"name":"clean install","description":"fresh"}`), http.StatusCreated)
	if want := (vm.Snapshot{Name: "clean install", ID: "snap-1", Description: "fresh", Current: true}); snap != want {
		t.Fatalf("snapshot = %+v, want %+v", snap, want)
	}
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/snapshots", `{"name":"clean install"}`), http.StatusConflict, "already_exists")
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/snapshots", `{}`), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/snapshots", `{"name":"x","live":true}`), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodGet, "/v1/vms/ghost/snapshots", ""), http.StatusNotFound, "not_found")

	list := decode[[]vm.Snapshot](t, e.do(http.MethodGet, "/v1/vms/web/snapshots", ""), http.StatusOK)
	if len(list) != 1 || list[0].Name != "clean install" {
		t.Fatalf("snapshots = %+v", list)
	}

	mach := decode[vm.Machine](t, e.do(http.MethodPost, "/v1/vms/web/snapshots/clean%20install/restore", ""), http.StatusOK)
	if mach.CurrentSnapshot != "clean install" || mach.State != vm.StateStopped {
		t.Fatalf("restored = %+v", mach)
	}
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/snapshots/missing/restore", ""), http.StatusNotFound, "not_found")

	wantNoContent(t, e.do(http.MethodDelete, "/v1/vms/web/snapshots/clean%20install", ""))
	wantError(t, e.do(http.MethodDelete, "/v1/vms/web/snapshots/clean%20install", ""), http.StatusNotFound, "not_found")
	wantEmptyList(t, e.do(http.MethodGet, "/v1/vms/web/snapshots", ""))
}

func TestExec(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managed("web", vm.StateRunning))
	var got vm.ExecRequest
	e.vbox.ExecFunc = func(_ vm.Machine, req vm.ExecRequest) (vm.ExecResult, error) {
		got = req
		return vm.ExecResult{ExitCode: 3, Stdout: "<out>", Stderr: "err", DurationMS: 7}, nil
	}
	body := `{"command":["ls","-l"],"env":{"LANG":"C"},"workdir":"/tmp","timeout_sec":9,"user":"root","password":"pw","transport":"guest"}`
	w := e.do(http.MethodPost, "/v1/vms/web/exec", body)
	res := decode[vm.ExecResult](t, w, http.StatusOK)
	if want := (vm.ExecResult{ExitCode: 3, Stdout: "<out>", Stderr: "err", DurationMS: 7, Transport: harness.TransportGuest}); res != want {
		t.Fatalf("result = %+v, want %+v", res, want)
	}
	if !strings.Contains(w.Body.String(), `"<out>"`) {
		t.Errorf("output was html-escaped: %s", w.Body)
	}
	want := vm.ExecRequest{
		Command:     []string{"ls", "-l"},
		Env:         map[string]string{"LANG": "C"},
		WorkDir:     "/tmp",
		TimeoutSec:  9,
		Credentials: vm.Credentials{User: "root", Password: "pw"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("provider got %+v, want %+v", got, want)
	}

	wantError(t, e.do(http.MethodPost, "/v1/vms/web/exec", `{}`), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/exec", `{"script":"id","transport":"telnet"}`), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/exec", `{"command":["id"],"timeout":5}`), http.StatusBadRequest, "invalid_argument")

	got = vm.ExecRequest{}
	ssh := `{"command":["id"],"ssh":{"user":"vmh","password":"pw"}}`
	msg := wantError(t, e.do(http.MethodPost, "/v1/vms/web/exec", ssh), http.StatusServiceUnavailable, "not_ready")
	if !strings.Contains(msg.Message, "ssh") || got.Command != nil {
		t.Fatalf("ssh options did not route to ssh: %q, guest got %+v", msg.Message, got)
	}
}

func TestCopy(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managed("web", vm.StateRunning))
	dir := t.TempDir()
	src := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	body := jsonBody(t, map[string]string{"host_path": src, "guest_path": "/tmp/in.txt", "user": "root", "password": "pw"})
	wantNoContent(t, e.do(http.MethodPost, "/v1/vms/web/copy-to", body))
	if data, _ := e.vbox.GuestFile("web", "/tmp/in.txt"); string(data) != "hello" {
		t.Fatalf("guest file = %q", data)
	}
	want := vm.CopyRequest{HostPath: src, GuestPath: "/tmp/in.txt", Credentials: vm.Credentials{User: "root", Password: "pw"}}
	if e.vbox.copy != want {
		t.Fatalf("copy request = %+v, want %+v", e.vbox.copy, want)
	}

	dst := filepath.Join(dir, "out.txt")
	body = jsonBody(t, map[string]string{"host_path": dst, "guest_path": "/tmp/in.txt"})
	wantNoContent(t, e.do(http.MethodPost, "/v1/vms/web/copy-from", body))
	if data, err := os.ReadFile(dst); err != nil || string(data) != "hello" {
		t.Fatalf("host file = %q, %v", data, err)
	}

	body = jsonBody(t, map[string]string{"host_path": dst, "guest_path": "/tmp/missing"})
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/copy-from", body), http.StatusNotFound, "not_found")
	body = jsonBody(t, map[string]string{"host_path": src})
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/copy-to", body), http.StatusBadRequest, "invalid_argument")
	body = jsonBody(t, map[string]string{"host_path": filepath.Join(dir, "nope"), "guest_path": "/tmp/x"})
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/copy-to", body), http.StatusBadRequest, "invalid_argument")
	body = jsonBody(t, map[string]string{"host_path": src, "guest_path": "/tmp/x", "mode": "0644"})
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/copy-to", body), http.StatusBadRequest, "invalid_argument")
}

func TestFiles(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managed("web", vm.StateRunning))
	data := []byte("\x00binary\xff data\r\n")
	path := "/tmp/my file & more.txt"
	target := "/v1/vms/web/files?path=" + url.QueryEscape(path)

	put := request(http.MethodPut, target, bytes.NewReader(data))
	put.Header.Set("X-VMH-User", "root")
	put.Header.Set("X-VMH-Password", "pw")
	put.Header.Set("X-VMH-Transport", "guest")
	wantNoContent(t, e.send(put))
	if got, _ := e.vbox.GuestFile("web", path); !bytes.Equal(got, data) {
		t.Fatalf("guest file = %q, want %q", got, data)
	}
	if e.vbox.copy.GuestPath != path || e.vbox.copy.Credentials != (vm.Credentials{User: "root", Password: "pw"}) {
		t.Fatalf("copy request = %+v", e.vbox.copy)
	}

	get := request(http.MethodGet, target, nil)
	get.Header.Set("X-VMH-User", "admin")
	w := e.send(get)
	wantStatus(t, w, http.StatusOK)
	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("content type = %q", ct)
	}
	if !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("read back %q, want %q", w.Body.Bytes(), data)
	}
	if e.vbox.copy.GuestPath != path || e.vbox.copy.User != "admin" {
		t.Fatalf("copy request = %+v", e.vbox.copy)
	}

	empty := request(http.MethodPut, "/v1/vms/web/files?path=/tmp/empty", nil)
	wantNoContent(t, e.send(empty))
	if got, ok := e.vbox.GuestFile("web", "/tmp/empty"); !ok || len(got) != 0 {
		t.Fatalf("empty upload = %q, %v", got, ok)
	}

	wantError(t, e.do(http.MethodPut, "/v1/vms/web/files", "x"), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodGet, "/v1/vms/web/files", ""), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodGet, "/v1/vms/web/files?path=/nope", ""), http.StatusNotFound, "not_found")

	broken := request(http.MethodPut, "/v1/vms/web/files?path=/tmp/b", iotest.ErrReader(errors.New("connection reset")))
	wantError(t, e.send(broken), http.StatusBadRequest, "invalid_argument")

	pigeon := request(http.MethodPut, "/v1/vms/web/files?path=/tmp/p", strings.NewReader("x"))
	pigeon.Header.Set("X-VMH-Transport", "pigeon")
	wantError(t, e.send(pigeon), http.StatusBadRequest, "invalid_argument")

	ssh := request(http.MethodPut, "/v1/vms/web/files?path=/tmp/s", strings.NewReader("x"))
	ssh.Header.Set("X-VMH-SSH-User", "vmh")
	ssh.Header.Set("X-VMH-SSH-Password", "pw")
	wantError(t, e.send(ssh), http.StatusServiceUnavailable, "not_ready")
	if _, ok := e.vbox.GuestFile("web", "/tmp/s"); ok {
		t.Fatal("ssh headers were ignored, the file went through guest tools")
	}
}

func TestGuestIP(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managed("web", vm.StateRunning))
	e.vbox.SetIP("web", "10.0.2.15")
	e.vbox.Put(managed("booting", vm.StateRunning))
	e.vbox.Put(managed("off", vm.StateStopped))

	got := decode[map[string]string](t, e.do(http.MethodGet, "/v1/vms/web/ip", ""), http.StatusOK)
	if got["ip"] != "10.0.2.15" {
		t.Fatalf("ip = %v", got)
	}
	wantError(t, e.do(http.MethodGet, "/v1/vms/booting/ip", ""), http.StatusServiceUnavailable, "not_ready")
	wantError(t, e.do(http.MethodGet, "/v1/vms/off/ip", ""), http.StatusConflict, "invalid_state")
}

func TestWait(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managed("web", vm.StateRunning))
	e.vbox.SetIP("web", "10.0.2.15")

	res := decode[harness.WaitResult](t, e.do(http.MethodPost, "/v1/vms/web/wait", `{"for":"running"}`), http.StatusOK)
	if res.Machine.Name != "web" || res.Machine.State != vm.StateRunning {
		t.Fatalf("wait running = %+v", res)
	}
	res = decode[harness.WaitResult](t, e.do(http.MethodPost, "/v1/vms/web/wait", `{"for":"ip","timeout_sec":5}`), http.StatusOK)
	if res.IP != "10.0.2.15" {
		t.Fatalf("wait ip = %+v", res)
	}

	var cred vm.Credentials
	e.vbox.ExecFunc = func(_ vm.Machine, req vm.ExecRequest) (vm.ExecResult, error) {
		cred = req.Credentials
		return vm.ExecResult{}, nil
	}
	wantStatus(t, e.do(http.MethodPost, "/v1/vms/web/wait", `{"for":"guest","user":"root","password":"pw"}`), http.StatusOK)
	if cred != (vm.Credentials{User: "root", Password: "pw"}) {
		t.Fatalf("guest probe credentials = %+v", cred)
	}

	wantError(t, e.do(http.MethodPost, "/v1/vms/web/wait", `{"for":"teatime"}`), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/wait", `{"for":"running","timeout_sec":-1}`), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/wait", `{"for":"running","interval_sec":1}`), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodPost, "/v1/vms/ghost/wait", `{"for":"running"}`), http.StatusNotFound, "not_found")
}

func TestWaitTimeout(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.vbox.Put(managed("off", vm.StateStopped))
	got := wantError(t, e.do(http.MethodPost, "/v1/vms/off/wait", `{"for":"running","timeout_sec":1}`), http.StatusGatewayTimeout, "timeout")
	if !strings.Contains(got.Message, "1s") {
		t.Fatalf("message does not mention the timeout: %q", got.Message)
	}
}

func TestScreenshot(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managed("web", vm.StateRunning))
	e.vbox.Put(managed("off", vm.StateStopped))

	r := request(http.MethodGet, "/v1/vms/web/screenshot", nil)
	r.Header.Set("X-VMH-User", "admin")
	r.Header.Set("X-VMH-Password", "pw")
	w := e.send(r)
	wantStatus(t, w, http.StatusOK)
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content type = %q", ct)
	}
	if !bytes.Equal(w.Body.Bytes(), memprovider.PNG) {
		t.Fatalf("body = %q", w.Body.Bytes())
	}
	if e.vbox.cred != (vm.Credentials{User: "admin", Password: "pw"}) {
		t.Fatalf("credentials = %+v", e.vbox.cred)
	}
	wantError(t, e.do(http.MethodGet, "/v1/vms/off/screenshot", ""), http.StatusConflict, "invalid_state")
}

func TestPortForwards(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managed("web", vm.StateStopped))
	e.vmw.Put(managed("db", vm.StateStopped))
	wantEmptyList(t, e.do(http.MethodGet, "/v1/vms/web/ports", ""))

	ssh := decode[vm.PortForward](t, e.do(http.MethodPost, "/v1/vms/web/ports", `{"guest_port":22}`), http.StatusCreated)
	if ssh.Name != "tcp-22" || ssh.Protocol != "tcp" || ssh.HostIP != "127.0.0.1" || ssh.HostPort == 0 || ssh.GuestPort != 22 {
		t.Fatalf("forward = %+v", ssh)
	}
	httpFwd := vm.PortForward{Name: "http", Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 8080, GuestPort: 80}
	got := decode[vm.PortForward](t, e.do(http.MethodPost, "/v1/vms/web/ports", `{"name":"http","host_port":8080,"guest_port":80}`), http.StatusCreated)
	if got != httpFwd {
		t.Fatalf("forward = %+v, want %+v", got, httpFwd)
	}
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/ports", `{"name":"http","guest_port":81}`), http.StatusConflict, "already_exists")
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/ports", `{"guest_port":0}`), http.StatusBadRequest, "invalid_argument")
	wantError(t, e.do(http.MethodPost, "/v1/vms/web/ports", `{"guest_port":22,"proto":"udp"}`), http.StatusBadRequest, "invalid_argument")

	list := decode[[]vm.PortForward](t, e.do(http.MethodGet, "/v1/vms/web/ports", ""), http.StatusOK)
	if want := []vm.PortForward{ssh, httpFwd}; !slices.Equal(list, want) {
		t.Fatalf("forwards = %+v, want %+v", list, want)
	}
	wantNoContent(t, e.do(http.MethodDelete, "/v1/vms/web/ports/tcp-22", ""))
	list = decode[[]vm.PortForward](t, e.do(http.MethodGet, "/v1/vms/web/ports", ""), http.StatusOK)
	if want := []vm.PortForward{httpFwd}; !slices.Equal(list, want) {
		t.Fatalf("forwards = %+v, want %+v", list, want)
	}
	wantError(t, e.do(http.MethodDelete, "/v1/vms/web/ports/tcp-22", ""), http.StatusNotFound, "not_found")
	wantError(t, e.do(http.MethodGet, "/v1/vms/ghost/ports", ""), http.StatusNotFound, "not_found")
	wantError(t, e.do(http.MethodPost, "/v1/vms/db/ports?provider=vmware", `{"guest_port":22}`), http.StatusNotImplemented, "unsupported")
}
