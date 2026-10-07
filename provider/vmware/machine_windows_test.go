package vmware

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fl4metf/vm-harness/internal/fakerun"
	"github.com/fl4metf/vm-harness/vm"
	"golang.org/x/sys/windows"
)

func shortPath(t *testing.T, path string) string {
	t.Helper()
	long, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetShortPathName(long, &buf[0], uint32(len(buf)))
	if err != nil {
		t.Fatal(err)
	}
	return windows.UTF16ToString(buf[:n])
}

func TestShortNamesResolveToTheLongPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Machines With Long Names")
	path := writeFile(t, filepath.Join(root, "web", "web.vmx"), simpleVMX("web"))
	short := shortPath(t, root)
	if short == root {
		t.Skip("8.3 names are disabled on this volume")
	}
	fake := fakerun.New()
	fake.On("-T ws list", "Total running VMs: 1\r\n"+path+"\r\n")
	p := New(Options{Vmrun: "vmrun", VDiskManager: "vdm", OVFTool: "ovftool", HostType: "ws", Root: short, Runner: fake})
	p.inventory = ""
	if p.root != root {
		t.Fatalf("root = %q, want %q", p.root, root)
	}
	for _, ref := range []string{"web", shortPath(t, path)} {
		m, err := p.Get(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if m.ID != path || m.State != vm.StateRunning {
			t.Errorf("Get(%q) = %s %s, want %s running", ref, m.ID, m.State, path)
		}
	}
	fake.On("-T ws list", "Total running VMs: 1\r\n"+shortPath(t, path)+"\r\n")
	if m, err := p.Get(context.Background(), "web"); err != nil || m.State != vm.StateRunning {
		t.Errorf("a short name in vmrun list must match: %+v, %v", m, err)
	}
}
