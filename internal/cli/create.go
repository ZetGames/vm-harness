package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/fl4metf/vm-harness/harness"
	"github.com/fl4metf/vm-harness/internal/strictjson"
	"github.com/fl4metf/vm-harness/vm"
)

const createHelp = `Create a managed VM. The boot disk comes from one of:
  --iso        an installer or live ISO, with a blank disk of --disk GB
  --image      a copy of a disk image (vmdk, vdi, vhd), e.g. a cloud image
  --appliance  an imported OVA or OVF that holds a single VM
  (none)       a blank disk

vmh drops the settings of an imported appliance that reach outside the VM,
such as serial and parallel ports, shared folders and remote display.

Cloud images: use --cloud-init (implied by --ssh-key, --package, --runcmd and
--user-data). Unless --user-data is given, vmh also generates an SSH key for
the guest user (default vmh), in addition to any --ssh-key or --password, and,
where the hypervisor supports it, forwards a free localhost port to guest port
22, so "vmh wait <vm> --for ssh" and "vmh exec" work without further setup.
With --user-data you manage users and keys yourself.

If config.json sets host_dirs, host paths must lie inside those directories or
<root>/files, --image must be a self-contained disk (no VMDK descriptor or
differencing disk), --iso a plain ISO 9660 image and --appliance an .ova file.
A writable --share must not overlap the folder of a VM vmh does not manage, and
inside the vmh root only <root>/files may be shared writable.

Installer ISOs (VirtualBox): --unattended with --user and --password installs
the OS without interaction.

Defaults, configurable in config.json: 2 CPUs, 2048 MB memory, 20 GB disk,
os linux, one NAT NIC.

-f reads a JSON spec; flags override its fields and relative paths in it are
relative to the file. Spec fields: name, provider, os_type, cpus, memory_mb,
disk_gb, firmware, iso, disk_image, appliance, nics [{mode, adapter, model}],
port_forwards [{name, protocol, host_ip, host_port, guest_port}],
shared_folders [{name, host_path, read_only}], cloud_init {user, password,
ssh_authorized_keys, hostname, packages, runcmd, user_data, network_config},
unattended {user, password, full_name, hostname, locale, time_zone,
product_key, install_additions, post_install}, labels {key: value}, start.`

const createExample = `  vmh create web --image noble-server-cloudimg-amd64.vmdk --os ubuntu --cloud-init --start
  vmh create win11 --iso Win11.iso --os windows11 --unattended --user admin --password Secret1
  vmh create lab --os debian --iso debian.iso --net bridged:eth0 --forward 8080:80 --label team=qa
  vmh create -f spec.json --cpus 4`

type createOptions struct {
	file      string
	osType    string
	cpus      int
	memoryMB  int
	diskGB    int
	firmware  string
	iso       string
	image     string
	appliance string
	nets      []string
	forwards  []string
	shares    []string
	labels    []string
	start     bool

	cloudInit bool
	sshKeys   []string
	packages  []string
	runcmds   []string
	userData  string

	user     string
	password string
	hostname string

	unattended       bool
	locale           string
	timeZone         string
	productKey       string
	installAdditions bool
	postInstall      string
}

func (a *app) createCommand() *cobra.Command {
	var o createOptions
	cmd := &cobra.Command{
		Use:     "create <name>",
		Short:   "Create a managed VM from an ISO, a disk image, an appliance or a blank disk",
		Long:    createHelp,
		Example: createExample,
		GroupID: groupMachines,
		Args:    cobra.MaximumNArgs(1),
		RunE: a.withManager(func(ctx context.Context, m *harness.Manager, args []string) error {
			spec, err := o.spec(args, a.stdin)
			if err != nil {
				return err
			}
			if a.provider != "" {
				spec.Provider = a.provider
			}
			mach, err := m.Create(ctx, spec)
			if err != nil {
				return err
			}
			return a.emitMachine(mach)
		}),
	}
	o.register(cmd.Flags())
	return cmd
}

func (o *createOptions) register(fs *pflag.FlagSet) {
	fs.SortFlags = false
	fs.StringVarP(&o.file, "file", "f", "", "read the spec from a JSON `file`, - for stdin")
	fs.StringVar(&o.osType, "os", "", "guest OS `type`: "+strings.Join(vm.OSTypes(), ", ")+", or a native type id")
	fs.IntVar(&o.cpus, "cpus", 0, "number of virtual CPUs")
	fs.IntVar(&o.memoryMB, "memory", 0, "memory in `MB`")
	fs.IntVar(&o.diskGB, "disk", 0, "disk size in `GB`; a copied --image grows to it")
	fs.StringVar(&o.firmware, "firmware", "", "firmware `type`: bios or efi")
	fs.StringVar(&o.iso, "iso", "", "attach this installer or live `iso`")
	fs.StringVar(&o.image, "image", "", "boot from a copy of this disk `image`")
	fs.StringVar(&o.appliance, "appliance", "", "import this single-VM OVA or OVF `appliance`")
	fs.StringArrayVar(&o.nets, "net", nil, "add a NIC; `spec` is mode[:adapter], mode one of nat, bridged, hostonly, natnetwork, internal, custom, none; repeatable")
	fs.StringArrayVar(&o.forwards, "forward", nil, "forward a host port; `spec` is [name=][host-ip:]host-port:guest-port[/udp], host-ip a loopback or local address (default 127.0.0.1), host port 0 picks a free one; repeatable")
	fs.StringArrayVar(&o.shares, "share", nil, "share a host directory; `spec` is name=host-path[:ro]; repeatable")
	fs.StringArrayVar(&o.labels, "label", nil, "set a label as `key=value`; repeatable")
	fs.BoolVar(&o.start, "start", false, "start the VM headless once it is created")

	fs.BoolVar(&o.cloudInit, "cloud-init", false, "configure the guest with a cloud-init seed")
	fs.StringArrayVar(&o.sshKeys, "ssh-key", nil, "authorize a public key, given as a `file` or as key text; repeatable")
	fs.StringArrayVar(&o.packages, "package", nil, "install a `package` on first boot; repeatable")
	fs.StringArrayVar(&o.runcmds, "runcmd", nil, "run a shell `command` on first boot; repeatable")
	fs.StringVar(&o.userData, "user-data", "", "use this cloud-init user-data `file` as is")

	fs.StringVar(&o.user, "user", "", "guest `user` to create (cloud-init default vmh)")
	fs.StringVar(&o.password, "password", "", "password of the guest user")
	fs.StringVar(&o.hostname, "hostname", "", "guest `hostname` (default: the VM name)")

	fs.BoolVar(&o.unattended, "unattended", false, "install the OS from --iso without interaction")
	fs.StringVar(&o.locale, "locale", "", "unattended install `locale`, e.g. en_US")
	fs.StringVar(&o.timeZone, "time-zone", "", "unattended install time `zone`, e.g. UTC")
	fs.StringVar(&o.productKey, "product-key", "", "Windows product `key` for the unattended install")
	fs.BoolVar(&o.installAdditions, "install-additions", false, "install guest additions during the unattended install")
	fs.StringVar(&o.postInstall, "post-install", "", "`command` to run at the end of the unattended install")
}

func (o *createOptions) spec(args []string, stdin io.Reader) (vm.Spec, error) {
	spec, err := loadSpec(o.file, stdin)
	if err != nil {
		return spec, err
	}
	if len(args) > 0 {
		spec.Name = args[0]
	}
	spec.OSType = cmp.Or(o.osType, spec.OSType)
	spec.CPUs = cmp.Or(o.cpus, spec.CPUs)
	spec.MemoryMB = cmp.Or(o.memoryMB, spec.MemoryMB)
	spec.DiskGB = cmp.Or(o.diskGB, spec.DiskGB)
	spec.Firmware = cmp.Or(o.firmware, spec.Firmware)
	spec.ISO = cmp.Or(absPath(o.iso), spec.ISO)
	spec.DiskImage = cmp.Or(absPath(o.image), spec.DiskImage)
	spec.Appliance = cmp.Or(absPath(o.appliance), spec.Appliance)
	spec.Start = spec.Start || o.start

	if len(o.nets) > 0 {
		if spec.NICs, err = parseAll(o.nets, parseNIC); err != nil {
			return spec, err
		}
	}
	if len(o.forwards) > 0 {
		if spec.PortForwards, err = parseAll(o.forwards, parseForward); err != nil {
			return spec, err
		}
	}
	if len(o.shares) > 0 {
		if spec.SharedFolders, err = parseAll(o.shares, parseShare); err != nil {
			return spec, err
		}
	}
	labels, err := parseLabels(o.labels, false)
	if err != nil {
		return spec, err
	}
	if spec.Labels == nil {
		spec.Labels = labels
	} else {
		maps.Copy(spec.Labels, labels)
	}
	if err := o.applyGuestSetup(&spec); err != nil {
		return spec, err
	}
	return spec, nil
}

func (o *createOptions) applyGuestSetup(spec *vm.Spec) error {
	wantCloudInit := o.cloudInit || len(o.sshKeys) > 0 || len(o.packages) > 0 || len(o.runcmds) > 0 || o.userData != ""
	if wantCloudInit && spec.CloudInit == nil {
		spec.CloudInit = &vm.CloudInit{}
	}
	wantUnattended := o.unattended || o.installAdditions || cmp.Or(o.locale, o.timeZone, o.productKey, o.postInstall) != ""
	if wantUnattended && spec.Unattended == nil {
		spec.Unattended = &vm.Unattended{}
	}
	if spec.CloudInit == nil && spec.Unattended == nil {
		if cmp.Or(o.user, o.password, o.hostname) != "" {
			return invalid("--user, --password and --hostname need --cloud-init or --unattended")
		}
		return nil
	}
	if ci := spec.CloudInit; ci != nil {
		if err := o.applyCloudInit(ci); err != nil {
			return err
		}
	}
	if u := spec.Unattended; u != nil {
		u.User = cmp.Or(o.user, u.User)
		u.Password = cmp.Or(o.password, u.Password)
		u.Hostname = cmp.Or(o.hostname, u.Hostname)
		u.Locale = cmp.Or(o.locale, u.Locale)
		u.TimeZone = cmp.Or(o.timeZone, u.TimeZone)
		u.ProductKey = cmp.Or(o.productKey, u.ProductKey)
		u.PostInstall = cmp.Or(o.postInstall, u.PostInstall)
		u.InstallAdditions = u.InstallAdditions || o.installAdditions
	}
	return nil
}

func (o *createOptions) applyCloudInit(ci *vm.CloudInit) error {
	ci.User = cmp.Or(o.user, ci.User)
	ci.Password = cmp.Or(o.password, ci.Password)
	ci.Hostname = cmp.Or(o.hostname, ci.Hostname)
	if len(o.packages) > 0 {
		ci.Packages = o.packages
	}
	if len(o.runcmds) > 0 {
		ci.RunCmd = o.runcmds
	}
	if len(o.sshKeys) > 0 {
		keys, err := authorizedKeys(o.sshKeys)
		if err != nil {
			return err
		}
		ci.SSHAuthorizedKeys = keys
	}
	if o.userData != "" {
		data, err := os.ReadFile(o.userData)
		if err != nil {
			return invalid("user data: %v", err)
		}
		ci.UserData = string(data)
	}
	return nil
}

func loadSpec(path string, stdin io.Reader) (vm.Spec, error) {
	var spec vm.Spec
	if path == "" {
		return spec, nil
	}
	r, dir := stdin, "."
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return spec, invalid("read spec: %v", err)
		}
		defer f.Close()
		r, dir = f, filepath.Dir(path)
	}
	if err := strictjson.Decode(r, &spec); err != nil {
		return spec, fmt.Errorf("spec %s: %w", path, err)
	}
	spec.ISO = resolvePath(dir, spec.ISO)
	spec.DiskImage = resolvePath(dir, spec.DiskImage)
	spec.Appliance = resolvePath(dir, spec.Appliance)
	for i := range spec.SharedFolders {
		spec.SharedFolders[i].HostPath = resolvePath(dir, spec.SharedFolders[i].HostPath)
	}
	return spec, nil
}

func resolvePath(dir, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return absPath(filepath.Join(dir, path))
}

func absPath(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func parseAll[T any](items []string, parse func(string) (T, error)) ([]T, error) {
	out := make([]T, 0, len(items))
	for _, item := range items {
		v, err := parse(item)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
