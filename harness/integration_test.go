package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZetGames/vm-harness/internal/iso9660"
	"github.com/ZetGames/vm-harness/provider/virtualbox"
	"github.com/ZetGames/vm-harness/vm"
)

type ownedVMs struct {
	*virtualbox.Provider
	names   []string
	onReset func()
}

func (o *ownedVMs) Reset(ctx context.Context, ref string) error {
	err := o.Provider.Reset(ctx, ref)
	if err == nil && o.onReset != nil {
		o.onReset()
	}
	return err
}

func (o *ownedVMs) List(ctx context.Context) ([]vm.Machine, error) {
	var out []vm.Machine
	for _, name := range o.names {
		mach, err := o.Get(ctx, name)
		if errors.Is(err, vm.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, mach)
	}
	return out, nil
}

func linkedDir(t *testing.T, target string) string {
	t.Helper()
	link := filepath.Join(t.TempDir(), "work")
	var err error
	if runtime.GOOS == "windows" {
		err = exec.Command("cmd", "/c", "mklink", "/J", link, target).Run()
	} else {
		err = os.Symlink(target, link)
	}
	if err != nil {
		t.Logf("sharing %s directly, the link failed: %v", target, err)
		return target
	}
	return link
}

func sharedFolderPath(t *testing.T, vboxmanage, id string) string {
	t.Helper()
	out, err := exec.Command(vboxmanage, "showvminfo", id, "--machinereadable").Output()
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(out)) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "SharedFolderPathMachineMapping1="); ok {
			path, err := strconv.Unquote(value)
			if err != nil {
				t.Fatal(err)
			}
			return path
		}
	}
	t.Fatalf("vm %s has no shared folder", id)
	return ""
}

func bootISO(t *testing.T) string {
	t.Helper()
	return writeISO(t, filepath.Join(t.TempDir(), "boot.iso"))
}

func writeISO(t *testing.T, path string) string {
	t.Helper()
	var image bytes.Buffer
	if err := iso9660.Write(&image, "VMHBOOT", []iso9660.File{{Name: "readme.txt", Data: []byte("not bootable")}}); err != nil {
		t.Fatal(err)
	}
	return writeFile(t, path, image.String())
}

func TestIntegrationVirtualBox(t *testing.T) {
	if os.Getenv("VMH_INTEGRATION") != "1" {
		t.Skip("set VMH_INTEGRATION=1 to run against the real VBoxManage")
	}
	ctx := context.Background()
	root := t.TempDir()
	iso := bootISO(t)
	vbox := virtualbox.New(virtualbox.Options{Root: filepath.Join(root, vm.VirtualBox)})
	info, err := vbox.Info(ctx)
	if err != nil {
		t.Skipf("VirtualBox is not usable: %v", err)
	}
	suffix := make([]byte, 4)
	rand.Read(suffix)
	name := "vmh-it-" + hex.EncodeToString(suffix)
	clone := name + "-linked"
	p := &ownedVMs{Provider: vbox, names: []string{name, clone}}
	m := New(Config{Root: root, Limits: Limits{MaxCPUs: 2, MaxMemoryMB: 512, MaxDiskGB: 2}}, p)
	m.poll = 200 * time.Millisecond
	t.Cleanup(func() {
		for _, n := range []string{clone, name} {
			if err := m.Delete(ctx, Ref{Provider: vm.VirtualBox, VM: n}, true); err != nil && !errors.Is(err, vm.ErrNotFound) {
				t.Errorf("cleanup of %s: %v", n, err)
			}
		}
	})
	ref := Ref{VM: name}
	keys := filepath.Join(root, "keys")

	shareTarget := t.TempDir()
	share := linkedDir(t, shareTarget)
	if runtime.GOOS == "windows" {
		_, err := m.Create(ctx, vm.Spec{Name: name, MemoryMB: 128, DiskGB: 1, SharedFolders: []vm.SharedFolder{{Name: "work", HostPath: `\\?\` + shareTarget}}})
		wantErr(t, err, vm.ErrInvalid)
	}
	created, err := m.Create(ctx, vm.Spec{
		Name: name, OSType: "ubuntu", CPUs: 1, MemoryMB: 128, DiskGB: 1, ISO: iso,
		Labels:        map[string]string{"suite": "harness"},
		CloudInit:     &vm.CloudInit{},
		SharedFolders: []vm.SharedFolder{{Name: "work", HostPath: share}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, want := sharedFolderPath(t, info.Binary, created.ID), mustCanonical(t, shareTarget); pathKey(got) != pathKey(want) {
		t.Fatalf("shared folder path = %q, want the resolved target %q", got, want)
	}
	key := created.Meta[vm.MetaSSHKey]
	sshFwd, ok := forwardNamed(created, sshForwardName)
	if !created.Managed || created.Meta[vm.MetaManaged] != created.ID || created.Meta[vm.MetaSSHUser] != "vmh" || !ok || sshFwd.HostPort == 0 {
		t.Fatalf("created = %+v", created)
	}
	if filepath.Dir(key) != keys || !strings.HasPrefix(filepath.Base(key), "virtualbox-"+name+"-") || !exists(key) || !exists(key+".pub") {
		t.Fatalf("ssh key = %q", key)
	}
	if created.SSH == nil || created.SSH.KeyPath != key || created.SSH.Port != sshFwd.HostPort {
		t.Fatalf("ssh access = %+v", created.SSH)
	}
	for _, dup := range []string{name, strings.ToUpper(name)} {
		_, err = m.Create(ctx, vm.Spec{Name: dup, MemoryMB: 128, DiskGB: 1})
		wantErr(t, err, vm.ErrExists)
	}
	_, err = m.Create(ctx, vm.Spec{Name: name + "-big", CPUs: 4})
	wantErr(t, err, vm.ErrLimit)
	_, err = m.Exec(ctx, ref, ExecRequest{ExecRequest: vm.ExecRequest{Command: []string{"true"}}})
	wantErr(t, err, vm.ErrInvalidState)

	for range 2 {
		running, err := m.Start(ctx, ref, false)
		if err != nil || running.State != vm.StateRunning {
			t.Fatalf("start = %s, %v", running.State, err)
		}
	}
	if _, err := m.Wait(ctx, ref, WaitRequest{For: WaitRunning, Timeout: 30 * time.Second}); err != nil {
		t.Fatalf("wait running: %v", err)
	}
	_, err = m.Wait(ctx, ref, WaitRequest{For: WaitIP, Timeout: 2 * time.Second})
	wantErr(t, err, context.DeadlineExceeded)
	if !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("wait ip error = %v", err)
	}
	waitRecoversBoot(t, m, p, ref, created)
	if png, err := m.Screenshot(ctx, ref, vm.Credentials{}); err != nil || len(png) < 8 {
		t.Fatalf("screenshot: %d bytes, %v", len(png), err)
	}
	_, err = m.Clone(ctx, ref, vm.CloneOptions{Name: clone, Linked: true})
	wantErr(t, err, vm.ErrInvalidState)
	if snaps, err := m.Snapshots(ctx, ref); err != nil || len(snaps) != 0 {
		t.Fatalf("snapshots after the refused clone = %+v, %v", snaps, err)
	}

	err = m.Delete(ctx, ref, false)
	wantErr(t, err, vm.ErrInvalidState)

	saved, err := m.Suspend(ctx, ref)
	if err != nil || saved.State != vm.StateSaved {
		t.Fatalf("suspend = %s, %v", saved.State, err)
	}
	discarded, err := m.Stop(ctx, ref, StopOptions{})
	if err != nil || discarded.State != vm.StateStopped {
		t.Fatalf("stop from saved = %s, %v", discarded.State, err)
	}

	if _, err := m.Start(ctx, ref, false); err != nil {
		t.Fatalf("start after discarding: %v", err)
	}
	start := time.Now()
	stopped, err := m.Stop(ctx, ref, StopOptions{Timeout: 3 * time.Second})
	if err != nil || stopped.State != vm.StateStopped {
		t.Fatalf("stop = %s, %v", stopped.State, err)
	}
	if elapsed := time.Since(start); elapsed < 3*time.Second {
		t.Fatalf("a guest without an OS stopped gracefully after %s", elapsed)
	}

	web, err := m.AddPortForward(ctx, ref, vm.PortForward{GuestPort: 80})
	if err != nil || web.Name != "tcp-80" || web.HostPort == 0 || web.HostPort == sshFwd.HostPort {
		t.Fatalf("add port forward = %+v, %v", web, err)
	}

	updated, err := m.Update(ctx, ref, vm.Changes{CPUs: 2, Labels: map[string]string{"stage": "updated"}})
	if err != nil || updated.CPUs != 2 || updated.Labels["stage"] != "updated" || updated.Labels["suite"] != "harness" {
		t.Fatalf("update = %+v, %v", updated, err)
	}

	snap, err := m.TakeSnapshot(ctx, ref, "first", "taken by the harness test")
	if err != nil || snap.Name != "first" || !snap.Current {
		t.Fatalf("take snapshot = %+v, %v", snap, err)
	}
	_, err = m.TakeSnapshot(ctx, ref, "first", "")
	wantErr(t, err, vm.ErrExists)
	if err := m.DeleteSnapshot(ctx, ref, "first"); err != nil {
		t.Fatalf("delete snapshot: %v", err)
	}

	linked, err := m.Clone(ctx, ref, vm.CloneOptions{Name: clone, Linked: true})
	if err != nil {
		t.Fatalf("linked clone: %v", err)
	}
	snaps, err := m.Snapshots(ctx, ref)
	if err != nil || len(snaps) != 1 || snaps[0].Name != cloneBaseSnapshot {
		t.Fatalf("source snapshots = %+v, %v", snaps, err)
	}
	cloneKey := linked.Meta[vm.MetaSSHKey]
	cloneSSH, _ := forwardNamed(linked, sshForwardName)
	cloneWeb, _ := forwardNamed(linked, web.Name)
	if !linked.Managed || linked.Meta[vm.MetaManaged] != linked.ID || linked.Meta[vm.MetaLinkedFrom] != created.ID ||
		linked.Labels["suite"] != "harness" || filepath.Dir(cloneKey) != keys ||
		!strings.HasPrefix(filepath.Base(cloneKey), "virtualbox-"+clone+"-") || !exists(cloneKey) ||
		cloneSSH.HostPort == 0 || cloneSSH.HostPort == sshFwd.HostPort ||
		cloneWeb.HostPort == 0 || cloneWeb.HostPort == web.HostPort {
		t.Fatalf("linked clone = %+v", linked)
	}
	if err := m.RemovePortForward(ctx, ref, web.Name); err != nil {
		t.Fatalf("remove port forward: %v", err)
	}

	err = m.Delete(ctx, ref, true)
	wantErr(t, err, vm.ErrInvalidState)
	if !strings.Contains(err.Error(), clone) {
		t.Fatalf("delete base error = %v", err)
	}

	if _, err := m.Start(ctx, Ref{VM: clone}, false); err != nil {
		t.Fatalf("start clone: %v", err)
	}
	if err := m.Delete(ctx, Ref{VM: clone}, true); err != nil {
		t.Fatalf("delete clone: %v", err)
	}
	if exists(cloneKey) || exists(cloneKey+".pub") {
		t.Fatal("clone key left behind")
	}

	known := writeFile(t, key+".known_hosts", "[127.0.0.1]:2222 ssh-ed25519 AAAA")
	if _, err := m.Start(ctx, ref, false); err != nil {
		t.Fatalf("start again: %v", err)
	}
	restored, err := m.RestoreSnapshot(ctx, ref, cloneBaseSnapshot)
	if err != nil || restored.State != vm.StateStopped || restored.CurrentSnapshot != cloneBaseSnapshot {
		t.Fatalf("restore = %+v, %v", restored, err)
	}
	if exists(known) {
		t.Fatal("known_hosts survived the restore")
	}

	if err := m.Delete(ctx, ref, false); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = m.Get(ctx, ref)
	wantErr(t, err, vm.ErrNotFound)
	if exists(key) || exists(key+".pub") {
		t.Fatal("key left behind")
	}
}

func waitRecoversBoot(t *testing.T, m *Manager, p *ownedVMs, ref Ref, created vm.Machine) {
	t.Helper()
	console := created.ConsoleLog
	if filepath.Base(console) != "serial.log" || filepath.Dir(console) != filepath.Dir(created.ConfigPath) {
		t.Fatalf("console log = %q, config = %q", console, created.ConfigPath)
	}
	m.bootStall = 2 * time.Second
	p.onReset = func() { time.AfterFunc(2*time.Second, func() { appendConsole(t, console, stalledBoot) }) }
	defer func() {
		m.bootStall = bootStallWindow
		p.onReset = nil
	}()
	time.AfterFunc(time.Second, func() { appendConsole(t, console, stalledBoot+ioapicPanic) })
	start := time.Now()
	res, err := m.Wait(context.Background(), ref, WaitRequest{For: WaitIP, Timeout: time.Minute})
	wantErr(t, err, vm.ErrNotReady)
	stall := "boot stalled for 2s at: Begin: Loading essential drivers ... — reset"
	if !slices.Equal(res.Recoveries, []string{panicRecovery, stall}) {
		t.Fatalf("recoveries = %q", res.Recoveries)
	}
	want := "is stuck again after 2 automatic resets (" + panicRecovery + "; " + stall + "): boot stalled for 2s at: Begin: Loading essential drivers ...; this wait gives up, and another wait would reset the vm again"
	if !strings.Contains(err.Error(), want) || time.Since(start) > 30*time.Second {
		t.Fatalf("wait error after %s = %v", time.Since(start), err)
	}
	if mach, err := m.Get(context.Background(), ref); err != nil || mach.State != vm.StateRunning {
		t.Fatalf("after the resets: %s, %v", mach.State, err)
	}
}
