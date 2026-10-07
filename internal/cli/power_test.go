package cli

import (
	"testing"

	"github.com/fl4metf/vm-harness/vm"
)

func TestPowerCycle(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateStopped, true)

	steps := []struct {
		args []string
		want vm.State
	}{
		{[]string{"start", "web"}, vm.StateRunning},
		{[]string{"start", "web", "--gui"}, vm.StateRunning},
		{[]string{"pause", "web"}, vm.StatePaused},
		{[]string{"resume", "web"}, vm.StateRunning},
		{[]string{"reset", "web"}, vm.StateRunning},
		{[]string{"suspend", "web"}, vm.StateSaved},
		{[]string{"start", "web"}, vm.StateRunning},
		{[]string{"stop", "web", "--timeout", "30"}, vm.StateStopped},
		{[]string{"stop", "web"}, vm.StateStopped},
		{[]string{"start", "web"}, vm.StateRunning},
		{[]string{"stop", "web", "--force"}, vm.StateStopped},
	}
	for _, s := range steps {
		got := decode[vm.Machine](t, e.ok(s.args...).stdout)
		if got.State != s.want {
			t.Fatalf("vmh %v: state %s, want %s", s.args, got.State, s.want)
		}
	}

	e.fail(5, "invalid_state", "pause", "web")
	e.fail(5, "invalid_state", "reset", "web")
	e.fail(3, "not_found", "start", "nothere")
}

func TestPowerText(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateStopped, true)
	if out := e.ok("start", "web", "-o", "text").stdout; out != "web  running\n" {
		t.Fatalf("text start printed %q", out)
	}
}
