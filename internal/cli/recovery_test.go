package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fl4metf/vm-harness/harness"
	"github.com/fl4metf/vm-harness/vm"
)

const (
	consolePanic = "[    1.481516] Kernel panic - not syncing: IO-APIC + timer doesn't work!  Boot with apic=debug and send a report.\r\n"
	panicNote    = "kernel panic: IO-APIC + timer doesn't work! — reset"
)

func bootingVM(t *testing.T, e *testEnv) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "serial.log")
	if err := os.WriteFile(path, []byte("Begin: Loading essential drivers ... "), 0o600); err != nil {
		t.Fatal(err)
	}
	e.vbox.Put(vm.Machine{Name: "web", State: vm.StateRunning, Managed: true, ConsoleLog: path})
	return path
}

func appendLog(t *testing.T, path, text string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Error(err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Error(err)
	}
}

func TestWaitReportsAutomaticResets(t *testing.T) {
	for _, format := range []string{formatJSON, formatText} {
		t.Run(format, func(t *testing.T) {
			e := newEnv(t)
			path := bootingVM(t, e)
			booted := false
			e.vbox.ExecFunc = func(vm.Machine, vm.ExecRequest) (vm.ExecResult, error) {
				if booted {
					return vm.ExecResult{}, nil
				}
				appendLog(t, path, consolePanic)
				return vm.ExecResult{}, fmt.Errorf("guest additions: %w", vm.ErrNotReady)
			}
			e.vbox.ResetFunc = func(vm.Machine) { booted = true }
			r := e.ok("wait", "web", "--for", "guest", "-u", "root", "--password", "pw", "-o", format)
			if format == formatJSON {
				res := decode[harness.WaitResult](t, r.stdout)
				if !slices.Equal(res.Recoveries, []string{panicNote}) || r.stderr != "" {
					t.Fatalf("result = %+v, stderr %q", res, r.stderr)
				}
				return
			}
			if r.stdout != "web  running\n" || r.stderr != "vmh: "+panicNote+"\n" {
				t.Fatalf("stdout %q, stderr %q", r.stdout, r.stderr)
			}
		})
	}
}

func TestIPWaitReportsAutomaticResets(t *testing.T) {
	e := newEnv(t)
	path := bootingVM(t, e)
	e.vbox.ResetFunc = func(vm.Machine) { e.vbox.SetIP("web", "10.0.2.15") }
	time.AfterFunc(300*time.Millisecond, func() { appendLog(t, path, consolePanic) })
	r := e.ok("ip", "web", "--wait", "1m", "-o", "text")
	if r.stdout != "10.0.2.15\n" || r.stderr != "vmh: "+panicNote+"\n" {
		t.Fatalf("stdout %q, stderr %q", r.stdout, r.stderr)
	}
}

func TestProvidersShowWarnings(t *testing.T) {
	e := newEnv(t)
	warning := "VirtualBox runs on top of Hyper-V"
	e.vbox.Warnings = []string{warning}

	providers := decode[[]harness.ProviderStatus](t, e.ok("providers").stdout)
	if !slices.Equal(providers[0].Info.Warnings, []string{warning}) || providers[1].Info.Warnings != nil {
		t.Fatalf("providers = %+v", providers)
	}
	out := e.ok("providers", "-o", "text").stdout
	if !strings.HasSuffix(out, "vmware      yes                 mem      \nwarning: virtualbox: "+warning+"\n") {
		t.Fatalf("text providers:\n%s", out)
	}
}

func TestWaitStopsWhenTheBootFailsAgain(t *testing.T) {
	e := newEnv(t)
	path := bootingVM(t, e)
	appendLog(t, path, consolePanic)
	e.vbox.ResetFunc = func(vm.Machine) { appendLog(t, path, consolePanic) }
	r := e.run("wait", "web", "--for", "ip", "-o", "text")
	failure := `vmh: boot of vm "web" is stuck again after 2 automatic resets (` + panicNote + "; " + panicNote + "): " +
		"kernel panic: IO-APIC + timer doesn't work!; this wait gives up, and another wait would reset the vm again, " +
		"so fix the cause first (see the console log " + path + "): not ready\n"
	if r.code != 10 || r.stderr != "vmh: "+panicNote+"\nvmh: "+panicNote+"\n"+failure {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	doc := decode[errorDocument](t, e.run("wait", "web", "--for", "ip").stdout)
	if doc.Error.Code != "not_ready" || !strings.Contains(doc.Error.Message, "after 2 automatic resets") {
		t.Fatalf("json error = %+v", doc.Error)
	}
}
