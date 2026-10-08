package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fl4metf/vm-harness/internal/flock"
	"github.com/fl4metf/vm-harness/vm"
)

type lockTable struct {
	mu   sync.Mutex
	held map[string]chan struct{}
}

func (t *lockTable) acquire(ctx context.Context, key string) (func(), error) {
	t.mu.Lock()
	sem, ok := t.held[key]
	if !ok {
		sem = make(chan struct{}, 1)
		t.held[key] = sem
	}
	t.mu.Unlock()
	return acquire(ctx, sem)
}

func acquire(ctx context.Context, sem chan struct{}) (func(), error) {
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	default:
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Manager) fileLock(ctx context.Context, name string) (func(), error) {
	dir := filepath.Join(m.cfg.Root, "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	return flock.Lock(ctx, filepath.Join(dir, name+".lock"))
}

func (m *Manager) creatingVM(ctx context.Context, fn func() error) error {
	release, err := acquire(ctx, m.creating)
	if err != nil {
		return fmt.Errorf("wait for other vms being created: %w", err)
	}
	defer release()
	unlock, err := m.fileLock(ctx, "create")
	if err != nil {
		return fmt.Errorf("wait for other vms being created: %w", err)
	}
	defer unlock()
	return fn()
}

func (m *Manager) inVMHRoot(mach vm.Machine) bool {
	if mach.ConfigPath == "" {
		return false
	}
	for _, root := range []string{m.cfg.Root, m.realRoot} {
		if within(filepath.Join(root, mach.Provider), mach.ConfigPath) {
			return true
		}
	}
	return false
}

func (m *Manager) annotate(mach vm.Machine) vm.Machine {
	mach.Managed = mach.Managed || m.inVMHRoot(mach)
	mach.SSH = sshAccess(mach)
	return mach
}

func (m *Manager) get(ctx context.Context, p vm.Provider, ref string) (vm.Machine, error) {
	mach, err := p.Get(ctx, ref)
	if err != nil {
		return vm.Machine{}, err
	}
	return m.annotate(mach), nil
}

func (m *Manager) guard(mach vm.Machine) error {
	if mach.Managed || m.cfg.AllowUnmanaged {
		return nil
	}
	return fmt.Errorf("vm %q was not created by vmh and is read-only to agents; a person can adopt it with the vmh CLI: %w", mach.Name, vm.ErrForbidden)
}

func (m *Manager) do(ctx context.Context, op string, ref Ref, fn func(vm.Provider, vm.Machine) error) (err error) {
	start := time.Now()
	provider, name := ref.Provider, ref.VM
	defer func() { m.logOp(ctx, op, provider, name, time.Since(start), err) }()
	p, mach, err := m.resolve(ctx, ref)
	if err != nil {
		return err
	}
	provider, name = p.Name(), mach.Name
	return fn(p, mach)
}

func (m *Manager) locked(ctx context.Context, op string, ref Ref, fn func(vm.Provider, vm.Machine) error) error {
	return m.do(ctx, op, ref, func(p vm.Provider, mach vm.Machine) error {
		unlock, err := m.lockVM(ctx, p, mach)
		if err != nil {
			return err
		}
		defer unlock()
		current, err := m.get(ctx, p, mach.ID)
		if err != nil {
			return err
		}
		return fn(p, current)
	})
}

func (m *Manager) lockVM(ctx context.Context, p vm.Provider, mach vm.Machine) (func(), error) {
	key := p.Name() + "-" + idHash(mach.ID)
	release, err := m.locks.acquire(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("wait for other operations on vm %q: %w", mach.Name, err)
	}
	unlock, err := m.fileLock(ctx, key)
	if err != nil {
		release()
		return nil, fmt.Errorf("wait for other operations on vm %q: %w", mach.Name, err)
	}
	return func() {
		unlock()
		release()
	}, nil
}

func (m *Manager) mutate(ctx context.Context, op string, ref Ref, fn func(vm.Provider, vm.Machine) error) error {
	return m.locked(ctx, op, ref, func(p vm.Provider, mach vm.Machine) error {
		if err := m.guard(mach); err != nil {
			return err
		}
		return fn(p, mach)
	})
}

func (m *Manager) inGuest(ctx context.Context, op string, ref Ref, fn func(vm.Provider, vm.Machine) error) error {
	return m.do(ctx, op, ref, func(p vm.Provider, mach vm.Machine) error {
		if err := m.guard(mach); err != nil {
			return err
		}
		if err := requireRunning(mach); err != nil {
			return err
		}
		return fn(p, mach)
	})
}

func requireRunning(mach vm.Machine) error {
	switch mach.State {
	case vm.StateRunning:
		return nil
	case vm.StatePaused:
		return fmt.Errorf("vm %q is paused, resume it first: %w", mach.Name, vm.ErrInvalidState)
	}
	return fmt.Errorf("vm %q is %s, start it first: %w", mach.Name, mach.State, vm.ErrInvalidState)
}

func (m *Manager) change(ctx context.Context, op string, ref Ref, fn func(vm.Provider, vm.Machine) error) (vm.Machine, error) {
	var out vm.Machine
	err := m.mutate(ctx, op, ref, func(p vm.Provider, mach vm.Machine) error {
		if err := fn(p, mach); err != nil {
			return err
		}
		var err error
		out, err = m.get(ctx, p, mach.ID)
		return err
	})
	return out, err
}
