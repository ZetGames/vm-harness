package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/fl4metf/vm-harness/harness"
)

func (a *app) snapCommand() *cobra.Command {
	return group("snap", "List, take, restore and delete snapshots", groupMachines,
		a.snapListCommand(),
		a.snapTakeCommand(),
		a.snapRestoreCommand(),
		a.snapRemoveCommand(),
	)
}

func (a *app) snapListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "ls <vm>",
		Aliases: []string{"list"},
		Short:   "List the snapshots of a VM",
		Args:    cobra.ExactArgs(1),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			snapshots, err := m.Snapshots(ctx, a.ref(args[0]))
			if err != nil {
				return err
			}
			return a.emit(snapshots, func(w io.Writer) { writeSnapshots(w, snapshots) })
		}),
	}
}

func (a *app) snapTakeCommand() *cobra.Command {
	var description string
	cmd := &cobra.Command{
		Use:   "take <vm> <name>",
		Short: "Take a snapshot; a running VM keeps running",
		Args:  cobra.ExactArgs(2),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			snap, err := m.TakeSnapshot(ctx, a.ref(args[0]), args[1], description)
			if err != nil {
				return err
			}
			return a.emit(snap, func(w io.Writer) { fmt.Fprintln(w, snap.Name) })
		}),
	}
	cmd.Flags().StringVarP(&description, "description", "d", "", "snapshot description")
	return cmd
}

func (a *app) snapRestoreCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <vm> <name>",
		Short: "Restore a snapshot, powering the VM off first if it is running",
		Args:  cobra.ExactArgs(2),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			mach, err := m.RestoreSnapshot(ctx, a.ref(args[0]), args[1])
			if err != nil {
				return err
			}
			return a.emitState(mach)
		}),
	}
}

func (a *app) snapRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <vm> <name>",
		Aliases: []string{"delete"},
		Short:   "Delete a snapshot, keeping the current VM state",
		Args:    cobra.ExactArgs(2),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			if err := m.DeleteSnapshot(ctx, a.ref(args[0]), args[1]); err != nil {
				return err
			}
			return a.emit(removal{VM: args[0], Snapshot: args[1], Deleted: true}, nil)
		}),
	}
}
