package vmware

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fl4metf/vm-harness/cloudinit"
	"github.com/fl4metf/vm-harness/internal/fakerun"
	"github.com/fl4metf/vm-harness/internal/iso9660"
	"github.com/fl4metf/vm-harness/runner"
	"github.com/fl4metf/vm-harness/vm"
)

func cloneWritesVMX(t *testing.T) func(fakerun.Call) (runner.Result, error) {
	return func(c fakerun.Call) (runner.Result, error) {
		src, dst := c.Args[3], c.Args[4]
		data := strings.ReplaceAll(readFile(t, src), `displayName = "web"`, `displayName = "copy"`)
		writeFile(t, strings.TrimSuffix(dst, ".vmx")+"-cl1.vmdk", "disk")
		return runner.Result{}, os.WriteFile(dst, []byte(data), 0o644)
	}
}

func seededVMX() string {
	return simpleVMX("web") +
		"sata0:1.present = \"TRUE\"\r\nsata0:1.deviceType = \"cdrom-image\"\r\nsata0:1.fileName = \"seed.iso\"\r\n" +
		"sata0:2.present = \"TRUE\"\r\nsata0:2.deviceType = \"cdrom-image\"\r\nsata0:2.fileName = \"C:\\iso\\tools.iso\"\r\n"
}

func writeTestSeed(t *testing.T, path string) {
	t.Helper()
	ci := vm.CloudInit{User: "vmh", Password: "secret", NetworkConfig: "version: 2\n"}
	if err := cloudinit.WriteSeed(path, ci, "vmh-web", "web"); err != nil {
		t.Fatal(err)
	}
}

func readSeed(t *testing.T, path string) map[string][]byte {
	t.Helper()
	img, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, files, err := iso9660.Read(img)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func checkReseeded(t *testing.T, src, dst, name string) {
	t.Helper()
	from, to := readSeed(t, src), readSeed(t, dst)
	if string(to["user-data"]) != string(from["user-data"]) || string(to["network-config"]) != string(from["network-config"]) {
		t.Errorf("clone seed lost the source user data:\n%s", to["user-data"])
	}
	meta := string(to["meta-data"])
	if !strings.Contains(meta, `instance-id: "vmh-`+name+`"`) || !strings.Contains(meta, `local-hostname: "`+name+`"`) {
		t.Errorf("clone seed keeps the source identity:\n%s", meta)
	}
}

func offlineVMSD() string {
	return strings.Join([]string{
		`.encoding = "windows-1251"`,
		`snapshot.lastUID = "2"`,
		`snapshot.current = "2"`,
		`snapshot0.uid = "1"`,
		`snapshot0.displayName = "live"`,
		`snapshot0.type = "1"`,
		`snapshot.numSnapshots = "2"`,
		`snapshot1.uid = "2"`,
		`snapshot1.parent = "1"`,
		`snapshot1.displayName = "s2 with space"`,
		"",
	}, "\r\n")
}

func TestFullClone(t *testing.T) {
	e := newTestEnv(t)
	src := e.addVM(t, "web", seededVMX())
	writeFile(t, metaPath(src), `{"managed": "x", "label.team": "red"}`)
	srcSeed := filepath.Join(e.root, "web", seedFile)
	writeTestSeed(t, srcSeed)
	e.fake.OnFunc("-T ws clone", cloneWritesVMX(t))
	m, err := e.Clone(context.Background(), "web", vm.CloneOptions{Name: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(e.root, "copy", "copy.vmx")
	if want := "-T ws clone " + src + " " + dst + " full -cloneName=copy"; !e.fake.Called(want) {
		t.Fatalf("calls = %q, want %q", e.commands(), want)
	}
	checkReseeded(t, srcSeed, filepath.Join(e.root, "copy", seedFile), "copy")
	v, err := readVMX(dst)
	if err != nil {
		t.Fatal(err)
	}
	if v.get("sata0:1.fileName") != seedFile || v.get("sata0:2.fileName") != `C:\iso\tools.iso` {
		t.Fatalf("media paths changed:\n%s", v.encode())
	}
	if v.get("msg.autoAnswer") != "TRUE" || v.get("answer.msg.serial.file.open") != "" {
		t.Fatalf("a clone must answer questions on its own and needs no serial answer without serial files:\n%s", v.encode())
	}
	if m.ID != dst || m.Name != "copy" || m.Managed || len(m.Meta) != 0 {
		t.Fatalf("the clone must start without metadata: %+v", m)
	}
}

func TestCloneFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows uses access control lists instead of mode bits")
	}
	e := newTestEnv(t)
	e.addVM(t, "web", seededVMX())
	writeTestSeed(t, filepath.Join(e.root, "web", seedFile))
	e.fake.OnFunc("-T ws clone", cloneWritesVMX(t))
	if _, err := e.Clone(context.Background(), "web", vm.CloneOptions{Name: "copy"}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{filepath.Join(e.root, "copy"): 0o700, filepath.Join(e.root, "copy", seedFile): 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
}

func TestCloneReseedsSeedReferencedByAbsolutePath(t *testing.T) {
	e := newTestEnv(t)
	seed := filepath.Join(e.root, "web", seedFile)
	e.addVM(t, "web", simpleVMX("web")+"sata0:1.deviceType = \"cdrom-image\"\r\nsata0:1.fileName = \""+seed+"\"\r\n")
	writeTestSeed(t, seed)
	e.fake.OnFunc("-T ws clone", cloneWritesVMX(t))
	if _, err := e.Clone(context.Background(), "web", vm.CloneOptions{Name: "copy"}); err != nil {
		t.Fatal(err)
	}
	v, err := readVMX(filepath.Join(e.root, "copy", "copy.vmx"))
	if err != nil {
		t.Fatal(err)
	}
	if v.get("sata0:1.fileName") != seedFile {
		t.Fatalf("clone still points at the source seed:\n%s", v.encode())
	}
	checkReseeded(t, seed, filepath.Join(e.root, "copy", seedFile), "copy")
}

func TestCloneWritesConsoleToOwnFolder(t *testing.T) {
	e := newTestEnv(t)
	src := e.addVM(t, "web", simpleVMX("web"))
	outside := filepath.Join(t.TempDir(), "com2.log")
	ports := strings.Join([]string{
		`serial0.present = "TRUE"`,
		`serial0.fileType = "file"`,
		`serial0.fileName = "` + filepath.Join(e.root, "web", consoleFile) + `"`,
		`serial1.present = "TRUE"`,
		`serial1.fileType = "file"`,
		`serial1.fileName = "` + outside + `"`,
		`serial2.present = "TRUE"`,
		`serial2.fileType = "pipe"`,
		`serial2.fileName = "pipe-name"`,
		"",
	}, "\r\n")
	writeFile(t, src, simpleVMX("web")+ports)
	e.fake.OnFunc("-T ws clone", cloneWritesVMX(t))
	m, err := e.Clone(context.Background(), "web", vm.CloneOptions{Name: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.root, "copy")
	if want := filepath.Join(dir, consoleFile); m.ConsoleLog != want {
		t.Errorf("console log = %q, want %q", m.ConsoleLog, want)
	}
	v, err := readVMX(m.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{
		"serial0.fileName":            consoleFile,
		"serial1.fileName":            "com2.log",
		"serial2.fileName":            "pipe-name",
		"answer.msg.serial.file.open": "Replace",
		"msg.autoAnswer":              "TRUE",
	}
	for key, want := range keys {
		if got := v.get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if m, err := e.Get(context.Background(), "web"); err != nil || m.ConsoleLog != filepath.Join(e.root, "web", consoleFile) {
		t.Errorf("source console = %q, %v", m.ConsoleLog, err)
	}
}

func TestLinkedClone(t *testing.T) {
	e := newTestEnv(t)
	src := e.addVM(t, "web", simpleVMX("web"))
	writeFile(t, filepath.Join(e.root, "web", "web.vmsd"), offlineVMSD())
	e.fake.OnFunc("-T ws clone", cloneWritesVMX(t))
	ctx := context.Background()
	if _, err := e.Clone(ctx, "web", vm.CloneOptions{Name: "copy", Linked: true}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(e.root, "copy", "copy.vmx")
	if want := "-T ws clone " + src + " " + dst + " linked -snapshot=s2 with space -cloneName=copy"; !e.fake.Called(want) {
		t.Fatalf("calls = %q, want %q", e.commands(), want)
	}
	e.setRunning(src)
	if _, err := e.Clone(ctx, "web", vm.CloneOptions{Name: "other", Linked: true, Snapshot: "S2 WITH SPACE"}); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(e.root, "other", "other.vmx")
	if want := "-T ws clone " + src + " " + other + " linked -snapshot=s2 with space -cloneName=other"; !e.fake.Called(want) {
		t.Fatalf("a snapshot taken while off can be cloned from a running vm: %q", e.commands())
	}
}

func TestCloneRefusesWhatVmrunCannotClone(t *testing.T) {
	cases := []struct {
		name    string
		opts    vm.CloneOptions
		running bool
		paused  bool
		kind    error
	}{
		{"running source", vm.CloneOptions{}, true, false, vm.ErrInvalidState},
		{"paused source", vm.CloneOptions{}, true, true, vm.ErrInvalidState},
		{"snapshot taken while running", vm.CloneOptions{Linked: true, Snapshot: "live"}, false, false, vm.ErrInvalidState},
		{"full clone of a live snapshot", vm.CloneOptions{Snapshot: "LIVE"}, false, false, vm.ErrInvalidState},
		{"missing snapshot", vm.CloneOptions{Linked: true, Snapshot: "nope"}, false, false, vm.ErrNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			src := e.addVM(t, "web", simpleVMX("web"))
			writeFile(t, filepath.Join(e.root, "web", "web.vmsd"), offlineVMSD())
			if c.running {
				e.setRunning(src)
			}
			if c.paused {
				writeFile(t, pausedMarker(src), "")
			}
			e.fake.OnFunc("-T ws clone", cloneWritesVMX(t))
			c.opts.Name = "copy"
			_, err := e.Clone(context.Background(), "web", c.opts)
			wantKind(t, err, c.kind)
			if e.fake.Called("-T ws clone") {
				t.Fatal("vmrun clone must not run")
			}
			if _, err := os.Stat(filepath.Join(e.root, "copy")); !os.IsNotExist(err) {
				t.Fatal("clone folder created")
			}
		})
	}
}

func TestCloneOfVMNeverOpenedInVMware(t *testing.T) {
	e := newTestEnv(t)
	src := writeFile(t, filepath.Join(t.TempDir(), "lab", "lab.vmx"), simpleVMX("lab"))
	writeFile(t, e.inventory, "vmlist1.config = \""+src+"\"\nvmlist1.DisplayName = \"lab\"\n")
	e.fake.OnFunc("-T ws clone", cloneWritesVMX(t))
	ctx := context.Background()
	_, err := e.Clone(ctx, "lab", vm.CloneOptions{Name: "copy"})
	wantKind(t, err, vm.ErrInvalidState)
	if e.fake.Called("-T ws clone") {
		t.Fatal("vmrun clone would rewrite the source vmx")
	}
	writeFile(t, src, simpleVMX("lab")+"extendedConfigFile = \"lab.vmxf\"\r\n")
	if _, err := e.Clone(ctx, "lab", vm.CloneOptions{Name: "copy"}); err != nil {
		t.Fatal(err)
	}
}

func TestLinkedCloneNeedsSnapshot(t *testing.T) {
	e := newTestEnv(t)
	e.addVM(t, "web", simpleVMX("web"))
	_, err := e.Clone(context.Background(), "web", vm.CloneOptions{Name: "copy", Linked: true})
	wantKind(t, err, vm.ErrInvalidState)
	if _, err := os.Stat(filepath.Join(e.root, "copy")); !os.IsNotExist(err) {
		t.Fatal("clone folder created")
	}
	if e.fake.Called("-T ws clone") {
		t.Fatal("vmrun clone must not run")
	}
}

func TestCloneFailures(t *testing.T) {
	e := newTestEnv(t)
	e.addVM(t, "web", simpleVMX("web"))
	e.addVM(t, "taken", simpleVMX("taken"))
	ctx := context.Background()

	_, err := e.Clone(ctx, "web", vm.CloneOptions{Name: "taken"})
	wantKind(t, err, vm.ErrExists)
	_, err = e.Clone(ctx, "web", vm.CloneOptions{Name: "a/b"})
	wantKind(t, err, vm.ErrInvalid)
	_, err = e.Clone(ctx, "missing", vm.CloneOptions{Name: "copy"})
	wantKind(t, err, vm.ErrNotFound)

	e.fake.OnResult("-T ws clone", failed(fixture(t, "vmrun/already-running.txt")))
	_, err = e.Clone(ctx, "web", vm.CloneOptions{Name: "copy"})
	wantKind(t, err, vm.ErrInvalidState)
	if _, err := os.Stat(filepath.Join(e.root, "copy")); !os.IsNotExist(err) {
		t.Fatal("failed clone left its folder")
	}
}
