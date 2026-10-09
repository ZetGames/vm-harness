package harness

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ZetGames/vm-harness/vm"
)

const defaultWaitTimeout = 5 * time.Minute

var waitGoals = map[string]string{
	WaitRunning: "be running",
	WaitStopped: "be stopped",
	WaitPaused:  "be paused",
	WaitSaved:   "be saved",
	WaitIP:      "report an ip address",
	WaitSSH:     "accept ssh connections",
	WaitGuest:   "accept guest commands",
}

var waitStates = map[string]vm.State{
	WaitRunning: vm.StateRunning,
	WaitStopped: vm.StateStopped,
	WaitPaused:  vm.StatePaused,
	WaitSaved:   vm.StateSaved,
}

type waitCheck func(ctx context.Context) (WaitResult, error)

func (m *Manager) Wait(ctx context.Context, ref Ref, req WaitRequest) (WaitResult, error) {
	start := time.Now()
	goal, ok := waitGoals[req.For]
	if !ok {
		return WaitResult{}, fmt.Errorf("cannot wait for %q, want running, stopped, paused, saved, ip, ssh or guest: %w", req.For, vm.ErrInvalid)
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultWaitTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var recoveries []string
	expired := func(last error) error {
		what := goal + resetNote(recoveries)
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait for vm %q to %s: %w", ref.VM, what, err)
		}
		if last == nil {
			return fmt.Errorf("timed out after %s waiting for vm %q to %s: %w", timeout, ref.VM, what, context.DeadlineExceeded)
		}
		return fmt.Errorf("timed out after %s waiting for vm %q to %s, last check: %v: %w", timeout, ref.VM, what, last, context.DeadlineExceeded)
	}

	p, mach, err := m.resolve(waitCtx, ref)
	if err != nil {
		if waitCtx.Err() != nil {
			return WaitResult{}, expired(nil)
		}
		return WaitResult{}, err
	}
	check, err := m.waitCheck(p, mach, req)
	if err != nil {
		return WaitResult{}, err
	}
	boot := m.watchBoot(p, mach, req.For)
	var last error
	for {
		res, err := check(waitCtx)
		if err == nil {
			res.ElapsedMS = time.Since(start).Milliseconds()
			res.Recoveries = recoveries
			return res, nil
		}
		if stopWaiting(err) {
			if len(recoveries) > 0 {
				err = fmt.Errorf("wait for vm %q to %s%s: %w", ref.VM, goal, resetNote(recoveries), err)
			}
			return WaitResult{Recoveries: recoveries}, err
		}
		if waitCtx.Err() == nil {
			last = err
		}
		if boot != nil && waitCtx.Err() == nil {
			note, err := m.recoverBoot(waitCtx, p, mach, boot, recoveries)
			if err != nil {
				return WaitResult{Recoveries: recoveries}, err
			}
			if note != "" {
				recoveries = append(recoveries, note)
			}
		}
		if sleep(waitCtx, m.poll) != nil {
			return WaitResult{Recoveries: recoveries}, expired(last)
		}
	}
}

func stopWaiting(err error) bool {
	for _, target := range []error{vm.ErrNotFound, vm.ErrForbidden, vm.ErrUnsupported, vm.ErrUnavailable, vm.ErrInvalid} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func (m *Manager) waitCheck(p vm.Provider, mach vm.Machine, req WaitRequest) (waitCheck, error) {
	switch req.For {
	case WaitIP:
		return m.ipCheck(p, mach)
	case WaitSSH:
		return m.sshCheck(p, mach, req.Access.SSH)
	case WaitGuest:
		return m.guestCheck(p, mach, req.Access.Credentials)
	}
	return m.stateCheck(p, mach, waitStates[req.For]), nil
}

func requireBooting(mach vm.Machine) error {
	if mach.State == vm.StateStopped || mach.State == vm.StateSaved {
		return fmt.Errorf("vm %q is %s, start it first: %w", mach.Name, mach.State, vm.ErrInvalidState)
	}
	return nil
}

func (m *Manager) stateCheck(p vm.Provider, mach vm.Machine, want vm.State) waitCheck {
	return func(ctx context.Context) (WaitResult, error) {
		current, err := m.get(ctx, p, mach.ID)
		if err != nil {
			return WaitResult{}, err
		}
		if current.State != want {
			return WaitResult{}, fmt.Errorf("vm is %s", current.State)
		}
		return WaitResult{Machine: current}, nil
	}
}

func (m *Manager) ipCheck(p vm.Provider, mach vm.Machine) (waitCheck, error) {
	if err := requireBooting(mach); err != nil {
		return nil, err
	}
	return func(ctx context.Context) (WaitResult, error) {
		ip, err := p.GuestIP(ctx, mach.ID)
		if err != nil {
			return WaitResult{}, err
		}
		if ip == "" {
			return WaitResult{}, errors.New("no ip address reported yet")
		}
		current, err := m.get(ctx, p, mach.ID)
		return WaitResult{Machine: current, IP: ip}, err
	}, nil
}

func (m *Manager) sshCheck(p vm.Provider, mach vm.Machine, opts SSHOptions) (waitCheck, error) {
	if err := m.guard(mach); err != nil {
		return nil, err
	}
	if mach.IsWindowsGuest() {
		return nil, fmt.Errorf("ssh is not supported for windows guests, wait for guest instead: %w", vm.ErrUnsupported)
	}
	auth, err := m.sshAuth(mach, opts)
	if err != nil {
		return nil, err
	}
	if err := requireBooting(mach); err != nil {
		return nil, err
	}
	return func(ctx context.Context) (WaitResult, error) {
		cfg := auth
		var err error
		if cfg.Host, cfg.Port, err = sshTarget(ctx, p, mach); err != nil {
			return WaitResult{}, err
		}
		if err := m.ssh.probe(ctx, cfg); err != nil {
			return WaitResult{}, err
		}
		current, err := m.get(ctx, p, mach.ID)
		return WaitResult{Machine: current}, err
	}, nil
}

func (m *Manager) guestCheck(p vm.Provider, mach vm.Machine, cred vm.Credentials) (waitCheck, error) {
	if err := m.guard(mach); err != nil {
		return nil, err
	}
	if cred.User == "" {
		return nil, fmt.Errorf("waiting for guest commands needs a guest user: %w", vm.ErrInvalid)
	}
	if err := requireBooting(mach); err != nil {
		return nil, err
	}
	probe := vm.ExecRequest{Command: []string{"/bin/true"}, Credentials: cred}
	if mach.IsWindowsGuest() {
		probe.Command = []string{vm.WindowsShell, "/c", "exit", "0"}
	}
	return func(ctx context.Context) (WaitResult, error) {
		res, err := p.Exec(ctx, mach.ID, probe)
		if errors.Is(err, vm.ErrInvalid) {
			return WaitResult{}, fmt.Errorf("guest login not accepted yet: %v", err)
		}
		if err != nil {
			return WaitResult{}, err
		}
		if res.ExitCode != 0 {
			return WaitResult{}, fmt.Errorf("guest probe exited with code %d", res.ExitCode)
		}
		current, err := m.get(ctx, p, mach.ID)
		return WaitResult{Machine: current}, err
	}, nil
}
