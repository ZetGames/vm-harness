package cli

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZetGames/vm-harness/harness"
	"github.com/ZetGames/vm-harness/vm"
)

func (a *app) startCommand() *cobra.Command {
	var gui bool
	cmd := &cobra.Command{
		Use:     "start <vm>",
		Short:   "Start a VM, headless unless --gui; a running VM is left alone",
		GroupID: groupPower,
		Args:    cobra.ExactArgs(1),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			mach, err := m.Start(ctx, a.ref(args[0]), gui)
			if err != nil {
				return err
			}
			return a.emitState(mach)
		}),
	}
	cmd.Flags().BoolVar(&gui, "gui", false, "open a console window")
	return cmd
}

func (a *app) stopCommand() *cobra.Command {
	var opts harness.StopOptions
	cmd := &cobra.Command{
		Use:   "stop <vm>",
		Short: "Shut a VM down, powering it off if it does not stop in time",
		Long: `Shut a VM down. vmh asks the guest OS to shut down and powers the VM off
if it is still running after --timeout. --force powers it off at once.
A stopped VM is left alone.`,
		GroupID: groupPower,
		Args:    cobra.ExactArgs(1),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			mach, err := m.Stop(ctx, a.ref(args[0]), opts)
			if err != nil {
				return err
			}
			return a.emitState(mach)
		}),
	}
	opts.Timeout = time.Minute
	cmd.Flags().BoolVar(&opts.Force, "force", false, "power off immediately, like pulling the plug")
	cmd.Flags().Var(durationValue{&opts.Timeout}, "timeout", "how long to wait for a clean shutdown")
	return cmd
}

func (a *app) transitionCommand(name, short string, op func(*harness.Manager, context.Context, harness.Ref) (vm.Machine, error)) *cobra.Command {
	return &cobra.Command{
		Use:     name + " <vm>",
		Short:   short,
		GroupID: groupPower,
		Args:    cobra.ExactArgs(1),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			mach, err := op(m, ctx, a.ref(args[0]))
			if err != nil {
				return err
			}
			return a.emitState(mach)
		}),
	}
}
