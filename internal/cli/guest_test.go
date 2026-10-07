package cli

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/fl4metf/vm-harness/harness"
	"github.com/fl4metf/vm-harness/internal/memprovider"
	"github.com/fl4metf/vm-harness/vm"
)

func TestExecPassesRequest(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)
	var got vm.ExecRequest
	e.vbox.ExecFunc = func(_ vm.Machine, req vm.ExecRequest) (vm.ExecResult, error) {
		got = req
		return vm.ExecResult{Stdout: "ok\n"}, nil
	}

	res := decode[vm.ExecResult](t, e.ok("exec", "web", "-u", "root", "--password", "pw", "--env", "A=1", "--env", "B=x=y",
		"--workdir", "/srv", "--timeout", "90s", "--", "ls", "-la", "/").stdout)
	if res.ExitCode != 0 || res.Stdout != "ok\n" || res.Transport != harness.TransportGuest {
		t.Errorf("result = %+v", res)
	}
	if !slices.Equal(got.Command, []string{"ls", "-la", "/"}) {
		t.Errorf("command = %q", got.Command)
	}
	if !maps.Equal(got.Env, map[string]string{"A": "1", "B": "x=y"}) {
		t.Errorf("env = %v", got.Env)
	}
	if got.WorkDir != "/srv" || got.TimeoutSec != 90 || got.User != "root" || got.Password != "pw" {
		t.Errorf("request = %+v", got)
	}

	e.ok("exec", "web", "--script", "echo hi && echo there", "--timeout", "1500ms")
	if got.Script != "echo hi && echo there" || len(got.Command) != 0 || got.TimeoutSec != 2 {
		t.Errorf("script request = %+v", got)
	}
}

func TestExecExitCode(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)
	e.vbox.ExecFunc = func(vm.Machine, vm.ExecRequest) (vm.ExecResult, error) {
		return vm.ExecResult{ExitCode: 42, Stdout: "partial <output> & more\n", Stderr: "boom\n"}, nil
	}

	r := e.run("exec", "web", "--", "false")
	if r.code != 42 {
		t.Fatalf("exit %d, want 42", r.code)
	}
	res := decode[vm.ExecResult](t, r.stdout)
	if res.ExitCode != 42 || res.Stderr != "boom\n" {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(r.stdout, "<output> & more") {
		t.Errorf("json output escapes html: %s", r.stdout)
	}

	r = e.run("-o", "text", "exec", "web", "--", "false")
	if r.code != 42 || r.stdout != "partial <output> & more\n" || r.stderr != "boom\n" {
		t.Errorf("text exec: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestExecExitCodeOutOfHostRange(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)
	e.vbox.ExecFunc = func(vm.Machine, vm.ExecRequest) (vm.ExecResult, error) {
		return vm.ExecResult{ExitCode: 256}, nil
	}
	r := e.run("exec", "web", "--", "cmd", "/c", "exit", "256")
	if want := hostExitCode(runtime.GOOS, 256); r.code != want || r.code == 0 {
		t.Errorf("exit %d, want %d", r.code, want)
	}
	if res := decode[vm.ExecResult](t, r.stdout); res.ExitCode != 256 {
		t.Errorf("json exit_code = %d, want 256", res.ExitCode)
	}
}

func TestHostExitCode(t *testing.T) {
	cases := []struct {
		goos       string
		code, want int
	}{
		{"linux", 1, 1},
		{"linux", 42, 42},
		{"linux", 255, 255},
		{"linux", 256, 1},
		{"linux", 512, 1},
		{"darwin", 1603, 1},
		{"freebsd", 3010, 1},
		{"linux", -1, 1},
		{"windows", 256, 256},
		{"windows", 3010, 3010},
		{"windows", -1, -1},
	}
	for _, c := range cases {
		if got := hostExitCode(c.goos, c.code); got != c.want {
			t.Errorf("hostExitCode(%s, %d) = %d, want %d", c.goos, c.code, got, c.want)
		}
	}
}

func TestExecText(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)
	e.vbox.ExecFunc = func(_ vm.Machine, req vm.ExecRequest) (vm.ExecResult, error) {
		return vm.ExecResult{Stdout: "a\tb\n", Stderr: "warning\n", Truncated: true}, nil
	}
	r := e.run("-o", "text", "exec", "web", "--", "cat", "x")
	if r.code != 0 {
		t.Fatalf("exit %d", r.code)
	}
	if r.stdout != "a\tb\n" {
		t.Errorf("stdout = %q", r.stdout)
	}
	if !strings.HasPrefix(r.stderr, "warning\n") || !strings.Contains(r.stderr, "vmh: output was cut") {
		t.Errorf("stderr = %q", r.stderr)
	}
}

func TestExecFailures(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)
	e.put(e.vbox, "off", vm.StateStopped, true)
	e.put(e.vbox, "legacy", vm.StateRunning, false)

	e.fail(125, "not_found", "exec", "missing", "--", "true")
	e.fail(125, "invalid_state", "exec", "off", "--", "true")
	e.fail(125, "forbidden", "exec", "legacy", "--", "true")
	e.fail(125, "invalid_argument", "exec", "web")
	e.fail(125, "invalid_argument", "exec", "web", "--script", "x", "--", "y")
	e.fail(125, "invalid_argument", "exec", "web", "--env", "NOVALUE", "--", "true")
	e.fail(125, "invalid_argument", "exec", "web", "--transport", "telnet", "--", "true")
	e.fail(125, "usage", "exec")
	e.fail(125, "usage", "exec", "web", "--timeout", "forever", "--", "true")

	r := e.run("-o", "text", "exec", "missing", "--", "true")
	if r.code != 125 || r.stdout != "" || !strings.HasPrefix(r.stderr, "vmh: ") {
		t.Errorf("text failure: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestCopy(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, filepath.Join(dir, "app.tar"), "payload")

	plan := decode[copyPlan](t, e.ok("cp", "app.tar", "web:/tmp/").stdout)
	want := copyPlan{VM: "web", Direction: toGuest, HostPath: filepath.Join(dir, "app.tar"), GuestPath: "/tmp/app.tar"}
	if plan != want {
		t.Errorf("plan = %+v, want %+v", plan, want)
	}
	if data, ok := e.vbox.GuestFile("web", "/tmp/app.tar"); !ok || string(data) != "payload" {
		t.Fatalf("guest file = %q, %v", data, ok)
	}

	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	e.ok("cp", "web:/tmp/app.tar", out)
	if data, err := os.ReadFile(filepath.Join(out, "app.tar")); err != nil || string(data) != "payload" {
		t.Fatalf("host file = %q, %v", data, err)
	}

	e.ok("cp", "web:/tmp/app.tar", filepath.Join(dir, "renamed.tar"))
	if _, err := os.Stat(filepath.Join(dir, "renamed.tar")); err != nil {
		t.Fatal(err)
	}

	if r := e.ok("-o", "text", "cp", "app.tar", "web:/tmp/again.tar"); r.stdout != "" {
		t.Errorf("text cp printed %q", r.stdout)
	}

	e.fail(9, "invalid_argument", "cp", "web:/a", "web:/b")
	e.fail(9, "invalid_argument", "cp", "a", "b")
	e.fail(9, "invalid_argument", "cp", filepath.Join(dir, "missing"), "web:/tmp/x")
	e.fail(3, "not_found", "cp", "web:/nope", out)
}

func TestIP(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)

	e.fail(10, "not_ready", "ip", "web")
	e.fail(7, "timeout", "ip", "web", "--wait", "50ms")

	e.vbox.SetIP("web", "10.0.2.15")
	if got := decode[ipResult](t, e.ok("ip", "web").stdout); got.IP != "10.0.2.15" {
		t.Errorf("ip = %q", got.IP)
	}
	if got := decode[ipResult](t, e.ok("ip", "web", "--wait", "1m").stdout); got.IP != "10.0.2.15" {
		t.Errorf("waited ip = %q", got.IP)
	}
	if out := e.ok("ip", "web", "-o", "text").stdout; out != "10.0.2.15\n" {
		t.Errorf("text ip = %q", out)
	}
}

func TestWait(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)
	e.vbox.SetIP("web", "192.168.56.10")

	res := decode[harness.WaitResult](t, e.ok("wait", "web", "--for", "running").stdout)
	if res.Machine.Name != "web" || res.Machine.State != vm.StateRunning {
		t.Errorf("wait result = %+v", res)
	}
	res = decode[harness.WaitResult](t, e.ok("wait", "web", "--for", "ip").stdout)
	if res.IP != "192.168.56.10" {
		t.Errorf("wait ip = %+v", res)
	}
	if out := e.ok("wait", "web", "--for", "ip", "-o", "text").stdout; out != "192.168.56.10\n" {
		t.Errorf("text wait ip = %q", out)
	}
	e.ok("wait", "web", "--for", "guest", "-u", "root", "--password", "pw")

	e.fail(9, "invalid_argument", "wait", "web")
	e.fail(9, "invalid_argument", "wait", "web", "--for", "dinner")
	e.fail(7, "timeout", "wait", "web", "--for", "paused", "--timeout", "50ms")
}

func TestScreenshot(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)
	t.Chdir(t.TempDir())

	got := decode[screenshotResult](t, e.ok("screenshot", "web", "shot.png").stdout)
	abs, _ := filepath.Abs("shot.png")
	if got.Path != abs || got.Bytes != len(memprovider.PNG) {
		t.Errorf("result = %+v", got)
	}
	data, err := os.ReadFile("shot.png")
	if err != nil || !bytes.Equal(data, memprovider.PNG) {
		t.Fatalf("screenshot file = %q, %v", data, err)
	}
	e.put(e.vbox, "off", vm.StateStopped, true)
	e.fail(5, "invalid_state", "screenshot", "off", "off.png")
}
