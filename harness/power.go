package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/fl4metf/vm-harness/vm"
)

const (
	defaultStopTimeout = time.Minute
	powerOffTimeout    = 30 * time.Second
)

func poweredOn(s vm.State) bool { return s == vm.StateRunning || s == vm.StatePaused }

func (m *Manager) Start(ctx context.Context, ref Ref, gui bool) (vm.Machine, error) {
	return m.change(ctx, "start", ref, func(p vm.Provider, mach vm.Machine) error {
		switch mach.State {
		case vm.StateRunning:
			return nil
		case vm.StatePaused:
			return fmt.Errorf("vm %q is paused, resume it instead: %w", mach.Name, vm.ErrInvalidState)
		}
		return p.Start(ctx, mach.ID, gui)
	})
}

func (m *Manager) Stop(ctx context.Context, ref Ref, opts StopOptions) (vm.Machine, error) {
	return m.change(ctx, "stop", ref, func(p vm.Provider, mach vm.Machine) error {
		switch {
		case mach.State == vm.StateStopped:
			return nil
		case opts.Force || mach.State != vm.StateRunning:
			return m.powerOff(ctx, p, mach.ID)
		}
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = defaultStopTimeout
		}
		return m.shutdown(ctx, p, mach, timeout)
	})
}

func (m *Manager) shutdown(ctx context.Context, p vm.Provider, mach vm.Machine, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	requestCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	err := p.Stop(requestCtx, mach.ID, false)
	timedOut := err != nil && requestCtx.Err() != nil && ctx.Err() == nil
	if timedOut || errors.Is(err, vm.ErrNotReady) || errors.Is(err, vm.ErrUnsupported) || errors.Is(err, vm.ErrInvalidState) {
		m.log.Info("graceful stop failed, powering off", "vm", mach.Name, "error", err)
		return m.powerOff(ctx, p, mach.ID)
	}
	if err != nil {
		return err
	}
	stopped, err := m.waitStopped(ctx, p, mach.ID, time.Until(deadline))
	if err != nil || stopped {
		return err
	}
	m.log.Info("graceful stop timed out, powering off", "vm", mach.Name, "timeout", timeout)
	return m.powerOff(ctx, p, mach.ID)
}

func (m *Manager) powerOff(ctx context.Context, p vm.Provider, id string) error {
	if err := p.Stop(ctx, id, true); err != nil {
		if mach, gerr := p.Get(ctx, id); gerr == nil && mach.State == vm.StateStopped {
			return nil
		}
		return err
	}
	stopped, err := m.waitStopped(ctx, p, id, powerOffTimeout)
	if err != nil {
		return err
	}
	if !stopped {
		return fmt.Errorf("vm %s did not power off within %s: %w", id, powerOffTimeout, vm.ErrInvalidState)
	}
	return nil
}

func (m *Manager) waitStopped(ctx context.Context, p vm.Provider, id string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		mach, err := p.Get(ctx, id)
		if err != nil {
			return false, err
		}
		if mach.State == vm.StateStopped {
			return true, nil
		}
		pause := min(m.poll, time.Until(deadline))
		if pause <= 0 {
			return false, nil
		}
		if err := sleep(ctx, pause); err != nil {
			return false, err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) Pause(ctx context.Context, ref Ref) (vm.Machine, error) {
	return m.transition(ctx, "pause", ref, vm.Provider.Pause, vm.StateRunning)
}

func (m *Manager) Resume(ctx context.Context, ref Ref) (vm.Machine, error) {
	return m.transition(ctx, "resume", ref, vm.Provider.Resume, vm.StatePaused)
}

func (m *Manager) Reset(ctx context.Context, ref Ref) (vm.Machine, error) {
	return m.transition(ctx, "reset", ref, vm.Provider.Reset, vm.StateRunning)
}

func (m *Manager) Suspend(ctx context.Context, ref Ref) (vm.Machine, error) {
	return m.transition(ctx, "suspend", ref, vm.Provider.Suspend, vm.StateRunning, vm.StatePaused)
}

func (m *Manager) transition(ctx context.Context, op string, ref Ref, call func(vm.Provider, context.Context, string) error, from ...vm.State) (vm.Machine, error) {
	return m.change(ctx, op, ref, func(p vm.Provider, mach vm.Machine) error {
		if !slices.Contains(from, mach.State) {
			return fmt.Errorf("cannot %s vm %q while it is %s: %w", op, mach.Name, mach.State, vm.ErrInvalidState)
		}
		return call(p, ctx, mach.ID)
	})
}
