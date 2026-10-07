package vmware

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fl4metf/vm-harness/internal/shellquote"
	"github.com/fl4metf/vm-harness/vm"
)

func runBatch(t *testing.T, req vm.ExecRequest) ([]byte, error) {
	t.Helper()
	script, err := batchScript(req)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "job.cmd")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	return exec.Command(vm.WindowsShell, "/c", file).Output()
}

func TestBatchScriptPassesArgumentsLiterally(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{
		`/c:x"&echo INJECTED&rem `, `a & b`, `"quoted"`, `"&calc&"`, `a"b`, `\"`, `back\slash\`, `trail\\`,
		`50%`, `%PATH%`, `!bang!`, `!PATH!`, `!`, `^caret`, `^`, `x^&y`, `<in>`, `pipe|`, `(paren)`,
		`with space`, ``, `/?`, `C:\Program Files\x`,
	}
	probe := `p"&echo INJECTED2&" %PATH% !PATH! ^ <>|`
	work := filepath.Join(t.TempDir(), "dir & 50% !x! ^y (z)")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runBatch(t, vm.ExecRequest{
		Command: append([]string{exe}, args...),
		Env:     map[string]string{"VMH_ECHO_ARGS": "1", "VMH_PROBE": probe},
		WorkDir: work,
	})
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var got []string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if want := append([]string{work, probe}, args...); !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestBatchScriptKeepsExitCode(t *testing.T) {
	_, err := runBatch(t, vm.ExecRequest{Command: []string{vm.WindowsShell, "/c", "exit 7"}})
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("err = %v", err)
	}
}

func TestBatchScriptPassesNonASCIIText(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "Данные café")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"привет", "café", "日本"}
	probe := "значение"
	env := map[string]string{"VMH_ECHO_ARGS": "1", "VMH_PROBE": probe}
	requests := map[string]vm.ExecRequest{
		"command": {Command: append([]string{exe}, args...), Env: env, WorkDir: work},
		"script":  {Script: shellquote.Windows(exe) + " " + strings.Join(args, " "), Env: env, WorkDir: work},
	}
	for name, req := range requests {
		out, err := runBatch(t, req)
		if err != nil {
			t.Fatalf("%s: %v: %s", name, err, out)
		}
		var got []string
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: output %q: %v", name, out, err)
		}
		if want := append([]string{work, probe}, args...); !slices.Equal(got, want) {
			t.Errorf("%s: got  %q\nwant %q", name, got, want)
		}
	}
}
