package vmware

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fl4metf/vm-harness/vm"
)

func TestIntegration(t *testing.T) {
	if os.Getenv("VMH_INTEGRATION") != "1" {
		t.Skip("set VMH_INTEGRATION=1 to run against the installed vmrun")
	}
	root := t.TempDir()
	p := New(Options{Root: root})
	root = p.root
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if _, err := p.Info(ctx); errors.Is(err, vm.ErrUnavailable) {
		t.Skip("vmrun is not installed")
	}

	name := "vmh-it-" + strings.ToLower(rand.Text()[:8])
	linked, running, full, imported, mac := name+"-linked", name+"-hot", name+"-full", name+"-ova", name+"-mac"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		for _, vmName := range []string{imported, linked, running, full, mac, name} {
			path := filepath.Join(root, vmName, vmName+".vmx")
			if !isFile(path) {
				continue
			}
			p.Stop(ctx, path, true)
			if err := p.Delete(ctx, path); err != nil {
				t.Errorf("cleanup %s: %v", vmName, err)
			}
		}
		if listed, err := p.runningPaths(ctx); err == nil {
			for _, path := range listed {
				if strings.Contains(path, "vmh-it-") {
					t.Errorf("still running after cleanup: %s", path)
				}
			}
		}
	})

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	state := func(ref string) vm.State {
		t.Helper()
		m, err := p.Get(ctx, ref)
		must(err)
		return m.State
	}

	m, err := p.Create(ctx, vm.Spec{
		Name: name, OSType: vm.NativeOSType(vm.VMware, "linux"), CPUs: 1, MemoryMB: 64, DiskGB: 1,
		Labels:    map[string]string{"suite": "integration"},
		Meta:      map[string]string{vm.MetaOSType: "linux"},
		CloudInit: &vm.CloudInit{User: "vmh", SSHAuthorizedKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIntegrationOnly vmh"}},
	})
	must(err)
	path := filepath.Join(root, name, name+".vmx")
	if m.ID != path || m.State != vm.StateStopped || m.Managed || m.Labels["suite"] != "integration" || m.Meta[vm.MetaOSType] != "linux" {
		t.Fatalf("created = %+v", m)
	}
	if !isFile(filepath.Join(root, name, seedFile)) || !isFile(filepath.Join(root, name, name+".vmdk")) || !isFile(metaPath(path)) {
		t.Fatal("disk, seed or metadata file missing")
	}
	must(p.SetMeta(ctx, name, map[string]string{vm.MetaManaged: path}))
	alias := path
	if runtime.GOOS == "windows" {
		alias = strings.ToUpper(path)
	}
	if byID, err := p.Get(ctx, alias); err != nil || byID.Name != name || byID.ID != path || !byID.Managed {
		t.Fatalf("get by id = %+v, %v", byID, err)
	}
	machines, err := p.List(ctx)
	must(err)
	if !slices.ContainsFunc(machines, func(m vm.Machine) bool { return m.ID == path }) {
		t.Fatalf("list misses %s", path)
	}

	must(p.Start(ctx, name, false))
	if s := state(name); s != vm.StateRunning {
		t.Fatalf("state after start = %s", s)
	}
	must(p.Start(ctx, name, false))
	before := readFile(t, path)
	must(p.SetMeta(ctx, name, map[string]string{"stage": "running", vm.LabelKey("hot"): "yes"}))
	if readFile(t, path) != before {
		t.Fatal("vmx edited while running")
	}
	_, err = p.GuestIP(ctx, name)
	wantKind(t, err, vm.ErrNotReady)
	_, err = p.Exec(ctx, name, vm.ExecRequest{Command: []string{"true"}, Credentials: vm.Credentials{User: "vmh", Password: "x"}})
	wantKind(t, err, vm.ErrNotReady)
	_, err = p.Screenshot(ctx, name, vm.Credentials{User: "vmh", Password: "x"})
	wantKind(t, err, vm.ErrNotReady)
	wantKind(t, p.Stop(ctx, name, false), vm.ErrNotReady)
	wantKind(t, p.Update(ctx, name, vm.Changes{CPUs: 2}), vm.ErrInvalidState)
	wantKind(t, p.Delete(ctx, name), vm.ErrInvalidState)
	_, err = p.Clone(ctx, name, vm.CloneOptions{Name: full})
	wantKind(t, err, vm.ErrInvalidState)
	if _, err := os.Stat(filepath.Join(root, full)); !os.IsNotExist(err) {
		t.Fatal("refused clone left a folder")
	}

	must(p.Pause(ctx, name))
	if s := state(name); s != vm.StatePaused {
		t.Fatalf("state after pause = %s", s)
	}
	must(p.Resume(ctx, name))
	if s := state(name); s != vm.StateRunning {
		t.Fatalf("state after resume = %s", s)
	}
	must(p.TakeSnapshot(ctx, name, "live", "taken while running"))
	must(p.Pause(ctx, name))
	must(p.Suspend(ctx, name))
	if s := state(name); s != vm.StateSaved {
		t.Fatalf("state after suspend = %s", s)
	}
	suspended, err := readVMX(path)
	must(err)
	vmss := suspendFile(path, suspended)
	if !isFile(vmss) {
		t.Fatalf("suspend file %q named by checkpoint.vmState is missing", vmss)
	}
	must(p.Stop(ctx, name, true))
	if s := state(name); s != vm.StateStopped || isFile(vmss) || isFile(basePath(vmss)+".vmem") {
		t.Fatalf("a forced stop must discard the suspended state, state = %s", s)
	}
	must(p.Start(ctx, name, false))
	if s := state(name); s != vm.StateRunning {
		t.Fatalf("state after a fresh start = %s", s)
	}
	must(p.Stop(ctx, name, true))
	if s := state(name); s != vm.StateStopped {
		t.Fatalf("state after hard stop = %s", s)
	}
	wantKind(t, p.Stop(ctx, name, true), vm.ErrInvalidState)
	if m, err := p.Get(ctx, name); err != nil || m.Meta["stage"] != "running" || m.Labels["hot"] != "yes" || !m.Managed {
		t.Fatalf("meta lost across power cycles: %+v, %v", m.Meta, err)
	}

	must(p.TakeSnapshot(ctx, name, "base", "чистая база"))
	wantKind(t, p.TakeSnapshot(ctx, name, "base", ""), vm.ErrExists)
	snapshots, err := p.Snapshots(ctx, name)
	must(err)
	want := []vm.Snapshot{
		{Name: "live", Description: "taken while running"},
		{Name: "base", Parent: "live", Description: "чистая база", Current: true},
	}
	if len(snapshots) != len(want) {
		t.Fatalf("snapshots = %+v", snapshots)
	}
	for i, s := range snapshots {
		if s.Name != want[i].Name || s.Parent != want[i].Parent || s.Description != want[i].Description || s.Current != want[i].Current || s.ID == "" {
			t.Errorf("snapshot %d = %+v, want %+v", i, s, want[i])
		}
	}

	must(p.RestoreSnapshot(ctx, name, "live"))
	if s := state(name); s != vm.StateSaved {
		t.Fatalf("reverting to a live snapshot should leave the vm saved, got %s", s)
	}
	must(p.RestoreSnapshot(ctx, name, "base"))
	if s := state(name); s != vm.StateStopped {
		t.Fatalf("state after revert to base = %s", s)
	}
	wantKind(t, p.RestoreSnapshot(ctx, name, "missing"), vm.ErrNotFound)

	lc, err := p.Clone(ctx, name, vm.CloneOptions{Name: linked, Linked: true})
	must(err)
	if lc.Name != linked || lc.State != vm.StateStopped || lc.Managed || len(lc.Meta) != 0 {
		t.Fatalf("linked clone = %+v", lc)
	}
	if !isFile(filepath.Join(root, linked, seedFile)) {
		t.Fatal("seed not written into the linked clone")
	}
	_, err = p.Clone(ctx, name, vm.CloneOptions{Name: name + "-live", Linked: true, Snapshot: "live"})
	wantKind(t, err, vm.ErrInvalidState)
	if _, err := os.Stat(filepath.Join(root, name+"-live")); !os.IsNotExist(err) {
		t.Fatal("failed clone left a folder")
	}
	must(p.Start(ctx, name, false))
	if _, err := p.Clone(ctx, name, vm.CloneOptions{Name: running, Linked: true, Snapshot: "BASE"}); err != nil {
		t.Fatalf("linked clone of a snapshot taken while off, from a running vm: %v", err)
	}
	must(p.Stop(ctx, name, true))
	fc, err := p.Clone(ctx, name, vm.CloneOptions{Name: full})
	must(err)
	if fc.Name != full || !isFile(filepath.Join(root, full, seedFile)) {
		t.Fatalf("full clone = %+v", fc)
	}
	if snapshots, err := p.Snapshots(ctx, name); err != nil || len(snapshots) != 2 || snapshots[1].Description != "чистая база" {
		t.Fatalf("descriptions after vmware rewrote the vmsd = %+v, %v", snapshots, err)
	}
	wantKind(t, p.Delete(ctx, name), vm.ErrInvalidState)
	for _, snapshot := range []string{"base", "Base", "live", "LIVE"} {
		wantKind(t, p.DeleteSnapshot(ctx, name, snapshot), vm.ErrInvalidState)
	}
	wantKind(t, p.DeleteSnapshot(ctx, name, "live/base"), vm.ErrInvalid)

	must(p.SetMeta(ctx, full, map[string]string{"label.copy": "yes", "stage": ""}))
	must(p.Update(ctx, name, vm.Changes{CPUs: 2, MemoryMB: 128, Labels: map[string]string{"suite": "", "tier": "two"}}))
	m, err = p.Get(ctx, name)
	must(err)
	if m.CPUs != 2 || m.MemoryMB != 128 || m.Labels["tier"] != "" || m.Labels["suite"] != "integration" {
		t.Fatalf("updated = %+v", m)
	}

	must(p.Start(ctx, linked, false))
	if s := state(linked); s != vm.StateRunning {
		t.Fatalf("linked clone state = %s", s)
	}
	must(p.Reset(ctx, linked))
	must(p.Stop(ctx, linked, true))
	if m, err := p.Get(ctx, full); err != nil || m.Labels["copy"] != "yes" || m.Meta["stage"] != "" {
		t.Fatalf("full clone meta = %+v, %v", m.Meta, err)
	}

	mm, err := p.Create(ctx, vm.Spec{Name: mac, CPUs: 1, MemoryMB: 64, DiskGB: 1, NICs: []vm.NIC{{Mode: vm.NetNAT, MAC: "52-54-00-12-34-56"}}})
	must(err)
	if mm.NICs[0].MAC != "52:54:00:12:34:56" {
		t.Fatalf("mac = %+v", mm.NICs)
	}
	must(p.Start(ctx, mac, false))
	must(p.Stop(ctx, mac, true))
	must(p.Delete(ctx, mac))

	if p.ovftool != "" {
		outside := t.TempDir()
		sentinel := writeFile(t, filepath.Join(outside, "serial.txt"), "host data")
		ova := hostileOVA(ctx, t, p.ovftool, filepath.Join(root, full, full+".vmx"), outside)
		im, err := p.Create(ctx, vm.Spec{Name: imported, Appliance: ova, CPUs: 1, MemoryMB: 96, Labels: map[string]string{"from": "ova"}})
		must(err)
		if im.Name != imported || im.MemoryMB != 96 || im.Labels["from"] != "ova" {
			t.Fatalf("imported = %+v", im)
		}
		v, err := readVMX(im.ConfigPath)
		must(err)
		for _, line := range v.lines {
			floppy := hasPrefixFold(line.key, "floppy") && !strings.EqualFold(line.key, "floppy0.present")
			if hostPortPattern.MatchString(line.key) || strings.Contains(line.value, outside) || floppy {
				t.Errorf("host device survived the import: %s = %q", line.key, line.value)
			}
		}
		if v.get("floppy0.present") != "FALSE" || v.get("sound.present") != "FALSE" {
			t.Errorf("imported floppy or sound device left connected:\n%s", v.encode())
		}
		must(p.Start(ctx, imported, false))
		must(p.Stop(ctx, imported, true))
		if entries, err := os.ReadDir(outside); err != nil || len(entries) != 1 || readFile(t, sentinel) != "host data" {
			t.Fatalf("the imported vm touched host files: %v, %v", entries, err)
		}
		must(p.Delete(ctx, imported))
	}

	must(p.Delete(ctx, linked))
	must(p.Delete(ctx, running))
	must(p.DeleteSnapshot(ctx, name, "LIVE"))
	must(p.Delete(ctx, full))
	must(p.Delete(ctx, name))
	for _, vmName := range []string{name, linked, running, full, imported, mac} {
		if _, err := os.Stat(filepath.Join(root, vmName)); !os.IsNotExist(err) {
			t.Errorf("%s folder still exists: %v", vmName, err)
		}
	}
	_, err = p.Get(ctx, name)
	wantKind(t, err, vm.ErrNotFound)
}

func hostileOVA(ctx context.Context, t *testing.T, ovftool, vmx, outside string) string {
	t.Helper()
	dir := t.TempDir()
	ovf := filepath.Join(dir, "export.ovf")
	if out, err := exec.CommandContext(ctx, ovftool, "--acceptAllEulas", vmx, ovf).CombinedOutput(); err != nil {
		t.Fatalf("export ovf: %v\n%s", err, out)
	}
	item := func(id, kind int, subtype, config string) string {
		return fmt.Sprintf(`<Item ovf:required="false"><rasd:AutomaticAllocation>true</rasd:AutomaticAllocation><rasd:ElementName>host%d</rasd:ElementName><rasd:InstanceID>%d</rasd:InstanceID><rasd:ResourceSubType>%s</rasd:ResourceSubType><rasd:ResourceType>%d</rasd:ResourceType>%s</Item>`, id, id, subtype, kind, config)
	}
	backing := func(key, value string) string {
		return `<vmw:Config ovf:required="false" vmw:key="` + key + `" vmw:value="` + html.EscapeString(value) + `"/>`
	}
	devices := item(90, 21, "vmware.serialport.file", backing("backing.fileName", filepath.Join(outside, "serial.txt"))) +
		item(91, 22, "vmware.parallelport.file", backing("backing.fileName", filepath.Join(outside, "parallel.txt"))) +
		item(92, 14, "vmware.floppy.device", backing("backing.deviceName", "A:")) +
		item(93, 1, "vmware.soundcard.hdaudio", "")
	text := readFile(t, ovf)
	end := strings.LastIndex(text, "</Item>")
	if end < 0 {
		t.Fatalf("no hardware items in the exported ovf:\n%s", text)
	}
	end += len("</Item>")
	writeFile(t, ovf, text[:end]+devices+text[end:])
	if err := os.Remove(filepath.Join(dir, "export.mf")); err != nil {
		t.Fatal(err)
	}
	ova := filepath.Join(dir, "hostile.ova")
	if out, err := exec.CommandContext(ctx, ovftool, "--acceptAllEulas", ovf, ova).CombinedOutput(); err != nil {
		t.Fatalf("pack ova: %v\n%s", err, out)
	}
	return ova
}
