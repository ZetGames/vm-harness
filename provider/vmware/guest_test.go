package vmware

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZetGames/vm-harness/internal/fakerun"
	"github.com/ZetGames/vm-harness/runner"
	"github.com/ZetGames/vm-harness/vm"
)

func TestPowerCommands(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web"))
	ctx := context.Background()
	for _, prefix := range []string{"start", "stop", "pause", "unpause", "reset", "suspend"} {
		e.fake.On("-T ws "+prefix, "")
	}
	steps := []struct {
		call func() error
		want string
	}{
		{func() error { return e.Start(ctx, "web", false) }, "start " + path + " nogui"},
		{func() error { return e.Start(ctx, "web", true) }, "start " + path + " gui"},
		{func() error { return e.Stop(ctx, "web", true) }, "stop " + path + " hard"},
		{func() error { return e.Pause(ctx, "web") }, "pause " + path},
		{func() error { return e.Resume(ctx, "web") }, "unpause " + path},
		{func() error { return e.Reset(ctx, "web") }, "reset " + path + " hard"},
		{func() error { return e.Suspend(ctx, "web") }, "suspend " + path + " hard"},
	}
	for _, s := range steps {
		if err := s.call(); err != nil {
			t.Fatal(err)
		}
		calls := e.commands()
		if got := calls[len(calls)-1]; got != "-T ws "+s.want {
			t.Errorf("command = %q, want %q", got, "-T ws "+s.want)
		}
	}
	e.fake.OnResult("-T ws stop", failed(fixture(t, "vmrun/not-powered-on.txt")))
	wantKind(t, e.Stop(ctx, "web", true), vm.ErrInvalidState)
	e.fake.OnResult("-T ws start", failed(fixture(t, "vmrun/cannot-be-found.txt")))
	wantKind(t, e.Start(ctx, "web", false), vm.ErrNotFound)
	wantKind(t, e.Start(ctx, "nope", false), vm.ErrNotFound)
}

func TestPauseIsTracked(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web"))
	e.setRunning(path)
	ctx := context.Background()
	for _, prefix := range []string{"pause", "unpause", "stop", "suspend", "reset", "start"} {
		e.fake.On("-T ws "+prefix, "")
	}
	state := func() vm.State {
		t.Helper()
		m, err := e.Get(ctx, "web")
		if err != nil {
			t.Fatal(err)
		}
		return m.State
	}
	for _, leave := range []func() error{
		func() error { return e.Resume(ctx, "web") },
		func() error { return e.Stop(ctx, "web", true) },
		func() error { return e.Suspend(ctx, "web") },
		func() error { return e.Reset(ctx, "web") },
		func() error { return e.Start(ctx, "web", false) },
	} {
		if err := e.Pause(ctx, "web"); err != nil {
			t.Fatal(err)
		}
		if s := state(); s != vm.StatePaused {
			t.Fatalf("state after pause = %s", s)
		}
		if err := leave(); err != nil {
			t.Fatal(err)
		}
		if s := state(); s != vm.StateRunning {
			t.Fatalf("state = %s, the pause marker must be cleared", s)
		}
	}
	e.fake.OnResult("-T ws pause", failed(fixture(t, "vmrun/not-powered-on.txt")))
	wantKind(t, e.Pause(ctx, "web"), vm.ErrInvalidState)
	if isFile(pausedMarker(path)) {
		t.Fatal("a failed pause must not leave a marker")
	}
}

func TestForcedStopDiscardsSuspendedState(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web")+"checkpoint.vmState = \"web-5cf61512.vmss\"\r\ncheckpoint.vmState.readOnly = \"FALSE\"\r\n")
	dir := filepath.Dir(path)
	vmss := writeFile(t, filepath.Join(dir, "web-5cf61512.vmss"), "state")
	vmem := writeFile(t, filepath.Join(dir, "web-5cf61512.vmem"), "memory")
	snapshot := writeFile(t, filepath.Join(dir, "web-Snapshot1.vmem"), "snapshot memory")
	e.fake.On("-T ws stop", "")
	ctx := context.Background()
	if err := e.Stop(ctx, "web", true); err != nil {
		t.Fatal(err)
	}
	if e.fake.Called("-T ws stop") {
		t.Fatal("vmrun cannot stop a suspended vm")
	}
	if isFile(vmss) || isFile(vmem) || !isFile(snapshot) {
		t.Fatal("only the suspended state may be removed")
	}
	if strings.Contains(readFile(t, path), "checkpoint.") {
		t.Fatalf("checkpoint keys left:\n%s", readFile(t, path))
	}
	if m, err := e.Get(ctx, "web"); err != nil || m.State != vm.StateStopped {
		t.Fatalf("state = %s, %v", m.State, err)
	}

	snapState := e.addVM(t, "snap", simpleVMX("snap")+"checkpoint.vmState = \"snap-Snapshot2.vmsn\"\r\n")
	vmsn := writeFile(t, filepath.Join(filepath.Dir(snapState), "snap-Snapshot2.vmsn"), "snapshot")
	if err := e.Stop(ctx, "snap", true); err != nil {
		t.Fatal(err)
	}
	if !isFile(vmsn) {
		t.Fatal("a snapshot file named by checkpoint.vmState must stay")
	}
	if m, err := e.Get(ctx, "snap"); err != nil || m.State != vm.StateStopped {
		t.Fatalf("state = %s, %v", m.State, err)
	}
}

func TestForcedStopKeepsOtherSuspendedState(t *testing.T) {
	e := newTestEnv(t)
	web := e.addVM(t, "web", simpleVMX("web"))
	dir := filepath.Dir(web)
	web2 := writeFile(t, filepath.Join(dir, "web-2.vmx"), simpleVMX("web-2")+"checkpoint.vmState = \"web-2-1a2b3c4d.vmss\"\r\n")
	vmss := writeFile(t, filepath.Join(dir, "web-2-1a2b3c4d.vmss"), "state")
	vmem := writeFile(t, filepath.Join(dir, "web-2-1a2b3c4d.vmem"), "memory")
	outside := writeFile(t, filepath.Join(t.TempDir(), "far-1a2b3c4d.vmss"), "state")
	far := e.addVM(t, "far", simpleVMX("far")+"checkpoint.vmState = \""+outside+"\"\r\n")
	ctx := context.Background()
	for path, want := range map[string]vm.State{web: vm.StateStopped, web2: vm.StateSaved, far: vm.StateSaved} {
		if m, err := e.Get(ctx, path); err != nil || m.State != want {
			t.Fatalf("%s: state = %s, %v, want %s", path, m.State, err, want)
		}
	}
	e.fake.On("-T ws stop", "")
	if err := e.Stop(ctx, web, true); err != nil {
		t.Fatal(err)
	}
	if err := discardSuspend(web); err != nil {
		t.Fatal(err)
	}
	if !isFile(vmss) || !isFile(vmem) {
		t.Fatal("the suspended state of another vm in the same folder was deleted")
	}
	if err := e.Stop(ctx, far, true); err != nil {
		t.Fatal(err)
	}
	if !isFile(outside) {
		t.Fatal("a suspend file outside the vm folder was deleted")
	}
	if m, err := e.Get(ctx, far); err != nil || m.State != vm.StateStopped {
		t.Fatalf("state = %s, %v", m.State, err)
	}
}

func TestGracefulStopNeedsTools(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web"))
	e.setRunning(path)
	e.fake.OnResult("-T ws checkToolsState", runner.Result{ExitCode: 254, Stdout: []byte(fixture(t, "vmrun/checktoolsstate-unknown.txt"))})
	e.fake.On("-T ws stop", "")
	ctx := context.Background()
	wantKind(t, e.Stop(ctx, "web", false), vm.ErrNotReady)
	if e.fake.Called("-T ws stop") {
		t.Fatal("soft stop without tools hangs in vmrun and must not be attempted")
	}
	e.fake.On("-T ws checkToolsState", "running\r\n")
	if err := e.Stop(ctx, "web", false); err != nil {
		t.Fatal(err)
	}
	if !e.fake.Called("-T ws stop " + path + " soft") {
		t.Fatalf("calls = %q", e.commands())
	}
	e.setRunning()
	e.fake.OnResult("-T ws checkToolsState", runner.Result{ExitCode: 254, Stdout: []byte("unknown\r\n")})
	wantKind(t, e.Stop(ctx, "web", false), vm.ErrInvalidState)
}

func TestPortForwardsUnsupported(t *testing.T) {
	e := newTestEnv(t)
	wantKind(t, e.AddPortForward(context.Background(), "web", vm.PortForward{Name: "ssh"}), vm.ErrUnsupported)
	wantKind(t, e.RemovePortForward(context.Background(), "web", "ssh"), vm.ErrUnsupported)
}

func TestGuestIP(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web"))
	ctx := context.Background()
	e.fake.On("-T ws getGuestIPAddress "+path, "192.168.80.130\r\n")
	ip, err := e.GuestIP(ctx, "web")
	if err != nil || ip != "192.168.80.130" {
		t.Fatalf("ip = %q, %v", ip, err)
	}
	cases := map[string]error{
		fixture(t, "vmrun/tools-not-running.txt"): vm.ErrNotReady,
		fixture(t, "vmrun/not-powered-on.txt"):    vm.ErrInvalidState,
		"Error: Unable to get the IP address\r\n": vm.ErrNotReady,
	}
	for out, kind := range cases {
		e.fake.OnResult("-T ws getGuestIPAddress", failed(out))
		_, err := e.GuestIP(ctx, "web")
		wantKind(t, err, kind)
	}
	e.fake.On("-T ws getGuestIPAddress", "unknown\r\n")
	_, err = e.GuestIP(ctx, "web")
	wantKind(t, err, vm.ErrNotReady)
}

type guestFiles struct {
	t       *testing.T
	scripts map[string]string
	outputs map[string]string
	deleted []string
}

func (g *guestFiles) install(f *fakerun.Fake, user string) {
	creds := "-T ws -gu " + user + " -gp pw "
	f.On("-T ws checkToolsState", "running\r\n")
	f.OnFunc(creds+"CopyFileFromHostToGuest", func(c fakerun.Call) (runner.Result, error) {
		g.scripts[c.Args[len(c.Args)-1]] = readFile(g.t, c.Args[len(c.Args)-2])
		return runner.Result{}, nil
	})
	f.OnFunc(creds+"CopyFileFromGuestToHost", func(c fakerun.Call) (runner.Result, error) {
		guest, host := c.Args[len(c.Args)-2], c.Args[len(c.Args)-1]
		for suffix, content := range g.outputs {
			if strings.HasSuffix(guest, suffix) {
				return runner.Result{}, os.WriteFile(host, []byte(content), 0o644)
			}
		}
		return failed("Error: A file was not found\r\n"), nil
	})
	f.OnFunc(creds+"deleteFileInGuest", func(c fakerun.Call) (runner.Result, error) {
		g.deleted = append(g.deleted, c.Args[len(c.Args)-1])
		return runner.Result{}, nil
	})
}

func runProgramArgs(t *testing.T, f *fakerun.Fake) []string {
	t.Helper()
	calls := f.Find("-T ws -gu ")
	for _, c := range calls {
		if c.Args[6] == "runProgramInGuest" {
			return c.Args[8:]
		}
	}
	t.Fatal("runProgramInGuest was not called")
	return nil
}

func TestExecLinux(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web"))
	g := &guestFiles{t: t, scripts: map[string]string{}, outputs: map[string]string{".out": "hi there\n", ".err": "warning\n"}}
	g.install(e.fake, "root")
	e.fake.OnResult("-T ws -gu root -gp pw runProgramInGuest "+path, failed("Guest program exited with non-zero exit code: 3\r\n"))
	res, err := e.Exec(context.Background(), "web", vm.ExecRequest{
		Command:     []string{"echo", "hi there"},
		Env:         map[string]string{"A": "1"},
		WorkDir:     "/work dir",
		Credentials: vm.Credentials{User: "root", Password: "pw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.Stdout != "hi there\n" || res.Stderr != "warning\n" {
		t.Fatalf("result = %+v", res)
	}
	args := runProgramArgs(t, e.fake)
	if len(args) != 2 || args[0] != "/bin/sh" || !strings.HasPrefix(args[1], "/tmp/vmh-") || !strings.HasSuffix(args[1], ".sh") {
		t.Fatalf("program = %q", args)
	}
	base := strings.TrimSuffix(args[1], ".sh")
	want := "exec >" + base + ".out 2>" + base + ".err\nexport A=1; cd '/work dir' || exit 1\necho 'hi there'\n"
	if got := g.scripts[args[1]]; got != want {
		t.Fatalf("script = %q, want %q", got, want)
	}
	if !slices.Equal(g.deleted, []string{base + ".sh", base + ".out", base + ".err"}) {
		t.Fatalf("deleted = %q", g.deleted)
	}
}

func TestExecWindows(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "win", simpleVMX("win")+"guestOS = \"windows9-64\"\r\n")
	g := &guestFiles{t: t, scripts: map[string]string{}, outputs: map[string]string{".out": "ok\r\n", ".err": ""}}
	g.install(e.fake, "Administrator")
	e.fake.On("-T ws -gu Administrator -gp pw runProgramInGuest "+path, "")
	res, err := e.Exec(context.Background(), "win", vm.ExecRequest{
		Command:     []string{`C:\Program Files\tool.exe`, "a&b", "50%"},
		Env:         map[string]string{"PATH_EXTRA": `C:\bin;%TEMP%`},
		WorkDir:     `C:\work`,
		Credentials: vm.Credentials{User: "Administrator", Password: "pw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.Stdout != "ok\r\n" {
		t.Fatalf("result = %+v", res)
	}
	args := runProgramArgs(t, e.fake)
	if len(args) != 5 || args[0] != `C:\Windows\System32\cmd.exe` || args[1] != "/c" {
		t.Fatalf("program = %q", args)
	}
	base := strings.TrimSuffix(args[2], ".cmd")
	if !strings.HasPrefix(base, `C:\Windows\Temp\vmh-`) || args[3] != ">"+base+".out" || args[4] != "2>"+base+".err" {
		t.Fatalf("program = %q", args)
	}
	want := "@echo off\r\n" +
		"setlocal DisableDelayedExpansion\r\n" +
		"set PATH_EXTRA=C:\\bin;%%TEMP%%\r\n" +
		"cd /d \"C:\\work\" || exit /b 1\r\n" +
		"set vmh_arg0=^\"C:\\Program Files\\tool.exe^\"\r\n" +
		"set vmh_arg1=a^&b\r\n" +
		"set vmh_arg2=50%%\r\n" +
		"setlocal EnableDelayedExpansion\r\n" +
		"!vmh_arg0! !vmh_arg1! !vmh_arg2!\r\n" +
		"exit /b %errorlevel%\r\n"
	if got := g.scripts[args[2]]; got != want {
		t.Fatalf("script = %q, want %q", got, want)
	}
}

func TestExecWindowsFollowsGenericOSType(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "win", simpleVMX("win")+"guestOS = \"other-64\"\r\n")
	writeFile(t, metaPath(path), `{"os_type": "windows11"}`)
	g := &guestFiles{t: t, scripts: map[string]string{}, outputs: map[string]string{".out": "", ".err": ""}}
	g.install(e.fake, "Administrator")
	e.fake.On("-T ws -gu Administrator -gp pw runProgramInGuest "+path, "")
	if _, err := e.Exec(context.Background(), "win", vm.ExecRequest{Command: []string{"ver"}, Credentials: vm.Credentials{User: "Administrator", Password: "pw"}}); err != nil {
		t.Fatal(err)
	}
	if args := runProgramArgs(t, e.fake); args[0] != vm.WindowsShell {
		t.Fatalf("program = %q", args)
	}
}

func TestExecScriptUsesShellAsIs(t *testing.T) {
	job := guestJob{base: "/tmp/vmh-x"}
	got, err := job.script(vm.ExecRequest{Script: "ls | wc -l"})
	if err != nil || got != "exec >/tmp/vmh-x.out 2>/tmp/vmh-x.err\nls | wc -l\n" {
		t.Fatalf("script = %q, %v", got, err)
	}
	win := guestJob{windows: true, base: `C:\Windows\Temp\vmh-x`}
	got, err = win.script(vm.ExecRequest{Script: "dir\nver"})
	if err != nil || got != "@echo off\r\nsetlocal DisableDelayedExpansion\r\ndir\r\nver\r\nexit /b %errorlevel%\r\n" {
		t.Fatalf("batch = %q, %v", got, err)
	}
}

func TestBatchScriptSwitchesToUTF8ForNonASCII(t *testing.T) {
	got, err := batchScript(vm.ExecRequest{Script: "echo привет", WorkDir: `C:\Данные`})
	want := "@echo off\r\nchcp 65001 >nul\r\nsetlocal DisableDelayedExpansion\r\ncd /d \"C:\\Данные\" || exit /b 1\r\necho привет\r\nexit /b %errorlevel%\r\n"
	if err != nil || got != want {
		t.Fatalf("batch = %q, %v", got, err)
	}
}

func TestBatchScriptRejectsLineBreaks(t *testing.T) {
	cases := []vm.ExecRequest{
		{Command: []string{"findstr", "a\r\necho INJECTED"}},
		{Command: []string{"whoami"}, Env: map[string]string{"A": "x\necho INJECTED"}},
		{Command: []string{"whoami"}, WorkDir: "C:\\\r\necho INJECTED"},
		{Command: []string{"whoami"}, WorkDir: `C:\x" & echo INJECTED & "`},
	}
	for _, req := range cases {
		_, err := batchScript(req)
		wantKind(t, err, vm.ErrInvalid)
	}
	e := newTestEnv(t)
	e.addVM(t, "web", simpleVMX("web"))
	_, err := e.Exec(context.Background(), "web", vm.ExecRequest{Command: []string{"true"}, Env: map[string]string{"A=B": "x"}, Credentials: vm.Credentials{User: "root"}})
	wantKind(t, err, vm.ErrInvalid)
}

func TestExecLinuxTimeoutRunsInGuest(t *testing.T) {
	job := guestJob{base: "/tmp/vmh-x"}
	got, err := job.script(vm.ExecRequest{Command: []string{"sleep", "60"}, TimeoutSec: 30})
	want := "if [ -z \"$VMH_TIMEOUT\" ]; then\n" +
		"exec >/tmp/vmh-x.out 2>/tmp/vmh-x.err\n" +
		"if timeout -k 5 1 true 2>/dev/null; then export VMH_TIMEOUT=1; exec timeout -k 5 30 /bin/sh \"$0\"; fi\n" +
		"fi\n" +
		"unset VMH_TIMEOUT\n" +
		"sleep 60\n"
	if err != nil || got != want {
		t.Fatalf("script = %q, %v\nwant %q", got, err, want)
	}
	if d := job.hostTimeout(30); d <= 30*time.Second {
		t.Fatalf("the host must wait longer than the guest timeout, got %s", d)
	}
}

func TestExecRequiresToolsAndCredentials(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web"))
	ctx := context.Background()
	_, err := e.Exec(ctx, "web", vm.ExecRequest{Command: []string{"true"}})
	wantKind(t, err, vm.ErrInvalid)
	_, err = e.Exec(ctx, "web", vm.ExecRequest{Credentials: vm.Credentials{User: "root"}})
	wantKind(t, err, vm.ErrInvalid)

	e.fake.OnResult("-T ws checkToolsState", runner.Result{ExitCode: 254, Stdout: []byte("unknown\r\n")})
	req := vm.ExecRequest{Command: []string{"true"}, Credentials: vm.Credentials{User: "root", Password: "pw"}}
	_, err = e.Exec(ctx, "web", req)
	wantKind(t, err, vm.ErrInvalidState)
	e.setRunning(path)
	_, err = e.Exec(ctx, "web", req)
	wantKind(t, err, vm.ErrNotReady)
	e.fake.On("-T ws checkToolsState", "installed\r\n")
	_, err = e.Exec(ctx, "web", req)
	wantKind(t, err, vm.ErrNotReady)
	if e.fake.Called("-T ws -gu") {
		t.Fatal("guest commands without running tools block for minutes and must not run")
	}
}

func TestExecTimeout(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web"))
	g := &guestFiles{t: t, scripts: map[string]string{}, outputs: map[string]string{".out": "partial", ".err": ""}}
	g.install(e.fake, "root")
	e.fake.OnFunc("-T ws -gu root -gp pw runProgramInGuest "+path, func(fakerun.Call) (runner.Result, error) {
		time.Sleep(1100 * time.Millisecond)
		return failed("Guest program exited with non-zero exit code: 124\r\n"), nil
	})
	req := vm.ExecRequest{Command: []string{"sleep", "60"}, TimeoutSec: 1, Credentials: vm.Credentials{User: "root", Password: "pw"}}
	_, err := e.Exec(context.Background(), "web", req)
	if !errors.Is(err, context.DeadlineExceeded) || vm.Code(err) != vm.CodeTimeout {
		t.Fatalf("err = %v", err)
	}
	if len(g.deleted) != 3 {
		t.Fatalf("guest files not cleaned up: %q", g.deleted)
	}

	e.fake.OnResult("-T ws -gu root -gp pw runProgramInGuest "+path, failed("Guest program exited with non-zero exit code: 124\r\n"))
	req.TimeoutSec = 30
	res, err := e.Exec(context.Background(), "web", req)
	if err != nil || res.ExitCode != 124 {
		t.Fatalf("a quick exit 124 is the command's own status: %+v, %v", res, err)
	}
}

func TestExecTimeoutKillsWindowsJob(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "win", simpleVMX("win")+"guestOS = \"windows9-64\"\r\n")
	g := &guestFiles{t: t, scripts: map[string]string{}, outputs: map[string]string{}}
	g.install(e.fake, "Administrator")
	creds := "-T ws -gu Administrator -gp pw "
	var killed []string
	e.fake.OnFunc(creds+"runProgramInGuest "+path, func(c fakerun.Call) (runner.Result, error) {
		if c.Args[8] == windowsKill {
			killed = c.Args[9:]
			return runner.Result{}, nil
		}
		time.Sleep(1200 * time.Millisecond)
		return runner.Result{}, context.DeadlineExceeded
	})
	e.fake.OnFunc(creds+"listProcessesInGuest "+path, func(fakerun.Call) (runner.Result, error) {
		var script string
		for name := range g.scripts {
			script = name
		}
		out := "Process list: 3\r\npid=4, owner=NT AUTHORITY\\SYSTEM, cmd=System\r\n" +
			"pid=4120, owner=WIN\\Administrator, cmd=\"C:\\Windows\\System32\\cmd.exe\" /c " + strings.ToUpper(script) + " >x.out 2>x.err\r\n" +
			"pid=4188, owner=WIN\\Administrator, cmd=C:\\tools\\long.exe\r\n"
		return runner.Result{Stdout: []byte(out)}, nil
	})
	_, err := e.Exec(context.Background(), "win", vm.ExecRequest{
		Command:     []string{`C:\tools\long.exe`},
		TimeoutSec:  1,
		Credentials: vm.Credentials{User: "Administrator", Password: "pw"},
	})
	if vm.Code(err) != vm.CodeTimeout {
		t.Fatalf("err = %v", err)
	}
	if want := []string{"/F", "/T", "/PID", "4120"}; !slices.Equal(killed, want) {
		t.Fatalf("taskkill args = %q, want %q", killed, want)
	}
	if len(g.deleted) != 3 {
		t.Fatalf("guest files not cleaned up: %q", g.deleted)
	}
}

func TestCopyBetweenHostAndGuest(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web"))
	e.fake.On("-T ws checkToolsState", "running\r\n")
	e.fake.On("-T ws -gu root -gp pw Copy", "")
	ctx := context.Background()
	cred := vm.Credentials{User: "root", Password: "pw"}
	if err := e.CopyTo(ctx, "web", vm.CopyRequest{HostPath: `C:\tmp\a.txt`, GuestPath: "/root/a.txt", Credentials: cred}); err != nil {
		t.Fatal(err)
	}
	if err := e.CopyFrom(ctx, "web", vm.CopyRequest{HostPath: `C:\tmp\b.txt`, GuestPath: "/var/log/syslog", Credentials: cred}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-T ws -gu root -gp pw CopyFileFromHostToGuest " + path + ` C:\tmp\a.txt /root/a.txt`,
		"-T ws -gu root -gp pw CopyFileFromGuestToHost " + path + ` /var/log/syslog C:\tmp\b.txt`,
	}
	for _, w := range want {
		if !e.fake.Called(w) {
			t.Errorf("missing %q in %q", w, e.commands())
		}
	}
	wantKind(t, e.CopyTo(ctx, "web", vm.CopyRequest{HostPath: "a", GuestPath: "b"}), vm.ErrInvalid)
}

func TestScreenshot(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web"))
	png := "\x89PNG\r\n\x1a\nimage"
	e.fake.On("-T ws checkToolsState", "running\r\n")
	e.fake.OnFunc("-T ws -gu root -gp pw captureScreen "+path, func(c fakerun.Call) (runner.Result, error) {
		return runner.Result{}, os.WriteFile(c.Args[len(c.Args)-1], []byte(png), 0o644)
	})
	ctx := context.Background()
	data, err := e.Screenshot(ctx, "web", vm.Credentials{User: "root", Password: "pw"})
	if err != nil || string(data) != png {
		t.Fatalf("screenshot = %q, %v", data, err)
	}
	_, err = e.Screenshot(ctx, "web", vm.Credentials{})
	wantKind(t, err, vm.ErrInvalid)
	e.fake.OnResult("-T ws -gu root -gp pw captureScreen", failed(fixture(t, "vmrun/anonymous-guest.txt")))
	_, err = e.Screenshot(ctx, "web", vm.Credentials{User: "root", Password: "pw"})
	wantKind(t, err, vm.ErrInvalid)
}
