package vmware

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ZetGames/vm-harness/vm"
)

func TestParseSnapshotTree(t *testing.T) {
	got := parseSnapshotTree(normalize([]byte(fixture(t, "vmrun/listsnapshots-tree.txt"))))
	want := []vm.Snapshot{{Name: "s1"}, {Name: "s2 with space", Parent: "s1"}, {Name: "s3", Parent: "s2 with space"}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v", got)
	}
	got = parseSnapshotTree(normalize([]byte(fixture(t, "vmrun/listsnapshots-flat.txt"))))
	if want := []vm.Snapshot{{Name: "s1"}, {Name: "s2 with space"}}; !slices.Equal(got, want) {
		t.Fatalf("flat: got %+v", got)
	}
	got = parseSnapshotTree("Total snapshots: 4\na\n\tb\n\t\tc\n\td\n")
	if want := []vm.Snapshot{{Name: "a"}, {Name: "b", Parent: "a"}, {Name: "c", Parent: "b"}, {Name: "d", Parent: "a"}}; !slices.Equal(got, want) {
		t.Fatalf("siblings: got %+v", got)
	}
	if got := parseSnapshotTree(normalize([]byte(fixture(t, "vmrun/listsnapshots-empty.txt")))); len(got) != 0 {
		t.Fatalf("empty: got %+v", got)
	}
}

func TestSnapshots(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web"))
	writeFile(t, filepath.Join(e.root, "web", "web.vmsd"), fixture(t, "vmsd/tree.vmsd"))
	e.fake.On("-T ws listSnapshots "+path+" showTree", fixture(t, "vmrun/listsnapshots-tree.txt"))
	got, err := e.Snapshots(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	want := []vm.Snapshot{
		{Name: "s1", ID: "1", Current: true},
		{Name: "s2 with space", ID: "2", Parent: "s1", Description: `before "upgrade"`},
		{Name: "s3", ID: "3", Parent: "s2 with space"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestSnapshotsWithoutVMSD(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web"))
	e.fake.On("-T ws listSnapshots "+path+" showTree", fixture(t, "vmrun/listsnapshots-empty.txt"))
	got, err := e.Snapshots(context.Background(), "web")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
	e.fake.OnResult("-T ws listSnapshots", failed(fixture(t, "vmrun/not-supported.txt")))
	_, err = e.Snapshots(context.Background(), "web")
	wantKind(t, err, vm.ErrUnsupported)
}

func TestTakeSnapshot(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web"))
	vmsd := filepath.Join(e.root, "web", "web.vmsd")
	e.fake.On("-T ws snapshot", "")
	ctx := context.Background()
	if err := e.TakeSnapshot(ctx, "web", "clean", ""); err != nil {
		t.Fatal(err)
	}
	if !e.fake.Called("-T ws snapshot " + path + " clean") {
		t.Fatalf("calls = %q", e.commands())
	}

	writeFile(t, vmsd, fixture(t, "vmsd/tree.vmsd"))
	if err := e.TakeSnapshot(ctx, "web", "s3", "after \"apt upgrade\""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, vmsd), "snapshot2.description = \"after |22apt upgrade|22\"\r\n") {
		t.Fatalf("description not stored:\n%s", readFile(t, vmsd))
	}

	e.fake.OnResult("-T ws snapshot", failed(fixture(t, "vmrun/snapshot-exists.txt")))
	wantKind(t, e.TakeSnapshot(ctx, "web", "s1", ""), vm.ErrExists)
	wantKind(t, e.TakeSnapshot(ctx, "web", "a/b", ""), vm.ErrInvalid)
	wantKind(t, e.TakeSnapshot(ctx, "web", "", ""), vm.ErrInvalid)
}

func TestRestoreSnapshot(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web"))
	e.fake.On("-T ws revertToSnapshot", "")
	if err := e.RestoreSnapshot(context.Background(), "web", "s1"); err != nil {
		t.Fatal(err)
	}
	if !e.fake.Called("-T ws revertToSnapshot " + path + " s1") {
		t.Fatalf("calls = %q", e.commands())
	}
	e.fake.OnResult("-T ws revertToSnapshot", failed(fixture(t, "vmrun/snapshot-missing.txt")))
	wantKind(t, e.RestoreSnapshot(context.Background(), "web", "nope"), vm.ErrNotFound)
}

func TestDeleteSnapshot(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "base", simpleVMX("base"))
	clone := e.addVM(t, "child", simpleVMX("child"))
	writeFile(t, filepath.Join(e.root, "base", "base.vmsd"), linkedVMSD(filepath.Join(e.root, "gone", "gone.vmx"), clone))
	e.fake.On("-T ws deleteSnapshot", "")
	ctx := context.Background()

	err := e.DeleteSnapshot(ctx, "base", "base")
	wantKind(t, err, vm.ErrInvalidState)
	err = e.DeleteSnapshot(ctx, "base", "Clone")
	wantKind(t, err, vm.ErrInvalidState)
	if e.fake.Called("-T ws deleteSnapshot") {
		t.Fatal("snapshots under a linked clone must not be deleted")
	}

	writeFile(t, filepath.Join(e.root, "base", "base.vmsd"), linkedVMSD(clone, filepath.Join(e.root, "gone", "gone.vmx")))
	if err := e.DeleteSnapshot(ctx, "base", "clone"); err != nil {
		t.Fatal(err)
	}
	if !e.fake.Called("-T ws deleteSnapshot " + path + " Clone") {
		t.Fatalf("vmrun must get the name from the vmsd: %q", e.commands())
	}
	wantKind(t, e.DeleteSnapshot(ctx, "base", "base"), vm.ErrInvalidState)
	wantKind(t, e.DeleteSnapshot(ctx, "base", "nope"), vm.ErrNotFound)
}

func TestDeleteSnapshotMatchesNamesLikeVmrun(t *testing.T) {
	e := newTestEnv(t)
	e.addVM(t, "base", simpleVMX("base"))
	clone := e.addVM(t, "child", simpleVMX("child"))
	vmsd := filepath.Join(e.root, "base", "base.vmsd")
	writeFile(t, vmsd, linkedVMSD(clone, filepath.Join(e.root, "gone", "gone.vmx")))
	e.fake.On("-T ws deleteSnapshot", "")
	ctx := context.Background()
	wantKind(t, e.DeleteSnapshot(ctx, "base", "BASE"), vm.ErrInvalidState)
	wantKind(t, e.DeleteSnapshot(ctx, "base", "base/Clone"), vm.ErrInvalid)
	writeFile(t, vmsd, readFile(t, vmsd)+"snapshot2.uid = \"3\"\r\nsnapshot2.displayName = \"CLONE\"\r\n")
	wantKind(t, e.DeleteSnapshot(ctx, "base", "clone"), vm.ErrInvalid)
	if e.fake.Called("-T ws deleteSnapshot") {
		t.Fatalf("calls = %q", e.commands())
	}
}
