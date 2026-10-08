package mcpserver

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fl4metf/vm-harness/harness"
	"github.com/fl4metf/vm-harness/internal/memprovider"
	"github.com/fl4metf/vm-harness/vm"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestProviders(t *testing.T) {
	f := newFixture(t)
	out := callOK[providersOutput](t, f, "vm_providers", nil)
	var names []string
	for _, p := range out.Providers {
		if !p.Available {
			t.Errorf("provider %s is not available: %s", p.Name, p.Error)
		}
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{vm.VirtualBox, vm.VMware}) {
		t.Fatalf("providers = %v", names)
	}
	if !out.Providers[0].Default || out.Providers[1].Default {
		t.Errorf("virtualbox should be the default: %+v", out.Providers)
	}
}

func TestList(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		args map[string]any
		want []string
	}{
		{nil, []string{"virtualbox/db", "virtualbox/legacy", "virtualbox/shared", "virtualbox/web", "vmware/desk", "vmware/shared"}},
		{map[string]any{"managed_only": true}, []string{"virtualbox/db", "virtualbox/shared", "virtualbox/web", "vmware/desk", "vmware/shared"}},
		{map[string]any{"provider": "vmware"}, []string{"vmware/desk", "vmware/shared"}},
	}
	for _, c := range cases {
		out := callOK[listOutput](t, f, "vm_list", c.args)
		var got []string
		for _, mach := range out.VMs {
			got = append(got, mach.Provider+"/"+mach.Name)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("vm_list %v = %v, want %v", c.args, got, c.want)
		}
	}
	f.callErr(t, "vm_list", map[string]any{"provider": "hyperv"}, "invalid_argument")
}

func TestGet(t *testing.T) {
	f := newFixture(t)
	mach := callOK[vm.Machine](t, f, "vm_get", map[string]any{"vm": "web"})
	if mach.Name != "web" || mach.Provider != vm.VirtualBox || mach.State != vm.StateRunning || !mach.Managed {
		t.Fatalf("vm_get web = %+v", mach)
	}
	if len(mach.PortForwards) != 1 || mach.PortForwards[0].Name != "http" {
		t.Errorf("port forwards = %+v", mach.PortForwards)
	}
	if legacy := callOK[vm.Machine](t, f, "vm_get", map[string]any{"vm": "legacy"}); legacy.Managed {
		t.Errorf("legacy is reported as managed")
	}
	if byID := callOK[vm.Machine](t, f, "vm_get", map[string]any{"vm": mach.ID}); byID.Name != "web" {
		t.Errorf("vm_get by id = %+v", byID)
	}
	f.callErr(t, "vm_get", map[string]any{"vm": "nope"}, "not_found")
	msg := f.callErr(t, "vm_get", map[string]any{"vm": "shared"}, "invalid_argument")
	if !strings.Contains(msg, "ambiguous") {
		t.Errorf("ambiguous lookup error = %q", msg)
	}
	if got := callOK[vm.Machine](t, f, "vm_get", map[string]any{"vm": "shared", "provider": "vmware"}); got.Provider != vm.VMware {
		t.Errorf("vm_get shared on vmware = %+v", got)
	}
}

func TestCreate(t *testing.T) {
	f := newFixture(t)
	image := filepath.Join(t.TempDir(), "cloud.vmdk")
	if err := os.WriteFile(image, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	mach := callOK[vm.Machine](t, f, "vm_create", map[string]any{
		"name":          "fresh",
		"provider":      "virtualbox",
		"os_type":       "ubuntu",
		"cpus":          1,
		"memory_mb":     512,
		"disk_image":    image,
		"nics":          []any{map[string]any{"mode": "nat", "model": "virtio"}},
		"port_forwards": []any{map[string]any{"guest_port": 80}},
		"cloud_init":    map[string]any{"user": "dev", "ssh_authorized_keys": []any{"ssh-ed25519 AAAA dev@test"}},
		"labels":        map[string]any{"team": "qa"},
		"start":         true,
	})
	if mach.Name != "fresh" || mach.Provider != vm.VirtualBox || mach.State != vm.StateRunning || !mach.Managed {
		t.Fatalf("created %+v", mach)
	}
	if mach.CPUs != 1 || mach.MemoryMB != 512 || mach.OSType != "Ubuntu_64" || mach.Labels["team"] != "qa" {
		t.Errorf("created %+v", mach)
	}
	if len(mach.NICs) != 1 || mach.NICs[0].Model != "virtio" {
		t.Errorf("nics = %+v", mach.NICs)
	}
	forwards := map[string]vm.PortForward{}
	for _, pf := range mach.PortForwards {
		forwards[pf.Name] = pf
	}
	if pf := forwards["tcp-80"]; pf.GuestPort != 80 || pf.HostPort == 0 {
		t.Errorf("tcp-80 forward = %+v", pf)
	}
	sshFwd, ok := forwards["vmh-ssh"]
	if !ok {
		t.Errorf("cloud-init vm has no ssh forward: %+v", mach.PortForwards)
	}
	if user := mustGet(t, f.vbox, "fresh").Meta[vm.MetaSSHUser]; user != "dev" {
		t.Errorf("ssh user = %q, want dev", user)
	}
	ssh := callOK[vm.Machine](t, f, "vm_get", map[string]any{"vm": "fresh"}).SSH
	if ssh == nil || ssh.User != "dev" || ssh.KeyPath == "" || ssh.Host != "127.0.0.1" || ssh.Port != sshFwd.HostPort {
		t.Errorf("vm_get ssh = %+v, want user dev, the generated key and the vmh-ssh forward %+v", ssh, sshFwd)
	} else if _, err := os.Stat(ssh.KeyPath); err != nil {
		t.Errorf("ssh key_path: %v", err)
	}
	msg := f.callErr(t, "vm_create", map[string]any{"name": "blank", "cloud_init": map[string]any{}}, "invalid_argument")
	if !strings.Contains(msg, "cloud-init needs") {
		t.Errorf("cloud-init without boot media error = %q", msg)
	}

	f.callErr(t, "vm_create", map[string]any{"name": "web", "provider": "virtualbox"}, "already_exists")
	f.callErr(t, "vm_create", map[string]any{"name": "bad name"}, "invalid_argument")
	f.callErr(t, "vm_create", map[string]any{"name": "x", "provider": "hyperv"}, "invalid_argument")
	msg = f.callErr(t, "vm_create", map[string]any{
		"name":           "shares",
		"shared_folders": []any{map[string]any{"name": "src", "host_path": filepath.Join(f.root, "missing")}},
	}, "invalid_argument")
	if !strings.Contains(msg, "shared folder src") {
		t.Errorf("shared folder error = %q", msg)
	}
	msg = f.callErr(t, "vm_create", map[string]any{"name": "setup", "unattended": map[string]any{"user": "admin"}}, "invalid_argument")
	if !strings.Contains(msg, "needs an iso") {
		t.Errorf("unattended error = %q", msg)
	}
	f.callErr(t, "vm_create", map[string]any{"name": "fwd", "provider": "vmware", "port_forwards": []any{map[string]any{"guest_port": 22}}}, "unsupported")
}

func TestPower(t *testing.T) {
	f := newFixture(t)
	steps := []struct {
		vm     string
		action string
		args   map[string]any
		want   vm.State
	}{
		{"db", "start", nil, vm.StateRunning},
		{"db", "start", nil, vm.StateRunning},
		{"db", "stop", map[string]any{"timeout_sec": 5}, vm.StateStopped},
		{"db", "stop", nil, vm.StateStopped},
		{"db", "start", map[string]any{"gui": true}, vm.StateRunning},
		{"db", "kill", nil, vm.StateStopped},
		{"web", "pause", nil, vm.StatePaused},
		{"web", "resume", nil, vm.StateRunning},
		{"web", "reset", nil, vm.StateRunning},
		{"web", "suspend", nil, vm.StateSaved},
		{"web", "start", nil, vm.StateRunning},
	}
	for _, s := range steps {
		args := map[string]any{"vm": s.vm, "action": s.action}
		for k, v := range s.args {
			args[k] = v
		}
		if got := callOK[vm.Machine](t, f, "vm_power", args); got.State != s.want {
			t.Fatalf("%s %s: state %s, want %s", s.action, s.vm, got.State, s.want)
		}
	}
	id := mustGet(t, f.vbox, "db").ID
	if !slices.Contains(f.vbox.Calls(), "kill "+id) {
		t.Errorf("kill did not force power-off: %v", f.vbox.Calls())
	}

	msg := f.callErr(t, "vm_power", map[string]any{"vm": "web", "action": "explode"}, "invalid_argument")
	if !strings.Contains(msg, "unknown action") {
		t.Errorf("unknown action error = %q", msg)
	}
	f.callErr(t, "vm_power", map[string]any{"vm": "db", "action": "pause"}, "invalid_state")
	f.callErr(t, "vm_power", map[string]any{"vm": "nope", "action": "start"}, "not_found")
	msg = f.callErr(t, "vm_power", map[string]any{"vm": "legacy", "action": "stop"}, "forbidden")
	if !strings.HasPrefix(msg, `forbidden: vm "legacy" `) || strings.Count(msg, "forbidden") != 1 || strings.Contains(msg, "adopt --") {
		t.Errorf("forbidden error = %q", msg)
	}
	if got := mustGet(t, f.vbox, "legacy"); got.State != vm.StateRunning {
		t.Errorf("unmanaged vm was touched: %s", got.State)
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t)
	f.callErr(t, "vm_delete", map[string]any{"vm": "web"}, "invalid_state")
	if msg := f.callText(t, "vm_delete", map[string]any{"vm": "web", "force": true}); !strings.Contains(msg, "deleted") {
		t.Errorf("vm_delete reply = %q", msg)
	}
	f.callErr(t, "vm_get", map[string]any{"vm": "web"}, "not_found")
	f.callErr(t, "vm_delete", map[string]any{"vm": "web"}, "not_found")
	f.callErr(t, "vm_delete", map[string]any{"vm": "legacy", "force": true}, "forbidden")
	f.callText(t, "vm_delete", map[string]any{"vm": "shared", "provider": "vmware"})
	if _, err := f.vbox.Get(t.Context(), "shared"); err != nil {
		t.Errorf("deleting vmware/shared touched virtualbox/shared: %v", err)
	}
}

func TestUpdate(t *testing.T) {
	f := newFixture(t)
	mach := callOK[vm.Machine](t, f, "vm_update", map[string]any{
		"vm": "db", "cpus": 4, "memory_mb": 4096, "labels": map[string]any{"env": "test", "owner": "ci"},
	})
	if mach.CPUs != 4 || mach.MemoryMB != 4096 || mach.Labels["env"] != "test" || mach.Labels["owner"] != "ci" {
		t.Fatalf("updated %+v", mach)
	}
	mach = callOK[vm.Machine](t, f, "vm_update", map[string]any{"vm": "db", "labels": map[string]any{"env": ""}})
	if _, ok := mach.Labels["env"]; ok || mach.Labels["owner"] != "ci" {
		t.Errorf("labels after removal = %v", mach.Labels)
	}
	if running := callOK[vm.Machine](t, f, "vm_update", map[string]any{"vm": "web", "labels": map[string]any{"role": "db"}}); running.Labels["role"] != "db" {
		t.Errorf("labels of a running vm = %v", running.Labels)
	}
	f.callErr(t, "vm_update", map[string]any{"vm": "web", "cpus": 2}, "invalid_state")
	f.callErr(t, "vm_update", map[string]any{"vm": "legacy", "labels": map[string]any{"a": "b"}}, "forbidden")
	f.callErr(t, "vm_update", map[string]any{"vm": "db", "labels": map[string]any{"bad key": "x"}}, "invalid_argument")
}

func TestClone(t *testing.T) {
	f := newFixture(t)
	full := callOK[vm.Machine](t, f, "vm_clone", map[string]any{"vm": "web", "name": "web-copy"})
	if full.Name != "web-copy" || !full.Managed || full.State != vm.StateStopped {
		t.Errorf("full clone = %+v", full)
	}
	f.callErr(t, "vm_clone", map[string]any{"vm": "legacy", "name": "legacy-copy"}, "forbidden")
	f.callErr(t, "vm_clone", map[string]any{"vm": "legacy", "name": "legacy-link", "linked": true}, "forbidden")
	if slices.ContainsFunc(f.vbox.Calls(), func(c string) bool { return strings.HasPrefix(c, "clone "+mustGet(t, f.vbox, "legacy").ID) }) {
		t.Errorf("an unmanaged vm was cloned: %v", f.vbox.Calls())
	}

	linked := callOK[vm.Machine](t, f, "vm_clone", map[string]any{"vm": "db", "name": "db-link", "linked": true})
	if !linked.Managed || mustGet(t, f.vbox, "db-link").Meta[vm.MetaLinkedFrom] != mustGet(t, f.vbox, "db").ID {
		t.Errorf("linked clone = %+v", linked)
	}
	snaps := callOK[snapshotOutput](t, f, "vm_snapshot", map[string]any{"vm": "db", "action": "list"})
	if len(snaps.Snapshots) != 1 || snaps.Snapshots[0].Name != "vmh-clone-base" {
		t.Errorf("source snapshots = %+v", snaps.Snapshots)
	}
	f.callErr(t, "vm_clone", map[string]any{"vm": "db", "name": "db-link"}, "already_exists")
	f.callErr(t, "vm_clone", map[string]any{"vm": "db", "name": "-bad"}, "invalid_argument")
	f.callErr(t, "vm_clone", map[string]any{"vm": "db", "name": "db-old", "linked": true, "snapshot": "missing"}, "invalid_state")
}

func TestSnapshots(t *testing.T) {
	f := newFixture(t)
	res := f.call(t, "vm_snapshot", map[string]any{"vm": "web", "action": "list"})
	if list, ok := res.StructuredContent.(map[string]any)["snapshots"].([]any); res.IsError || !ok || len(list) != 0 {
		t.Fatalf("empty snapshot list = %v", res.StructuredContent)
	}

	taken := callOK[snapshotOutput](t, f, "vm_snapshot", map[string]any{"vm": "web", "action": "take", "name": "base", "description": "clean install"})
	if taken.Snapshot == nil || taken.Snapshot.Name != "base" || taken.Snapshot.Description != "clean install" || !taken.Snapshot.Current {
		t.Fatalf("taken = %+v", taken.Snapshot)
	}
	callOK[snapshotOutput](t, f, "vm_snapshot", map[string]any{"vm": "web", "action": "take", "name": "second"})
	listed := callOK[snapshotOutput](t, f, "vm_snapshot", map[string]any{"vm": "web", "action": "list"})
	if len(listed.Snapshots) != 2 || listed.Snapshots[1].Parent != "base" {
		t.Fatalf("listed = %+v", listed.Snapshots)
	}

	restored := callOK[snapshotOutput](t, f, "vm_snapshot", map[string]any{"vm": "web", "action": "restore", "name": "base"})
	if restored.Machine == nil || restored.Machine.State != vm.StateStopped || restored.Machine.CurrentSnapshot != "base" {
		t.Fatalf("restored = %+v", restored.Machine)
	}
	f.callErr(t, "vm_snapshot", map[string]any{"vm": "web", "action": "restore", "name": "missing"}, "not_found")
	f.callErr(t, "vm_snapshot", map[string]any{"vm": "web", "action": "take", "name": "base"}, "already_exists")

	left := callOK[snapshotOutput](t, f, "vm_snapshot", map[string]any{"vm": "web", "action": "delete", "name": "second"})
	if len(left.Snapshots) != 1 || left.Snapshots[0].Name != "base" {
		t.Errorf("after delete = %+v", left.Snapshots)
	}
	f.callErr(t, "vm_snapshot", map[string]any{"vm": "web", "action": "delete", "name": "second"}, "not_found")
	f.callErr(t, "vm_snapshot", map[string]any{"vm": "web", "action": "rename", "name": "base"}, "invalid_argument")
	f.callErr(t, "vm_snapshot", map[string]any{"vm": "legacy", "action": "take", "name": "mine"}, "forbidden")
	callOK[snapshotOutput](t, f, "vm_snapshot", map[string]any{"vm": "legacy", "action": "list"})
}

func TestExec(t *testing.T) {
	f := newFixture(t)
	var got vm.ExecRequest
	f.vbox.ExecFunc = func(_ vm.Machine, req vm.ExecRequest) (vm.ExecResult, error) {
		got = req
		return vm.ExecResult{ExitCode: 3, Stdout: "out", Stderr: "err"}, nil
	}
	res := callOK[vm.ExecResult](t, f, "vm_exec", map[string]any{
		"vm":          "web",
		"command":     []any{"ls", "-l", "/"},
		"env":         map[string]any{"LANG": "C"},
		"workdir":     "/tmp",
		"timeout_sec": 30,
		"user":        "root",
		"password":    "secret",
	})
	if res.ExitCode != 3 || res.Stdout != "out" || res.Stderr != "err" || res.Transport != harness.TransportGuest {
		t.Errorf("exec result = %+v", res)
	}
	if !slices.Equal(got.Command, []string{"ls", "-l", "/"}) || got.Env["LANG"] != "C" || got.WorkDir != "/tmp" ||
		got.TimeoutSec != 30 || got.User != "root" || got.Password != "secret" {
		t.Errorf("guest saw %+v", got)
	}

	callOK[vm.ExecResult](t, f, "vm_exec", map[string]any{"vm": "web", "script": "echo hi | wc -c"})
	if got.Script != "echo hi | wc -c" || got.Command != nil {
		t.Errorf("script request = %+v", got)
	}

	f.callErr(t, "vm_exec", map[string]any{"vm": "web"}, "invalid_argument")
	f.callErr(t, "vm_exec", map[string]any{"vm": "web", "command": []any{"true"}, "transport": "telnet"}, "invalid_argument")
	f.callErr(t, "vm_exec", map[string]any{"vm": "legacy", "command": []any{"true"}}, "forbidden")
	f.callErr(t, "vm_exec", map[string]any{"vm": "db", "command": []any{"true"}}, "invalid_state")
	msg := f.callErr(t, "vm_exec", map[string]any{"vm": "web", "command": []any{"true"}, "transport": "ssh"}, "invalid_argument")
	if !strings.Contains(msg, "no ssh user") {
		t.Errorf("ssh without user = %q", msg)
	}
	msg = f.callErr(t, "vm_exec", map[string]any{"vm": "web", "command": []any{"true"}, "transport": "ssh", "ssh": map[string]any{"user": "dev"}}, "invalid_argument")
	if !strings.Contains(msg, "no ssh key") {
		t.Errorf("ssh user override was not used: %q", msg)
	}
}

func TestCopy(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "app.conf")
	if err := os.WriteFile(src, []byte("port=80\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.callText(t, "vm_copy", map[string]any{"vm": "web", "direction": "to_guest", "host_path": src, "guest_path": "/etc/app.conf", "user": "root", "password": "pw"})
	if data, _ := f.vbox.GuestFile("web", "/etc/app.conf"); string(data) != "port=80\n" {
		t.Fatalf("guest file = %q", data)
	}

	dst := filepath.Join(dir, "back.conf")
	f.callText(t, "vm_copy", map[string]any{"vm": "web", "direction": "from_guest", "host_path": dst, "guest_path": "/etc/app.conf"})
	if data, err := os.ReadFile(dst); err != nil || string(data) != "port=80\n" {
		t.Fatalf("host file = %q, %v", data, err)
	}

	f.callErr(t, "vm_copy", map[string]any{"vm": "web", "direction": "sideways", "host_path": src, "guest_path": "/x"}, "invalid_argument")
	f.callErr(t, "vm_copy", map[string]any{"vm": "web", "direction": "from_guest", "host_path": dst, "guest_path": "/missing"}, "not_found")
	f.callErr(t, "vm_copy", map[string]any{"vm": "legacy", "direction": "to_guest", "host_path": src, "guest_path": "/x"}, "forbidden")
}

func TestFiles(t *testing.T) {
	f := newFixture(t)
	f.callText(t, "vm_write_file", map[string]any{"vm": "web", "guest_path": "/etc/motd", "content": "hello\n", "user": "root", "password": "pw"})
	if data, _ := f.vbox.GuestFile("web", "/etc/motd"); string(data) != "hello\n" {
		t.Fatalf("guest text file = %q", data)
	}
	binary := []byte{0xff, 0xfe, 0x00, 0x01}
	f.callText(t, "vm_write_file", map[string]any{"vm": "web", "guest_path": "/bin/blob", "content": base64.StdEncoding.EncodeToString(binary), "encoding": "base64"})
	if data, _ := f.vbox.GuestFile("web", "/bin/blob"); !bytes.Equal(data, binary) {
		t.Fatalf("guest binary file = %v", data)
	}

	plain := callOK[fileContent](t, f, "vm_read_file", map[string]any{"vm": "web", "guest_path": "/etc/motd"})
	if plain != (fileContent{Path: "/etc/motd", Size: 6, Content: "hello\n"}) {
		t.Errorf("read text = %+v", plain)
	}
	blob := callOK[fileContent](t, f, "vm_read_file", map[string]any{"vm": "web", "guest_path": "/bin/blob"})
	decoded, err := base64.StdEncoding.DecodeString(blob.Content)
	if blob.Encoding != "base64" || blob.Size != len(binary) || err != nil || !bytes.Equal(decoded, binary) {
		t.Errorf("read binary = %+v", blob)
	}

	f.callErr(t, "vm_write_file", map[string]any{"vm": "web", "guest_path": "/x", "content": "%%%", "encoding": "base64"}, "invalid_argument")
	f.callErr(t, "vm_write_file", map[string]any{"vm": "web", "guest_path": "/x", "content": "00", "encoding": "hex"}, "invalid_argument")
	f.callErr(t, "vm_write_file", map[string]any{"vm": "legacy", "guest_path": "/x", "content": "x"}, "forbidden")
	f.callErr(t, "vm_read_file", map[string]any{"vm": "web", "guest_path": "/missing"}, "not_found")
	f.callErr(t, "vm_read_file", map[string]any{"vm": "legacy", "guest_path": "/etc/motd"}, "forbidden")
}

func TestIP(t *testing.T) {
	f := newFixture(t)
	f.callErr(t, "vm_ip", map[string]any{"vm": "web"}, "not_ready")
	f.vbox.SetIP("web", "10.0.2.15")
	if out := callOK[ipOutput](t, f, "vm_ip", map[string]any{"vm": "web"}); out.IP != "10.0.2.15" {
		t.Errorf("ip = %q", out.IP)
	}
	f.callErr(t, "vm_ip", map[string]any{"vm": "db"}, "invalid_state")
	f.callErr(t, "vm_ip", map[string]any{"vm": "nope"}, "not_found")
}

func TestWait(t *testing.T) {
	f := newFixture(t)
	f.vbox.SetIP("web", "10.0.2.15")
	if res := callOK[harness.WaitResult](t, f, "vm_wait", map[string]any{"vm": "web", "for": "running"}); res.Machine.Name != "web" {
		t.Errorf("wait running = %+v", res)
	}
	if res := callOK[harness.WaitResult](t, f, "vm_wait", map[string]any{"vm": "web", "for": "ip"}); res.IP != "10.0.2.15" {
		t.Errorf("wait ip = %+v", res)
	}
	var probeUser string
	f.vbox.ExecFunc = func(_ vm.Machine, req vm.ExecRequest) (vm.ExecResult, error) {
		probeUser = req.User
		return vm.ExecResult{}, nil
	}
	callOK[harness.WaitResult](t, f, "vm_wait", map[string]any{"vm": "web", "for": "guest", "user": "root", "password": "pw"})
	if probeUser != "root" {
		t.Errorf("guest probe ran as %q", probeUser)
	}

	f.callErr(t, "vm_wait", map[string]any{"vm": "web", "for": "soon"}, "invalid_argument")
	f.callErr(t, "vm_wait", map[string]any{"vm": "nope", "for": "running"}, "not_found")
	f.callErr(t, "vm_wait", map[string]any{"vm": "legacy", "for": "guest"}, "forbidden")
	msg := f.callErr(t, "vm_wait", map[string]any{"vm": "db", "for": "running", "timeout_sec": 1}, "timeout")
	if !strings.Contains(msg, "timed out after 1s") {
		t.Errorf("timeout error = %q", msg)
	}
}

func TestScreenshot(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"web", "legacy"} {
		res := f.call(t, "vm_screenshot", map[string]any{"vm": name})
		if res.IsError || len(res.Content) != 1 || res.StructuredContent != nil {
			t.Fatalf("screenshot %s = %+v", name, res)
		}
		img, ok := res.Content[0].(*mcp.ImageContent)
		if !ok || img.MIMEType != "image/png" || !bytes.Equal(img.Data, memprovider.PNG) {
			t.Fatalf("screenshot %s content = %#v", name, res.Content[0])
		}
	}
	f.callErr(t, "vm_screenshot", map[string]any{"vm": "db"}, "invalid_state")
	f.callErr(t, "vm_screenshot", map[string]any{"vm": "nope", "user": "root", "password": "pw"}, "not_found")
}

func TestPorts(t *testing.T) {
	f := newFixture(t)
	listed := callOK[portOutput](t, f, "vm_port", map[string]any{"vm": "web", "action": "list"})
	if len(listed.Forwards) != 1 || listed.Forwards[0].Name != "http" {
		t.Fatalf("listed = %+v", listed)
	}
	added := callOK[portOutput](t, f, "vm_port", map[string]any{"vm": "web", "action": "add", "guest_port": 22})
	if pf := added.Forward; pf == nil || pf.Name != "tcp-22" || pf.Protocol != "tcp" || pf.HostIP != "127.0.0.1" || pf.HostPort == 0 {
		t.Fatalf("added = %+v", added.Forward)
	}
	explicit := callOK[portOutput](t, f, "vm_port", map[string]any{"vm": "web", "action": "add", "name": "dns", "protocol": "udp", "host_port": 15353, "guest_port": 53})
	if pf := explicit.Forward; pf == nil || pf.HostPort != 15353 || pf.Protocol != "udp" {
		t.Fatalf("explicit = %+v", explicit.Forward)
	}
	left := callOK[portOutput](t, f, "vm_port", map[string]any{"vm": "web", "action": "remove", "name": "http"})
	var names []string
	for _, pf := range left.Forwards {
		names = append(names, pf.Name)
	}
	if !slices.Equal(names, []string{"tcp-22", "dns"}) {
		t.Errorf("after remove = %v", names)
	}

	res := f.call(t, "vm_port", map[string]any{"vm": "db", "action": "list"})
	if list, ok := res.StructuredContent.(map[string]any)["port_forwards"].([]any); res.IsError || !ok || len(list) != 0 {
		t.Errorf("empty port list = %v", res.StructuredContent)
	}

	f.callErr(t, "vm_port", map[string]any{"vm": "web", "action": "remove", "name": "http"}, "not_found")
	f.callErr(t, "vm_port", map[string]any{"vm": "web", "action": "add", "name": "dns", "guest_port": 53}, "already_exists")
	f.callErr(t, "vm_port", map[string]any{"vm": "web", "action": "add"}, "invalid_argument")
	f.callErr(t, "vm_port", map[string]any{"vm": "web", "action": "flip"}, "invalid_argument")
	f.callErr(t, "vm_port", map[string]any{"vm": "desk", "action": "add", "guest_port": 22}, "unsupported")
	f.callErr(t, "vm_port", map[string]any{"vm": "legacy", "action": "add", "guest_port": 22}, "forbidden")
}

func mustGet(t *testing.T, p *memprovider.Provider, ref string) vm.Machine {
	t.Helper()
	mach, err := p.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return mach
}

func TestWaitReportsAutomaticResets(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(t.TempDir(), "serial.log")
	if err := os.WriteFile(path, []byte("Begin: Loading essential drivers ... "), 0o600); err != nil {
		t.Fatal(err)
	}
	boot := managed("boot", vm.StateRunning)
	boot.ConsoleLog = path
	f.vbox.Put(boot)
	booted := false
	f.vbox.ExecFunc = func(vm.Machine, vm.ExecRequest) (vm.ExecResult, error) {
		if booted {
			return vm.ExecResult{}, nil
		}
		log, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return vm.ExecResult{}, err
		}
		defer log.Close()
		if _, err := log.WriteString("\r\n[    1.48] Kernel panic - not syncing: Attempted to kill init! exitcode=0x00000009\r\n"); err != nil {
			return vm.ExecResult{}, err
		}
		return vm.ExecResult{}, vm.ErrNotReady
	}
	f.vbox.ResetFunc = func(vm.Machine) { booted = true }
	res := callOK[harness.WaitResult](t, f, "vm_wait", map[string]any{"vm": "boot", "for": "guest", "user": "root", "password": "pw"})
	if !slices.Equal(res.Recoveries, []string{"kernel panic: Attempted to kill init! exitcode=0x00000009 — reset"}) {
		t.Fatalf("recoveries = %q", res.Recoveries)
	}
	if mach := callOK[vm.Machine](t, f, "vm_get", map[string]any{"vm": "boot"}); mach.ConsoleLog != path {
		t.Errorf("console_log = %q", mach.ConsoleLog)
	}
}

func TestProvidersCarryWarnings(t *testing.T) {
	f := newFixture(t)
	f.vbox.Warnings = []string{"VirtualBox runs on top of Hyper-V"}
	out := callOK[providersOutput](t, f, "vm_providers", nil)
	if !slices.Equal(out.Providers[0].Info.Warnings, f.vbox.Warnings) || out.Providers[1].Info.Warnings != nil {
		t.Fatalf("providers = %+v", out.Providers)
	}
}
