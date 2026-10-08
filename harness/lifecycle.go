package harness

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/fl4metf/vm-harness/vm"
)

const cloneBaseSnapshot = "vmh-clone-base"

func (m *Manager) Delete(ctx context.Context, ref Ref, force bool) error {
	return m.mutate(ctx, "delete", ref, func(p vm.Provider, mach vm.Machine) error {
		clones, err := linkedClones(ctx, p, mach)
		if err != nil {
			return err
		}
		if len(clones) > 0 {
			return fmt.Errorf("vm %q is the base of linked clones %s, delete them first: %w", mach.Name, strings.Join(clones, ", "), vm.ErrInvalidState)
		}
		if mach.State != vm.StateStopped {
			if !force && mach.State != vm.StateSaved {
				return fmt.Errorf("vm %q is %s, stop it first or delete with force: %w", mach.Name, mach.State, vm.ErrInvalidState)
			}
			if err := m.powerOff(ctx, p, mach.ID); err != nil {
				return err
			}
		}
		if err := p.Delete(ctx, mach.ID); err != nil {
			return err
		}
		os.Remove(m.bootMarkPath(p, mach.ID))
		if key := mach.Meta[vm.MetaSSHKey]; key == "" || ownsKey(p.Name(), mach.Name, key) {
			m.removeKey(key)
			m.forgetHostKeys(mach)
		}
		return nil
	})
}

func linkedClones(ctx context.Context, p vm.Provider, mach vm.Machine) ([]string, error) {
	machines, err := p.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("look for linked clones of %q: %w", mach.Name, err)
	}
	var names []string
	for _, other := range machines {
		if from := other.Meta[vm.MetaLinkedFrom]; from != "" && strings.EqualFold(from, mach.ID) {
			names = append(names, other.Name)
		}
	}
	return names, nil
}

func (m *Manager) Update(ctx context.Context, ref Ref, ch vm.Changes) (vm.Machine, error) {
	if ch.CPUs < 0 || ch.MemoryMB < 0 {
		return vm.Machine{}, fmt.Errorf("cpus and memory must not be negative: %w", vm.ErrInvalid)
	}
	if err := m.cfg.Limits.check(ch.CPUs, ch.MemoryMB, 0); err != nil {
		return vm.Machine{}, err
	}
	if err := checkLabels(ch.Labels); err != nil {
		return vm.Machine{}, err
	}
	return m.change(ctx, "update", ref, func(p vm.Provider, mach vm.Machine) error {
		if ch.CPUs > 0 || ch.MemoryMB > 0 {
			if mach.State != vm.StateStopped {
				return fmt.Errorf("vm %q is %s, stop it before changing cpus or memory: %w", mach.Name, mach.State, vm.ErrInvalidState)
			}
			if err := p.Update(ctx, mach.ID, vm.Changes{CPUs: ch.CPUs, MemoryMB: ch.MemoryMB}); err != nil {
				return err
			}
		}
		if len(ch.Labels) == 0 {
			return nil
		}
		meta := make(map[string]string, len(ch.Labels))
		for k, v := range ch.Labels {
			meta[vm.LabelKey(k)] = v
		}
		return p.SetMeta(ctx, mach.ID, meta)
	})
}

func (m *Manager) Adopt(ctx context.Context, ref Ref) (vm.Machine, error) {
	return m.setManaged(ctx, "adopt", ref, true)
}

func (m *Manager) Release(ctx context.Context, ref Ref) (vm.Machine, error) {
	return m.setManaged(ctx, "release", ref, false)
}

func (m *Manager) setManaged(ctx context.Context, op string, ref Ref, managed bool) (vm.Machine, error) {
	var out vm.Machine
	err := m.locked(ctx, op, ref, func(p vm.Provider, mach vm.Machine) error {
		marker := ""
		if managed {
			marker = mach.ID
		} else if m.inVMHRoot(mach) {
			return fmt.Errorf("vm %q is stored under %s, where every vm is managed by vmh: %w", mach.Name, m.cfg.Root, vm.ErrInvalid)
		}
		if err := p.SetMeta(ctx, mach.ID, map[string]string{vm.MetaManaged: marker}); err != nil {
			return err
		}
		var err error
		out, err = m.get(ctx, p, mach.ID)
		return err
	})
	return out, err
}

func (m *Manager) Clone(ctx context.Context, ref Ref, opts vm.CloneOptions) (vm.Machine, error) {
	if err := checkName("vm", opts.Name); err != nil {
		return vm.Machine{}, err
	}
	var out vm.Machine
	err := m.mutate(ctx, "clone", ref, func(p vm.Provider, src vm.Machine) error {
		if err := m.cfg.Limits.check(src.CPUs, src.MemoryMB, 0); err != nil {
			return fmt.Errorf("a clone of vm %q would exceed the limits: %w", src.Name, err)
		}
		return m.creatingVM(ctx, func() error {
			if err := m.checkVMLimit(ctx); err != nil {
				return err
			}
			if err := ensureAbsent(ctx, p, opts.Name); err != nil {
				return err
			}
			based, err := cloneBase(ctx, p, src, &opts)
			if err != nil {
				return err
			}
			out, err = m.cloneAndPrepare(ctx, p, src, opts)
			if err != nil && based {
				if derr := p.DeleteSnapshot(context.WithoutCancel(ctx), src.ID, cloneBaseSnapshot); derr != nil {
					m.log.Warn("remove base snapshot after a failed clone", "vm", src.Name, "error", derr)
				}
			}
			return err
		})
	})
	return out, err
}

func cloneBase(ctx context.Context, p vm.Provider, src vm.Machine, opts *vm.CloneOptions) (bool, error) {
	if !opts.Linked || opts.Snapshot != "" {
		return false, nil
	}
	snapshots, err := p.Snapshots(ctx, src.ID)
	if err != nil {
		return false, err
	}
	if len(snapshots) > 0 {
		return false, nil
	}
	if src.State != vm.StateStopped {
		return false, fmt.Errorf("vm %q is %s and has no snapshot to link a clone to; stop it, or take a snapshot while it is stopped: %w", src.Name, src.State, vm.ErrInvalidState)
	}
	if err := p.TakeSnapshot(ctx, src.ID, cloneBaseSnapshot, "base for linked clones"); err != nil {
		return false, err
	}
	opts.Snapshot = cloneBaseSnapshot
	return true, nil
}

func (m *Manager) cloneAndPrepare(ctx context.Context, p vm.Provider, src vm.Machine, opts vm.CloneOptions) (vm.Machine, error) {
	clone, err := p.Clone(ctx, src.ID, opts)
	if err != nil {
		return vm.Machine{}, err
	}
	out, err := m.prepareClone(ctx, p, src, clone, opts.Linked)
	if err != nil {
		if derr := p.Delete(context.WithoutCancel(ctx), clone.ID); derr != nil {
			return vm.Machine{}, fmt.Errorf("prepare clone %q: %w (removing the clone failed too: %v)", clone.Name, err, derr)
		}
		return vm.Machine{}, fmt.Errorf("prepare clone %q: %w", clone.Name, err)
	}
	return out, nil
}

func (m *Manager) prepareClone(ctx context.Context, p vm.Provider, src, clone vm.Machine, linked bool) (out vm.Machine, err error) {
	meta := map[string]string{
		vm.MetaManaged:    clone.ID,
		vm.MetaOSType:     src.Meta[vm.MetaOSType],
		vm.MetaSSHUser:    src.Meta[vm.MetaSSHUser],
		vm.MetaSSHKey:     "",
		vm.MetaLinkedFrom: "",
	}
	if linked {
		meta[vm.MetaLinkedFrom] = src.ID
	}
	for k, v := range src.Labels {
		meta[vm.LabelKey(k)] = v
	}
	if srcKey := src.Meta[vm.MetaSSHKey]; srcKey != "" {
		key, copyErr := m.copyKey(srcKey, p.Name(), clone.Name)
		if copyErr != nil {
			return vm.Machine{}, copyErr
		}
		defer func() {
			if err != nil {
				m.removeKey(key)
			}
		}()
		meta[vm.MetaSSHKey] = key
	}
	if err := p.SetMeta(ctx, clone.ID, meta); err != nil {
		return vm.Machine{}, err
	}
	if err := m.remapForwards(ctx, p, clone); err != nil {
		return vm.Machine{}, err
	}
	if out, err = m.get(ctx, p, clone.ID); err == nil {
		m.forgetHostKeys(out)
	}
	return out, err
}

func (m *Manager) remapForwards(ctx context.Context, p vm.Provider, clone vm.Machine) error {
	if len(clone.PortForwards) == 0 {
		return nil
	}
	picker, err := m.newPortPicker(ctx)
	if err != nil {
		return err
	}
	for _, pf := range clone.PortForwards {
		if pf.HostPort == 0 {
			continue
		}
		if pf.HostPort, err = picker.pick(pf.Protocol, pf.HostIP); err != nil {
			return err
		}
		if err := p.RemovePortForward(ctx, clone.ID, pf.Name); err != nil {
			return err
		}
		if err := p.AddPortForward(ctx, clone.ID, pf); err != nil {
			return err
		}
	}
	return nil
}
