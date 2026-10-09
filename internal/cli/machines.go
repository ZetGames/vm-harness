package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ZetGames/vm-harness/harness"
	"github.com/ZetGames/vm-harness/vm"
)

func (a *app) providersCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "providers",
		Short: "List hypervisors, their versions, features and availability",
		Long: `List hypervisors, their versions, features and availability, then warnings
about the host setup, such as VirtualBox running on top of Hyper-V. In JSON,
info.max_reliable_cpus is the most vCPUs a VM of that provider boots reliably
with on this host (1 for VirtualBox on Hyper-V); "vmh create" without --cpus
uses no more than that.`,
		GroupID: groupMachines,
		Args:    cobra.NoArgs,
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, _ []string) error {
			providers := m.Providers(ctx)
			return a.emit(providers, func(w io.Writer) { writeProviders(w, providers) })
		}),
	}
}

func (a *app) listCommand() *cobra.Command {
	var managed bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List VMs of all available hypervisors",
		GroupID: groupMachines,
		Args:    cobra.NoArgs,
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, _ []string) error {
			machines, err := m.List(ctx, harness.ListOptions{Provider: a.provider, ManagedOnly: managed})
			if err != nil {
				return err
			}
			return a.emit(machines, func(w io.Writer) { writeMachines(w, machines) })
		}),
	}
	cmd.Flags().BoolVar(&managed, "managed", false, "only VMs managed by vmh")
	return cmd
}

func (a *app) showCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "show <vm>",
		Short:   "Show a VM: state, hardware, labels, NICs and port forwards",
		GroupID: groupMachines,
		Args:    cobra.ExactArgs(1),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			mach, err := m.Get(ctx, a.ref(args[0]))
			if err != nil {
				return err
			}
			return a.emitMachine(mach)
		}),
	}
}

func (a *app) cloneCommand() *cobra.Command {
	var opts vm.CloneOptions
	cmd := &cobra.Command{
		Use:   "clone <source> <name>",
		Short: "Clone a VM into a new managed VM",
		Long: `Clone a managed VM into a new managed VM.

A full clone copies the disks. A linked clone shares the source disks through
a snapshot and is much faster. Without --snapshot a linked clone uses the
current snapshot; a stopped source without snapshots gets one named
vmh-clone-base. The clone keeps the source's labels and SSH key, and each of
its NAT port forwards moves to a free host port.`,
		GroupID: groupMachines,
		Args:    cobra.ExactArgs(2),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			opts.Name = args[1]
			mach, err := m.Clone(ctx, a.ref(args[0]), opts)
			if err != nil {
				return err
			}
			return a.emitMachine(mach)
		}),
	}
	cmd.Flags().BoolVar(&opts.Linked, "linked", false, "make a linked clone")
	cmd.Flags().StringVar(&opts.Snapshot, "snapshot", "", "clone the state of this `snapshot`")
	return cmd
}

func (a *app) setCommand() *cobra.Command {
	var (
		changes vm.Changes
		labels  []string
	)
	cmd := &cobra.Command{
		Use:   "set <vm>",
		Short: "Change CPUs, memory or labels of a VM",
		Long: `Change CPUs, memory or labels of a VM.

CPUs and memory can only change while the VM is stopped; labels can change at
any time. Remove a label with --label key-.`,
		Example: "  vmh set web --cpus 4 --memory 8192\n  vmh set web --label role=db --label temp-",
		GroupID: groupMachines,
		Args:    cobra.ExactArgs(1),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			var err error
			if changes.Labels, err = parseLabels(labels, true); err != nil {
				return err
			}
			if changes.CPUs == 0 && changes.MemoryMB == 0 && len(changes.Labels) == 0 {
				return invalid("nothing to change, pass --cpus, --memory or --label")
			}
			mach, err := m.Update(ctx, a.ref(args[0]), changes)
			if err != nil {
				return err
			}
			return a.emitMachine(mach)
		}),
	}
	cmd.Flags().IntVar(&changes.CPUs, "cpus", 0, "number of virtual CPUs")
	cmd.Flags().IntVar(&changes.MemoryMB, "memory", 0, "memory in MB")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "set a label (`key=value`) or remove one (key-); repeatable")
	return cmd
}

func (a *app) removeCommand() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "rm <vm>",
		Aliases: []string{"delete"},
		Short:   "Delete a VM and its disks",
		Long: `Delete a VM and its disks. This cannot be undone.

A running VM is refused unless --force is given, which powers it off first.
A VM that linked clones are based on cannot be deleted before its clones.`,
		GroupID: groupMachines,
		Args:    cobra.ExactArgs(1),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			if err := m.Delete(ctx, a.ref(args[0]), force); err != nil {
				return err
			}
			return a.emit(removal{VM: args[0], Deleted: true}, nil)
		}),
	}
	cmd.Flags().BoolVar(&force, "force", false, "power the VM off first if it is running")
	return cmd
}

func (a *app) adoptCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "adopt <vm>",
		Short: "Mark an existing VM as managed so vmh may change it",
		Long: `Mark an existing VM as managed so vmh may change it.

vmh refuses to modify, run commands in or delete VMs it did not create.
Adopting a VM lifts that protection for this VM only. A person confirms it by
typing the VM name, so adopt runs only on an interactive terminal.`,
		GroupID: groupMachines,
		Args:    cobra.ExactArgs(1),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			if !interactive(a.stdin, a.stdout) {
				return fmt.Errorf("adopt needs a person to confirm it at an interactive terminal: %w", vm.ErrForbidden)
			}
			mach, err := m.Get(ctx, a.ref(args[0]))
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stderr, "Adopting vm %q (%s) lets vmh, and every agent using it, change, run commands in and delete it.\n", mach.Name, mach.Provider)
			fmt.Fprint(a.stderr, "type the VM name to adopt it: ")
			typed, err := readLine(ctx, a.stdin)
			if err != nil {
				return err
			}
			if typed != mach.Name {
				return fmt.Errorf("the typed name does not match, vm %q was not adopted: %w", mach.Name, vm.ErrForbidden)
			}
			if mach, err = m.Adopt(ctx, harness.Ref{Provider: mach.Provider, VM: mach.ID}); err != nil {
				return err
			}
			return a.emitMachine(mach)
		}),
	}
}

var interactive = func(stdin io.Reader, stdout io.Writer) bool {
	return isTerminal(stdin) && isTerminal(stdout)
}

func readLine(ctx context.Context, r io.Reader) (string, error) {
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(r).ReadString('\n')
		line <- s
	}()
	select {
	case s := <-line:
		return strings.TrimSpace(s), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (a *app) releaseCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "release <vm>",
		Short: "Mark a VM as unmanaged, protecting it from changes by vmh",
		Long: `Mark a VM as unmanaged, protecting it from changes by vmh.

VMs stored under the vmh root directory are always managed and cannot be
released.`,
		GroupID: groupMachines,
		Args:    cobra.ExactArgs(1),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			mach, err := m.Release(ctx, a.ref(args[0]))
			if err != nil {
				return err
			}
			return a.emitMachine(mach)
		}),
	}
}
