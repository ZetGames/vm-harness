package vmware

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fl4metf/vm-harness/internal/fakerun"
	"github.com/fl4metf/vm-harness/runner"
	"github.com/fl4metf/vm-harness/vm"
)

func cp1251(t *testing.T, s string) string {
	t.Helper()
	var b []byte
	for _, r := range s {
		switch {
		case r < 0x80:
			b = append(b, byte(r))
		case r >= 'А' && r <= 'я':
			b = append(b, byte(r-'А'+0xC0))
		default:
			t.Fatalf("%q has no windows-1251 byte in this helper", r)
		}
	}
	return string(b)
}

func TestLegacyCharsetFiles(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web"))
	clone := e.addVM(t, "child", simpleVMX("child"))
	vmsd := filepath.Join(e.root, "web", "web.vmsd")
	writeFile(t, vmsd, cp1251(t, strings.Join([]string{
		`.encoding = "windows-1251"`,
		`snapshot.lastUID = "1"`,
		`snapshot.current = "1"`,
		`snapshot0.uid = "1"`,
		`snapshot0.displayName = "база"`,
		`snapshot0.description = "чистая система"`,
		`snapshot0.clone0 = "` + clone + `"`,
		`snapshot0.numClones = "1"`,
		"",
	}, "\r\n")))
	ctx := context.Background()
	e.fake.On("-T ws listSnapshots", "Total snapshots: 1\r\nбаза\r\n")
	snapshots, err := e.Snapshots(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if want := []vm.Snapshot{{Name: "база", ID: "1", Description: "чистая система", Current: true}}; !slices.Equal(snapshots, want) {
		t.Fatalf("snapshots = %+v", snapshots)
	}
	if m, err := e.Get(ctx, "web"); err != nil || m.CurrentSnapshot != "база" {
		t.Fatalf("current snapshot = %q, %v", m.CurrentSnapshot, err)
	}
	e.fake.On("-T ws deleteSnapshot", "")
	wantKind(t, e.DeleteSnapshot(ctx, "web", "БАЗА"), vm.ErrInvalidState)
	if e.fake.Called("-T ws deleteSnapshot") {
		t.Fatal("the linked clone guard must see a windows-1251 name")
	}

	e.fake.OnFunc("-T ws snapshot "+path, func(c fakerun.Call) (runner.Result, error) {
		writeFile(t, vmsd, readFile(t, vmsd)+cp1251(t, "snapshot1.uid = \"2\"\r\nsnapshot1.parent = \"1\"\r\nsnapshot1.displayName = \"второй\"\r\n"))
		return runner.Result{}, nil
	})
	if err := e.TakeSnapshot(ctx, "web", "второй", "после обновления"); err != nil {
		t.Fatal(err)
	}
	data := readFile(t, vmsd)
	for _, want := range []string{".encoding = \"UTF-8\"\r\n", "snapshot0.displayName = \"база\"\r\n", "snapshot1.description = \"после обновления\"\r\n"} {
		if !strings.Contains(data, want) {
			t.Errorf("vmsd lacks %q:\n%s", want, data)
		}
	}

	lab := writeFile(t, filepath.Join(t.TempDir(), "Лаборатория", "lab.vmx"), simpleVMX("Лаборатория"))
	writeFile(t, e.inventory, cp1251(t, ".encoding = \"windows-1251\"\r\nvmlist1.config = \""+lab+"\"\r\nvmlist1.DisplayName = \"Лаборатория\"\r\n"))
	if got, err := e.resolve(ctx, "Лаборатория"); err != nil || got != lab {
		t.Fatalf("resolve = %q, %v; want %q", got, err, lab)
	}
}
