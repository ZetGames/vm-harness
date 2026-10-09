package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZetGames/vm-harness/vm"
)

type opEnv struct {
	ctx  context.Context
	m    *Manager
	ref  Ref
	host string
}

type guardedOp struct {
	name string
	run  func(o opEnv) error
}

func guardedOps() []guardedOp {
	ignore := func(_ any, err error) error { return err }
	ssh := Access{SSH: SSHOptions{User: "u", Password: "p"}}
	command := vm.ExecRequest{Command: []string{"id"}}
	quick := func(cond string, acc Access) WaitRequest {
		return WaitRequest{For: cond, Timeout: 20 * time.Millisecond, Access: acc}
	}
	return []guardedOp{
		{"delete", func(o opEnv) error { return o.m.Delete(o.ctx, o.ref, false) }},
		{"delete force", func(o opEnv) error { return o.m.Delete(o.ctx, o.ref, true) }},
		{"update", func(o opEnv) error {
			return ignore(o.m.Update(o.ctx, o.ref, vm.Changes{Labels: map[string]string{"k": "v"}}))
		}},
		{"start", func(o opEnv) error { return ignore(o.m.Start(o.ctx, o.ref, false)) }},
		{"stop", func(o opEnv) error {
			return ignore(o.m.Stop(o.ctx, o.ref, StopOptions{Timeout: 20 * time.Millisecond}))
		}},
		{"kill", func(o opEnv) error { return ignore(o.m.Stop(o.ctx, o.ref, StopOptions{Force: true})) }},
		{"pause", func(o opEnv) error { return ignore(o.m.Pause(o.ctx, o.ref)) }},
		{"resume", func(o opEnv) error { return ignore(o.m.Resume(o.ctx, o.ref)) }},
		{"reset", func(o opEnv) error { return ignore(o.m.Reset(o.ctx, o.ref)) }},
		{"suspend", func(o opEnv) error { return ignore(o.m.Suspend(o.ctx, o.ref)) }},
		{"take snapshot", func(o opEnv) error { return ignore(o.m.TakeSnapshot(o.ctx, o.ref, "s2", "")) }},
		{"restore snapshot", func(o opEnv) error { return ignore(o.m.RestoreSnapshot(o.ctx, o.ref, "s1")) }},
		{"delete snapshot", func(o opEnv) error { return o.m.DeleteSnapshot(o.ctx, o.ref, "s1") }},
		{"exec", func(o opEnv) error { return ignore(o.m.Exec(o.ctx, o.ref, ExecRequest{ExecRequest: command})) }},
		{"exec over ssh", func(o opEnv) error {
			return ignore(o.m.Exec(o.ctx, o.ref, ExecRequest{ExecRequest: command, SSH: ssh.SSH}))
		}},
		{"copy to", func(o opEnv) error {
			return o.m.CopyTo(o.ctx, o.ref, CopyRequest{CopyRequest: vm.CopyRequest{HostPath: o.host, GuestPath: "/tmp/x"}})
		}},
		{"copy from", func(o opEnv) error {
			return o.m.CopyFrom(o.ctx, o.ref, CopyRequest{CopyRequest: vm.CopyRequest{HostPath: o.host + ".out", GuestPath: "/tmp/x"}})
		}},
		{"write file", func(o opEnv) error { return o.m.WriteFile(o.ctx, o.ref, "/tmp/x", []byte("x"), Access{}) }},
		{"read file", func(o opEnv) error { return ignore(o.m.ReadFile(o.ctx, o.ref, "/etc/hostname", Access{})) }},
		{"read file over ssh", func(o opEnv) error { return ignore(o.m.ReadFile(o.ctx, o.ref, "/etc/hostname", ssh)) }},
		{"add port forward", func(o opEnv) error { return ignore(o.m.AddPortForward(o.ctx, o.ref, vm.PortForward{GuestPort: 80})) }},
		{"remove port forward", func(o opEnv) error { return o.m.RemovePortForward(o.ctx, o.ref, "web") }},
		{"linked clone", func(o opEnv) error {
			return ignore(o.m.Clone(o.ctx, o.ref, vm.CloneOptions{Name: "copy", Linked: true}))
		}},
		{"full clone", func(o opEnv) error {
			return ignore(o.m.Clone(o.ctx, o.ref, vm.CloneOptions{Name: "copy"}))
		}},
		{"wait for guest", func(o opEnv) error { return ignore(o.m.Wait(o.ctx, o.ref, quick(WaitGuest, Access{}))) }},
		{"wait for ssh", func(o opEnv) error { return ignore(o.m.Wait(o.ctx, o.ref, quick(WaitSSH, ssh))) }},
	}
}

func foreignWithHistory(t *testing.T, e *testEnv) string {
	t.Helper()
	legacy := foreignVM("legacy", vm.StateRunning)
	legacy.PortForwards = []vm.PortForward{{Name: "web", Protocol: "tcp", HostPort: 18080, GuestPort: 80}}
	e.vbox.Put(legacy)
	if err := e.vbox.TakeSnapshot(t.Context(), "legacy", "s1", ""); err != nil {
		t.Fatal(err)
	}
	return writeFile(t, filepath.Join(t.TempDir(), "payload"), "payload")
}

func TestGuardrailsRejectUnmanaged(t *testing.T) {
	for _, op := range guardedOps() {
		t.Run(op.name, func(t *testing.T) {
			e := newEnv(t)
			stub := stubSSH(e.m)
			host := foreignWithHistory(t, e)
			before := len(e.vbox.Calls())
			err := op.run(opEnv{t.Context(), e.m, Ref{VM: "legacy"}, host})
			wantErr(t, err, vm.ErrForbidden)
			if !strings.Contains(err.Error(), `vm "legacy" was not created by vmh`) || strings.Contains(err.Error(), "vmh adopt") {
				t.Fatalf("error = %v", err)
			}
			if calls := e.vbox.Calls()[before:]; len(calls) > 0 {
				t.Fatalf("provider was touched: %v", calls)
			}
			if stub.used() {
				t.Fatal("ssh was used")
			}
			if mach := mustGet(t, e.vbox, "legacy"); mach.State != vm.StateRunning || mach.Managed {
				t.Fatalf("machine changed: %+v", mach)
			}
		})
	}
}

func TestGuardrailsAllowUnmanagedWhenConfigured(t *testing.T) {
	for _, op := range guardedOps() {
		t.Run(op.name, func(t *testing.T) {
			e := newEnvWith(t, Config{AllowUnmanaged: true})
			stubSSH(e.m)
			host := foreignWithHistory(t, e)
			err := op.run(opEnv{t.Context(), e.m, Ref{VM: "legacy"}, host})
			if errors.Is(err, vm.ErrForbidden) {
				t.Fatalf("still forbidden: %v", err)
			}
		})
	}
}

func TestReadOnlyOperationsOnUnmanaged(t *testing.T) {
	e := newEnv(t)
	foreignWithHistory(t, e)
	before := len(e.vbox.Calls())
	e.vbox.SetIP("legacy", "10.0.2.15")
	ref := Ref{VM: "legacy"}
	ctx := t.Context()
	if _, err := e.m.Get(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if snaps, err := e.m.Snapshots(ctx, ref); err != nil || len(snaps) != 1 {
		t.Fatalf("Snapshots = %v, %v", snaps, err)
	}
	if ip, err := e.m.GuestIP(ctx, ref); err != nil || ip != "10.0.2.15" {
		t.Fatalf("GuestIP = %q, %v", ip, err)
	}
	if png, err := e.m.Screenshot(ctx, ref, vm.Credentials{}); err != nil || len(png) == 0 {
		t.Fatalf("Screenshot = %d bytes, %v", len(png), err)
	}
	if _, err := e.m.Wait(ctx, ref, WaitRequest{For: WaitRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Wait(ctx, ref, WaitRequest{For: WaitIP}); err != nil {
		t.Fatal(err)
	}
	if calls := e.vbox.Calls()[before:]; len(calls) > 0 {
		t.Fatalf("read-only operations changed the vm: %v", calls)
	}
}

func TestAdoptAndRelease(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(foreignVM("legacy", vm.StateStopped))
	ref := Ref{VM: "legacy"}
	id := mustGet(t, e.vbox, "legacy").ID

	_, err := e.m.Start(t.Context(), ref, false)
	wantErr(t, err, vm.ErrForbidden)

	adopted, err := e.m.Adopt(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if !adopted.Managed || adopted.Meta[vm.MetaManaged] != id {
		t.Fatalf("adopt left managed=%v meta %v", adopted.Managed, adopted.Meta)
	}
	if _, err := e.m.Start(t.Context(), ref, false); err != nil {
		t.Fatal(err)
	}

	released, err := e.m.Release(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if released.Managed || released.Meta[vm.MetaManaged] != "" {
		t.Fatalf("release left %v", released.Meta)
	}
	_, err = e.m.Stop(t.Context(), ref, StopOptions{Force: true})
	wantErr(t, err, vm.ErrForbidden)

	_, err = e.m.Adopt(t.Context(), Ref{VM: "missing"})
	wantErr(t, err, vm.ErrNotFound)
}

func TestManagedMarkerMustNameTheVM(t *testing.T) {
	e := newEnv(t)
	for name, marker := range map[string]string{"copied": "virtualbox-original-id", "legacy-flag": "1"} {
		e.vbox.Put(vm.Machine{Name: name, State: vm.StateStopped, Meta: map[string]string{vm.MetaManaged: marker}})
		got, err := e.m.Get(t.Context(), vboxRef(name))
		if err != nil {
			t.Fatal(err)
		}
		if got.Managed {
			t.Fatalf("%s with marker %q counts as managed", name, marker)
		}
		wantErr(t, e.m.Delete(t.Context(), vboxRef(name), true), vm.ErrForbidden)
	}
	if calls := callsOf(e.vbox, "delete"); len(calls) > 0 {
		t.Fatalf("deleted: %v", calls)
	}
	managed, err := e.m.List(t.Context(), ListOptions{ManagedOnly: true})
	if err != nil || len(managed) != 0 {
		t.Fatalf("managed vms = %v, %v", managed, err)
	}
}

func TestVMsInsideTheRootAreManaged(t *testing.T) {
	e := newEnv(t)
	stray := foreignVM("stray", vm.StateStopped)
	stray.ConfigPath = filepath.Join(e.root, vm.VirtualBox, "stray", "stray.vbox")
	e.vbox.Put(stray)
	outside := foreignVM("outside", vm.StateStopped)
	outside.ConfigPath = filepath.Join(e.root+"-other", vm.VirtualBox, "outside", "outside.vbox")
	e.vbox.Put(outside)

	got, err := e.m.Get(t.Context(), vboxRef("stray"))
	if err != nil || !got.Managed {
		t.Fatalf("vm inside the root: managed=%v, %v", got.Managed, err)
	}
	if got, _ := e.m.Get(t.Context(), vboxRef("outside")); got.Managed {
		t.Fatal("vm next to the root counts as managed")
	}
	_, err = e.m.Release(t.Context(), vboxRef("stray"))
	wantErr(t, err, vm.ErrInvalid)
	if err := e.m.Delete(t.Context(), vboxRef("stray"), false); err != nil {
		t.Fatal(err)
	}
}

func TestRootLocationSurvivesSymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	m := New(Config{Root: link})
	mach := vm.Machine{ID: "7", Provider: vm.VirtualBox, ConfigPath: filepath.Join(real, vm.VirtualBox, "web", "web.vbox")}
	if !m.inVMHRoot(mach) {
		t.Fatal("a machine under the resolved root must count as vmh-owned")
	}
	mach.ConfigPath = filepath.Join(real, "elsewhere", "web.vbox")
	if m.inVMHRoot(mach) {
		t.Fatal("a machine outside <root>/<provider> must not count as vmh-owned")
	}
}
