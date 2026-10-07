package harness

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fl4metf/vm-harness/vm"
)

func TestTakeSnapshot(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("a", vm.StateRunning))
	first, err := e.m.TakeSnapshot(t.Context(), vboxRef("a"), "clean", "fresh install")
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != "clean" || first.Description != "fresh install" || first.ID == "" || !first.Current {
		t.Fatalf("snapshot = %+v", first)
	}
	second, err := e.m.TakeSnapshot(t.Context(), vboxRef("a"), "after tools", "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Parent != "clean" || !second.Current {
		t.Fatalf("snapshot = %+v", second)
	}
	_, err = e.m.TakeSnapshot(t.Context(), vboxRef("a"), "clean", "")
	wantErr(t, err, vm.ErrExists)
	if snaps, _ := e.vbox.Snapshots(t.Context(), "a"); len(snaps) != 2 {
		t.Fatalf("snapshots = %+v", snaps)
	}
}

func TestTakeSnapshotValidation(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("a", vm.StateRunning))
	cases := []struct{ name, description string }{
		{"", ""},
		{" leading", ""},
		{"a/b", ""},
		{"line\nbreak", ""},
		{strings.Repeat("s", 65), ""},
		{"ok", "two\nlines"},
		{"ok", strings.Repeat("d", 1025)},
	}
	for _, c := range cases {
		_, err := e.m.TakeSnapshot(t.Context(), vboxRef("a"), c.name, c.description)
		wantErr(t, err, vm.ErrInvalid)
	}
	if calls := e.vbox.Calls(); len(calls) > 0 {
		t.Fatalf("provider called: %v", calls)
	}
}

func TestRestoreSnapshot(t *testing.T) {
	cases := []struct {
		name   string
		state  vm.State
		forced bool
	}{
		{name: "running vm is powered off first", state: vm.StateRunning, forced: true},
		{name: "paused vm is powered off first", state: vm.StatePaused, forced: true},
		{name: "stopped vm", state: vm.StateStopped},
		{name: "saved vm", state: vm.StateSaved},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			mach := managedVM("a", c.state)
			mach.Meta[vm.MetaSSHKey] = filepath.Join(e.root, "keys", "virtualbox-a-00112233")
			e.vbox.Put(mach)
			id := mustGet(t, e.vbox, "a").ID
			if err := e.vbox.TakeSnapshot(t.Context(), id, "base", ""); err != nil {
				t.Fatal(err)
			}
			key := writeFile(t, mach.Meta[vm.MetaSSHKey], "private")
			known := writeFile(t, key+".known_hosts", "[127.0.0.1]:2222 ssh-ed25519 AAAA")
			got, err := e.m.RestoreSnapshot(t.Context(), vboxRef("a"), "base")
			if err != nil {
				t.Fatal(err)
			}
			if got.CurrentSnapshot != "base" {
				t.Fatalf("current snapshot = %q", got.CurrentSnapshot)
			}
			if c.state != vm.StateSaved && got.State != vm.StateStopped {
				t.Fatalf("state = %s", got.State)
			}
			want := opsOn(id, "snapshot", "restore")
			if c.forced {
				want = opsOn(id, "snapshot", "kill", "restore")
			}
			if calls := e.vbox.Calls(); !slices.Equal(calls, want) {
				t.Fatalf("calls = %v, want %v", calls, want)
			}
			if exists(known) || !exists(key) {
				t.Fatalf("after restore: known_hosts kept=%v, key kept=%v", exists(known), exists(key))
			}
		})
	}
}

func TestRestoreMissingSnapshotKeepsVMRunning(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("a", vm.StateRunning))
	_, err := e.m.RestoreSnapshot(t.Context(), vboxRef("a"), "nope")
	wantErr(t, err, vm.ErrNotFound)
	if state := mustGet(t, e.vbox, "a").State; state != vm.StateRunning {
		t.Fatalf("state = %s", state)
	}
	_, err = e.m.RestoreSnapshot(t.Context(), vboxRef("a"), "")
	wantErr(t, err, vm.ErrInvalid)
}

func TestSnapshotsAndDelete(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("a", vm.StateStopped))
	snaps, err := e.m.Snapshots(t.Context(), vboxRef("a"))
	if err != nil || snaps == nil || len(snaps) != 0 {
		t.Fatalf("Snapshots = %#v, %v", snaps, err)
	}
	if _, err := e.m.TakeSnapshot(t.Context(), vboxRef("a"), "s1", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.m.DeleteSnapshot(t.Context(), vboxRef("a"), "s1"); err != nil {
		t.Fatal(err)
	}
	if snaps, _ := e.m.Snapshots(t.Context(), vboxRef("a")); len(snaps) != 0 {
		t.Fatalf("snapshots = %+v", snaps)
	}
	wantErr(t, e.m.DeleteSnapshot(t.Context(), vboxRef("a"), "s1"), vm.ErrNotFound)
	wantErr(t, e.m.DeleteSnapshot(t.Context(), vboxRef("a"), ""), vm.ErrInvalid)
}
