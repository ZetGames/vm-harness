package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/fl4metf/vm-harness/harness"
	"github.com/fl4metf/vm-harness/vm"
)

const execHelp = `Run a command in a VM. Put the command after --, or pass a shell script with
--script (run by /bin/sh -c, or by cmd.exe /c in Windows guests).

Two transports exist. SSH is used when vmh generated a key for the VM at create
time or --ssh-key or --ssh-password is given; otherwise the hypervisor's guest
tools run the command (VirtualBox Guest Additions or VMware Tools, needs -u and
--password). Force one with --transport.

In text mode the guest's stdout and stderr are written to vmh's stdout and
stderr. In JSON mode vmh prints {"exit_code", "stdout", "stderr", "duration_ms",
"transport", "truncated"}; each stream is cut at 1 MiB. Either way vmh exits
with the guest command's exit code, or 125 if vmh itself failed. Linux and
macOS keep only exit codes 0 to 255, so there a guest code outside that range
makes vmh exit 1; exit_code in the JSON result keeps the real value.`

const execExample = `  vmh exec web -- uname -a
  vmh exec web --env DEBUG=1 --workdir /srv -- ./run-tests.sh --fast
  vmh exec web --script 'apt-get update && apt-get install -y nginx' --timeout 10m
  vmh exec win11 -u admin --password Secret1 -- cmd.exe /c ver`

type guestFlags struct {
	user      string
	password  string
	transport string
	ssh       harness.SSHOptions
}

func (g *guestFlags) registerCredentials(fs *pflag.FlagSet) {
	fs.StringVarP(&g.user, "user", "u", "", "guest `user` for the guest tools")
	fs.StringVar(&g.password, "password", "", "password of the guest user")
}

func (g *guestFlags) registerSSH(fs *pflag.FlagSet) {
	fs.StringVar(&g.ssh.User, "ssh-user", "", "SSH `user` (default: the user vmh created)")
	fs.StringVar(&g.ssh.KeyPath, "ssh-key", "", "SSH private key `file` (default: the key vmh generated)")
	fs.StringVar(&g.ssh.Password, "ssh-password", "", "SSH password")
}

func (g *guestFlags) registerTransport(fs *pflag.FlagSet) {
	fs.StringVar(&g.transport, "transport", harness.TransportAuto, "`transport` to use: auto, guest or ssh")
}

func (g *guestFlags) credentials() vm.Credentials {
	return vm.Credentials{User: g.user, Password: g.password}
}

func (g *guestFlags) access() harness.Access {
	return harness.Access{Credentials: g.credentials(), Transport: g.transport, SSH: g.ssh}
}

func (a *app) execCommand() *cobra.Command {
	var (
		g       guestFlags
		script  string
		workDir string
		env     []string
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:     "exec <vm> [flags] -- <command> [args...]",
		Short:   "Run a command in a VM and exit with its exit code",
		Long:    execHelp,
		Example: execExample,
		GroupID: groupGuest,
		Args:    cobra.MinimumNArgs(1),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			vars, err := parseEnv(env)
			if err != nil {
				return err
			}
			req := harness.ExecRequest{
				ExecRequest: vm.ExecRequest{
					Command:     args[1:],
					Script:      script,
					Env:         vars,
					WorkDir:     workDir,
					TimeoutSec:  seconds(timeout),
					Credentials: g.credentials(),
				},
				Transport: g.transport,
				SSH:       g.ssh,
			}
			res, err := m.Exec(ctx, a.ref(args[0]), req)
			if err != nil {
				return err
			}
			if err := a.writeExecResult(res); err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return exitStatus(hostExitCode(runtime.GOOS, res.ExitCode))
			}
			return nil
		}),
	}
	fs := cmd.Flags()
	fs.SortFlags = false
	fs.StringVar(&script, "script", "", "run this shell `script` instead of a command")
	fs.StringArrayVar(&env, "env", nil, "set an environment variable as `NAME=value`; repeatable")
	fs.StringVar(&workDir, "workdir", "", "working `directory` in the guest")
	fs.Var(durationValue{&timeout}, "timeout", "kill the command after this long (default: no limit)")
	g.registerCredentials(fs)
	g.registerTransport(fs)
	g.registerSSH(fs)
	return cmd
}

func hostExitCode(goos string, code int) int {
	if goos != "windows" && (code < 0 || code > 255) {
		return 1
	}
	return code
}

func (a *app) writeExecResult(res vm.ExecResult) error {
	if a.format == formatJSON {
		return a.writeJSON(res)
	}
	if _, err := io.WriteString(a.stdout, res.Stdout); err != nil {
		return err
	}
	if _, err := io.WriteString(a.stderr, res.Stderr); err != nil {
		return err
	}
	if res.Truncated {
		fmt.Fprintln(a.stderr, "vmh: output was cut at 1 MiB per stream")
	}
	return nil
}

const (
	toGuest   = "to_guest"
	fromGuest = "from_guest"
)

type copyPlan struct {
	VM        string `json:"vm"`
	Direction string `json:"direction"`
	HostPath  string `json:"host_path"`
	GuestPath string `json:"guest_path"`
}

func planCopy(src, dst string) (copyPlan, error) {
	srcVM, srcPath, srcInGuest := splitGuestPath(src)
	dstVM, dstPath, dstInGuest := splitGuestPath(dst)
	switch {
	case srcInGuest && dstInGuest:
		return copyPlan{}, invalid("cannot copy from one vm to another, copy through the host")
	case !srcInGuest && !dstInGuest:
		return copyPlan{}, invalid("one side must be a guest path written as <vm>:<path>")
	case dstInGuest:
		host := absPath(srcPath)
		if strings.HasSuffix(dstPath, "/") || strings.HasSuffix(dstPath, `\`) {
			dstPath += filepath.Base(host)
		}
		return copyPlan{VM: dstVM, Direction: toGuest, HostPath: host, GuestPath: dstPath}, nil
	}
	host := absPath(dstPath)
	if fi, err := os.Stat(host); err == nil && fi.IsDir() {
		host = filepath.Join(host, guestBase(srcPath))
	}
	return copyPlan{VM: srcVM, Direction: fromGuest, HostPath: host, GuestPath: srcPath}, nil
}

func (a *app) copyCommand() *cobra.Command {
	var g guestFlags
	cmd := &cobra.Command{
		Use:   "cp <src> <dst>",
		Short: "Copy a file between the host and a VM",
		Long: `Copy one file between the host and a VM, over the same transport as exec.
Write the guest side as <vm>:<path>. A host directory as destination receives
the file under its guest name; a guest destination ending in / receives it
under its host name. Host paths like C:\dir or D:/dir are never taken for a VM.

Copying to the host never writes hypervisor files (.vmx, .vbox, .vmdk, ...),
into the folder of a VM vmh does not manage, or into the vmh root other than
<root>/files. If config.json sets host_dirs, the host path must lie inside one
of them or <root>/files.`,
		Example: "  vmh cp ./app.tar web:/tmp/\n  vmh cp web:/var/log/syslog .",
		GroupID: groupGuest,
		Args:    cobra.ExactArgs(2),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			plan, err := planCopy(args[0], args[1])
			if err != nil {
				return err
			}
			req := harness.CopyRequest{
				CopyRequest: vm.CopyRequest{HostPath: plan.HostPath, GuestPath: plan.GuestPath, Credentials: g.credentials()},
				Transport:   g.transport,
				SSH:         g.ssh,
			}
			if plan.Direction == toGuest {
				err = m.CopyTo(ctx, a.ref(plan.VM), req)
			} else {
				err = m.CopyFrom(ctx, a.ref(plan.VM), req)
			}
			if err != nil {
				return err
			}
			return a.emit(plan, nil)
		}),
	}
	g.registerCredentials(cmd.Flags())
	g.registerTransport(cmd.Flags())
	g.registerSSH(cmd.Flags())
	return cmd
}

type ipResult struct {
	IP         string   `json:"ip"`
	Recoveries []string `json:"recoveries,omitempty"`
}

func (a *app) ipCommand() *cobra.Command {
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "ip <vm>",
		Short: "Print the IP address the guest reports",
		Long: `Print the IP address the guest reports through the guest tools; on VMware,
when the tools do not answer, the address VMware's DHCP server leased to the
VM's MAC since the VM was powered on, if that lease has not ended; a reset or a
reboot inside the guest is not a new power-on. Without --wait this fails with
not_ready (exit 10) while no address is known. --wait resets a stuck boot like
"vmh wait --for ip".`,
		GroupID: groupGuest,
		Args:    cobra.ExactArgs(1),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			res, err := guestIP(ctx, m, a.ref(args[0]), wait)
			a.noteRecoveries(res.Recoveries)
			if err != nil {
				return err
			}
			return a.emit(res, func(w io.Writer) { fmt.Fprintln(w, res.IP) })
		}),
	}
	cmd.Flags().Var(durationValue{&wait}, "wait", "wait up to this long for an address")
	return cmd
}

func guestIP(ctx context.Context, m *harness.Manager, ref harness.Ref, wait time.Duration) (ipResult, error) {
	if wait <= 0 {
		ip, err := m.GuestIP(ctx, ref)
		return ipResult{IP: ip}, err
	}
	res, err := m.Wait(ctx, ref, harness.WaitRequest{For: harness.WaitIP, Timeout: wait})
	return ipResult{IP: res.IP, Recoveries: res.Recoveries}, err
}

func (a *app) noteRecoveries(recoveries []string) {
	if a.format == formatJSON {
		return
	}
	for _, r := range recoveries {
		fmt.Fprintf(a.stderr, "vmh: %s\n", r)
	}
}

func (a *app) waitCommand() *cobra.Command {
	var (
		g   guestFlags
		req harness.WaitRequest
	)
	cmd := &cobra.Command{
		Use:   "wait <vm> --for <condition>",
		Short: "Wait until a VM is running, stopped, has an IP, or accepts SSH or guest commands",
		Long: `Wait until a condition holds, checking every second:
  running, stopped, paused, saved   the power state
  ip                                the guest reports an IP address
  ssh                               an SSH login works (vmh's key, or --ssh-*)
  guest                             the guest tools run commands (-u, --password)
Fails with timeout (exit 7) when --timeout passes first. A rejected guest login
is retried until then, because cloud-init may still be creating the user.

While waiting for ip, ssh or guest, vmh reads the serial console log of a VM
it manages, if the VM has one (VMs created with --cloud-init write serial.log
in their folder: "console" in "vmh show", console_log in JSON). Only the
current boot counts: the output after the last "Linux version" line and after
the last reset or start by vmh that succeeded. vmh hard-resets the VM and keeps
waiting when that boot shows a kernel panic or "Invalid MAC Address" (a network
card that came up broken), or when the log has not been written for 90s while
the boot has reached neither a login prompt nor the cloud-init "finished" line.
Time the VM spends paused or busy with another vmh operation does not count. In
text mode each reset is printed on stderr; JSON results list them in
"recoveries", and an error message names them. After two resets a boot that is
stuck again fails the wait at once with not_ready (exit 10); another wait would
reset the VM again, so fix the cause first. Windows guests and VMs that vmh does
not manage are never reset; adopted VMs are managed.`,
		Example: "  vmh wait web --for ssh --timeout 10m",
		GroupID: groupGuest,
		Args:    cobra.ExactArgs(1),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			req.Access = g.access()
			res, err := m.Wait(ctx, a.ref(args[0]), req)
			a.noteRecoveries(res.Recoveries)
			if err != nil {
				return err
			}
			return a.emit(res, func(w io.Writer) {
				if req.For == harness.WaitIP {
					fmt.Fprintln(w, res.IP)
					return
				}
				fmt.Fprintf(w, "%s\t%s\n", res.Machine.Name, res.Machine.State)
			})
		}),
	}
	req.Timeout = 5 * time.Minute
	cmd.Flags().StringVar(&req.For, "for", "", "`condition`: running, stopped, paused, saved, ip, ssh or guest")
	cmd.Flags().Var(durationValue{&req.Timeout}, "timeout", "give up after this long")
	g.registerCredentials(cmd.Flags())
	g.registerSSH(cmd.Flags())
	return cmd
}

type screenshotResult struct {
	VM    string `json:"vm"`
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
}

func (a *app) screenshotCommand() *cobra.Command {
	var g guestFlags
	cmd := &cobra.Command{
		Use:     "screenshot <vm> <file.png>",
		Short:   "Save a PNG of the VM screen; VMware needs -u and --password",
		GroupID: groupGuest,
		Args:    cobra.ExactArgs(2),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			png, err := m.Screenshot(ctx, a.ref(args[0]), g.credentials())
			if err != nil {
				return err
			}
			path := absPath(args[1])
			if err := os.WriteFile(path, png, 0o644); err != nil {
				return err
			}
			res := screenshotResult{VM: args[0], Path: path, Bytes: len(png)}
			return a.emit(res, func(w io.Writer) { fmt.Fprintln(w, path) })
		}),
	}
	g.registerCredentials(cmd.Flags())
	return cmd
}
