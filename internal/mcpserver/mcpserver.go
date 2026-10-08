package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/fl4metf/vm-harness/harness"
	"github.com/fl4metf/vm-harness/vm"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const instructions = "vmh creates and controls virtual machines on VirtualBox and VMware. " +
	"Only VMs created by vmh (managed: true) can be changed; vmh never changes, clones or runs commands in other " +
	"VMs on the host, which stay visible but read-only. " +
	"Typical flow: vm_providers, vm_create (for a cloud image pass disk_image and cloud_init with start: true), " +
	"vm_wait for ssh, then vm_exec, vm_write_file and vm_read_file; vm_get shows the ssh user, key and address " +
	"for connecting with your own ssh client. Take a snapshot before risky changes. Creating, cloning, " +
	"snapshotting and waiting can take minutes. " +
	"Errors read \"<code>: <message>\" with code one of not_found, already_exists, invalid_argument, " +
	"invalid_state, forbidden, unsupported, not_ready, unavailable, limit_exceeded, timeout, canceled or internal."

type server struct {
	m *harness.Manager
}

func New(m *harness.Manager, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "vmh", Version: version}, &mcp.ServerOptions{Instructions: instructions})
	h := &server{m: m}

	addTool(s, &mcp.Tool{
		Name: "vm_providers",
		Description: "List the hypervisors vmh supports, whether each is available on this host, its version, its features " +
			"(port_forward, linked_clone, cloud_init, ...) and warnings about the host setup, such as VirtualBox running " +
			"on top of Hyper-V, where guests boot slowly and can hang. max_reliable_cpus, when set, is the most vCPUs a VM " +
			"of that provider boots reliably with on this host (1 for VirtualBox on Hyper-V); vm_create without cpus caps " +
			"its default at it and gives an appliance that many. Call it first to pick a provider.",
		Annotations: readOnly(),
	}, h.providers)

	addTool(s, &mcp.Tool{
		Name: "vm_list",
		Description: "List VMs on every available provider or on one. VMs with managed false were not created by vmh " +
			"and are read-only: they cannot be changed, cloned or used to run commands.",
		Annotations: readOnly(),
	}, h.list)

	addTool(s, &mcp.Tool{
		Name: "vm_get",
		Description: "Show one VM: state, CPUs, memory, NICs, port forwards, labels, current snapshot, whether " +
			"vmh manages it and, for VMs created with cloud_init, ssh with the user, the private key_path and, " +
			"when a port forward to guest port 22 exists, the host and port to connect to (otherwise use vm_ip " +
			"and port 22). console_log is the host file that receives the guest serial console, when the VM has one.",
		Annotations: readOnly(),
	}, h.get)

	addTool(s, &mcp.Tool{
		Name: "vm_create",
		Description: "Create a new VM managed by vmh. Boot media is one of iso (with a new blank disk), disk_image " +
			"(a copy of an existing disk such as a cloud image, usually with cloud_init) or appliance (import of a " +
			"single-VM OVA or OVF); cloud_init needs one of them. Unless cloud_init has user_data, vmh adds an SSH " +
			"key it generates to the guest user, next to any keys or password you pass, so vm_exec works over SSH; " +
			"on VirtualBox it also forwards a free 127.0.0.1 port to guest port 22. Host paths are on the machine " +
			"running vmh and, when vmh is limited to host_dirs, must lie inside them. Set start to boot the VM. " +
			"Copying a large disk_image or appliance can take minutes.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: hint(false), OpenWorldHint: hint(false)},
	}, h.create)

	addTool(s, &mcp.Tool{
		Name: "vm_power",
		Description: "Change the power state of a VM managed by vmh: start boots it (headless unless gui), stop asks " +
			"the guest to shut down and forces power-off after timeout_sec (default 60), kill powers off at once, " +
			"pause and resume freeze and unfreeze it, reset hard-reboots it, suspend saves its state to disk. " +
			"stop on a VM that is not running (paused, saved or stuck) powers it off at once and discards a saved " +
			"state. start on a running VM and stop on a stopped VM do nothing. Returns the VM.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: hint(true), OpenWorldHint: hint(false)},
	}, h.power)

	addContentTool(s, &mcp.Tool{
		Name: "vm_delete",
		Description: "Delete a VM managed by vmh together with its disks; this cannot be undone. A running or " +
			"paused VM is refused unless force is true, which powers it off first. A VM that linked clones depend " +
			"on cannot be deleted before them.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: hint(true), IdempotentHint: true, OpenWorldHint: hint(false)},
	}, h.deleteVM)

	addTool(s, &mcp.Tool{
		Name: "vm_update",
		Description: "Change the CPUs or memory of a VM managed by vmh (the VM must be stopped) and set or remove " +
			"labels in any power state (an empty value removes one). Returns the VM.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: hint(false), IdempotentHint: true, OpenWorldHint: hint(false)},
	}, h.update)

	addTool(s, &mcp.Tool{
		Name: "vm_clone",
		Description: "Clone a VM managed by vmh into a new stopped VM called name, also managed by vmh; VMs that " +
			"vmh did not create cannot be cloned. A full clone copies the disks and can take minutes; on VMware " +
			"the source must be stopped. A linked clone shares the source disks, needs the linked_clone feature " +
			"and is based on snapshot (default the current snapshot; a source without snapshots must be stopped " +
			"and gets a new vmh-clone-base snapshot).",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: hint(false), OpenWorldHint: hint(false)},
	}, h.clone)

	addTool(s, &mcp.Tool{
		Name: "vm_snapshot",
		Description: "List, take, restore or delete snapshots of a VM. list works on any VM; the other actions need " +
			"a VM managed by vmh. restore powers the VM off first and discards its current state. delete returns " +
			"the remaining snapshots. take and restore on a running VM can take minutes.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: hint(true), OpenWorldHint: hint(false)},
	}, h.snapshot)

	addTool(s, &mcp.Tool{
		Name: "vm_exec",
		Description: "Run a command in the guest of a running VM managed by vmh and return exit_code, stdout and " +
			"stderr (each cut at 1 MiB). Give command (argv, no shell) or script (run with /bin/sh -c, cmd.exe /c " +
			"on Windows). Transport auto uses SSH when vmh holds a key for the VM (VMs created with cloud_init) " +
			"or ssh.key_path or ssh.password is given, otherwise the guest tools with user and password. A VM " +
			"that is not running fails with invalid_state. A non-zero exit_code is a result, not an error.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: hint(true), OpenWorldHint: hint(false)},
	}, h.exec)

	addContentTool(s, &mcp.Tool{
		Name: "vm_copy",
		Description: "Copy one file between the host running vmh and the guest of a running VM managed by vmh: " +
			"to_guest uploads host_path to guest_path, from_guest downloads guest_path to host_path. Uses the same " +
			"transports and credentials as vm_exec. When vmh is limited to host_dirs, host_path must lie inside " +
			"them. from_guest never writes hypervisor files (.vmx, .vbox, .vmdk, ...), into the folder of a VM vmh " +
			"does not manage, or into the vmh root other than <root>/files.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: hint(true), IdempotentHint: true, OpenWorldHint: hint(false)},
	}, h.copyFile)

	addContentTool(s, &mcp.Tool{
		Name: "vm_write_file",
		Description: "Write content to a file in the guest of a running VM managed by vmh, replacing the file if it " +
			"exists. Use encoding base64 for binary data. Uses the same transports and credentials as vm_exec.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: hint(true), IdempotentHint: true, OpenWorldHint: hint(false)},
	}, h.writeFile)

	addTool(s, &mcp.Tool{
		Name: "vm_read_file",
		Description: "Read a file of at most 32 MiB from the guest of a running VM managed by vmh. Text comes back " +
			"as is; content that is not valid UTF-8 comes back base64-encoded with encoding set to base64. Uses the " +
			"same transports and credentials as vm_exec.",
		Annotations: readOnly(),
	}, h.readFile)

	addTool(s, &mcp.Tool{
		Name: "vm_ip",
		Description: "Return the guest IPv4 address reported by the guest tools; on VMware, when the tools do not " +
			"answer, the address VMware's DHCP server leased to the VM since its current power-on, if that lease has " +
			"not ended (a reset or a reboot inside the guest is not a new power-on). Fails with not_ready while the " +
			"guest is still booting; use vm_wait with for ip to block until it is known.",
		Annotations: readOnly(),
	}, h.ip)

	addTool(s, &mcp.Tool{
		Name: "vm_wait",
		Description: "Block until a VM is running, stopped, paused or saved, reports an ip, accepts ssh logins, or " +
			"runs guest commands (guest, needs user and password). Gives up after timeout_sec (default 300) with a " +
			"timeout error naming the last failed check, and at once on errors that waiting cannot fix, such as a " +
			"missing VM or no user given; a rejected guest login is retried, because cloud_init may still be " +
			"creating the user. Booting a new VM can take minutes; if your client gives up on long calls, pass a " +
			"shorter timeout_sec and call again. Waiting for ip, ssh or guest may hard-reset a stuck VM that vmh " +
			"manages and that has a console_log: when the current boot shows a kernel panic or a network card " +
			"without a valid MAC address, or prints nothing for 90 seconds before a login prompt, vmh resets the VM, " +
			"at most twice per call, and lists each reset in recoveries (errors name them too). Paused or busy VMs " +
			"and Windows guests are never reset. When the boot is stuck again after two resets, the call fails at " +
			"once with not_ready; another call would reset the VM again, so fix the cause the error names first. " +
			"Returns the VM, the ip for ip, the time waited and the recoveries.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: hint(true), OpenWorldHint: hint(false)},
	}, h.wait)

	addContentTool(s, &mcp.Tool{
		Name: "vm_screenshot",
		Description: "Capture the screen of a running VM as a PNG image. VMware captures through VMware Tools and " +
			"needs the guest user and password.",
		Annotations: readOnly(),
	}, h.screenshot)

	addTool(s, &mcp.Tool{
		Name: "vm_port",
		Description: "List, add or remove NAT port forwards of a VM (VirtualBox only; on VMware connect to the guest " +
			"ip instead). add needs guest_port; host_ip must be a loopback address or an address of this host " +
			"(default 127.0.0.1), host_port 0 picks a free port on it, and the effective forward is returned. " +
			"remove needs name and returns the remaining forwards. add and remove need a VM managed by vmh.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: hint(true), OpenWorldHint: hint(false)},
	}, h.port)

	return s
}

func Run(ctx context.Context, m *harness.Manager, version string, in io.Reader, out io.Writer) error {
	return serve(ctx, New(m, version), in, out)
}

func serve(ctx context.Context, s *mcp.Server, in io.Reader, out io.Writer) error {
	err := s.Run(ctx, &mcp.IOTransport{Reader: io.NopCloser(in), Writer: nopWriteCloser{out}})
	if ctx.Err() != nil || errors.Is(err, io.EOF) || errors.Is(err, errServerClosing) {
		return nil
	}
	return err
}

var errServerClosing = &jsonrpc.Error{Code: -32004}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func addTool[In, Out any](s *mcp.Server, t *mcp.Tool, h func(context.Context, In) (Out, error)) {
	mcp.AddTool(s, t, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		out, err := h(ctx, in)
		return nil, out, toolError(err)
	})
}

func addContentTool[In any](s *mcp.Server, t *mcp.Tool, h func(context.Context, In) (*mcp.CallToolResult, error)) {
	mcp.AddTool(s, t, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		res, err := h(ctx, in)
		return res, nil, toolError(err)
	})
}

func toolError(err error) error {
	if err == nil {
		return nil
	}
	code := vm.Code(err)
	msg := err.Error()
	if s := sentinel(err, code); s != "" {
		msg = strings.TrimPrefix(strings.TrimSuffix(msg, ": "+s), s+": ")
	}
	return errors.New(code + ": " + msg)
}

func sentinel(err error, code string) string {
	switch e := err.(type) {
	case nil:
		return ""
	case interface{ Unwrap() error }:
		return sentinel(e.Unwrap(), code)
	case interface{ Unwrap() []error }:
		for _, inner := range e.Unwrap() {
			if s := sentinel(inner, code); s != "" {
				return s
			}
		}
		return ""
	}
	if code == vm.CodeInternal || vm.Code(err) != code {
		return ""
	}
	return err.Error()
}

func readOnly() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: hint(false)}
}

func hint(b bool) *bool { return &b }

func text(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

func unknownAction(action, want string) error {
	return fmt.Errorf("unknown action %q, want %s: %w", action, want, vm.ErrInvalid)
}
