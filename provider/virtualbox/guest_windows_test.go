package virtualbox

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/fl4metf/vm-harness/vm"
)

const argvHelperEnv = "VMH_VBOX_ARGV_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(argvHelperEnv) == "1" {
		json.NewEncoder(os.Stdout).Encode(os.Args[1:])
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runLikeGuest(t *testing.T, dir string, argv []string) string {
	t.Helper()
	cmd := exec.Command(argv[0])
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: strings.Join(argv, " ")}
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", cmd.SysProcAttr.CmdLine, err, out)
	}
	return string(out)
}

func TestWindowsCommandReachesProgramUnchanged(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv(argvHelperEnv, "1")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	want := []string{"a b", "x&whoami", `C:\data\R&D.txt`, `say "hi"`, "%PATH%", "%", "a^b", "(x)|<y>>z", "!z!", "", `trail\`, `C:\Program Files\`, `"`}
	for _, name := range []string{"argv helper.exe", "argv&(helper)^%x%.exe", "argvhelper"} {
		if err := os.WriteFile(filepath.Join(dir, strings.TrimSuffix(name, ".exe")+".exe"), data, 0o755); err != nil {
			t.Fatal(err)
		}
		argv, unquoted, err := guestCommand(true, vm.ExecRequest{Command: append([]string{name}, want...)})
		if err != nil || !unquoted {
			t.Fatalf("argv = %q, unquoted = %v, err = %v", argv, unquoted, err)
		}
		var got []string
		if err := json.Unmarshal([]byte(runLikeGuest(t, dir, argv)), &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: program saw %q\nwant %q", name, got, want)
		}
	}
}

func TestWindowsMultiLineScriptRunsEveryLine(t *testing.T) {
	cases := []struct {
		script, out string
		code        int
	}{
		{"echo one\necho two\r\nexit /b 3", "one\r\ntwo\r\n", 3},
		{"echo a\r\ncmd /c exit 5\r\n", "a\r\n", 5},
		{"set V=50%%\r\necho %V% !V!\n", "50% !V!\r\n", 0},
	}
	for _, c := range cases {
		file := filepath.Join(t.TempDir(), "run.cmd")
		if err := os.WriteFile(file, []byte(batchFile(c.script)), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(vm.WindowsShell, "/d", "/c", file).Output()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if string(out) != c.out || code != c.code {
			t.Errorf("%q: output %q, exit code %d", c.script, out, code)
		}
	}
}

func TestWindowsScriptRunsVerbatim(t *testing.T) {
	argv, unquoted, err := guestCommand(true, vm.ExecRequest{Script: `echo "a b"& dir "C:\Program Files" >nul && echo ok`})
	if err != nil || !unquoted {
		t.Fatalf("argv = %q, unquoted = %v, err = %v", argv, unquoted, err)
	}
	if got := runLikeGuest(t, t.TempDir(), argv); got != "\"a b\"\r\nok\r\n" {
		t.Fatalf("output = %q", got)
	}
}
