package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ZetGames/vm-harness/harness"
	"github.com/ZetGames/vm-harness/vm"
)

func (a *app) portCommand() *cobra.Command {
	return group("port", "List, add and remove NAT port forwards (VirtualBox)", groupMachines,
		a.portListCommand(),
		a.portAddCommand(),
		a.portRemoveCommand(),
	)
}

func (a *app) portListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "ls <vm>",
		Aliases: []string{"list"},
		Short:   "List the port forwards of a VM",
		Args:    cobra.ExactArgs(1),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			mach, err := m.Get(ctx, a.ref(args[0]))
			if err != nil {
				return err
			}
			forwards := mach.PortForwards
			if forwards == nil {
				forwards = []vm.PortForward{}
			}
			return a.emit(forwards, func(w io.Writer) { writeForwards(w, forwards) })
		}),
	}
}

func (a *app) portAddCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "add <vm> [name=][host-ip:]host-port:guest-port[/udp]",
		Short: "Forward a host port to a guest port",
		Long: `Forward a host port to a guest port through NIC 1, which must be NAT.
The host IP defaults to 127.0.0.1 and must be a loopback address or an address
of this host, host port 0 picks a free port, and the name defaults to
<protocol>-<guest port>. Prints the forward as created.`,
		Example: "  vmh port add web 8080:80\n  vmh port add web dns=0:53/udp",
		Args:    cobra.ExactArgs(2),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			pf, err := parseForward(args[1])
			if err != nil {
				return err
			}
			if pf, err = m.AddPortForward(ctx, a.ref(args[0]), pf); err != nil {
				return err
			}
			return a.emit(pf, func(w io.Writer) { fmt.Fprintln(w, formatForward(pf)) })
		}),
	}
}

func (a *app) portRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <vm> <name>",
		Aliases: []string{"delete"},
		Short:   "Remove a port forward by name",
		Args:    cobra.ExactArgs(2),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			if err := m.RemovePortForward(ctx, a.ref(args[0]), args[1]); err != nil {
				return err
			}
			return a.emit(removal{VM: args[0], PortForward: args[1], Deleted: true}, nil)
		}),
	}
}
