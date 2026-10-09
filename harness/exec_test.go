package harness

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ZetGames/vm-harness/internal/sshexec"
	"github.com/ZetGames/vm-harness/vm"
)

func sshVM(name string, forwards ...vm.PortForward) vm.Machine {
	mach := managedVM(name, vm.StateRunning)
	mach.Meta[vm.MetaSSHUser] = "vmh"
	mach.Meta[vm.MetaSSHKey] = filepath.Join("keys", "virtualbox-"+name)
	mach.PortForwards = forwards
	return mach
}

func execCmd(transport string, ssh SSHOptions, argv ...string) ExecRequest {
	return ExecRequest{ExecRequest: vm.ExecRequest{Command: argv}, Transport: transport, SSH: ssh}
}

func TestExecTransportSelection(t *testing.T) {
	key := filepath.Join("keys", "virtualbox-a")
	sshFwd := vm.PortForward{Name: sshForwardName, Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 40022, GuestPort: 22}
	cases := []struct {
		name      string
		machine   vm.Machine
		ip        string
		transport string
		ssh       SSHOptions
		want      sshexec.Config
		err       error
	}{
		{
			name:    "guest by default",
			machine: managedVM("a", vm.StateRunning),
		},
		{
			name:    "ssh when a key is on record",
			machine: sshVM("a", sshFwd),
			want:    sshexec.Config{Host: "127.0.0.1", Port: 40022, User: "vmh", KeyPath: key},
		},
		{
			name:      "explicit guest wins over the key on record",
			machine:   sshVM("a", sshFwd),
			transport: TransportGuest,
		},
		{
			name:      "forward on all interfaces is reached on loopback",
			machine:   sshVM("a", vm.PortForward{Name: "ssh", Protocol: "tcp", HostIP: "0.0.0.0", HostPort: 2200, GuestPort: 22}),
			transport: TransportAuto,
			want:      sshexec.Config{Host: "127.0.0.1", Port: 2200, User: "vmh", KeyPath: key},
		},
		{
			name:    "udp forward to port 22 is ignored",
			machine: sshVM("a", vm.PortForward{Name: "u", Protocol: "udp", HostPort: 5000, GuestPort: 22}),
			ip:      "10.0.2.15",
			want:    sshexec.Config{Host: "10.0.2.15", Port: 22, User: "vmh", KeyPath: key},
		},
		{
			name:    "forward on an address of another host is not dialled",
			machine: sshVM("a", vm.PortForward{Name: "pivot", Protocol: "tcp", HostIP: "192.0.2.10", HostPort: 22, GuestPort: 22}),
			ip:      "10.0.2.15",
			want:    sshexec.Config{Host: "10.0.2.15", Port: 22, User: "vmh", KeyPath: key},
		},
		{
			name:    "guest ip without a forward",
			machine: sshVM("a"),
			ip:      "192.168.56.10",
			want:    sshexec.Config{Host: "192.168.56.10", Port: 22, User: "vmh", KeyPath: key},
		},
		{
			name:      "explicit credentials",
			machine:   sshVM("a", sshFwd),
			transport: TransportSSH,
			ssh:       SSHOptions{User: "root", KeyPath: "id", Password: "pw"},
			want:      sshexec.Config{Host: "127.0.0.1", Port: 40022, User: "root", KeyPath: "id", Password: "pw"},
		},
		{
			name:    "password selects ssh",
			machine: managedVM("a", vm.StateRunning),
			ip:      "10.0.2.15",
			ssh:     SSHOptions{User: "dev", Password: "pw"},
			want:    sshexec.Config{Host: "10.0.2.15", Port: 22, User: "dev", Password: "pw"},
		},
		{
			name:      "no ssh user",
			machine:   managedVM("a", vm.StateRunning),
			transport: TransportSSH,
			ssh:       SSHOptions{Password: "pw"},
			err:       vm.ErrInvalid,
		},
		{
			name:      "no ssh credentials",
			machine:   managedVM("a", vm.StateRunning),
			transport: TransportSSH,
			ssh:       SSHOptions{User: "dev"},
			err:       vm.ErrInvalid,
		},
		{
			name:    "guest ip not ready",
			machine: sshVM("a"),
			err:     vm.ErrNotReady,
		},
		{
			name:      "unknown transport",
			machine:   managedVM("a", vm.StateRunning),
			transport: "winrm",
			err:       vm.ErrInvalid,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			stub := stubSSH(e.m)
			e.vbox.Put(c.machine)
			if c.ip != "" {
				e.vbox.SetIP("a", c.ip)
			}
			res, err := e.m.Exec(t.Context(), vboxRef("a"), execCmd(c.transport, c.ssh, "uname"))
			if c.err != nil {
				wantErr(t, err, c.err)
				if stub.used() || len(callsOf(e.vbox, "exec")) > 0 {
					t.Fatal("command ran despite the error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.want.Host == "" {
				if res.Transport != TransportGuest || stub.used() || len(callsOf(e.vbox, "exec")) != 1 || res.Stdout != "uname\n" {
					t.Fatalf("guest exec not used: %+v", res)
				}
				return
			}
			c.want.KnownHostsPath = e.m.knownHostsPath(mustGet(t, e.vbox, "a"))
			if c.ssh.KeyPath != "" {
				c.want.KeyPath = mustCanonical(t, c.ssh.KeyPath)
			}
			if cfg := stub.lastConfig(t); cfg != c.want {
				t.Fatalf("ssh config = %+v\nwant %+v", cfg, c.want)
			}
			if res.Transport != TransportSSH || res.Stdout != "ssh uname" || len(callsOf(e.vbox, "exec")) > 0 {
				t.Fatalf("ssh exec not used: %+v", res)
			}
		})
	}
}

func TestExecWindowsGuest(t *testing.T) {
	e := newEnv(t)
	stub := stubSSH(e.m)
	win := sshVM("win", vm.PortForward{Name: sshForwardName, Protocol: "tcp", HostPort: 40022, GuestPort: 22})
	win.Meta[vm.MetaOSType] = "windows11"
	e.vbox.Put(win)

	_, err := e.m.Exec(t.Context(), vboxRef("win"), execCmd(TransportSSH, SSHOptions{}, "whoami"))
	wantErr(t, err, vm.ErrUnsupported)
	res, err := e.m.Exec(t.Context(), vboxRef("win"), execCmd("", SSHOptions{}, "whoami"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Transport != TransportGuest || stub.used() {
		t.Fatalf("auto picked %s for a windows guest", res.Transport)
	}
}

func TestExecPinsHostKeysForEveryVM(t *testing.T) {
	e := newEnvWith(t, Config{AllowUnmanaged: true})
	stub := stubSSH(e.m)
	e.vbox.Put(foreignVM("Ubuntu 22.04", vm.StateRunning))
	e.vbox.Put(foreignVM("../escape", vm.StateRunning))
	e.vbox.Put(sshVM("keyed"))
	for _, name := range []string{"Ubuntu 22.04", "../escape", "keyed"} {
		e.vbox.SetIP(name, "10.0.2.15")
		_, err := e.m.Exec(t.Context(), vboxRef(name), execCmd(TransportSSH, SSHOptions{User: "u", Password: "p"}, "id"))
		if err != nil {
			t.Fatal(err)
		}
		known := stub.lastConfig(t).KnownHostsPath
		want := filepath.Join(e.root, "keys")
		if name == "keyed" {
			want = "keys"
		}
		if filepath.Dir(known) != want || !strings.HasSuffix(known, ".known_hosts") || strings.Contains(known, "escape") {
			t.Fatalf("known_hosts for %q = %q", name, known)
		}
	}
	ubuntu, escape := mustGet(t, e.vbox, "Ubuntu 22.04"), mustGet(t, e.vbox, "../escape")
	if e.m.knownHostsPath(ubuntu) == e.m.knownHostsPath(escape) {
		t.Fatal("two vms share a known_hosts file")
	}
}

func TestExecValidation(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("a", vm.StateRunning))
	cases := []vm.ExecRequest{
		{},
		{Command: []string{"ls"}, Script: "ls"},
		{Command: []string{"ls"}, TimeoutSec: -1},
		{Command: []string{"env"}, Env: map[string]string{"BAD-NAME": "x"}},
		{Command: []string{"env"}, Env: map[string]string{"1X": "x"}},
	}
	for _, req := range cases {
		_, err := e.m.Exec(t.Context(), vboxRef("a"), ExecRequest{ExecRequest: req})
		wantErr(t, err, vm.ErrInvalid)
	}
	if calls := e.vbox.Calls(); len(calls) > 0 {
		t.Fatalf("provider called: %v", calls)
	}
}

func TestExecPassesRequestAndKeepsExitCode(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("a", vm.StateRunning))
	var seen vm.ExecRequest
	e.vbox.ExecFunc = func(_ vm.Machine, req vm.ExecRequest) (vm.ExecResult, error) {
		seen = req
		return vm.ExecResult{ExitCode: 3, Stderr: "boom"}, nil
	}
	req := vm.ExecRequest{
		Script:      "make test",
		Env:         map[string]string{"CI": "1"},
		WorkDir:     "/src",
		TimeoutSec:  30,
		Credentials: vm.Credentials{User: "root", Password: "pw"},
	}
	res, err := e.m.Exec(t.Context(), vboxRef("a"), ExecRequest{ExecRequest: req})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.Stderr != "boom" || res.Truncated {
		t.Fatalf("result = %+v", res)
	}
	if seen.Script != req.Script || seen.Env["CI"] != "1" || seen.WorkDir != "/src" || seen.TimeoutSec != 30 || seen.User != "root" || seen.Password != "pw" {
		t.Fatalf("provider saw %+v", seen)
	}
}

func TestExecTruncatesOutput(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		stderr string
		cut    bool
	}{
		{"small", "ok", "", false},
		{"exactly the limit", strings.Repeat("a", maxOutput), "", false},
		{"stdout over", strings.Repeat("a", maxOutput+10), "warn", true},
		{"stderr over", "", strings.Repeat("e", maxOutput+1), true},
		{"multibyte runes", strings.Repeat("€", maxOutput/3+5), "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.vbox.Put(managedVM("a", vm.StateRunning))
			e.vbox.ExecFunc = func(vm.Machine, vm.ExecRequest) (vm.ExecResult, error) {
				return vm.ExecResult{Stdout: c.stdout, Stderr: c.stderr}, nil
			}
			res, err := e.m.Exec(t.Context(), vboxRef("a"), execCmd("", SSHOptions{}, "cat", "big"))
			if err != nil {
				t.Fatal(err)
			}
			if res.Truncated != c.cut {
				t.Fatalf("truncated = %v", res.Truncated)
			}
			for _, out := range []string{res.Stdout, res.Stderr} {
				if len(out) > maxOutput || !utf8.ValidString(out) {
					t.Fatalf("output of %d bytes, valid utf-8 %v", len(out), utf8.ValidString(out))
				}
			}
			if !strings.HasPrefix(c.stdout, res.Stdout) || !strings.HasPrefix(c.stderr, res.Stderr) {
				t.Fatal("output is not a prefix of the original")
			}
			if c.cut && len(res.Stdout) < maxOutput-3 && len(res.Stderr) < maxOutput-3 {
				t.Fatal("cut more than needed")
			}
		})
	}
}

func TestExecTruncatesSSHOutput(t *testing.T) {
	e := newEnv(t)
	stub := stubSSH(e.m)
	stub.run = func(vm.ExecRequest) (vm.ExecResult, error) {
		return vm.ExecResult{Stdout: strings.Repeat("x", maxOutput*2)}, nil
	}
	e.vbox.Put(sshVM("a", vm.PortForward{Name: sshForwardName, Protocol: "tcp", HostPort: 40022, GuestPort: 22}))
	res, err := e.m.Exec(t.Context(), vboxRef("a"), execCmd("", SSHOptions{}, "yes"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != maxOutput || !res.Truncated || res.Transport != TransportSSH {
		t.Fatalf("stdout %d bytes, truncated %v, transport %s", len(res.Stdout), res.Truncated, res.Transport)
	}
}

func TestCopyOverGuestTools(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("a", vm.StateRunning))
	dir := t.TempDir()
	host := writeFile(t, filepath.Join(dir, "in.txt"), "hello")
	t.Chdir(dir)

	err := e.m.CopyTo(t.Context(), vboxRef("a"), CopyRequest{CopyRequest: vm.CopyRequest{HostPath: "in.txt", GuestPath: "/tmp/in.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if data, ok := e.vbox.GuestFile("a", "/tmp/in.txt"); !ok || string(data) != "hello" {
		t.Fatalf("guest file = %q, %v", data, ok)
	}
	out := filepath.Join(dir, "out.txt")
	if err := e.m.CopyFrom(t.Context(), vboxRef("a"), CopyRequest{CopyRequest: vm.CopyRequest{HostPath: "out.txt", GuestPath: "/tmp/in.txt"}}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(out); string(data) != "hello" {
		t.Fatalf("host file = %q", data)
	}

	invalid := []CopyRequest{
		{CopyRequest: vm.CopyRequest{HostPath: host}},
		{CopyRequest: vm.CopyRequest{GuestPath: "/tmp/x"}},
		{CopyRequest: vm.CopyRequest{HostPath: filepath.Join(dir, "missing"), GuestPath: "/tmp/x"}},
		{CopyRequest: vm.CopyRequest{HostPath: dir, GuestPath: "/tmp/x"}},
	}
	for _, req := range invalid {
		wantErr(t, e.m.CopyTo(t.Context(), vboxRef("a"), req), vm.ErrInvalid)
	}
	wantErr(t, e.m.CopyFrom(t.Context(), vboxRef("a"), CopyRequest{CopyRequest: vm.CopyRequest{GuestPath: "/tmp/x"}}), vm.ErrInvalid)
	wantErr(t, e.m.CopyFrom(t.Context(), vboxRef("a"), CopyRequest{CopyRequest: vm.CopyRequest{HostPath: out, GuestPath: "/nope"}}), vm.ErrNotFound)
}

func TestCopyOverSSH(t *testing.T) {
	e := newEnv(t)
	stub := stubSSH(e.m)
	e.vbox.Put(sshVM("a", vm.PortForward{Name: sshForwardName, Protocol: "tcp", HostPort: 40022, GuestPort: 22}))
	host := writeFile(t, filepath.Join(t.TempDir(), "in.txt"), "over ssh")
	if err := e.m.CopyTo(t.Context(), vboxRef("a"), CopyRequest{CopyRequest: vm.CopyRequest{HostPath: host, GuestPath: "/srv/in.txt"}}); err != nil {
		t.Fatal(err)
	}
	if string(stub.files["/srv/in.txt"]) != "over ssh" {
		t.Fatalf("uploaded %q", stub.files["/srv/in.txt"])
	}
	if cfg := stub.lastConfig(t); cfg.Port != 40022 || cfg.User != "vmh" {
		t.Fatalf("ssh config = %+v", cfg)
	}
	out := filepath.Join(t.TempDir(), "out.txt")
	if err := e.m.CopyFrom(t.Context(), vboxRef("a"), CopyRequest{CopyRequest: vm.CopyRequest{HostPath: out, GuestPath: "/srv/in.txt"}}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(out); string(data) != "over ssh" {
		t.Fatalf("downloaded %q", data)
	}
	if calls := append(callsOf(e.vbox, "copyto"), callsOf(e.vbox, "copyfrom")...); len(calls) > 0 {
		t.Fatalf("guest tools used: %v", calls)
	}
}

func TestWriteAndReadFile(t *testing.T) {
	e := newEnv(t)
	stub := stubSSH(e.m)
	e.vbox.Put(managedVM("a", vm.StateRunning))
	e.vbox.SetIP("a", "10.0.2.15")
	guest := Access{Credentials: vm.Credentials{User: "root", Password: "pw"}}
	ssh := Access{Transport: TransportSSH, SSH: SSHOptions{User: "dev", Password: "pw"}}

	for name, acc := range map[string]Access{"guest": guest, "ssh": ssh} {
		t.Run(name, func(t *testing.T) {
			data := []byte("line one\nline two\x00binary")
			if err := e.m.WriteFile(t.Context(), vboxRef("a"), "/etc/motd", data, acc); err != nil {
				t.Fatal(err)
			}
			got, err := e.m.ReadFile(t.Context(), vboxRef("a"), "/etc/motd", acc)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("read %q", got)
			}
			_, err = e.m.ReadFile(t.Context(), vboxRef("a"), "/missing", acc)
			wantErr(t, err, vm.ErrNotFound)
		})
	}
	if _, ok := e.vbox.GuestFile("a", "/etc/motd"); !ok {
		t.Fatal("guest transport did not write through the provider")
	}
	if _, ok := stub.files["/etc/motd"]; !ok {
		t.Fatal("ssh transport did not upload")
	}
}

func TestReadFileLimit(t *testing.T) {
	e := newEnv(t)
	stub := stubSSH(e.m)
	e.vbox.Put(sshVM("a", vm.PortForward{Name: sshForwardName, Protocol: "tcp", HostPort: 40022, GuestPort: 22}))
	stub.files["/var/log/huge"] = bytes.Repeat([]byte{'x'}, maxReadFile+1)
	stub.files["/var/log/fits"] = bytes.Repeat([]byte{'x'}, 1024)
	_, err := e.m.ReadFile(t.Context(), vboxRef("a"), "/var/log/huge", Access{})
	wantErr(t, err, vm.ErrLimit)
	if got, err := e.m.ReadFile(t.Context(), vboxRef("a"), "/var/log/fits", Access{}); err != nil || len(got) != 1024 {
		t.Fatalf("ReadFile = %d bytes, %v", len(got), err)
	}
	if !slices.Equal(stub.limits, []int64{maxReadFile, maxReadFile}) {
		t.Fatalf("ssh reads were limited to %v, want the read_file limit", stub.limits)
	}

	e.vbox.Put(managedVM("g", vm.StateRunning))
	guest := Access{Credentials: vm.Credentials{User: "root", Password: "pw"}}
	if err := e.m.WriteFile(t.Context(), vboxRef("g"), "/huge", bytes.Repeat([]byte{'x'}, maxReadFile+1), guest); err != nil {
		t.Fatal(err)
	}
	_, err = e.m.ReadFile(t.Context(), vboxRef("g"), "/huge", guest)
	wantErr(t, err, vm.ErrLimit)
}

func TestGuestOperationsNeedARunningVM(t *testing.T) {
	ops := map[string]func(m *Manager, ctx context.Context, host string) error{
		"exec": func(m *Manager, ctx context.Context, _ string) error {
			_, err := m.Exec(ctx, vboxRef("a"), execCmd("", SSHOptions{}, "true"))
			return err
		},
		"copy to": func(m *Manager, ctx context.Context, host string) error {
			return m.CopyTo(ctx, vboxRef("a"), CopyRequest{CopyRequest: vm.CopyRequest{HostPath: host, GuestPath: "/tmp/x"}})
		},
		"copy from": func(m *Manager, ctx context.Context, host string) error {
			return m.CopyFrom(ctx, vboxRef("a"), CopyRequest{CopyRequest: vm.CopyRequest{HostPath: host + ".out", GuestPath: "/tmp/x"}})
		},
		"write file": func(m *Manager, ctx context.Context, _ string) error {
			return m.WriteFile(ctx, vboxRef("a"), "/tmp/x", []byte("x"), Access{})
		},
		"read file": func(m *Manager, ctx context.Context, _ string) error {
			_, err := m.ReadFile(ctx, vboxRef("a"), "/tmp/x", Access{})
			return err
		},
		"screenshot": func(m *Manager, ctx context.Context, _ string) error {
			_, err := m.Screenshot(ctx, vboxRef("a"), vm.Credentials{})
			return err
		},
	}
	for _, state := range []vm.State{vm.StateStopped, vm.StateSaved, vm.StatePaused} {
		for name, op := range ops {
			t.Run(string(state)+" "+name, func(t *testing.T) {
				e := newEnv(t)
				stub := stubSSH(e.m)
				mach := sshVM("a", vm.PortForward{Name: sshForwardName, Protocol: "tcp", HostPort: 40022, GuestPort: 22})
				mach.State = state
				e.vbox.Put(mach)
				host := writeFile(t, filepath.Join(t.TempDir(), "in"), "x")
				err := op(e.m, t.Context(), host)
				wantErr(t, err, vm.ErrInvalidState)
				hint := "start it first"
				if state == vm.StatePaused {
					hint = "resume it first"
				}
				if !strings.Contains(err.Error(), hint) {
					t.Fatalf("error %q does not say %q", err, hint)
				}
				if stub.used() || len(e.vbox.Calls()) > 0 {
					t.Fatalf("guest was contacted: ssh %v, provider %v", stub.used(), e.vbox.Calls())
				}
			})
		}
	}
}

func TestCopyFromRefusesProtectedHostPaths(t *testing.T) {
	e := newEnv(t)
	stubSSH(e.m)
	e.vbox.Put(managedVM("a", vm.StateRunning))
	data := []byte("from the guest")
	if err := e.m.WriteFile(t.Context(), vboxRef("a"), "/tmp/x", data, Access{}); err != nil {
		t.Fatal(err)
	}
	vmDir := t.TempDir()
	legacy := foreignVM("legacy", vm.StateStopped)
	legacy.ConfigPath = filepath.Join(vmDir, "legacy.vbox")
	e.vmw.Put(legacy)
	notes := writeFile(t, filepath.Join(vmDir, "notes.txt"), "keep")
	writeFile(t, filepath.Join(e.root, "config.json"), "{}")
	free := t.TempDir()

	refused := map[string]string{
		"file in an unmanaged vm folder":  notes,
		"new file in an unmanaged folder": filepath.Join(vmDir, "Logs", "new.log"),
		"vm settings file":                filepath.Join(free, "other.VMX"),
		"virtual disk":                    filepath.Join(free, "disk.vdi"),
		"vmh metadata sidecar":            filepath.Join(free, "web.vmh.json"),
		"vmh config":                      filepath.Join(e.root, "config.json"),
		"vmh key directory":               filepath.Join(e.root, "keys", "virtualbox-a-0011"),
	}
	for name, host := range refused {
		t.Run(name, func(t *testing.T) {
			err := e.m.CopyFrom(t.Context(), vboxRef("a"), CopyRequest{CopyRequest: vm.CopyRequest{HostPath: host, GuestPath: "/tmp/x"}})
			wantErr(t, err, vm.ErrForbidden)
		})
	}
	if got, _ := os.ReadFile(notes); string(got) != "keep" {
		t.Fatalf("file in the unmanaged folder changed to %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(e.root, "config.json")); string(got) != "{}" {
		t.Fatalf("config changed to %q", got)
	}
	err := e.m.CopyFrom(t.Context(), vboxRef("a"), CopyRequest{CopyRequest: vm.CopyRequest{HostPath: free, GuestPath: "/tmp/x"}})
	wantErr(t, err, vm.ErrInvalid)
	out := filepath.Join(free, "out.txt")
	if err := e.m.CopyFrom(t.Context(), vboxRef("a"), CopyRequest{CopyRequest: vm.CopyRequest{HostPath: out, GuestPath: "/tmp/x"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, data) {
		t.Fatalf("copied %q", got)
	}
}

func TestHostDirsConfineHostPaths(t *testing.T) {
	allowed := t.TempDir()
	e := newEnvWith(t, Config{HostDirs: []string{"", allowed}})
	stubSSH(e.m)
	e.vbox.Put(managedVM("a", vm.StateRunning))
	inside := writeFile(t, filepath.Join(allowed, "in.txt"), "inside")
	outside := writeFile(t, filepath.Join(t.TempDir(), "secret.txt"), "outside")
	outsideDir := filepath.Dir(outside)
	copyTo := func(host string) error {
		return e.m.CopyTo(t.Context(), vboxRef("a"), CopyRequest{CopyRequest: vm.CopyRequest{HostPath: host, GuestPath: "/tmp/in"}})
	}
	copyFrom := func(host string) error {
		return e.m.CopyFrom(t.Context(), vboxRef("a"), CopyRequest{CopyRequest: vm.CopyRequest{HostPath: host, GuestPath: "/tmp/in"}})
	}

	if err := copyTo(inside); err != nil {
		t.Fatal(err)
	}
	if err := copyFrom(filepath.Join(allowed, "sub", "..", "out.txt")); err != nil {
		t.Fatal(err)
	}
	wantErr(t, copyTo(outside), vm.ErrForbidden)
	t.Chdir(outsideDir)
	wantErr(t, copyTo("secret.txt"), vm.ErrForbidden)
	wantErr(t, copyTo(filepath.Join(allowed, "..", filepath.Base(outsideDir), "secret.txt")), vm.ErrForbidden)
	wantErr(t, copyTo(filepath.Join(outsideDir, "missing.txt")), vm.ErrForbidden)
	wantErr(t, copyFrom(filepath.Join(outsideDir, "dropped.txt")), vm.ErrForbidden)
	if exists(filepath.Join(outsideDir, "dropped.txt")) {
		t.Fatal("file written outside the host directories")
	}
	if data, _ := e.vbox.GuestFile("a", "/tmp/in"); string(data) != "inside" {
		t.Fatalf("guest got %q", data)
	}

	link := filepath.Join(allowed, "escape")
	if err := os.Symlink(outsideDir, link); err != nil {
		t.Logf("symlinks unavailable, skipping the symlink case: %v", err)
		return
	}
	wantErr(t, copyTo(filepath.Join(link, "secret.txt")), vm.ErrForbidden)
	wantErr(t, copyFrom(filepath.Join(link, "planted.txt")), vm.ErrForbidden)
	if exists(filepath.Join(outsideDir, "planted.txt")) {
		t.Fatal("file written through a symlink out of the host directories")
	}
}

func TestSSHKeyPathConfinedToHostDirs(t *testing.T) {
	allowed := t.TempDir()
	e := newEnvWith(t, Config{HostDirs: []string{allowed}})
	stub := stubSSH(e.m)
	e.vbox.Put(managedVM("a", vm.StateRunning))
	e.vbox.SetIP("a", "10.0.2.15")
	exec := func(key string) error {
		_, err := e.m.Exec(t.Context(), vboxRef("a"), execCmd(TransportSSH, SSHOptions{User: "u", KeyPath: key}, "id"))
		return err
	}
	wantErr(t, exec(filepath.Join(t.TempDir(), "id_ed25519")), vm.ErrForbidden)
	if stub.used() {
		t.Fatal("ssh ran with a key outside the host directories")
	}
	for _, key := range []string{filepath.Join(allowed, "id"), filepath.Join(e.root, "keys", "virtualbox-a-0011")} {
		if err := exec(key); err != nil {
			t.Fatalf("key %s: %v", key, err)
		}
		if cfg := stub.lastConfig(t); cfg.KeyPath != mustCanonical(t, key) || cfg.Host != "10.0.2.15" {
			t.Fatalf("ssh config = %+v", cfg)
		}
	}
}
