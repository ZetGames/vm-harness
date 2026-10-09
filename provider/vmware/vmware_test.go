package vmware

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ZetGames/vm-harness/internal/fakerun"
	"github.com/ZetGames/vm-harness/runner"
	"github.com/ZetGames/vm-harness/vm"
)

const exitFailed = 255

func TestMain(m *testing.M) {
	if os.Getenv("VMH_ECHO_ARGS") == "1" {
		wd, _ := os.Getwd()
		json.NewEncoder(os.Stdout).Encode(append([]string{wd, os.Getenv("VMH_PROBE")}, os.Args[1:]...))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type testEnv struct {
	*Provider
	fake *fakerun.Fake
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	fake := fakerun.New()
	fake.On("-T ws list", "Total running VMs: 0\r\n")
	root := filepath.Join(t.TempDir(), "vms")
	p := New(Options{
		Vmrun:        "vmrun",
		VDiskManager: "vmware-vdiskmanager",
		OVFTool:      "ovftool",
		HostType:     "ws",
		Root:         root,
		Runner:       fake,
	})
	p.inventory = filepath.Join(t.TempDir(), "inventory.vmls")
	p.leases = nil
	return &testEnv{Provider: p, fake: fake}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func failed(stdout string) runner.Result {
	return runner.Result{ExitCode: exitFailed, Stdout: []byte(stdout)}
}

func (e *testEnv) addVM(t *testing.T, name, content string) string {
	t.Helper()
	dir := filepath.Join(e.root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".vmx")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func (e *testEnv) setRunning(paths ...string) {
	out := "Total running VMs: " + strconv.Itoa(len(paths)) + "\r\n"
	for _, path := range paths {
		out += path + "\r\n"
	}
	e.fake.On("-T ws list", out)
}

func (e *testEnv) commands() []string {
	var lines []string
	for _, c := range e.fake.Calls() {
		if c.String() != "-T ws list" {
			lines = append(lines, c.String())
		}
	}
	return lines
}

func wantKind(t *testing.T, err, kind error) {
	t.Helper()
	if !errors.Is(err, kind) {
		t.Fatalf("err = %v, want %v", err, kind)
	}
}

func simpleVMX(name string) string {
	return ".encoding = \"UTF-8\"\r\ndisplayName = \"" + name + "\"\r\nguestOS = \"ubuntu-64\"\r\nnumvcpus = \"2\"\r\nmemsize = \"1024\"\r\n"
}

func storedMeta(t *testing.T, path string) map[string]string {
	t.Helper()
	values, err := readMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	return values
}

func TestInfo(t *testing.T) {
	e := newTestEnv(t)
	e.fake.OnFunc("", func(c fakerun.Call) (runner.Result, error) {
		if len(c.Args) != 0 {
			t.Errorf("unexpected args %v", c.Args)
		}
		return failed(fixture(t, "vmrun/help.txt")), nil
	})
	info, err := e.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Provider != vm.VMware || info.Version != "1.17.0 build-24583834" || info.Root != e.root || info.Binary != "vmrun" {
		t.Fatalf("info = %+v", info)
	}
	for _, f := range []string{vm.FeatureSnapshots, vm.FeatureLinkedClone, vm.FeatureAppliance, vm.FeatureGuestExec} {
		if !slices.Contains(info.Features, f) {
			t.Errorf("missing feature %s", f)
		}
	}
	for _, f := range []string{vm.FeaturePortForward, vm.FeatureUnattended} {
		if slices.Contains(info.Features, f) {
			t.Errorf("unexpected feature %s", f)
		}
	}
	e.ovftool = ""
	e.hostType = "player"
	for _, f := range e.features() {
		if f == vm.FeatureAppliance || f == vm.FeatureSnapshots || f == vm.FeatureLinkedClone {
			t.Errorf("player without ovftool should not report %s", f)
		}
	}
}

func TestUnavailableVmrun(t *testing.T) {
	e := newTestEnv(t)
	e.vmrun = ""
	ctx := context.Background()
	calls := map[string]func() error{
		"Info": func() error { _, err := e.Info(ctx); return err },
		"List": func() error { _, err := e.List(ctx); return err },
		"Get":  func() error { _, err := e.Get(ctx, "web"); return err },
		"Create": func() error {
			_, err := e.Create(ctx, vm.Spec{Name: "web", CPUs: 1, MemoryMB: 64, DiskGB: 1})
			return err
		},
		"Delete":      func() error { return e.Delete(ctx, "web") },
		"Update":      func() error { return e.Update(ctx, "web", vm.Changes{CPUs: 2}) },
		"SetMeta":     func() error { return e.SetMeta(ctx, "web", map[string]string{"a": "b"}) },
		"Clone":       func() error { _, err := e.Clone(ctx, "web", vm.CloneOptions{Name: "copy"}); return err },
		"Start":       func() error { return e.Start(ctx, "web", false) },
		"Stop":        func() error { return e.Stop(ctx, "web", true) },
		"Pause":       func() error { return e.Pause(ctx, "web") },
		"Resume":      func() error { return e.Resume(ctx, "web") },
		"Reset":       func() error { return e.Reset(ctx, "web") },
		"Suspend":     func() error { return e.Suspend(ctx, "web") },
		"Snapshots":   func() error { _, err := e.Snapshots(ctx, "web"); return err },
		"Take":        func() error { return e.TakeSnapshot(ctx, "web", "s", "") },
		"Restore":     func() error { return e.RestoreSnapshot(ctx, "web", "s") },
		"DelSnapshot": func() error { return e.DeleteSnapshot(ctx, "web", "s") },
		"GuestIP":     func() error { _, err := e.GuestIP(ctx, "web"); return err },
		"Exec": func() error {
			_, err := e.Exec(ctx, "web", vm.ExecRequest{Command: []string{"true"}, Credentials: vm.Credentials{User: "u"}})
			return err
		},
		"CopyTo":     func() error { return e.CopyTo(ctx, "web", vm.CopyRequest{Credentials: vm.Credentials{User: "u"}}) },
		"CopyFrom":   func() error { return e.CopyFrom(ctx, "web", vm.CopyRequest{Credentials: vm.Credentials{User: "u"}}) },
		"Screenshot": func() error { _, err := e.Screenshot(ctx, "web", vm.Credentials{User: "u"}); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, vm.ErrUnavailable) {
			t.Errorf("%s: err = %v, want unavailable", name, err)
		}
	}
	if len(e.fake.Calls()) != 0 {
		t.Fatalf("nothing should run without vmrun: %v", e.fake.Calls())
	}
}

func TestMissingBinaryIsUnavailable(t *testing.T) {
	e := newTestEnv(t)
	e.fake.OnError("-T ws list", &exec.Error{Name: "vmrun", Err: exec.ErrNotFound})
	_, err := e.List(context.Background())
	wantKind(t, err, vm.ErrUnavailable)
}

func TestMissingDiskManager(t *testing.T) {
	e := newTestEnv(t)
	e.vdiskmanager = ""
	_, err := e.Create(context.Background(), vm.Spec{Name: "web", CPUs: 1, MemoryMB: 64, DiskGB: 1})
	wantKind(t, err, vm.ErrUnavailable)
}

func TestFindTool(t *testing.T) {
	t.Setenv("PATH", "")
	dir := t.TempDir()
	exe := func(name string) string {
		if runtime.GOOS == "windows" {
			return name + ".exe"
		}
		return name
	}
	vmrun := filepath.Join(dir, exe("vmrun"))
	ovftool := filepath.Join(dir, "OVFTool", exe("ovftool"))
	for _, path := range []string{vmrun, ovftool} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dirs := []string{filepath.Join(dir, "missing"), dir}
	if got := findTool("vmrun", dirs); got != vmrun {
		t.Errorf("vmrun = %q", got)
	}
	if got := findTool("ovftool", dirs); got != ovftool {
		t.Errorf("ovftool = %q", got)
	}
	if got := findTool("vmware-vdiskmanager", dirs); got != "" {
		t.Errorf("vdiskmanager = %q", got)
	}
}

func TestClassifyRealOutputs(t *testing.T) {
	cases := map[string]error{
		"vmrun/cannot-be-found.txt":         vm.ErrNotFound,
		"vmrun/snapshot-missing.txt":        vm.ErrNotFound,
		"vmrun/clone-invalid-snapshot.txt":  vm.ErrNotFound,
		"vmrun/snapshot-exists.txt":         vm.ErrExists,
		"vmrun/not-powered-on.txt":          vm.ErrInvalidState,
		"vmrun/already-running.txt":         vm.ErrInvalidState,
		"vmrun/paused.txt":                  vm.ErrInvalidState,
		"vmrun/clone-state-unchanged.txt":   vm.ErrInvalidState,
		"vmrun/tools-not-running.txt":       vm.ErrNotReady,
		"vmrun/anonymous-guest.txt":         vm.ErrInvalid,
		"vmrun/cannot-read-config.txt":      vm.ErrInvalid,
		"vmrun/not-supported.txt":           vm.ErrUnsupported,
		"vdiskmanager/create-exists.txt":    vm.ErrExists,
		"vdiskmanager/convert-not-disk.txt": vm.ErrInvalid,
		"ovftool/import-exists.txt":         vm.ErrExists,
		"vdiskmanager/expand-smaller.txt":   nil,
		"vmrun/checktoolsstate-unknown.txt": nil,
		"vmrun/listsnapshots-tree.txt":      nil,
		"vdiskmanager/convert.txt":          nil,
		"ovftool/import.txt":                nil,
		"vmrun/list-running.txt":            nil,
		"vmrun/listsnapshots-flat.txt":      nil,
		"vmrun/listsnapshots-empty.txt":     nil,
		"vdiskmanager/create.txt":           nil,
		"vdiskmanager/expand.txt":           nil,
		"ovftool/version.txt":               nil,
		"vmrun/list-empty.txt":              nil,
		"vmrun/help.txt":                    nil,
	}
	for name, want := range cases {
		if got := classify(normalize([]byte(fixture(t, name)))); got != want {
			t.Errorf("%s: kind = %v, want %v", name, got, want)
		}
	}
}

func TestCommandErrorHidesGuestPassword(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web"))
	e.fake.On("-T ws checkToolsState", "running\r\n")
	e.fake.OnResult("-T ws -gu admin -gp s3cret CopyFileFromHostToGuest", failed("Error: Invalid user name or password for the guest OS\r\n"))
	err := e.CopyTo(context.Background(), "web", vm.CopyRequest{HostPath: "a", GuestPath: "/b", Credentials: vm.Credentials{User: "admin", Password: "s3cret"}})
	wantKind(t, err, vm.ErrInvalid)
	var cmdErr *vm.CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("err = %T", err)
	}
	if strings.Contains(err.Error(), "s3cret") || slices.Contains(cmdErr.Args, "s3cret") {
		t.Fatalf("password leaked: %v %v", err, cmdErr.Args)
	}
	if cmdErr.Args[0] != "CopyFileFromHostToGuest" || cmdErr.Args[1] != path {
		t.Fatalf("args = %v", cmdErr.Args)
	}
}

func TestParseList(t *testing.T) {
	got := parseList(normalize([]byte(fixture(t, "vmrun/list-running.txt"))))
	if len(got) != 1 || got[0] != `C:\vms\web\web.vmx` {
		t.Fatalf("got %q", got)
	}
	if got := parseList(normalize([]byte(fixture(t, "vmrun/list-empty.txt")))); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}
