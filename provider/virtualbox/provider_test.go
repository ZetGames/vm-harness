package virtualbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fl4metf/vm-harness/internal/fakerun"
	"github.com/fl4metf/vm-harness/runner"
	"github.com/fl4metf/vm-harness/vm"
)

const (
	demoID   = "c22863c5-d853-4cc1-be2f-7b70cba6e7b6"
	linkedID = "55a44c04-5891-48ee-a92c-14a94777132d"
)

func newFake(t *testing.T) *fakerun.Fake {
	return fakerun.New().On("list ostypes", fixture(t, "list-ostypes.txt"))
}

func newProvider(t *testing.T, f *fakerun.Fake) *Provider {
	t.Helper()
	p := New(Options{VBoxManage: "VBoxManage", Root: t.TempDir(), Runner: f})
	p.retryWait = time.Millisecond
	return p
}

func commands(f *fakerun.Fake) []string {
	var out []string
	for _, c := range f.Calls() {
		if s := c.String(); s != "list ostypes" {
			out = append(out, s)
		}
	}
	return out
}

func failure(t *testing.T, name string) runner.Result {
	return runner.Result{ExitCode: 1, Stderr: []byte(fixture(t, name))}
}

func esc(path string) string { return strings.ReplaceAll(path, `\`, `\\`) }

func vmInfoText(lines ...string) string { return strings.Join(lines, "\r\n") + "\r\n" }

func TestGet(t *testing.T) {
	ssh := vm.PortForward{Name: "ssh", Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 2222, GuestPort: 22}
	udp := vm.PortForward{Name: "udp_5353_53", Protocol: "udp", HostPort: 5353, GuestIP: "10.0.2.15", GuestPort: 53}
	persist := vm.PortForward{Name: "persist", Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 2240, GuestPort: 22}
	nat := []vm.NIC{{Mode: vm.NetNAT, Model: "82540EM", MAC: "08:00:27:00:00:01"}}
	cases := []struct {
		fixture string
		want    vm.Machine
	}{
		{"showvminfo-stopped.txt", vm.Machine{
			ID: demoID, Name: "demo-1", State: vm.StateStopped, ConfigPath: `C:\vms\demo-1\demo-1.vbox`,
			NICs: nat, PortForwards: []vm.PortForward{ssh, udp},
		}},
		{"showvminfo-running.txt", vm.Machine{
			ID: demoID, Name: "demo-1", State: vm.StateRunning, ConfigPath: `C:\vms\demo-1\demo-1.vbox`,
			NICs: nat, PortForwards: []vm.PortForward{ssh, udp},
		}},
		{"showvminfo-paused.txt", vm.Machine{}},
		{"showvminfo-saved.txt", vm.Machine{}},
		{"showvminfo-aborted.txt", vm.Machine{}},
		{"showvminfo-linked.txt", vm.Machine{
			ID: linkedID, Name: "demo-linked", State: vm.StateStopped, ConfigPath: `C:\vms\demo-linked\demo-linked.vbox`,
			NICs: nat, PortForwards: []vm.PortForward{persist, ssh, udp},
		}},
		{"showvminfo-snapshots.txt", vm.Machine{
			ID: demoID, Name: "demo-1", State: vm.StateStopped, ConfigPath: `C:\vms\demo-1\demo-1.vbox`,
			NICs: nat, PortForwards: []vm.PortForward{persist, ssh, udp}, CurrentSnapshot: "s3",
		}},
	}
	states := map[string]vm.State{
		"showvminfo-paused.txt":  vm.StatePaused,
		"showvminfo-saved.txt":   vm.StateSaved,
		"showvminfo-aborted.txt": vm.StateStopped,
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			id := parseVMInfo(fixture(t, c.fixture)).id()
			f := newFake(t).
				On("showvminfo x ", fixture(t, c.fixture)).
				On("getextradata", "Key: vmh/managed, Value: "+id+"\r\nKey: vmh/label.team, Value: red\r\nKey: other, Value: x\r\n")
			m, err := newProvider(t, f).Get(context.Background(), "x")
			if err != nil {
				t.Fatal(err)
			}
			if want, ok := states[c.fixture]; ok {
				if m.State != want {
					t.Fatalf("state = %q, want %q", m.State, want)
				}
				return
			}
			want := c.want
			want.Provider = vm.VirtualBox
			want.OSType = "Ubuntu_64"
			want.CPUs = 2
			want.MemoryMB = 512
			want.Firmware = "efi"
			want.Managed = true
			want.Labels = map[string]string{"team": "red"}
			want.Meta = map[string]string{"managed": want.ID, "label.team": "red"}
			if !reflect.DeepEqual(m, want) {
				t.Fatalf("machine =\n%+v\nwant\n%+v", m, want)
			}
			if !f.Called("getextradata " + want.ID + " enumerate") {
				t.Fatal("extradata must be read by uuid")
			}
		})
	}
}

func TestGetNICs(t *testing.T) {
	f := newFake(t).On("showvminfo", fixture(t, "showvminfo-nics.txt")).On("getextradata", "")
	m, err := newProvider(t, f).Get(context.Background(), "demo-src")
	if err != nil {
		t.Fatal(err)
	}
	want := []vm.NIC{
		{Mode: vm.NetNAT, Model: "virtio", MAC: "08:00:27:00:00:01"},
		{Mode: vm.NetNATNetwork, Adapter: "vmhtestnet", Model: "82540EM", MAC: "08:00:27:00:00:02"},
		{Mode: vm.NetBridged, Adapter: "Dummy Adapter X", Model: "82540EM", MAC: "08:00:27:00:00:03"},
		{Mode: vm.NetInternal, Adapter: "vmhintnet", Model: "82540EM", MAC: "08:00:27:00:00:04"},
		{Mode: vm.NetHostOnly, Adapter: "Dummy HostOnly", Model: "82540EM", MAC: "08:00:27:00:00:05"},
	}
	if !reflect.DeepEqual(m.NICs, want) {
		t.Fatalf("nics = %+v", m.NICs)
	}
	if m.Firmware != "bios" || m.Managed || m.Labels != nil {
		t.Fatalf("machine = %+v", m)
	}
}

func TestGetNotFound(t *testing.T) {
	f := newFake(t).OnResult("showvminfo", failure(t, "err-not-found.txt"))
	_, err := newProvider(t, f).Get(context.Background(), "nope")
	if !errors.Is(err, vm.ErrNotFound) || vm.Code(err) != "not_found" {
		t.Fatalf("err = %v", err)
	}
	var cmdErr *vm.CommandError
	if !errors.As(err, &cmdErr) || cmdErr.Path != "VBoxManage" || cmdErr.Args[0] != "showvminfo" || cmdErr.ExitCode != 1 {
		t.Fatalf("command error = %+v", cmdErr)
	}
	if len(f.Calls()) != 1 {
		t.Fatalf("not found must not be retried: %v", commands(f))
	}
}

func TestBadReference(t *testing.T) {
	f := newFake(t)
	p := newProvider(t, f)
	for _, ref := range []string{"", "--delete-all"} {
		if _, err := p.Get(context.Background(), ref); !errors.Is(err, vm.ErrInvalid) {
			t.Errorf("Get(%q) err = %v", ref, err)
		}
		if err := p.Start(context.Background(), ref, false); !errors.Is(err, vm.ErrInvalid) {
			t.Errorf("Start(%q) err = %v", ref, err)
		}
	}
	if len(f.Calls()) != 0 {
		t.Fatalf("calls = %v", commands(f))
	}
}

func TestList(t *testing.T) {
	f := newFake(t).
		On("list vms", fixture(t, "list-vms.txt")).
		On("showvminfo "+demoID, fixture(t, "showvminfo-stopped.txt")).
		OnResult("showvminfo 0b3e4233", failure(t, "err-not-found-uuid.txt")).
		On("showvminfo "+linkedID, fixture(t, "showvminfo-linked.txt")).
		On("getextradata", "")
	machines, err := newProvider(t, f).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(machines) != 2 || machines[0].Name != "demo-1" || machines[1].Name != "demo-linked" {
		t.Fatalf("machines = %+v", machines)
	}
}

func TestListKeepsVMsThatFailToLoad(t *testing.T) {
	f := newFake(t).
		On("list vms", fixture(t, "list-vms.txt")).
		On("showvminfo", fixture(t, "showvminfo-stopped.txt")).
		OnError("getextradata", errors.New("boom"))
	machines, err := newProvider(t, f).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []vm.Machine{
		{ID: demoID, Name: "demo-1", Provider: vm.VirtualBox, State: vm.StateUnknown},
		{ID: "0b3e4233-7102-4087-bbd8-2ea3a0f123a4", Name: "demo-2", Provider: vm.VirtualBox, State: vm.StateUnknown},
		{ID: linkedID, Name: "demo-linked", Provider: vm.VirtualBox, State: vm.StateUnknown},
	}
	if !reflect.DeepEqual(machines, want) {
		t.Fatalf("machines = %+v", machines)
	}
}

func TestListSkipsLoadingInaccessibleVMs(t *testing.T) {
	const ghost = "11111111-2222-3333-4444-555555555555"
	limited := runner.Result{ExitCode: 1, Stderr: []byte("VBoxManage.exe: error: The object functionality is limited\r\n" +
		"VBoxManage.exe: error: Details: code E_ACCESSDENIED (0x80070005), component MachineWrap, interface IMachine, callee IUnknown\r\n")}
	f := newFake(t).
		On("list vms", `"demo-1" {`+demoID+"}\r\n\"<inaccessible>\" {"+ghost+"}\r\n").
		On("showvminfo "+demoID, fixture(t, "showvminfo-stopped.txt")).
		OnResult("showvminfo "+ghost, limited).
		OnResult("getextradata "+ghost, limited).
		On("getextradata "+demoID, "")
	p := newProvider(t, f)
	machines, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(machines) != 2 || machines[0].ID != demoID || machines[0].State != vm.StateStopped ||
		machines[1].ID != ghost || machines[1].State != vm.StateUnknown || machines[1].Managed {
		t.Fatalf("machines = %+v", machines)
	}
	if f.Called("showvminfo "+ghost) || f.Called("getextradata "+ghost) {
		t.Fatalf("inaccessible vm was loaded: %q", commands(f))
	}

	if _, err := p.Get(context.Background(), ghost); !errors.Is(err, vm.ErrInvalidState) {
		t.Fatalf("get inaccessible: err = %v", err)
	}
	if got := len(f.Find("showvminfo " + ghost)); got != 1 {
		t.Fatalf("limited object retried %d times", got)
	}
}

func TestRetryOnTransientLock(t *testing.T) {
	f := newFake(t).
		OnResult("showvminfo", failure(t, "err-access-denied.txt"), failure(t, "err-locked.txt"), runner.Result{Stdout: []byte(fixture(t, "showvminfo-stopped.txt"))}).
		On("getextradata", "")
	m, err := newProvider(t, f).Get(context.Background(), demoID)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != demoID || len(f.Find("showvminfo")) != 3 {
		t.Fatalf("machine %+v after %d attempts", m, len(f.Find("showvminfo")))
	}
}

func TestRetryIsBounded(t *testing.T) {
	f := newFake(t).OnResult("modifyvm", failure(t, "err-locked.txt"))
	_, err := newProvider(t, f).run(context.Background(), "modifyvm", demoID, "--memory", "256")
	if !errors.Is(err, vm.ErrInvalidState) {
		t.Fatalf("err = %v", err)
	}
	if got := len(f.Find("modifyvm")); got != maxAttempts {
		t.Fatalf("attempts = %d", got)
	}
}

func TestRetryHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := newFake(t).OnFunc("startvm", func(fakerun.Call) (runner.Result, error) {
		cancel()
		return failure(t, "err-already-running.txt"), nil
	})
	p := newProvider(t, f)
	p.retryWait = time.Minute
	if err := p.Start(ctx, demoID, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestUnavailableBinary(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("VBOX_MSI_INSTALL_PATH", "")
	t.Setenv("VBOX_INSTALL_PATH", "")
	saved := installPaths
	installPaths = nil
	t.Cleanup(func() { installPaths = saved })

	f := newFake(t)
	p := New(Options{Root: t.TempDir(), Runner: f})
	ctx := context.Background()
	if _, err := p.Info(ctx); !errors.Is(err, vm.ErrUnavailable) {
		t.Errorf("Info err = %v", err)
	}
	if _, err := p.List(ctx); !errors.Is(err, vm.ErrUnavailable) {
		t.Errorf("List err = %v", err)
	}
	if _, err := p.Get(ctx, "x"); !errors.Is(err, vm.ErrUnavailable) {
		t.Errorf("Get err = %v", err)
	}
	if _, err := p.Create(ctx, vm.Spec{Name: "x", DiskGB: 1}); !errors.Is(err, vm.ErrUnavailable) {
		t.Errorf("Create err = %v", err)
	}
	if err := p.Stop(ctx, "x", true); !errors.Is(err, vm.ErrUnavailable) {
		t.Errorf("Stop err = %v", err)
	}
	if len(f.Calls()) != 0 {
		t.Fatalf("runner was called: %v", commands(f))
	}
}

func TestMissingConfiguredBinary(t *testing.T) {
	p := New(Options{VBoxManage: filepath.Join(t.TempDir(), "VBoxManage.exe")})
	if _, err := p.Info(context.Background()); !errors.Is(err, vm.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestDiscoverFromInstallPathEnv(t *testing.T) {
	dir := t.TempDir()
	exe := "VBoxManage"
	if filepath.Separator == '\\' {
		exe += ".exe"
	}
	bin := filepath.Join(dir, exe)
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	t.Setenv("VBOX_MSI_INSTALL_PATH", dir+string(filepath.Separator))
	if got := discover(); got != bin {
		t.Fatalf("discover = %q, want %q", got, bin)
	}
}

func TestInfo(t *testing.T) {
	f := newFake(t).On("--version", fixture(t, "version.txt"))
	p := newProvider(t, f)
	info, err := p.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != vm.VirtualBox || info.Provider != vm.VirtualBox || info.Version != "7.2.16r174877" || info.Root != p.root || len(info.Features) != 11 {
		t.Fatalf("info = %+v", info)
	}

	f = newFake(t).On("--version", "WARNING: The vboxdrv kernel module is not loaded.\n         Run sudo /sbin/vboxconfig\n7.0.10r158379\n")
	if info, err := newProvider(t, f).Info(context.Background()); err != nil || info.Version != "7.0.10r158379" {
		t.Fatalf("info = %+v, err = %v", info, err)
	}
}

func TestInfoRejectsOldVirtualBox(t *testing.T) {
	for _, version := range []string{"6.1.50_Ubuntur161033\n", "garbage\n"} {
		f := newFake(t).On("--version", version)
		info, err := newProvider(t, f).Info(context.Background())
		if vm.Code(err) != vm.CodeUnavailable || !strings.Contains(err.Error(), "7.0 or newer") {
			t.Fatalf("%q: err = %v", version, err)
		}
		if info.Version != strings.TrimSpace(version) {
			t.Fatalf("%q: info = %+v", version, info)
		}
	}
}

func TestPowerCommands(t *testing.T) {
	cases := []struct {
		do   func(p *Provider) error
		want string
	}{
		{func(p *Provider) error { return p.Start(context.Background(), demoID, false) }, "startvm " + demoID + " --type headless"},
		{func(p *Provider) error { return p.Start(context.Background(), demoID, true) }, "startvm " + demoID + " --type gui"},
		{func(p *Provider) error { return p.Stop(context.Background(), demoID, false) }, "controlvm " + demoID + " acpipowerbutton"},
		{func(p *Provider) error { return p.Pause(context.Background(), demoID) }, "controlvm " + demoID + " pause"},
		{func(p *Provider) error { return p.Resume(context.Background(), demoID) }, "controlvm " + demoID + " resume"},
		{func(p *Provider) error { return p.Reset(context.Background(), demoID) }, "controlvm " + demoID + " reset"},
		{func(p *Provider) error { return p.Suspend(context.Background(), demoID) }, "controlvm " + demoID + " savestate"},
	}
	for _, c := range cases {
		f := newFake(t).On("startvm", fixture(t, "startvm.txt")).On("controlvm", "")
		if err := c.do(newProvider(t, f)); err != nil {
			t.Fatalf("%s: %v", c.want, err)
		}
		if got := commands(f); len(got) != 1 || got[0] != c.want {
			t.Errorf("calls = %q, want %q", got, c.want)
		}
	}
}

func TestForceStop(t *testing.T) {
	running := fixture(t, "showvminfo-running.txt")
	cases := []struct {
		name, info, want string
	}{
		{"running", running, "controlvm " + demoID + " poweroff"},
		{"paused", fixture(t, "showvminfo-paused.txt"), "controlvm " + demoID + " poweroff"},
		{"guru meditation", strings.Replace(running, `VMState="running"`, `VMState="gurumeditation"`, 1), "controlvm " + demoID + " poweroff"},
		{"busy", strings.Replace(running, `VMState="running"`, `VMState="stopping"`, 1), "controlvm " + demoID + " poweroff"},
		{"saved", fixture(t, "showvminfo-saved.txt"), "discardstate " + demoID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t).On("showvminfo", c.info).On("controlvm", "").On("discardstate", "")
			if err := newProvider(t, f).Stop(context.Background(), "demo-1", true); err != nil {
				t.Fatal(err)
			}
			if got := commands(f); len(got) != 2 || got[1] != c.want {
				t.Fatalf("calls = %q, want %q", got, c.want)
			}
		})
	}
}

func TestStopWhenNotRunning(t *testing.T) {
	f := newFake(t).On("showvminfo", fixture(t, "showvminfo-stopped.txt")).OnResult("controlvm", failure(t, "err-not-running.txt"))
	if err := newProvider(t, f).Stop(context.Background(), demoID, true); !errors.Is(err, vm.ErrInvalidState) {
		t.Fatalf("err = %v", err)
	}
}

func TestSnapshots(t *testing.T) {
	f := newFake(t).OnResult("snapshot", runner.Result{ExitCode: 1, Stdout: []byte(fixture(t, "snapshot-list-none.txt"))})
	snaps, err := newProvider(t, f).Snapshots(context.Background(), demoID)
	if err != nil || snaps == nil || len(snaps) != 0 {
		t.Fatalf("snapshots = %v, err = %v", snaps, err)
	}

	f = newFake(t).On("snapshot", fixture(t, "snapshot-list-branched.txt"))
	snaps, err = newProvider(t, f).Snapshots(context.Background(), demoID)
	if err != nil || len(snaps) != 3 || !snaps[2].Current {
		t.Fatalf("snapshots = %+v, err = %v", snaps, err)
	}
	if got := commands(f)[0]; got != "snapshot "+demoID+" list --machinereadable" {
		t.Fatalf("call = %q", got)
	}

	f = newFake(t).OnResult("snapshot", failure(t, "err-not-found.txt"))
	if _, err := newProvider(t, f).Snapshots(context.Background(), "nope"); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestTakeSnapshot(t *testing.T) {
	cases := []struct {
		name, info, desc string
		want             string
		err              error
	}{
		{"stopped", "showvminfo-stopped.txt", "", "snapshot " + demoID + " take s9", nil},
		{"running", "showvminfo-running.txt", "before upgrade", "snapshot " + demoID + " take s9 --description before upgrade", nil},
		{"duplicate", "showvminfo-stopped.txt", "", "", vm.ErrExists},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name := "s9"
			if c.err != nil {
				name = "s2"
			}
			f := newFake(t).
				On("showvminfo", fixture(t, c.info)).
				On("snapshot "+demoID+" list", fixture(t, "snapshot-list-branched.txt")).
				On("snapshot "+demoID+" take", fixture(t, "snapshot-take.txt"))
			err := newProvider(t, f).TakeSnapshot(context.Background(), "demo-1", name, c.desc)
			if !errors.Is(err, c.err) {
				t.Fatalf("err = %v, want %v", err, c.err)
			}
			takes := f.Find("snapshot " + demoID + " take")
			switch {
			case c.err != nil && len(takes) != 0:
				t.Fatalf("snapshot taken despite error: %v", takes)
			case c.err == nil && (len(takes) != 1 || takes[0].String() != c.want):
				t.Fatalf("take = %v, want %q", takes, c.want)
			}
		})
	}
}

func TestRestoreSnapshot(t *testing.T) {
	f := newFake(t).On("showvminfo", fixture(t, "showvminfo-running.txt"))
	if err := newProvider(t, f).RestoreSnapshot(context.Background(), demoID, "s1"); !errors.Is(err, vm.ErrInvalidState) {
		t.Fatalf("running: err = %v", err)
	}

	for _, info := range []string{"showvminfo-stopped.txt", "showvminfo-saved.txt"} {
		f = newFake(t).On("showvminfo", fixture(t, info)).On("snapshot", "Restoring snapshot 's1' (fd5d2e6b-be95-45e8-9760-4f28f551cbf2)\r\n")
		if err := newProvider(t, f).RestoreSnapshot(context.Background(), "demo-1", "s1"); err != nil {
			t.Fatalf("%s: %v", info, err)
		}
		if !f.Called("snapshot " + demoID + " restore s1") {
			t.Fatalf("calls = %v", commands(f))
		}
	}

	f = newFake(t).On("showvminfo", fixture(t, "showvminfo-stopped.txt")).OnResult("snapshot", failure(t, "err-snapshot-missing.txt"))
	if err := newProvider(t, f).RestoreSnapshot(context.Background(), demoID, "nosuch"); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("missing: err = %v", err)
	}
}

func TestDeleteSnapshot(t *testing.T) {
	f := newFake(t).On("snapshot", "")
	if err := newProvider(t, f).DeleteSnapshot(context.Background(), demoID, "s1"); err != nil {
		t.Fatal(err)
	}
	if got := commands(f); got[0] != "snapshot "+demoID+" delete s1" {
		t.Fatalf("calls = %q", got)
	}
}

func TestPortForwards(t *testing.T) {
	web := vm.PortForward{Name: "web", Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 8080, GuestPort: 80}
	cases := []struct {
		name, info string
		do         func(p *Provider) error
		want       string
		err        error
	}{
		{"add running", "showvminfo-running.txt", func(p *Provider) error { return p.AddPortForward(context.Background(), "demo-1", web) },
			"controlvm " + demoID + " natpf1 web,tcp,127.0.0.1,8080,,80", nil},
		{"add paused", "showvminfo-paused.txt", func(p *Provider) error { return p.AddPortForward(context.Background(), "demo-1", web) },
			"controlvm " + demoID + " natpf1 web,tcp,127.0.0.1,8080,,80", nil},
		{"add stopped", "showvminfo-stopped.txt", func(p *Provider) error { return p.AddPortForward(context.Background(), "demo-1", web) },
			"modifyvm " + demoID + " --natpf1 web,tcp,127.0.0.1,8080,,80", nil},
		{"add saved", "showvminfo-saved.txt", func(p *Provider) error { return p.AddPortForward(context.Background(), "demo-1", web) },
			"", vm.ErrInvalidState},
		{"remove running", "showvminfo-running.txt", func(p *Provider) error { return p.RemovePortForward(context.Background(), "demo-1", "ssh") },
			"controlvm " + demoID + " natpf1 delete ssh", nil},
		{"remove stopped", "showvminfo-stopped.txt", func(p *Provider) error { return p.RemovePortForward(context.Background(), "demo-1", "ssh") },
			"modifyvm " + demoID + " --natpf1 delete ssh", nil},
		{"remove missing", "showvminfo-stopped.txt", func(p *Provider) error { return p.RemovePortForward(context.Background(), "demo-1", "web") },
			"", vm.ErrNotFound},
		{"bad protocol", "showvminfo-stopped.txt", func(p *Provider) error {
			return p.AddPortForward(context.Background(), "demo-1", vm.PortForward{Name: "x", Protocol: "icmp", HostPort: 1, GuestPort: 1})
		}, "", vm.ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t).On("showvminfo", fixture(t, c.info)).On("controlvm", "").On("modifyvm", "")
			err := c.do(newProvider(t, f))
			if !errors.Is(err, c.err) {
				t.Fatalf("err = %v, want %v", err, c.err)
			}
			changes := append(f.Find("controlvm"), f.Find("modifyvm")...)
			if c.want == "" {
				if len(changes) != 0 {
					t.Fatalf("unexpected change %v", changes)
				}
				return
			}
			if len(changes) != 1 || changes[0].String() != c.want {
				t.Fatalf("changes = %v, want %q", changes, c.want)
			}
		})
	}
}

func TestPortForwardNeedsNAT(t *testing.T) {
	info := vmInfoText(`name="x"`, `UUID="`+demoID+`"`, `VMState="poweroff"`, `nic1="bridged"`)
	f := newFake(t).On("showvminfo", info)
	err := newProvider(t, f).AddPortForward(context.Background(), "x", vm.PortForward{Name: "a", Protocol: "tcp", HostPort: 1, GuestPort: 2})
	if !errors.Is(err, vm.ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestSetMeta(t *testing.T) {
	f := newFake(t).On("setextradata", "")
	err := newProvider(t, f).SetMeta(context.Background(), demoID, map[string]string{"label.team": "red", "ssh_user": "", "managed": "1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"setextradata " + demoID + " vmh/label.team red",
		"setextradata " + demoID + " vmh/managed 1",
		"setextradata " + demoID + " vmh/ssh_user",
	}
	if got := commands(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %q", got)
	}
}

func TestUpdate(t *testing.T) {
	f := newFake(t).On("showvminfo", fixture(t, "showvminfo-aborted.txt")).On("modifyvm", "")
	if err := newProvider(t, f).Update(context.Background(), "demo-2", vm.Changes{CPUs: 4, MemoryMB: 2048}); err != nil {
		t.Fatal(err)
	}
	if !f.Called("modifyvm 0b3e4233-7102-4087-bbd8-2ea3a0f123a4 --cpus 4 --memory 2048") {
		t.Fatalf("calls = %q", commands(f))
	}

	for _, info := range []string{"showvminfo-running.txt", "showvminfo-saved.txt"} {
		f = newFake(t).On("showvminfo", fixture(t, info)).On("modifyvm", "")
		if err := newProvider(t, f).Update(context.Background(), "demo-1", vm.Changes{MemoryMB: 256}); !errors.Is(err, vm.ErrInvalidState) {
			t.Fatalf("%s: err = %v", info, err)
		}
		if f.Called("modifyvm") {
			t.Fatalf("%s: modifyvm called", info)
		}
	}

	f = newFake(t)
	if err := newProvider(t, f).Update(context.Background(), "demo-1", vm.Changes{}); err != nil || len(f.Calls()) != 0 {
		t.Fatalf("empty update: err = %v, calls = %v", err, commands(f))
	}
}

func TestCommandErrorMessage(t *testing.T) {
	f := newFake(t).OnResult("startvm", failure(t, "err-already-running.txt"))
	p := newProvider(t, f)
	p.retryWait = 0
	err := p.Start(context.Background(), demoID, false)
	want := fmt.Sprintf("VBoxManage startvm: %v (exit 1): VBoxManage.exe: error: The machine 'demo-1' is already locked by a session", vm.ErrInvalidState)
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("err = %v", err)
	}
}
