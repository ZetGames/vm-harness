package harness

import (
	"context"
	"fmt"
	"slices"

	"github.com/fl4metf/vm-harness/vm"
)

const maxSnapshotDescription = 1024

func (m *Manager) Snapshots(ctx context.Context, ref Ref) ([]vm.Snapshot, error) {
	p, mach, err := m.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	snapshots, err := p.Snapshots(ctx, mach.ID)
	if err != nil {
		return nil, err
	}
	if snapshots == nil {
		snapshots = []vm.Snapshot{}
	}
	return snapshots, nil
}

func (m *Manager) TakeSnapshot(ctx context.Context, ref Ref, name, description string) (vm.Snapshot, error) {
	if err := checkSnapshotName(name); err != nil {
		return vm.Snapshot{}, err
	}
	if len(description) > maxSnapshotDescription || hasControl(description) {
		return vm.Snapshot{}, fmt.Errorf("snapshot description: at most %d characters on one line: %w", maxSnapshotDescription, vm.ErrInvalid)
	}
	var out vm.Snapshot
	err := m.mutate(ctx, "snapshot_take", ref, func(p vm.Provider, mach vm.Machine) error {
		if err := p.TakeSnapshot(ctx, mach.ID, name, description); err != nil {
			return err
		}
		snapshots, err := p.Snapshots(ctx, mach.ID)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(snapshots, named(name))
		if i < 0 {
			return fmt.Errorf("snapshot %q of vm %q was taken but is not listed", name, mach.Name)
		}
		out = snapshots[i]
		return nil
	})
	return out, err
}

func (m *Manager) RestoreSnapshot(ctx context.Context, ref Ref, name string) (vm.Machine, error) {
	if name == "" {
		return vm.Machine{}, fmt.Errorf("snapshot name is required: %w", vm.ErrInvalid)
	}
	return m.change(ctx, "snapshot_restore", ref, func(p vm.Provider, mach vm.Machine) error {
		snapshots, err := p.Snapshots(ctx, mach.ID)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(snapshots, named(name)) {
			return fmt.Errorf("vm %q has no snapshot %q: %w", mach.Name, name, vm.ErrNotFound)
		}
		if poweredOn(mach.State) {
			if err := m.powerOff(ctx, p, mach.ID); err != nil {
				return err
			}
		}
		if err := p.RestoreSnapshot(ctx, mach.ID, name); err != nil {
			return err
		}
		m.forgetHostKeys(mach)
		return nil
	})
}

func (m *Manager) DeleteSnapshot(ctx context.Context, ref Ref, name string) error {
	if name == "" {
		return fmt.Errorf("snapshot name is required: %w", vm.ErrInvalid)
	}
	return m.mutate(ctx, "snapshot_delete", ref, func(p vm.Provider, mach vm.Machine) error {
		return p.DeleteSnapshot(ctx, mach.ID, name)
	})
}

func named(name string) func(vm.Snapshot) bool {
	return func(s vm.Snapshot) bool { return s.Name == name }
}
