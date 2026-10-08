package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fl4metf/vm-harness/internal/memprovider"
	"github.com/fl4metf/vm-harness/vm"
)

func TestDelete(t *testing.T) {
	cases := []struct {
		name  string
		state vm.State
		force bool
		calls []string
		err   error
	}{
		{name: "stopped", state: vm.StateStopped, calls: []string{"delete"}},
		{name: "saved state is discarded", state: vm.StateSaved, calls: []string{"kill", "delete"}},
		{name: "running without force", state: vm.StateRunning, err: vm.ErrInvalidState},
		{name: "paused without force", state: vm.StatePaused, err: vm.ErrInvalidState},
		{name: "busy without force", state: vm.StateBusy, err: vm.ErrInvalidState},
		{name: "running with force", state: vm.StateRunning, force: true, calls: []string{"kill", "delete"}},
		{name: "paused with force", state: vm.StatePaused, force: true, calls: []string{"kill", "delete"}},
		{name: "busy with force", state: vm.StateBusy, force: true, calls: []string{"kill", "delete"}},
		{name: "unknown with force", state: vm.StateUnknown, force: true, calls: []string{"kill", "delete"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			keys := filepath.Join(e.root, "keys")
			key := filepath.Join(keys, "virtualbox-a-00112233")
			mach := managedVM("a", c.state)
			mach.Meta[vm.MetaSSHKey] = key
			e.vbox.Put(mach)
			id := mustGet(t, e.vbox, "a").ID
			for _, name := range []string{"virtualbox-a-00112233", "virtualbox-a-00112233.pub", "virtualbox-a-00112233.known_hosts", "virtualbox-a-00112233.bak", "virtualbox-a", "vmware-a-00112233"} {
				writeFile(t, filepath.Join(keys, name), name)
			}
			err := e.m.Delete(t.Context(), vboxRef("a"), c.force)
			if c.err != nil {
				wantErr(t, err, c.err)
				if !strings.Contains(err.Error(), "force") {
					t.Fatalf("error does not suggest force: %v", err)
				}
				if !exists(key) {
					t.Fatal("keys removed although the vm was kept")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := opsOn(id, c.calls...); !slices.Equal(e.vbox.Calls(), want) {
				t.Fatalf("calls = %v, want %v", e.vbox.Calls(), want)
			}
			_, err = e.vbox.Get(t.Context(), "a")
			wantErr(t, err, vm.ErrNotFound)
			entries, _ := os.ReadDir(keys)
			var left []string
			for _, entry := range entries {
				left = append(left, entry.Name())
			}
			if want := []string{"virtualbox-a", "virtualbox-a-00112233.bak", "vmware-a-00112233"}; !slices.Equal(left, want) {
				t.Fatalf("key files left = %v, want %v", left, want)
			}
		})
	}
}

func TestDeleteForgetsHostKeysOfVMsWithoutKeys(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("a", vm.StateStopped))
	e.vbox.Put(managedVM("b", vm.StateStopped))
	knownA := writeFile(t, e.m.knownHostsPath(mustGet(t, e.vbox, "a")), "a")
	knownB := writeFile(t, e.m.knownHostsPath(mustGet(t, e.vbox, "b")), "b")
	if err := e.m.Delete(t.Context(), vboxRef("a"), false); err != nil {
		t.Fatal(err)
	}
	if exists(knownA) || !exists(knownB) {
		t.Fatalf("known_hosts a kept=%v, b kept=%v", exists(knownA), exists(knownB))
	}
}

func TestDeleteRefusesBaseOfLinkedClones(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("base", vm.StateStopped))
	for _, name := range []string{"left", "right"} {
		if _, err := e.m.Clone(t.Context(), vboxRef("base"), vm.CloneOptions{Name: name, Linked: true}); err != nil {
			t.Fatal(err)
		}
	}
	err := e.m.Delete(t.Context(), vboxRef("base"), true)
	wantErr(t, err, vm.ErrInvalidState)
	if !strings.Contains(err.Error(), "left, right") {
		t.Fatalf("error does not name the clones: %v", err)
	}
	if calls := callsOf(e.vbox, "delete"); len(calls) > 0 {
		t.Fatalf("provider delete called: %v", calls)
	}
	for _, name := range []string{"left", "right"} {
		if err := e.m.Delete(t.Context(), vboxRef(name), false); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.m.Delete(t.Context(), vboxRef("base"), false); err != nil {
		t.Fatal(err)
	}
}

func TestUpdate(t *testing.T) {
	e := newEnvWith(t, Config{Limits: Limits{MaxCPUs: 8, MaxMemoryMB: 8192}})
	stopped := managedVM("stopped", vm.StateStopped)
	stopped.Meta[vm.LabelKey("team")] = "red"
	e.vbox.Put(stopped)
	e.vbox.Put(managedVM("running", vm.StateRunning))

	got, err := e.m.Update(t.Context(), vboxRef("stopped"), vm.Changes{CPUs: 4, MemoryMB: 4096, Labels: map[string]string{"team": "", "env": "ci"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.CPUs != 4 || got.MemoryMB != 4096 || len(got.Labels) != 1 || got.Labels["env"] != "ci" {
		t.Fatalf("machine = %+v", got)
	}

	_, err = e.m.Update(t.Context(), vboxRef("running"), vm.Changes{CPUs: 2})
	wantErr(t, err, vm.ErrInvalidState)
	got, err = e.m.Update(t.Context(), vboxRef("running"), vm.Changes{Labels: map[string]string{"owner": "ops"}})
	if err != nil || got.Labels["owner"] != "ops" {
		t.Fatalf("label update on running vm = %v, %v", got.Labels, err)
	}

	invalid := []struct {
		ch  vm.Changes
		err error
	}{
		{vm.Changes{CPUs: 16}, vm.ErrLimit},
		{vm.Changes{MemoryMB: 16384}, vm.ErrLimit},
		{vm.Changes{CPUs: -1}, vm.ErrInvalid},
		{vm.Changes{Labels: map[string]string{"bad key": "v"}}, vm.ErrInvalid},
	}
	before := len(callsOf(e.vbox, "update"))
	for _, c := range invalid {
		_, err := e.m.Update(t.Context(), vboxRef("stopped"), c.ch)
		wantErr(t, err, c.err)
	}
	if len(callsOf(e.vbox, "update")) != before {
		t.Fatal("provider updated despite invalid changes")
	}
}

func createWithKey(t *testing.T, e *testEnv, name string) vm.Machine {
	t.Helper()
	src, err := e.m.Create(t.Context(), vm.Spec{
		Name:         name,
		OSType:       "ubuntu",
		Labels:       map[string]string{"team": "red"},
		DiskImage:    cloudImage(t),
		CloudInit:    &vm.CloudInit{},
		PortForwards: []vm.PortForward{{GuestPort: 80}, {Protocol: "udp", GuestPort: 53}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func TestFullCloneFixups(t *testing.T) {
	e := newEnv(t)
	src := createWithKey(t, e, "src")
	e.vbox.MaxReliableCPUs = 1
	srcKey := src.Meta[vm.MetaSSHKey]
	writeFile(t, srcKey+".known_hosts", "[127.0.0.1]:2222 ssh-ed25519 AAAA")
	clone, err := e.m.Clone(t.Context(), vboxRef("src"), vm.CloneOptions{Name: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	if !clone.Managed || clone.Meta[vm.MetaManaged] != clone.ID || clone.Labels["team"] != "red" || clone.Meta[vm.MetaOSType] != "ubuntu" || clone.Meta[vm.MetaSSHUser] != "vmh" {
		t.Fatalf("clone meta = %v", clone.Meta)
	}
	if src.CPUs != 2 || clone.CPUs != src.CPUs {
		t.Fatalf("clone has %d cpus, source %d", clone.CPUs, src.CPUs)
	}
	if _, ok := clone.Meta[vm.MetaLinkedFrom]; ok {
		t.Fatal("full clone marked as linked")
	}
	key := clone.Meta[vm.MetaSSHKey]
	if filepath.Dir(key) != filepath.Join(e.root, "keys") || !strings.HasPrefix(filepath.Base(key), "virtualbox-copy-") || key == srcKey {
		t.Fatalf("clone ssh_key = %q", key)
	}
	for _, suffix := range []string{"", ".pub"} {
		want, _ := os.ReadFile(srcKey + suffix)
		got, err := os.ReadFile(key + suffix)
		if err != nil || string(got) != string(want) {
			t.Fatalf("copied key%s differs: %v", suffix, err)
		}
	}
	if exists(key + ".known_hosts") {
		t.Fatal("the clone inherited the source's host keys")
	}
	if len(clone.PortForwards) != len(src.PortForwards) {
		t.Fatalf("clone forwards = %+v", clone.PortForwards)
	}
	used := map[int]bool{}
	for _, pf := range src.PortForwards {
		used[pf.HostPort] = true
	}
	for _, pf := range clone.PortForwards {
		srcPF, _ := forwardNamed(src, pf.Name)
		if pf.HostPort == 0 || used[pf.HostPort] || pf.GuestPort != srcPF.GuestPort || pf.Protocol != srcPF.Protocol {
			t.Fatalf("clone forward %+v, source %+v", pf, srcPF)
		}
		used[pf.HostPort] = true
	}
	again := mustGet(t, e.vbox, "src")
	if again.Meta[vm.MetaSSHKey] != srcKey || !slices.Equal(again.PortForwards, src.PortForwards) || !exists(srcKey+".known_hosts") {
		t.Fatal("source changed")
	}
}

func TestLinkedCloneOfManagedSourceTakesBaseSnapshot(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("src", vm.StateStopped))
	src := mustGet(t, e.vbox, "src")
	clone, err := e.m.Clone(t.Context(), vboxRef("src"), vm.CloneOptions{Name: "copy", Linked: true})
	if err != nil {
		t.Fatal(err)
	}
	if clone.Meta[vm.MetaLinkedFrom] != src.ID || !clone.Managed {
		t.Fatalf("clone meta = %v", clone.Meta)
	}
	snaps, _ := e.vbox.Snapshots(t.Context(), "src")
	if len(snaps) != 1 || snaps[0].Name != cloneBaseSnapshot {
		t.Fatalf("source snapshots = %+v", snaps)
	}

	second, err := e.m.Clone(t.Context(), vboxRef("src"), vm.CloneOptions{Name: "copy2", Linked: true})
	if err != nil {
		t.Fatal(err)
	}
	if snaps, _ := e.vbox.Snapshots(t.Context(), "src"); len(snaps) != 1 {
		t.Fatalf("base snapshot taken twice: %+v", snaps)
	}
	if second.Meta[vm.MetaLinkedFrom] != src.ID {
		t.Fatalf("second clone meta = %v", second.Meta)
	}

	full, err := e.m.Clone(t.Context(), vboxRef("copy"), vm.CloneOptions{Name: "copy3"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := full.Meta[vm.MetaLinkedFrom]; ok {
		t.Fatal("full clone of a linked clone kept linked_from")
	}
}

func TestLinkedCloneOfAPoweredOnSourceWithoutSnapshot(t *testing.T) {
	for _, state := range []vm.State{vm.StateRunning, vm.StatePaused, vm.StateSaved} {
		t.Run(string(state), func(t *testing.T) {
			e := newEnv(t)
			e.vbox.Put(managedVM("src", state))
			_, err := e.m.Clone(t.Context(), vboxRef("src"), vm.CloneOptions{Name: "copy", Linked: true})
			wantErr(t, err, vm.ErrInvalidState)
			if !strings.Contains(err.Error(), "stop it") {
				t.Fatalf("error = %v", err)
			}
			if calls := e.vbox.Calls(); len(calls) > 0 {
				t.Fatalf("provider changed: %v", calls)
			}
			if err := e.vbox.TakeSnapshot(t.Context(), "src", "mine", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := e.m.Clone(t.Context(), vboxRef("src"), vm.CloneOptions{Name: "copy", Linked: true}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type failingClone struct {
	*memprovider.Provider
}

func (f failingClone) Clone(context.Context, string, vm.CloneOptions) (vm.Machine, error) {
	return vm.Machine{}, errors.New("clone failed")
}

func TestFailedLinkedCloneRemovesItsBaseSnapshot(t *testing.T) {
	p := failingClone{memprovider.New(vm.VirtualBox)}
	p.Put(managedVM("src", vm.StateStopped))
	m := newManager(t, Config{}, p)
	_, err := m.Clone(t.Context(), Ref{VM: "src"}, vm.CloneOptions{Name: "copy", Linked: true})
	if err == nil || !strings.Contains(err.Error(), "clone failed") {
		t.Fatalf("err = %v", err)
	}
	if snaps, _ := p.Snapshots(t.Context(), "src"); len(snaps) != 0 {
		t.Fatalf("snapshots left = %+v", snaps)
	}
	if err := p.TakeSnapshot(t.Context(), "src", "mine", ""); err != nil {
		t.Fatal(err)
	}
	_, err = m.Clone(t.Context(), Ref{VM: "src"}, vm.CloneOptions{Name: "copy", Linked: true})
	if err == nil {
		t.Fatal("clone succeeded")
	}
	if snaps, _ := p.Snapshots(t.Context(), "src"); len(snaps) != 1 || snaps[0].Name != "mine" {
		t.Fatalf("snapshots = %+v", snaps)
	}
}

func TestCloneOfUnmanagedSource(t *testing.T) {
	for _, linked := range []bool{false, true} {
		e := newEnv(t)
		e.vbox.Put(foreignVM("legacy", vm.StateStopped))
		_, err := e.m.Clone(t.Context(), vboxRef("legacy"), vm.CloneOptions{Name: "copy", Linked: linked})
		wantErr(t, err, vm.ErrForbidden)
		if calls := e.vbox.Calls(); len(calls) > 0 {
			t.Fatalf("provider touched: %v", calls)
		}
	}

	e := newEnvWith(t, Config{AllowUnmanaged: true})
	e.vbox.Put(foreignVM("legacy", vm.StateStopped))
	clone, err := e.m.Clone(t.Context(), vboxRef("legacy"), vm.CloneOptions{Name: "copy", Linked: true})
	if err != nil {
		t.Fatal(err)
	}
	if !clone.Managed || clone.Meta[vm.MetaLinkedFrom] != mustGet(t, e.vbox, "legacy").ID {
		t.Fatalf("clone meta = %v", clone.Meta)
	}
}

func TestCloneLimitsAndValidation(t *testing.T) {
	e := newEnvWith(t, Config{Limits: Limits{MaxVMs: 2, MaxCPUs: 4, MaxMemoryMB: 4096}})
	src := managedVM("src", vm.StateStopped)
	src.CPUs, src.MemoryMB = 2, 2048
	e.vbox.Put(src)
	for _, name := range []string{"bad name", "web.", "PRN"} {
		_, err := e.m.Clone(t.Context(), vboxRef("src"), vm.CloneOptions{Name: name})
		wantErr(t, err, vm.ErrInvalid)
	}
	_, err := e.m.Clone(t.Context(), vboxRef("src"), vm.CloneOptions{Name: "SRC"})
	wantErr(t, err, vm.ErrExists)
	if _, err := e.m.Clone(t.Context(), vboxRef("src"), vm.CloneOptions{Name: "one"}); err != nil {
		t.Fatal(err)
	}
	_, err = e.m.Clone(t.Context(), vboxRef("src"), vm.CloneOptions{Name: "two"})
	wantErr(t, err, vm.ErrLimit)
	_, err = e.m.Create(t.Context(), vm.Spec{Name: "three"})
	wantErr(t, err, vm.ErrLimit)
	_, err = e.m.Clone(t.Context(), vboxRef("missing"), vm.CloneOptions{Name: "x"})
	wantErr(t, err, vm.ErrNotFound)

	e = newEnvWith(t, Config{Limits: Limits{MaxCPUs: 4, MaxMemoryMB: 4096}})
	big := managedVM("big", vm.StateStopped)
	big.CPUs, big.MemoryMB = 32, 2048
	e.vbox.Put(big)
	huge := managedVM("huge", vm.StateStopped)
	huge.CPUs, huge.MemoryMB = 2, 65536
	e.vbox.Put(huge)
	for _, name := range []string{"big", "huge"} {
		_, err := e.m.Clone(t.Context(), vboxRef(name), vm.CloneOptions{Name: name + "-copy"})
		wantErr(t, err, vm.ErrLimit)
	}
	if calls := callsOf(e.vbox, "clone"); len(calls) > 0 {
		t.Fatalf("cloned: %v", calls)
	}
}

type failingSetMeta struct {
	*memprovider.Provider
}

func (f failingSetMeta) SetMeta(context.Context, string, map[string]string) error {
	return errors.New("extradata write failed")
}

func TestCloneRollsBackWhenFixupsFail(t *testing.T) {
	p := failingSetMeta{memprovider.New(vm.VirtualBox)}
	src := managedVM("src", vm.StateStopped)
	src.Meta[vm.MetaSSHUser] = "vmh"
	p.Put(src)
	m := newManager(t, Config{}, p)
	key := filepath.Join(m.Config().Root, "keys", "virtualbox-src-00112233")
	writeFile(t, key, "private")
	if err := p.Provider.SetMeta(t.Context(), "src", map[string]string{vm.MetaSSHKey: key}); err != nil {
		t.Fatal(err)
	}
	_, err := m.Clone(t.Context(), Ref{VM: "src"}, vm.CloneOptions{Name: "copy"})
	if err == nil || !strings.Contains(err.Error(), "extradata write failed") {
		t.Fatalf("err = %v", err)
	}
	_, err = p.Get(t.Context(), "copy")
	wantErr(t, err, vm.ErrNotFound)
	entries, _ := os.ReadDir(filepath.Join(m.Config().Root, "keys"))
	if len(entries) != 1 || entries[0].Name() != filepath.Base(key) {
		t.Fatalf("key files = %v", entries)
	}
}

func TestDeleteNeverRemovesFilesOutsideTheKeyDirectory(t *testing.T) {
	e := newEnv(t)
	outside := writeFile(t, filepath.Join(e.root, "escape"), "keep me")
	outsidePub := writeFile(t, filepath.Join(e.root, "escape.pub"), "keep me")
	mach := managedVM("a", vm.StateStopped)
	mach.Meta[vm.MetaSSHKey] = filepath.Join(e.root, "keys", "..", "escape")
	e.vbox.Put(mach)
	if err := e.m.Delete(t.Context(), vboxRef("a"), false); err != nil {
		t.Fatal(err)
	}
	if !exists(outside) || !exists(outsidePub) {
		t.Fatal("file outside the key directory was removed")
	}
}

func TestDeleteOfAnAdoptedCopyKeepsTheOriginalsKey(t *testing.T) {
	e := newEnv(t)
	keys := filepath.Join(e.root, "keys")
	key := filepath.Join(keys, "virtualbox-web-0a1b2c3d")
	for _, suffix := range []string{"", ".pub", ".known_hosts"} {
		writeFile(t, key+suffix, "web")
	}
	web := managedVM("web", vm.StateStopped)
	web.Meta[vm.MetaSSHKey] = key
	web.Meta[vm.MetaSSHUser] = "vmh"
	e.vbox.Put(web)
	webID := mustGet(t, e.vbox, "web").ID
	for _, name := range []string{"web Clone", "web-0a1b2c3d", "we"} {
		e.vbox.Put(vm.Machine{Name: name, State: vm.StateStopped, Meta: map[string]string{
			vm.MetaManaged: webID, vm.MetaSSHKey: key, vm.MetaSSHUser: "vmh",
		}})
		if _, err := e.m.Adopt(t.Context(), vboxRef(name)); err != nil {
			t.Fatal(err)
		}
		if err := e.m.Delete(t.Context(), vboxRef(name), false); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{"", ".pub", ".known_hosts"} {
			if !exists(key + suffix) {
				t.Fatalf("deleting %q removed %s", name, filepath.Base(key+suffix))
			}
		}
	}
	if err := e.m.Delete(t.Context(), vboxRef("web"), false); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(keys); len(entries) != 0 {
		t.Fatalf("key files left after deleting the owner: %v", entries)
	}
}
