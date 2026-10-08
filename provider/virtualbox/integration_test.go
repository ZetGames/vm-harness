package virtualbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fl4metf/vm-harness/cloudinit"
	"github.com/fl4metf/vm-harness/internal/hostinfo"
	"github.com/fl4metf/vm-harness/runner"
	"github.com/fl4metf/vm-harness/vm"
)

func integrationProvider(t *testing.T) *Provider {
	t.Helper()
	if os.Getenv("VMH_INTEGRATION") != "1" {
		t.Skip("set VMH_INTEGRATION=1 to run against the real VBoxManage")
	}
	p := New(Options{Root: t.TempDir()})
	if _, err := p.Info(context.Background()); err != nil {
		t.Fatalf("VBoxManage is not usable: %v", err)
	}
	return p
}

func TestIntegration(t *testing.T) {
	p := integrationProvider(t)
	ctx := context.Background()
	root := p.root

	name := "vmh-it-" + randomSuffix(t)
	linkedName, fullName, importName, cancelName := name+"-linked", name+"-full", name+"-imp", name+"-cancel"
	t.Cleanup(func() {
		for _, n := range []string{linkedName, fullName, importName, cancelName, name} {
			m, err := p.Get(ctx, n)
			if err != nil {
				continue
			}
			if m.State != vm.StateStopped {
				p.Stop(ctx, m.ID, true)
			}
			if err := p.Delete(ctx, m.ID); err != nil {
				t.Errorf("cleanup of %s: %v", n, err)
			}
		}
	})

	iso := filepath.Join(t.TempDir(), "install.iso")
	if err := cloudinit.WriteSeed(iso, vm.CloudInit{User: "vmh"}, "install", ""); err != nil {
		t.Fatal(err)
	}
	sshPort := freePort(t)
	m, err := p.Create(ctx, vm.Spec{
		Name: name, OSType: "Ubuntu_64", CPUs: 1, MemoryMB: 128, DiskGB: 1, ISO: iso,
		Labels:       map[string]string{"team": "qa"},
		Meta:         map[string]string{vm.MetaOSType: "ubuntu"},
		PortForwards: []vm.PortForward{{Name: "ssh", Protocol: "tcp", HostIP: "127.0.0.1", HostPort: sshPort, GuestPort: 22}},
		CloudInit:    &vm.CloudInit{User: "vmh", Password: "vmh"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if m.Managed || m.Labels["team"] != "qa" || m.Meta[vm.MetaOSType] != "ubuntu" || m.OSType != "Ubuntu_64" ||
		m.CPUs != 1 || m.MemoryMB != 128 || m.State != vm.StateStopped || len(m.NICs) != 1 || m.NICs[0].Mode != vm.NetNAT || m.NICs[0].Model != "virtio" {
		t.Fatalf("created machine = %+v", m)
	}
	if info, err := p.inspect(ctx, m.ID); err != nil || info.fields["paravirtprovider"] != "default" {
		t.Fatalf("paravirt provider = %q, %v", info.fields["paravirtprovider"], err)
	}
	if !hasForward(m, "ssh") {
		t.Fatalf("forwards = %+v", m.PortForwards)
	}
	dir := filepath.Join(root, name)
	if _, err := os.Stat(filepath.Join(dir, seedName)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !samePath(m.ConsoleLog, filepath.Join(dir, serialName)) {
		t.Fatalf("console log = %q", m.ConsoleLog)
	}
	if err := p.SetMeta(ctx, m.ID, map[string]string{vm.MetaManaged: m.ID}); err != nil {
		t.Fatalf("tag: %v", err)
	}
	if m, err = p.Get(ctx, m.ID); err != nil || !m.Managed {
		t.Fatalf("tagged machine = %+v, %v", m, err)
	}

	byName, err := p.Get(ctx, name)
	if err != nil || byName.ID != m.ID {
		t.Fatalf("get by name = %+v, %v", byName, err)
	}
	all, err := p.List(ctx)
	if err != nil || !slices.ContainsFunc(all, func(x vm.Machine) bool { return x.ID == m.ID }) {
		t.Fatalf("list = %v, %v", all, err)
	}
	if _, err := p.Create(ctx, vm.Spec{Name: name, DiskGB: 1}); !errors.Is(err, vm.ErrExists) {
		t.Fatalf("duplicate create: %v", err)
	}

	if err := p.Start(ctx, m.ID, false); err != nil {
		t.Fatalf("start: %v", err)
	}
	expectState(t, p, m.ID, vm.StateRunning)
	shot, err := p.Screenshot(ctx, m.ID, vm.Credentials{})
	if err != nil || !bytes.HasPrefix(shot, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("screenshot: %d bytes, %v", len(shot), err)
	}
	if _, err := p.GuestIP(ctx, m.ID); !errors.Is(err, vm.ErrNotReady) {
		t.Fatalf("guest ip without additions: %v", err)
	}
	if _, err := p.Exec(ctx, m.ID, vm.ExecRequest{Command: []string{"/bin/true"}, Credentials: vm.Credentials{User: "vmh", Password: "vmh"}}); !errors.Is(err, vm.ErrNotReady) {
		t.Fatalf("exec without additions: %v", err)
	}
	if err := p.Delete(ctx, m.ID); !errors.Is(err, vm.ErrInvalidState) {
		t.Fatalf("delete while running: %v", err)
	}
	if err := p.TakeSnapshot(ctx, m.ID, "s1", "live snapshot"); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	snaps, err := p.Snapshots(ctx, m.ID)
	if err != nil || len(snaps) != 1 || snaps[0].Name != "s1" || !snaps[0].Current || snaps[0].Description != "live snapshot" {
		t.Fatalf("snapshots = %+v, %v", snaps, err)
	}
	if err := p.TakeSnapshot(ctx, m.ID, "s1", ""); !errors.Is(err, vm.ErrExists) {
		t.Fatalf("duplicate snapshot: %v", err)
	}
	checkForwardRoundTrip(t, p, m.ID)

	if err := p.Stop(ctx, m.ID, true); err != nil {
		t.Fatalf("stop: %v", err)
	}
	expectState(t, p, m.ID, vm.StateStopped)
	if err := p.RestoreSnapshot(ctx, m.ID, "s1"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	expectState(t, p, m.ID, vm.StateSaved)
	if err := p.Stop(ctx, m.ID, true); err != nil {
		t.Fatalf("force stop from saved: %v", err)
	}
	expectState(t, p, m.ID, vm.StateStopped)
	if err := p.RestoreSnapshot(ctx, m.ID, "s1"); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	if err := p.RestoreSnapshot(ctx, m.ID, "nosuch"); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("restore missing snapshot: %v", err)
	}
	if err := p.Start(ctx, m.ID, false); err != nil {
		t.Fatalf("start from saved: %v", err)
	}
	if err := p.Stop(ctx, m.ID, true); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	expectState(t, p, m.ID, vm.StateStopped)
	checkForwardRoundTrip(t, p, m.ID)

	if err := p.SetMeta(ctx, m.ID, map[string]string{vm.LabelKey("env"): "ci", vm.LabelKey("team"): ""}); err != nil {
		t.Fatalf("set meta: %v", err)
	}
	if err := p.Update(ctx, m.ID, vm.Changes{CPUs: 2, MemoryMB: 192}); err != nil {
		t.Fatalf("update: %v", err)
	}
	m, err = p.Get(ctx, m.ID)
	if err != nil || m.CPUs != 2 || m.MemoryMB != 192 || len(m.Labels) != 1 || m.Labels["env"] != "ci" {
		t.Fatalf("after update = %+v, %v", m, err)
	}

	linked, err := p.Clone(ctx, m.ID, vm.CloneOptions{Name: linkedName, Linked: true})
	if err != nil {
		t.Fatalf("linked clone: %v", err)
	}
	full, err := p.Clone(ctx, m.ID, vm.CloneOptions{Name: fullName})
	if err != nil {
		t.Fatalf("full clone: %v", err)
	}
	for _, c := range []vm.Machine{linked, full} {
		checkClone(t, p, c, filepath.Join(root, c.Name))
	}
	if err := p.Start(ctx, full.ID, false); err != nil {
		t.Fatalf("start full clone: %v", err)
	}
	if err := p.Stop(ctx, full.ID, true); err != nil {
		t.Fatalf("stop full clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, fullName, serialName)); err != nil {
		t.Fatalf("clone serial log: %v", err)
	}
	checkCancelledClone(t, p, m.ID, cancelName)

	imported := checkImport(t, p, m, importName)
	checkCancelledImport(t, p, m, cancelName)

	for _, c := range []vm.Machine{linked, full, imported, m} {
		if err := p.Delete(ctx, c.ID); err != nil {
			t.Fatalf("delete %s: %v", c.Name, err)
		}
		if _, err := p.Get(ctx, c.ID); !errors.Is(err, vm.ErrNotFound) {
			t.Fatalf("%s still registered: %v", c.Name, err)
		}
		if _, err := os.Stat(filepath.Join(root, c.Name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left files behind: %v", c.Name, err)
		}
	}
	for _, list := range []string{"vms", "hdds", "dvds"} {
		out, err := p.run(ctx, "list", list)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, name) || strings.Contains(strings.ToLower(out), strings.ToLower(root)) || strings.Contains(out, iso) {
			t.Errorf("list %s still mentions the test vms:\n%s", list, out)
		}
	}
}

func TestIntegrationHostWarnings(t *testing.T) {
	p := integrationProvider(t)
	info, err := p.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	platform := hostinfo.Detect()
	hyperV := runtime.GOOS == "windows" && platform == hostinfo.HyperVRoot
	if got := slices.Contains(info.Warnings, hyperVWarning); got != hyperV {
		t.Fatalf("hyper-v warning = %v on platform %d: %q", got, platform, info.Warnings)
	}
	if want := platform != hostinfo.BareMetal && !hyperV; slices.Contains(info.Warnings, nestedWarning) != want {
		t.Fatalf("nested warning missing or unexpected on platform %d: %q", platform, info.Warnings)
	}
	if hyperV != (info.MaxReliableCPUs == 1) {
		t.Fatalf("max reliable cpus = %d on platform %d", info.MaxReliableCPUs, platform)
	}
	t.Logf("platform %d, max reliable cpus %d, warnings: %q", platform, info.MaxReliableCPUs, info.Warnings)
}

func TestIntegrationImportResetsHostSettings(t *testing.T) {
	p := integrationProvider(t)
	ctx := context.Background()
	name := "vmh-it-" + randomSuffix(t)
	t.Cleanup(func() { removeVMs(p, name) })
	host := t.TempDir()
	victim := filepath.Join(host, "victim.txt")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := name + "-src"
	serial := "VBoxInternal/Devices/serial/0/LUN#0/"
	scratchVM(t, p, src,
		[]string{"setextradata", src, serial + "Driver", "Char"},
		[]string{"setextradata", src, serial + "AttachedDriver/Driver", "RawFile"},
		[]string{"setextradata", src, serial + "AttachedDriver/Config/Location", victim},
		[]string{"setextradata", src, "VBoxInternal2/SharedFoldersEnableSymlinksCreate/share", "1"},
		[]string{"modifyvm", src, "--memory", "64", "--nic1", "nat",
			"--natpf1", "lan,tcp,127.0.0.1," + strconv.Itoa(freePort(t)) + ",,22",
			"--nic-trace1", "on", "--nic-trace-file1", filepath.Join(host, "trace.pcap"),
			"--nat-localhostreachable1", "on", "--nat-tftp-prefix1", host,
			"--vrde", "on", "--vrde-auth-type", "null", "--vrde-address", "127.0.0.1",
			"--uart2", "0x2F8", "3", "--uart-mode2", "disconnected",
			"--recording", "on", "--recording-file", filepath.Join(host, "screen.webm"),
			"--snapshot-folder", filepath.Join(host, "snaps"),
			"--clipboard-mode", "bidirectional", "--usb-ohci", "on"},
		[]string{"usbfilter", "add", "0", "--target", src, "--name", "none", "--vendorid", "ffff", "--productid", "fffe"},
	)
	m, err := p.Create(ctx, vm.Spec{Name: name + "-imp", Appliance: exportVMs(t, p, src)})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if err := p.Start(ctx, m.ID, false); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := p.Stop(ctx, m.ID, true); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if data, err := os.ReadFile(victim); err != nil || string(data) != "precious" {
		t.Fatalf("host file behind the imported serial port = %q, %v", data, err)
	}
	if entries, _ := os.ReadDir(host); len(entries) != 1 {
		t.Fatalf("imported vm wrote into %s: %v", host, entries)
	}
	info, err := p.inspect(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"vrde": "off", "recording_enabled": "off", "clipboard": "disabled", "localhostReachable": "0",
		"uart1": "off", "uart2": "off", "uart3": "off", "uart4": "off", "lpt1": "off", "lpt2": "off"}
	for key, value := range want {
		if info.fields[key] != value {
			t.Errorf("%s = %q, want %q", key, info.fields[key], value)
		}
	}
	if _, ok := info.fields["USBFilterActive1"]; ok || len(info.natRules) > 0 || !samePath(filepath.Dir(info.fields["SnapFldr"]), info.dir()) {
		t.Errorf("usb filter, forwards %v or snapshot folder %s survived the import", info.natRules, info.fields["SnapFldr"])
	}
	if out, err := p.run(ctx, "getextradata", m.ID, "enumerate"); err != nil || strings.TrimSpace(out) != "" {
		t.Errorf("extradata after import = %q, %v", out, err)
	}
	if err := p.Delete(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationImportRefusesMultiVMAppliance(t *testing.T) {
	p := integrationProvider(t)
	ctx := context.Background()
	name := "vmh-it-" + randomSuffix(t)
	t.Cleanup(func() { removeVMs(p, name) })
	scratchVM(t, p, name+"-a")
	scratchVM(t, p, name+"-b")
	ova := exportVMs(t, p, name+"-a", name+"-b")
	before, err := p.listVMs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(ctx, vm.Spec{Name: name + "-two", Appliance: ova}); !errors.Is(err, vm.ErrInvalid) {
		t.Fatalf("import of a two vm appliance: %v", err)
	}
	if after, err := p.listVMs(ctx); err != nil || !slices.Equal(after, before) {
		t.Fatalf("registered vms changed: %v -> %v, %v", before, after, err)
	}
	if _, err := os.Stat(filepath.Join(p.root, name+"-two")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused import left files behind: %v", err)
	}
}

func TestIntegrationImportRefusesForeignDisks(t *testing.T) {
	p := integrationProvider(t)
	ctx := context.Background()
	name := "vmh-it-" + randomSuffix(t)
	t.Cleanup(func() { removeVMs(p, name) })
	newDisk := func(path string) string {
		out, err := p.run(ctx, "createmedium", "disk", "--filename", path, "--size", "8", "--format", "VDI")
		if err != nil {
			t.Fatal(err)
		}
		id, _ := lineValue(out, "Medium created. UUID:")
		return id
	}
	disk, loose := filepath.Join(t.TempDir(), "user.vdi"), filepath.Join(t.TempDir(), "loose.vdi")
	diskID, looseID := newDisk(disk), newDisk(loose)
	if _, err := p.run(ctx, "closemedium", "disk", loose); err != nil {
		t.Fatal(err)
	}
	user := name + "-user"
	scratchVM(t, p, user,
		[]string{"storagectl", user, "--name", "SATA", "--add", "sata", "--portcount", "1"},
		[]string{"storageattach", user, "--storagectl", "SATA", "--port", "0", "--device", "0", "--type", "hdd", "--medium", disk})
	scratchVM(t, p, name+"-src")
	ova := exportVMs(t, p, name+"-src")
	attach := func(id string) string {
		return `<StorageControllers><StorageController name="SATA" type="AHCI" PortCount="1"><AttachedDevice type="HardDisk" port="0" device="0">` +
			`<Image uuid="{` + id + `}"/></AttachedDevice></StorageController></StorageControllers></Hardware>`
	}
	edits := map[string]func(string) string{
		"disk": func(ovf string) string { return strings.Replace(ovf, "</Hardware>", attach(diskID), 1) },
		"snapshot": func(ovf string) string {
			id := newUUID()
			start, end := strings.Index(ovf, "<Hardware>"), strings.Index(ovf, "</Hardware>")
			snapshot := `<Snapshot uuid="{` + id + `}" name="s" timeStamp="2026-01-01T00:00:00Z">` + ovf[start:end] + attach(diskID) + "</Snapshot>"
			ovf = ovf[:start] + snapshot + ovf[start:]
			return strings.Replace(ovf, "<vbox:Machine ", `<vbox:Machine currentSnapshot="{`+id+`}" `, 1)
		},
		"registry": func(ovf string) string {
			registry := `<MediaRegistry><HardDisks><HardDisk uuid="{` + looseID + `}" location="` + loose + `" format="VDI" type="Normal"/></HardDisks></MediaRegistry>`
			i := strings.Index(ovf, "<vbox:Machine ")
			i += strings.Index(ovf[i:], ">") + 1
			return strings.Replace(ovf[:i]+registry+ovf[i:], "</Hardware>", attach(looseID), 1)
		},
	}
	for kind, edit := range edits {
		vmName := name + "-" + kind
		if _, err := p.Create(ctx, vm.Spec{Name: vmName, Appliance: rewriteOVA(t, ova, edit)}); !errors.Is(err, vm.ErrInvalid) {
			t.Fatalf("import of an appliance with a foreign %s: %v", kind, err)
		}
		if _, err := p.Get(ctx, vmName); !errors.Is(err, vm.ErrNotFound) {
			t.Fatalf("refused %s import is still registered: %v", kind, err)
		}
		for _, file := range []string{disk, loose} {
			if _, err := os.Stat(file); err != nil {
				t.Fatalf("refused %s import removed a foreign disk: %v", kind, err)
			}
		}
		if _, err := os.Stat(filepath.Join(p.root, vmName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refused %s import left files behind: %v", kind, err)
		}
	}
	if out, err := p.run(ctx, "showmediuminfo", "disk", disk); err != nil || strings.Count(out, "(UUID: ") != 1 || !strings.Contains(out, user) {
		t.Fatalf("foreign disk after the refused imports: %s, %v", out, err)
	}
	if out, err := p.run(ctx, "list", "hdds"); err != nil || strings.Contains(out, looseID) {
		t.Fatalf("refused import left the loose disk registered: %v", err)
	}
}

func rewriteOVA(t *testing.T, src string, edit func(string) string) string {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	var buf bytes.Buffer
	r, w := tar.NewReader(in), tar.NewWriter(&buf)
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(h.Name, ".ovf") {
			continue
		}
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		data = []byte(edit(string(data)))
		h.Size = int64(len(data))
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	ova := filepath.Join(t.TempDir(), "crafted.ova")
	if err := os.WriteFile(ova, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return ova
}

func scratchVM(t *testing.T, p *Provider, name string, settings ...[]string) {
	t.Helper()
	ctx := context.Background()
	if _, err := p.run(ctx, "createvm", "--name", name, "--register", "--basefolder", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.run(ctx, "unregistervm", name, "--delete") })
	for _, args := range settings {
		if _, err := p.run(ctx, args...); err != nil {
			t.Fatal(err)
		}
	}
}

func exportVMs(t *testing.T, p *Provider, names ...string) string {
	t.Helper()
	ova := filepath.Join(t.TempDir(), "appliance.ova")
	if _, err := p.run(context.Background(), append(append([]string{"export"}, names...), "--output", ova)...); err != nil {
		t.Fatalf("export: %v", err)
	}
	return ova
}

func removeVMs(p *Provider, prefix string) {
	ctx := context.Background()
	entries, _ := p.listVMs(ctx)
	for _, e := range entries {
		if strings.HasPrefix(e.name, prefix) {
			p.run(ctx, "controlvm", e.id, "poweroff")
			p.run(ctx, "unregistervm", e.id, "--delete")
		}
	}
}

type cancelDuring struct {
	runner.Runner
	command string
	cancel  context.CancelFunc
}

func (r cancelDuring) Run(ctx context.Context, name string, args ...string) (runner.Result, error) {
	if len(args) > 0 && args[0] == r.command {
		time.AfterFunc(20*time.Millisecond, r.cancel)
	}
	return r.Runner.Run(ctx, name, args...)
}

func cancellingProvider(p *Provider, command string) (*Provider, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	return New(Options{VBoxManage: p.bin, Root: p.root, Runner: cancelDuring{runner.Exec{}, command, cancel}}), ctx
}

func checkCancelledClone(t *testing.T, p *Provider, src, name string) {
	t.Helper()
	cp, ctx := cancellingProvider(p, "clonevm")
	clone, err := cp.Clone(ctx, src, vm.CloneOptions{Name: name})
	checkCancelled(t, p, name, clone, err)
}

func checkCancelledImport(t *testing.T, p *Provider, src vm.Machine, name string) {
	t.Helper()
	ova := exportVMs(t, p, src.ID)
	cp, ctx := cancellingProvider(p, "import")
	m, err := cp.Create(ctx, vm.Spec{Name: name, Appliance: ova})
	checkCancelled(t, p, name, m, err)
}

func checkCancelled(t *testing.T, p *Provider, name string, m vm.Machine, err error) {
	t.Helper()
	ctx := context.Background()
	if err == nil {
		t.Logf("%s finished before the cancel", name)
		if err := p.Delete(ctx, m.ID); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled %s: %v", name, err)
	}
	if _, err := p.Get(ctx, name); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("cancelled %s is still registered: %v", name, err)
	}
	if _, err := os.Stat(filepath.Join(p.root, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled %s left files behind: %v", name, err)
	}
}

func checkImport(t *testing.T, p *Provider, src vm.Machine, name string) vm.Machine {
	t.Helper()
	ctx := context.Background()
	port := freePort(t)
	m, err := p.Create(ctx, vm.Spec{
		Name: name, Appliance: exportVMs(t, p, src.ID), CPUs: 1,
		Meta:         map[string]string{vm.MetaOSType: "ubuntu"},
		PortForwards: []vm.PortForward{{Name: "ssh", Protocol: "tcp", HostIP: "127.0.0.1", HostPort: port, GuestPort: 22}},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !samePath(m.ConfigPath, filepath.Join(p.root, name, name+".vbox")) || m.CPUs != 1 || m.Managed || m.ConsoleLog != "" {
		t.Fatalf("imported machine = %+v", m)
	}
	if want := map[string]string{vm.MetaOSType: "ubuntu"}; !reflect.DeepEqual(m.Meta, want) {
		t.Fatalf("imported meta = %v, want %v", m.Meta, want)
	}
	i := slices.IndexFunc(m.PortForwards, func(pf vm.PortForward) bool { return pf.Name == "ssh" })
	if i < 0 || m.PortForwards[i].HostPort != port {
		t.Fatalf("imported forwards = %+v", m.PortForwards)
	}
	return m
}

func checkForwardRoundTrip(t *testing.T, p *Provider, id string) {
	t.Helper()
	ctx := context.Background()
	web := vm.PortForward{Name: "web", Protocol: "tcp", HostIP: "127.0.0.1", HostPort: freePort(t), GuestPort: 80}
	if err := p.AddPortForward(ctx, id, web); err != nil {
		t.Fatalf("add forward: %v", err)
	}
	m, err := p.Get(ctx, id)
	if err != nil || !hasForward(m, "web") {
		t.Fatalf("forward missing: %+v, %v", m.PortForwards, err)
	}
	if err := p.RemovePortForward(ctx, id, "web"); err != nil {
		t.Fatalf("remove forward: %v", err)
	}
	if err := p.RemovePortForward(ctx, id, "web"); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("remove missing forward: %v", err)
	}
	m, err = p.Get(ctx, id)
	if err != nil || hasForward(m, "web") || !hasForward(m, "ssh") {
		t.Fatalf("forwards after removal: %+v, %v", m.PortForwards, err)
	}
}

func checkClone(t *testing.T, p *Provider, c vm.Machine, dir string) {
	t.Helper()
	info, err := p.inspect(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(dir, seedName)
	if !slices.ContainsFunc(info.dvdImages(), func(image string) bool { return samePath(image, seed) }) {
		t.Fatalf("%s does not use its own seed: %v", c.Name, info.dvdImages())
	}
	if data, err := os.ReadFile(seed); err != nil || !bytes.Contains(data, []byte(`"vmh-`+c.Name+`"`)) {
		t.Fatalf("%s seed was not given its own instance id: %v", c.Name, err)
	}
	if got := info.fields["uartmode1"]; !samePath(strings.TrimPrefix(got, "file,"), filepath.Join(dir, serialName)) {
		t.Fatalf("%s serial port = %q", c.Name, got)
	}
	if !samePath(c.ConsoleLog, filepath.Join(dir, serialName)) {
		t.Fatalf("%s console log = %q", c.Name, c.ConsoleLog)
	}
	if c.State != vm.StateStopped || !hasForward(c, "ssh") {
		t.Fatalf("clone = %+v", c)
	}
}

func expectState(t *testing.T, p *Provider, id string, want vm.State) {
	t.Helper()
	m, err := p.Get(context.Background(), id)
	if err != nil || m.State != want {
		t.Fatalf("state = %q, %v; want %q", m.State, err, want)
	}
}

func hasForward(m vm.Machine, name string) bool {
	return slices.ContainsFunc(m.PortForwards, func(pf vm.PortForward) bool { return pf.Name == name })
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
