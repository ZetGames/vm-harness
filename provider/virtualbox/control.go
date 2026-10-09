package virtualbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ZetGames/vm-harness/vm"
)

func (p *Provider) control(ctx context.Context, ref string, args ...string) error {
	if err := checkRef(ref); err != nil {
		return err
	}
	_, err := p.run(ctx, append([]string{"controlvm", ref}, args...)...)
	return err
}

func (p *Provider) Start(ctx context.Context, ref string, gui bool) error {
	if err := checkRef(ref); err != nil {
		return err
	}
	frontend := "headless"
	if gui {
		frontend = "gui"
	}
	_, err := p.run(ctx, "startvm", ref, "--type", frontend)
	return err
}

func (p *Provider) Stop(ctx context.Context, ref string, force bool) error {
	if !force {
		return p.control(ctx, ref, "acpipowerbutton")
	}
	info, err := p.inspect(ctx, ref)
	if err != nil {
		return err
	}
	if info.state() == vm.StateSaved {
		_, err = p.run(ctx, "discardstate", info.id())
		return err
	}
	return p.control(ctx, info.id(), "poweroff")
}

func (p *Provider) Pause(ctx context.Context, ref string) error {
	return p.control(ctx, ref, "pause")
}

func (p *Provider) Resume(ctx context.Context, ref string) error {
	return p.control(ctx, ref, "resume")
}

func (p *Provider) Reset(ctx context.Context, ref string) error {
	return p.control(ctx, ref, "reset")
}

func (p *Provider) Suspend(ctx context.Context, ref string) error {
	return p.control(ctx, ref, "savestate")
}

func (p *Provider) Snapshots(ctx context.Context, ref string) ([]vm.Snapshot, error) {
	if err := checkRef(ref); err != nil {
		return nil, err
	}
	out, err := p.run(ctx, "snapshot", ref, "list", "--machinereadable")
	var cmdErr *vm.CommandError
	if errors.As(err, &cmdErr) && strings.Contains(cmdErr.Stdout, "does not have any snapshots") {
		return []vm.Snapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	return parseSnapshots(out), nil
}

func (p *Provider) TakeSnapshot(ctx context.Context, ref, name, description string) error {
	if name == "" {
		return fmt.Errorf("snapshot name is empty: %w", vm.ErrInvalid)
	}
	info, err := p.inspect(ctx, ref)
	if err != nil {
		return err
	}
	snapshots, err := p.Snapshots(ctx, info.id())
	if err != nil {
		return err
	}
	if slices.ContainsFunc(snapshots, func(s vm.Snapshot) bool { return s.Name == name }) {
		return fmt.Errorf("snapshot %s of vm %s: %w", name, info.name(), vm.ErrExists)
	}
	args := []string{"snapshot", info.id(), "take", name}
	if description != "" {
		args = append(args, "--description", description)
	}
	_, err = p.run(ctx, args...)
	return err
}

func (p *Provider) RestoreSnapshot(ctx context.Context, ref, name string) error {
	info, err := p.inspect(ctx, ref)
	if err != nil {
		return err
	}
	if s := info.state(); s != vm.StateStopped && s != vm.StateSaved {
		return fmt.Errorf("vm %s is %s, stop it before restoring: %w", info.name(), s, vm.ErrInvalidState)
	}
	_, err = p.run(ctx, "snapshot", info.id(), "restore", name)
	return err
}

func (p *Provider) DeleteSnapshot(ctx context.Context, ref, name string) error {
	if err := checkRef(ref); err != nil {
		return err
	}
	_, err := p.run(ctx, "snapshot", ref, "delete", name)
	return err
}

func (p *Provider) AddPortForward(ctx context.Context, ref string, pf vm.PortForward) error {
	rule, err := natRule(pf)
	if err != nil {
		return err
	}
	info, err := p.natTarget(ctx, ref)
	if err != nil {
		return err
	}
	return p.natpf(ctx, info, rule)
}

func (p *Provider) RemovePortForward(ctx context.Context, ref, name string) error {
	info, err := p.natTarget(ctx, ref)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(info.forwards, func(pf vm.PortForward) bool { return pf.Name == name }) {
		return fmt.Errorf("port forward %s on vm %s: %w", name, info.name(), vm.ErrNotFound)
	}
	return p.natpf(ctx, info, "delete", name)
}

func (p *Provider) natTarget(ctx context.Context, ref string) (vmInfo, error) {
	info, err := p.inspect(ctx, ref)
	if err != nil {
		return vmInfo{}, err
	}
	if info.fields["nic1"] != "nat" {
		return vmInfo{}, errForwardNeedsNAT
	}
	return info, nil
}

func (p *Provider) natpf(ctx context.Context, info vmInfo, change ...string) error {
	var args []string
	switch s := info.state(); s {
	case vm.StateRunning, vm.StatePaused:
		args = append([]string{"controlvm", info.id(), "natpf1"}, change...)
	case vm.StateStopped:
		args = append([]string{"modifyvm", info.id(), "--natpf1"}, change...)
	default:
		return fmt.Errorf("vm %s is %s: %w", info.name(), s, vm.ErrInvalidState)
	}
	_, err := p.run(ctx, args...)
	return err
}
