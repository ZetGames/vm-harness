package vmware

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/fl4metf/vm-harness/vm"
)

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(data))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestReadInventory(t *testing.T) {
	entries := readInventory(filepath.Join("testdata", "inventory.vmls"))
	want := []inventoryEntry{{`C:\vms\other\Old Box.vmx`, "old"}, {`C:\vms\lab\lab.vmx`, "lab"}}
	if runtime.GOOS != "windows" {
		want = nil
	}
	if !slices.Equal(entries, want) {
		t.Fatalf("entries = %v", entries)
	}
	if readInventory(filepath.Join(t.TempDir(), "missing.vmls")) != nil {
		t.Fatal("a missing inventory should be empty")
	}
}

func TestResolve(t *testing.T) {
	e := newTestEnv(t)
	web := e.addVM(t, "web", simpleVMX("web"))
	other := t.TempDir()
	lab := writeFile(t, filepath.Join(other, "Lab Box", "lab.vmx"), simpleVMX("lab"))
	listed := writeFile(t, filepath.Join(other, "run", "run.vmx"), simpleVMX("runner"))
	stray := writeFile(t, filepath.Join(other, "stray", "stray.vmx"), simpleVMX("stray"))
	writeFile(t, e.inventory, ".encoding = \"windows-1251\"\nvmlist1.config = \""+lab+"\"\nvmlist1.DisplayName = \"lab\"\nvmlist2.config = \"\"\n")
	e.setRunning(listed)
	ctx := context.Background()
	cases := map[string]string{
		"web":    web,
		web:      web,
		"lab":    lab,
		lab:      lab,
		"runner": listed,
		listed:   listed,
	}
	for ref, want := range cases {
		got, err := e.resolve(ctx, ref)
		if err != nil || got != want {
			t.Errorf("resolve(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
	for _, ref := range []string{"nope", stray, "stray", filepath.Join(e.root, "gone", "gone.vmx"), "../web", "web/web.vmx", ""} {
		_, err := e.resolve(ctx, ref)
		wantKind(t, err, vm.ErrNotFound)
	}
}

func TestResolveReturnsOnDiskSpelling(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Skip("needs a case-insensitive file system")
	}
	e := newTestEnv(t)
	web := e.addVM(t, "web", simpleVMX("web"))
	e.setRunning(web)
	for _, ref := range []string{"WEB", strings.ToUpper(web)} {
		m, err := e.Get(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if m.ID != web || m.State != vm.StateRunning {
			t.Errorf("Get(%q) = %s %s, want %s running", ref, m.ID, m.State, web)
		}
	}
}

func TestRunningDetectionFollowsLinks(t *testing.T) {
	e := newTestEnv(t)
	web := e.addVM(t, "web", simpleVMX("web"))
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Dir(web), alias); err != nil {
		t.Skipf("cannot create a directory link: %v", err)
	}
	e.setRunning(filepath.Join(alias, "web.vmx"))
	m, err := e.Get(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if m.State != vm.StateRunning {
		t.Fatalf("state = %s, want running", m.State)
	}
	machines, err := e.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(machines) != 1 {
		t.Fatalf("the same vm was listed twice: %+v", machines)
	}
}

func TestGetReadsVMwareWrittenFiles(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", fixture(t, "vmx/vmware-written.vmx"))
	writeFile(t, filepath.Join(e.root, "web", "web.vmsd"), fixture(t, "vmsd/tree.vmsd"))
	m, err := e.Get(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != path || m.ConfigPath != path || m.Name != "web" || m.Provider != vm.VMware {
		t.Fatalf("identity = %+v", m)
	}
	if m.State != vm.StateSaved {
		t.Errorf("state = %s, want saved because checkpoint.vmState is set", m.State)
	}
	if m.OSType != "other5xlinux-64" || m.CPUs != 1 || m.MemoryMB != 128 || m.Firmware != "bios" {
		t.Errorf("hardware = %+v", m)
	}
	if want := []vm.NIC{{Mode: vm.NetNAT, Model: "e1000e", MAC: "00:0c:29:2a:0c:99"}}; !slices.Equal(m.NICs, want) {
		t.Errorf("nics = %+v", m.NICs)
	}
	if m.Managed || len(m.Meta) != 0 || m.Labels != nil {
		t.Errorf("vmh keys inside the vmx must be ignored: meta = %v", m.Meta)
	}
	if m.CurrentSnapshot != "s1" {
		t.Errorf("current snapshot = %q", m.CurrentSnapshot)
	}

	writeJSON(t, metaPath(path), map[string]string{"managed": path, "label.team": `a"b|c`, "note": `C:\path`, "empty": ""})
	m, err = e.Get(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Managed || m.Labels["team"] != `a"b|c` || m.Meta["note"] != `C:\path` || len(m.Meta) != 3 {
		t.Errorf("meta = %v labels = %v", m.Meta, m.Labels)
	}

	writeJSON(t, metaPath(path), map[string]string{"managed": filepath.Join(t.TempDir(), "x.vmx")})
	if m, err = e.Get(context.Background(), "web"); err != nil || m.Managed {
		t.Errorf("a marker naming another vmx must not make the vm managed: %+v, %v", m, err)
	}
	writeFile(t, metaPath(path), `{"managed": 1}`)
	if _, err := e.Get(context.Background(), "web"); err == nil {
		t.Error("a corrupt metadata file must be reported")
	}
}

func TestConsoleLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "web.vmx")
	abs := filepath.Join(t.TempDir(), "console.txt")
	port := func(n int, present, fileType, file string) string {
		return fmt.Sprintf("serial%[1]d.present = \"%[2]s\"\nserial%[1]d.fileType = \"%[3]s\"\nserial%[1]d.fileName = \"%[4]s\"\n", n, present, fileType, file)
	}
	cases := []struct {
		name, vmx, want string
	}{
		{"none", simpleVMX("web"), ""},
		{"relative", port(0, "TRUE", "file", consoleFile), filepath.Join(dir, consoleFile)},
		{"absolute", port(0, "TRUE", "file", abs), abs},
		{"not present", port(0, "FALSE", "file", consoleFile), ""},
		{"disconnected", port(0, "TRUE", "file", consoleFile) + "serial0.startConnected = \"FALSE\"\n", ""},
		{"no file name", port(0, "TRUE", "file", ""), ""},
		{"pipe then file", port(0, "TRUE", "pipe", "pipe-name") + port(3, "true", "FILE", "com4.log"), filepath.Join(dir, "com4.log")},
		{"beyond the last port", port(4, "TRUE", "file", consoleFile), ""},
	}
	for _, c := range cases {
		if got := consoleLog(path, parseVMX([]byte(c.vmx))); got != c.want {
			t.Errorf("%s: console log = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestStateDetection(t *testing.T) {
	e := newTestEnv(t)
	running := e.addVM(t, "running", simpleVMX("running"))
	paused := e.addVM(t, "paused", simpleVMX("paused"))
	writeFile(t, pausedMarker(paused), "")
	suspended := e.addVM(t, "suspended", simpleVMX("suspended")+"checkpoint.vmState = \"suspended-5cf61512.vmss\"\r\n")
	writeFile(t, filepath.Join(e.root, "suspended", "suspended-5cf61512.vmss"), "")
	e.addVM(t, "stale", simpleVMX("stale"))
	writeFile(t, filepath.Join(e.root, "stale", "stale-5cf61512.vmss"), "")
	off := e.addVM(t, "off", simpleVMX("off"))
	writeFile(t, pausedMarker(off), "")
	writeFile(t, filepath.Join(e.root, "off", "other-5cf61512.vmss"), "")
	listed := running
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		listed = strings.ToUpper(running)
	}
	e.setRunning(listed, paused)
	want := map[string]vm.State{"running": vm.StateRunning, "paused": vm.StatePaused, "suspended": vm.StateSaved, "stale": vm.StateStopped, "off": vm.StateStopped}
	for name, state := range want {
		m, err := e.Get(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		if m.State != state {
			t.Errorf("%s: state = %s, want %s", name, m.State, state)
		}
	}
	if m, _ := e.Get(context.Background(), suspended); m.Name != "suspended" {
		t.Errorf("get by path = %+v", m)
	}
}

func TestList(t *testing.T) {
	e := newTestEnv(t)
	web := e.addVM(t, "web", simpleVMX("web"))
	api := e.addVM(t, "api", simpleVMX("api"))
	writeFile(t, filepath.Join(e.root, "notes", "readme.txt"), "not a vm")
	lab := writeFile(t, filepath.Join(t.TempDir(), "lab", "lab.vmx"), simpleVMX("lab"))
	broken := writeFile(t, filepath.Join(t.TempDir(), "broken", "broken.vmx"), simpleVMX("broken"))
	writeFile(t, metaPath(broken), "{")
	writeFile(t, e.inventory, "vmlist1.config = \""+web+"\"\nvmlist1.DisplayName = \"web\"\nvmlist2.config = \""+lab+"\"\nvmlist2.DisplayName = \"lab\"\nvmlist3.config = \""+broken+"\"\nvmlist4.config = \"relative.vmx\"\n")
	e.setRunning(api, filepath.Join(t.TempDir(), "ghost", "ghost.vmx"))
	machines, err := e.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]vm.State{}
	for _, m := range machines {
		got[m.Name] = m.State
	}
	want := map[string]vm.State{"web": vm.StateStopped, "api": vm.StateRunning, "lab": vm.StateStopped}
	if len(machines) != len(want) {
		t.Fatalf("machines = %+v", machines)
	}
	for name, state := range want {
		if got[name] != state {
			t.Errorf("%s: state = %q, want %s", name, got[name], state)
		}
	}
}

func TestUpdate(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web"))
	writeFile(t, metaPath(path), `{"label.team": "red"}`)
	ctx := context.Background()
	if err := e.Update(ctx, "web", vm.Changes{CPUs: 4, MemoryMB: 2048, Labels: map[string]string{"env": "prod", "team": ""}}); err != nil {
		t.Fatal(err)
	}
	v, err := readVMX(path)
	if err != nil {
		t.Fatal(err)
	}
	if v.get("numvcpus") != "4" || v.get("cpuid.coresPerSocket") != "4" || v.get("memsize") != "2048" {
		t.Errorf("hardware not updated:\n%s", v.encode())
	}
	if meta := readFile(t, metaPath(path)); meta != `{"label.team": "red"}` {
		t.Errorf("update must leave labels to SetMeta, metadata = %s", meta)
	}
	if strings.Contains(readFile(t, path), "label.") {
		t.Errorf("labels leaked into the vmx:\n%s", readFile(t, path))
	}
	wantKind(t, e.Update(ctx, "web", vm.Changes{MemoryMB: 1001}), vm.ErrInvalid)

	e.setRunning(path)
	before := readFile(t, path)
	wantKind(t, e.Update(ctx, "web", vm.Changes{CPUs: 1}), vm.ErrInvalidState)
	if err := e.Update(ctx, "web", vm.Changes{Labels: map[string]string{"a": "b"}}); err != nil {
		t.Fatal(err)
	}
	if readFile(t, path) != before || readFile(t, metaPath(path)) != `{"label.team": "red"}` {
		t.Fatal("an update with only labels must change nothing")
	}

	e.addVM(t, "saved", simpleVMX("saved")+"checkpoint.vmState = \"saved-1.vmss\"\r\n")
	e.setRunning()
	wantKind(t, e.Update(ctx, "saved", vm.Changes{CPUs: 1}), vm.ErrInvalidState)
}

func TestSetMeta(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web"))
	ctx := context.Background()
	before := readFile(t, path)
	e.setRunning(path)
	if err := e.SetMeta(ctx, "web", map[string]string{vm.MetaManaged: path, vm.MetaSSHUser: "vmh"}); err != nil {
		t.Fatal(err)
	}
	m, err := e.Get(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Managed || m.Meta[vm.MetaSSHUser] != "vmh" {
		t.Fatalf("meta = %v", m.Meta)
	}
	if readFile(t, path) != before {
		t.Fatal("metadata must not be written into the vmx")
	}
	if err := e.SetMeta(ctx, "web", map[string]string{vm.MetaManaged: ""}); err != nil {
		t.Fatal(err)
	}
	if meta := storedMeta(t, path); len(meta) != 1 || meta[vm.MetaSSHUser] != "vmh" {
		t.Fatalf("meta = %v", meta)
	}
	if strings.Contains(readFile(t, metaPath(path)), vm.MetaManaged) {
		t.Fatal("an empty value must delete the key")
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()

	t.Run("stopped vm in root", func(t *testing.T) {
		e := newTestEnv(t)
		path := e.addVM(t, "web", simpleVMX("web"))
		writeFile(t, filepath.Join(e.root, "web", seedFile), "seed")
		e.fake.On("-T ws deleteVM", "")
		if err := e.Delete(ctx, "web"); err != nil {
			t.Fatal(err)
		}
		if !e.fake.Called("-T ws deleteVM " + path) {
			t.Fatalf("calls = %v", e.commands())
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Fatalf("vm dir left behind: %v", err)
		}
	})

	t.Run("running vm", func(t *testing.T) {
		e := newTestEnv(t)
		path := e.addVM(t, "web", simpleVMX("web"))
		e.setRunning(path)
		wantKind(t, e.Delete(ctx, "web"), vm.ErrInvalidState)
		if e.fake.Called("-T ws deleteVM") {
			t.Fatal("deleteVM must not run for a running vm")
		}
	})

	t.Run("vm outside root keeps its folder", func(t *testing.T) {
		e := newTestEnv(t)
		path := writeFile(t, filepath.Join(t.TempDir(), "lab", "lab.vmx"), simpleVMX("lab"))
		writeFile(t, metaPath(path), "{}")
		writeFile(t, pausedMarker(path), "")
		notes := writeFile(t, filepath.Join(filepath.Dir(path), "notes.txt"), "keep")
		writeFile(t, e.inventory, "vmlist1.config = \""+path+"\"\n")
		e.fake.On("-T ws deleteVM", "")
		if err := e.Delete(ctx, path); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("files outside root must be left to vmrun: %v", err)
		}
		if isFile(metaPath(path)) || isFile(pausedMarker(path)) || !isFile(notes) {
			t.Fatal("only the vmh files may be removed next to a vm outside root")
		}
	})

	t.Run("vm outside root sharing its folder", func(t *testing.T) {
		e := newTestEnv(t)
		dir := t.TempDir()
		path := writeFile(t, filepath.Join(dir, "evil.vmx"), simpleVMX("evil")+"sata0:0.fileName = \"user.vmdk\"\r\n")
		writeFile(t, filepath.Join(dir, "user.vmx"), simpleVMX("user"))
		writeFile(t, e.inventory, "vmlist1.config = \""+path+"\"\n")
		e.fake.On("-T ws deleteVM", "")
		wantKind(t, e.Delete(ctx, path), vm.ErrForbidden)
		if e.fake.Called("-T ws deleteVM") {
			t.Fatal("deleteVM must not run")
		}
	})

	t.Run("unknown vm outside root", func(t *testing.T) {
		e := newTestEnv(t)
		path := writeFile(t, filepath.Join(t.TempDir(), "lab", "lab.vmx"), simpleVMX("lab"))
		e.fake.On("-T ws deleteVM", "")
		wantKind(t, e.Delete(ctx, path), vm.ErrNotFound)
		if e.fake.Called("-T ws deleteVM") {
			t.Fatal("deleteVM must not run")
		}
	})

	t.Run("unreadable vm in root", func(t *testing.T) {
		e := newTestEnv(t)
		path := e.addVM(t, "broken", "garbage = = \"\n")
		e.fake.OnResult("-T ws deleteVM", failed(fixture(t, "vmrun/cannot-read-config.txt")))
		if err := e.Delete(ctx, "broken"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Fatal("broken vm folder should be removed")
		}
	})

	t.Run("failure keeps files", func(t *testing.T) {
		e := newTestEnv(t)
		path := e.addVM(t, "web", simpleVMX("web"))
		e.fake.OnResult("-T ws deleteVM", failed(fixture(t, "vmrun/already-running.txt")))
		wantKind(t, e.Delete(ctx, "web"), vm.ErrInvalidState)
		if _, err := os.Stat(path); err != nil {
			t.Fatal("files must stay when deleteVM fails")
		}
	})

	t.Run("base of linked clones", func(t *testing.T) {
		e := newTestEnv(t)
		e.addVM(t, "base", simpleVMX("base"))
		clone := e.addVM(t, "child", simpleVMX("child"))
		writeFile(t, filepath.Join(e.root, "base", "base.vmsd"), linkedVMSD(clone, filepath.Join(e.root, "gone", "gone.vmx")))
		err := e.Delete(ctx, "base")
		wantKind(t, err, vm.ErrInvalidState)
		if !strings.Contains(err.Error(), clone) || strings.Contains(err.Error(), "gone.vmx") {
			t.Fatalf("err = %v", err)
		}
		if e.fake.Called("-T ws deleteVM") {
			t.Fatal("deleteVM must not run")
		}
	})
}

func linkedVMSD(baseClone, childClone string) string {
	return strings.Join([]string{
		`.encoding = "windows-1251"`,
		`snapshot.lastUID = "2"`,
		`snapshot.current = "2"`,
		`snapshot0.uid = "1"`,
		`snapshot0.displayName = "base"`,
		`snapshot0.clone0 = "` + baseClone + `"`,
		`snapshot0.numClones = "1"`,
		`snapshot.numSnapshots = "2"`,
		`snapshot1.uid = "2"`,
		`snapshot1.parent = "1"`,
		`snapshot1.displayName = "Clone"`,
		`snapshot1.description = "Created by clone operation."`,
		`snapshot1.clone0 = "` + childClone + `"`,
		`snapshot1.numClones = "1"`,
		"",
	}, "\r\n")
}
