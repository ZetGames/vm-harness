package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/fl4metf/vm-harness/harness"
	"github.com/fl4metf/vm-harness/vm"
)

var version = "dev"

var newManager = harness.Open

const (
	groupMachines = "machines"
	groupPower    = "power"
	groupGuest    = "guest"
	groupServers  = "servers"
)

const rootHelp = `vmh creates and manages VirtualBox and VMware virtual machines.

A <vm> argument is a VM name or provider ID. If the same name exists in both
hypervisors, pick one with --provider. Only VMs created by vmh (managed VMs)
can be changed; other VMs are read-only until a person adopts them at an
interactive terminal.

Output is JSON when stdout is not a terminal and text otherwise; choose with
--output or VMH_OUTPUT. In JSON mode every command prints exactly one JSON
document on stdout, errors included: {"error":{"code":"...","message":"..."}}.

Exit codes: 0 ok, 1 internal, 2 usage, 3 not_found, 4 forbidden,
5 invalid_state, 6 unsupported, 7 timeout, 8 already_exists,
9 invalid_argument, 10 not_ready, 11 unavailable, 12 limit_exceeded,
130 canceled. "vmh exec" exits with the guest command's exit code, or 125 if
vmh failed.`

type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer

	configPath string
	provider   string
	output     string

	format string
	parsed bool
}

type exitStatus int

func (s exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(s)) }

func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, stop)
	return run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{stdin: stdin, stdout: stdout, stderr: stderr}
	root := a.rootCommand()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return 0
	}
	var status exitStatus
	if errors.As(err, &status) {
		return int(status)
	}
	return a.fail(cmd, err)
}

func (a *app) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:               "vmh",
		Short:             "Create and manage VirtualBox and VMware virtual machines",
		Long:              rootHelp,
		SilenceErrors:     true,
		SilenceUsage:      true,
		CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: true},
		PersistentPreRunE: func(*cobra.Command, []string) error {
			format, err := outputFormat(a.output, a.stdout)
			a.format = format
			if err != nil {
				return err
			}
			a.parsed = true
			return nil
		},
	}
	flags := root.PersistentFlags()
	flags.StringVar(&a.configPath, "config", "", "config `file` (default $VMH_CONFIG, else <root>/config.json)")
	flags.StringVarP(&a.provider, "provider", "p", "", "hypervisor `name`: virtualbox or vmware (default: whichever has the VM; for create $VMH_PROVIDER or the first available)")
	flags.StringVarP(&a.output, "output", "o", "", "output `format`: json or text (default $VMH_OUTPUT, else text on a terminal and json otherwise)")

	root.AddGroup(
		&cobra.Group{ID: groupMachines, Title: "Machines:"},
		&cobra.Group{ID: groupPower, Title: "Power:"},
		&cobra.Group{ID: groupGuest, Title: "Guest access:"},
		&cobra.Group{ID: groupServers, Title: "Servers:"},
	)
	root.AddCommand(
		a.providersCommand(),
		a.listCommand(),
		a.showCommand(),
		a.createCommand(),
		a.cloneCommand(),
		a.setCommand(),
		a.removeCommand(),
		a.adoptCommand(),
		a.releaseCommand(),
		a.snapCommand(),
		a.portCommand(),
		a.startCommand(),
		a.stopCommand(),
		a.transitionCommand("pause", "Pause a running VM", (*harness.Manager).Pause),
		a.transitionCommand("resume", "Resume a paused VM", (*harness.Manager).Resume),
		a.transitionCommand("reset", "Hard-reset a running VM", (*harness.Manager).Reset),
		a.transitionCommand("suspend", "Save the state of a running VM to disk and power it off", (*harness.Manager).Suspend),
		a.execCommand(),
		a.copyCommand(),
		a.ipCommand(),
		a.waitCommand(),
		a.screenshotCommand(),
		a.serveCommand(),
		a.mcpCommand(),
		a.versionCommand(),
	)
	return root
}

type usageError string

func (e usageError) Error() string { return string(e) }

func group(use, short, groupID string, children ...*cobra.Command) *cobra.Command {
	names := make([]string, len(children))
	for i, child := range children {
		names[i] = child.Name()
	}
	missing := usageError("missing subcommand, want one of: " + strings.Join(names, ", "))
	cmd := &cobra.Command{
		Use:                   use,
		Short:                 short,
		GroupID:               groupID,
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(*cobra.Command, []string) error {
			return missing
		},
	}
	cmd.AddCommand(children...)
	return cmd
}

func (a *app) withManager(fn func(ctx context.Context, m *harness.Manager, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		m, err := a.open(nil)
		if err != nil {
			return err
		}
		return fn(cmd.Context(), m, args)
	}
}

func (a *app) loadConfig() (harness.Config, error) {
	cfg, err := harness.LoadConfig(a.configPath)
	if err != nil {
		return cfg, err
	}
	if a.provider != "" {
		cfg.DefaultProvider = a.provider
	}
	return cfg, nil
}

func (a *app) open(logger *slog.Logger) (*harness.Manager, error) {
	cfg, err := a.loadConfig()
	if err != nil {
		return nil, err
	}
	cfg.Logger = logger
	return newManager(cfg), nil
}

func (a *app) ref(name string) harness.Ref {
	return harness.Ref{Provider: a.provider, VM: name}
}

func (a *app) fail(cmd *cobra.Command, err error) int {
	if a.format == "" {
		a.format, _ = outputFormat(a.output, a.stdout)
	}
	code := vm.Code(err)
	message := err.Error()
	var usage usageError
	if !a.parsed || errors.As(err, &usage) {
		code = codeUsage
		message = fmt.Sprintf("%s (run '%s --help' for usage)", message, cmd.CommandPath())
	}
	if a.format == formatJSON {
		a.writeJSON(errorDocument{Error: errorBody{Code: code, Message: message}})
	} else {
		fmt.Fprintf(a.stderr, "vmh: %s\n", message)
	}
	if cmd.CommandPath() == "vmh exec" {
		return execFailure
	}
	return exitCode(code)
}

const (
	codeUsage   = "usage"
	execFailure = 125
)

var exitCodes = map[string]int{
	codeUsage:           2,
	vm.CodeNotFound:     3,
	vm.CodeForbidden:    4,
	vm.CodeInvalidState: 5,
	vm.CodeUnsupported:  6,
	vm.CodeTimeout:      7,
	vm.CodeExists:       8,
	vm.CodeInvalid:      9,
	vm.CodeNotReady:     10,
	vm.CodeUnavailable:  11,
	vm.CodeLimit:        12,
	vm.CodeCanceled:     130,
}

func exitCode(code string) int {
	if n, ok := exitCodes[code]; ok {
		return n
	}
	return 1
}
