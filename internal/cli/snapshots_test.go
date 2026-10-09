package cli

import (
	"testing"

	"github.com/ZetGames/vm-harness/vm"
)

func TestSnapshots(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)

	if out := e.ok("snap", "ls", "web").stdout; out != "[]\n" {
		t.Fatalf("empty snapshot list printed %q", out)
	}
	snap := decode[vm.Snapshot](t, e.ok("snap", "take", "web", "base", "-d", "clean install").stdout)
	if snap.Name != "base" || snap.Description != "clean install" || !snap.Current {
		t.Fatalf("take = %+v", snap)
	}
	e.ok("snap", "take", "web", "second")
	e.fail(8, "already_exists", "snap", "take", "web", "base")
	e.fail(9, "invalid_argument", "snap", "take", "web", "bad/name")

	snapshots := decode[[]vm.Snapshot](t, e.ok("snap", "list", "web").stdout)
	if len(snapshots) != 2 || snapshots[1].Parent != "base" || !snapshots[1].Current {
		t.Fatalf("list = %+v", snapshots)
	}

	mach := decode[vm.Machine](t, e.ok("snap", "restore", "web", "base").stdout)
	if mach.State != vm.StateStopped || mach.CurrentSnapshot != "base" {
		t.Fatalf("restore = %+v", mach)
	}
	e.fail(3, "not_found", "snap", "restore", "web", "nope")

	got := decode[removal](t, e.ok("snap", "rm", "web", "second").stdout)
	if got != (removal{VM: "web", Snapshot: "second", Deleted: true}) {
		t.Fatalf("rm = %+v", got)
	}
	e.fail(3, "not_found", "snap", "rm", "web", "second")

	out := e.ok("snap", "ls", "web", "-o", "text").stdout
	want := "NAME  CURRENT  PARENT  DESCRIPTION\nbase  yes              clean install\n"
	if out != want {
		t.Fatalf("text list:\n%q\nwant:\n%q", out, want)
	}
}
