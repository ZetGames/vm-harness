package vmware

import (
	"context"
	"fmt"

	"github.com/fl4metf/vm-harness/vm"
)

var errPortForward = fmt.Errorf("vmware nat port forwarding is set per host network, not per vm: %w", vm.ErrUnsupported)

func (p *Provider) Start(ctx context.Context, ref string, gui bool) error {
	mode := "nogui"
	if gui {
		mode = "gui"
	}
	return p.power(ctx, ref, false, "start", mode)
}

func (p *Provider) Stop(ctx context.Context, ref string, force bool) error {
	if force {
		return p.powerOff(ctx, ref)
	}
	path, err := p.resolve(ctx, ref)
	if err != nil {
		return err
	}
	if err := p.toolsReady(ctx, path); err != nil {
		return err
	}
	if _, err := p.vmrunCmd(ctx, "stop", path, "soft"); err != nil {
		return err
	}
	return setPaused(path, false)
}

func (p *Provider) powerOff(ctx context.Context, ref string) error {
	path, err := p.resolve(ctx, ref)
	if err != nil {
		return err
	}
	v, err := readVMX(path)
	if err != nil {
		return err
	}
	running, err := p.running(ctx)
	if err != nil {
		return err
	}
	if vmState(path, v, running) == vm.StateSaved {
		return discardSuspend(path)
	}
	if _, err := p.vmrunCmd(ctx, "stop", path, "hard"); err != nil {
		return err
	}
	return setPaused(path, false)
}

func (p *Provider) Pause(ctx context.Context, ref string) error {
	return p.power(ctx, ref, true, "pause")
}

func (p *Provider) Resume(ctx context.Context, ref string) error {
	return p.power(ctx, ref, false, "unpause")
}

func (p *Provider) Reset(ctx context.Context, ref string) error {
	return p.power(ctx, ref, false, "reset", "hard")
}

func (p *Provider) Suspend(ctx context.Context, ref string) error {
	return p.power(ctx, ref, false, "suspend", "hard")
}

func (p *Provider) power(ctx context.Context, ref string, paused bool, command string, args ...string) error {
	path, err := p.resolve(ctx, ref)
	if err != nil {
		return err
	}
	if _, err := p.vmrunCmd(ctx, append([]string{command, path}, args...)...); err != nil {
		return err
	}
	return setPaused(path, paused)
}

func (p *Provider) AddPortForward(context.Context, string, vm.PortForward) error {
	return errPortForward
}

func (p *Provider) RemovePortForward(context.Context, string, string) error {
	return errPortForward
}
