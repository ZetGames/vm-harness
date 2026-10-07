package harness

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/fl4metf/vm-harness/internal/memprovider"
	"github.com/fl4metf/vm-harness/vm"
)

func TestStartIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("a", vm.StateStopped))
	for range 2 {
		got, err := e.m.Start(t.Context(), vboxRef("a"), false)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != vm.StateRunning {
			t.Fatalf("state = %s", got.State)
		}
	}
	if calls := callsOf(e.vbox, "start"); len(calls) != 1 {
		t.Fatalf("start calls = %v", calls)
	}
}

func TestStartFromSavedAndPaused(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("saved", vm.StateSaved))
	e.vbox.Put(managedVM("paused", vm.StatePaused))
	got, err := e.m.Start(t.Context(), vboxRef("saved"), false)
	if err != nil || got.State != vm.StateRunning {
		t.Fatalf("Start(saved) = %s, %v", got.State, err)
	}
	_, err = e.m.Start(t.Context(), vboxRef("paused"), false)
	wantErr(t, err, vm.ErrInvalidState)
}

func TestStopIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("a", vm.StateRunning))
	for range 2 {
		got, err := e.m.Stop(t.Context(), vboxRef("a"), StopOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got.State != vm.StateStopped {
			t.Fatalf("state = %s", got.State)
		}
	}
	if got, want := e.vbox.Calls(), opsOn(mustGet(t, e.vbox, "a").ID, "stop"); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestStopGracefulTimesOutThenForces(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("a", vm.StateRunning))
	e.vbox.SoftStopFunc = func(vm.Machine) error { return nil }
	start := time.Now()
	got, err := e.m.Stop(t.Context(), vboxRef("a"), StopOptions{Timeout: 60 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Fatalf("forced after %s, before the timeout", elapsed)
	}
	if got.State != vm.StateStopped {
		t.Fatalf("state = %s", got.State)
	}
	if calls, want := e.vbox.Calls(), opsOn(got.ID, "stop", "kill"); !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestStopGracefulCompletesWhileWaiting(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("a", vm.StateRunning))
	e.vbox.SoftStopFunc = func(vm.Machine) error {
		time.AfterFunc(20*time.Millisecond, func() { e.vbox.SetState("a", vm.StateStopped) })
		return nil
	}
	got, err := e.m.Stop(t.Context(), vboxRef("a"), StopOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != vm.StateStopped {
		t.Fatalf("state = %s", got.State)
	}
	if calls := callsOf(e.vbox, "kill"); len(calls) > 0 {
		t.Fatalf("forced although the guest shut down: %v", calls)
	}
}

func TestStopGracefulRequestFailure(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		forced bool
	}{
		{"guest not ready", vm.ErrNotReady, true},
		{"unsupported", vm.ErrUnsupported, true},
		{"state changed under the request", vm.ErrInvalidState, true},
		{"other failure", vm.ErrInvalid, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.vbox.Put(managedVM("a", vm.StateRunning))
			e.vbox.SoftStopFunc = func(vm.Machine) error { return c.err }
			start := time.Now()
			got, err := e.m.Stop(t.Context(), vboxRef("a"), StopOptions{Timeout: time.Minute})
			if time.Since(start) > 5*time.Second {
				t.Fatal("waited for the graceful timeout")
			}
			if !c.forced {
				wantErr(t, err, c.err)
				if state := mustGet(t, e.vbox, "a").State; state != vm.StateRunning {
					t.Fatalf("state = %s", state)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.State != vm.StateStopped || len(callsOf(e.vbox, "kill")) != 1 {
				t.Fatalf("state %s, calls %v", got.State, e.vbox.Calls())
			}
		})
	}
}

func TestStopForce(t *testing.T) {
	cases := []struct {
		name  string
		state vm.State
		opts  StopOptions
		calls []string
		err   error
	}{
		{name: "force", state: vm.StateRunning, opts: StopOptions{Force: true}, calls: []string{"kill"}},
		{name: "paused is powered off", state: vm.StatePaused, calls: []string{"kill"}},
		{name: "already stopped", state: vm.StateStopped, opts: StopOptions{Force: true}},
		{name: "saved state is discarded", state: vm.StateSaved, calls: []string{"kill"}},
		{name: "saved with force", state: vm.StateSaved, opts: StopOptions{Force: true}, calls: []string{"kill"}},
		{name: "busy", state: vm.StateBusy, calls: []string{"kill"}},
		{name: "unknown", state: vm.StateUnknown, calls: []string{"kill"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.vbox.Put(managedVM("a", c.state))
			got, err := e.m.Stop(t.Context(), vboxRef("a"), c.opts)
			if c.err != nil {
				wantErr(t, err, c.err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.State != vm.StateStopped || !slices.Equal(e.vbox.Calls(), opsOn(got.ID, c.calls...)) {
				t.Fatalf("state %s, calls %v", got.State, e.vbox.Calls())
			}
		})
	}
}

func TestPowerTransitions(t *testing.T) {
	type op func(*Manager, *testing.T) (vm.Machine, error)
	pause := func(m *Manager, t *testing.T) (vm.Machine, error) { return m.Pause(t.Context(), vboxRef("a")) }
	resume := func(m *Manager, t *testing.T) (vm.Machine, error) { return m.Resume(t.Context(), vboxRef("a")) }
	reset := func(m *Manager, t *testing.T) (vm.Machine, error) { return m.Reset(t.Context(), vboxRef("a")) }
	suspend := func(m *Manager, t *testing.T) (vm.Machine, error) { return m.Suspend(t.Context(), vboxRef("a")) }
	cases := []struct {
		name string
		op   op
		from vm.State
		want vm.State
	}{
		{"pause running", pause, vm.StateRunning, vm.StatePaused},
		{"pause stopped", pause, vm.StateStopped, ""},
		{"pause paused", pause, vm.StatePaused, ""},
		{"resume paused", resume, vm.StatePaused, vm.StateRunning},
		{"resume running", resume, vm.StateRunning, ""},
		{"reset running", reset, vm.StateRunning, vm.StateRunning},
		{"reset stopped", reset, vm.StateStopped, ""},
		{"suspend running", suspend, vm.StateRunning, vm.StateSaved},
		{"suspend paused", suspend, vm.StatePaused, vm.StateSaved},
		{"suspend stopped", suspend, vm.StateStopped, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.vbox.Put(managedVM("a", c.from))
			got, err := c.op(e.m, t)
			if c.want == "" {
				wantErr(t, err, vm.ErrInvalidState)
				if calls := e.vbox.Calls(); len(calls) > 0 {
					t.Fatalf("provider called: %v", calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.State != c.want {
				t.Fatalf("state = %s, want %s", got.State, c.want)
			}
		})
	}
}

type lateShutdown struct {
	*memprovider.Provider
}

func (l lateShutdown) Stop(ctx context.Context, ref string, force bool) error {
	if !force {
		return l.Provider.Stop(ctx, ref, force)
	}
	l.SetState(ref, vm.StateStopped)
	return fmt.Errorf("machine %q is not currently running: %w", ref, vm.ErrInvalidState)
}

func TestPowerOffToleratesGuestThatAlreadyStopped(t *testing.T) {
	p := lateShutdown{memprovider.New(vm.VirtualBox)}
	p.Put(managedVM("a", vm.StateRunning))
	m := newManager(t, Config{}, p)
	got, err := m.Stop(t.Context(), Ref{VM: "a"}, StopOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != vm.StateStopped {
		t.Fatalf("state = %s", got.State)
	}
}

type hangingSoftStop struct {
	*memprovider.Provider
}

func (h hangingSoftStop) Stop(ctx context.Context, ref string, force bool) error {
	if force {
		return h.Provider.Stop(ctx, ref, force)
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestStopCutsAHangingGracefulRequest(t *testing.T) {
	p := hangingSoftStop{memprovider.New(vm.VirtualBox)}
	p.Put(managedVM("a", vm.StateRunning))
	m := newManager(t, Config{}, p)
	start := time.Now()
	got, err := m.Stop(t.Context(), Ref{VM: "a"}, StopOptions{Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("stopped after %s", elapsed)
	}
	if got.State != vm.StateStopped || len(callsOf(p.Provider, "kill")) != 1 {
		t.Fatalf("state %s, calls %v", got.State, p.Calls())
	}
}
