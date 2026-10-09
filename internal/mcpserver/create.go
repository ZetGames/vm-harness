package mcpserver

import (
	"context"

	"github.com/ZetGames/vm-harness/vm"
)

type createInput struct {
	Name          string            `json:"name" jsonschema:"name of the new VM: letters, digits, dot, underscore or dash, up to 63 characters"`
	Provider      string            `json:"provider,omitempty" jsonschema:"virtualbox or vmware, default the configured provider or the first available one"`
	OSType        string            `json:"os_type,omitempty" jsonschema:"guest OS: arch, debian, fedora, freebsd, linux, oracle, other, rhel, ubuntu, windows10, windows11, windows2019, windows2022 or windows2025; default linux; a native provider OS id is passed through"`
	CPUs          int               `json:"cpus,omitempty" jsonschema:"virtual CPUs; when omitted 2 unless configured otherwise and an appliance keeps its own count, but when the provider reports max_reliable_cpus (1 for VirtualBox on Hyper-V hosts) the default is capped at it and an appliance gets that many; a value you pass is used as given"`
	MemoryMB      int               `json:"memory_mb,omitempty" jsonschema:"memory in MiB, default 2048 unless configured otherwise"`
	DiskGB        int               `json:"disk_gb,omitempty" jsonschema:"disk size in GiB, default 20 unless configured otherwise; a smaller disk_image is grown to it"`
	Firmware      string            `json:"firmware,omitempty" jsonschema:"bios or efi"`
	ISO           string            `json:"iso,omitempty" jsonschema:"host path of an ISO image to boot from, attached next to a new blank disk; when vmh is limited to host_dirs it must be a plain ISO 9660 file"`
	DiskImage     string            `json:"disk_image,omitempty" jsonschema:"host path of a disk image (vmdk, vdi, vhd) such as a cloud image; vmh copies it and never changes the original; when vmh is limited to host_dirs it must be self-contained, not a VMDK descriptor or a differencing disk"`
	Appliance     string            `json:"appliance,omitempty" jsonschema:"host path of an OVA or OVF appliance holding a single VM; vmh drops imported settings that reach outside the VM, such as serial ports, shared folders and remote display; when vmh is limited to host_dirs only .ova files are accepted"`
	NICs          []nic             `json:"nics,omitempty" jsonschema:"network adapters in order, default one nat adapter"`
	PortForwards  []portForward     `json:"port_forwards,omitempty" jsonschema:"NAT port forwards, VirtualBox only, the first NIC must be nat"`
	SharedFolders []sharedFolder    `json:"shared_folders,omitempty" jsonschema:"host directories shared with the guest"`
	CloudInit     *cloudInit        `json:"cloud_init,omitempty" jsonschema:"first-boot configuration for cloud images, delivered as a NoCloud seed ISO"`
	Unattended    *unattended       `json:"unattended,omitempty" jsonschema:"unattended OS installation from iso, VirtualBox only"`
	Labels        map[string]string `json:"labels,omitempty" jsonschema:"free-form labels, keys of letters, digits, dot, underscore or dash"`
	Start         bool              `json:"start,omitempty" jsonschema:"boot the VM headless once it is created"`
}

type nic struct {
	Mode    string `json:"mode" jsonschema:"nat, bridged, hostonly, natnetwork, internal, custom or none"`
	Adapter string `json:"adapter,omitempty" jsonschema:"VirtualBox: host interface for bridged and hostonly, network name for natnetwork and internal; VMware: a virtual network such as vmnet2"`
	Model   string `json:"model,omitempty" jsonschema:"emulated NIC model, such as 82540EM or virtio on VirtualBox and e1000e or vmxnet3 on VMware; VirtualBox gives the first NIC of a Linux cloud_init VM virtio when omitted"`
	MAC     string `json:"mac,omitempty" jsonschema:"fixed MAC address, generated when omitted"`
}

type portForward struct {
	Name      string `json:"name,omitempty" jsonschema:"forward name, default protocol-guestport such as tcp-22"`
	Protocol  string `json:"protocol,omitempty" jsonschema:"tcp or udp, default tcp"`
	HostIP    string `json:"host_ip,omitempty" jsonschema:"host address to listen on: a loopback address or an address of this host, default 127.0.0.1"`
	HostPort  int    `json:"host_port,omitempty" jsonschema:"host port, 0 or omitted picks a free one"`
	GuestIP   string `json:"guest_ip,omitempty" jsonschema:"guest address, normally omitted"`
	GuestPort int    `json:"guest_port,omitempty" jsonschema:"guest port to forward to, required when adding a forward"`
}

type sharedFolder struct {
	Name     string `json:"name" jsonschema:"share name seen by the guest"`
	HostPath string `json:"host_path" jsonschema:"existing directory on the host running vmh"`
	ReadOnly bool   `json:"read_only,omitempty" jsonschema:"share the directory read-only; a writable share must not overlap the folder of a VM vmh does not manage, and inside the vmh root only <root>/files may be shared writable"`
}

type cloudInit struct {
	User              string   `json:"user,omitempty" jsonschema:"login user to create with passwordless sudo, default vmh"`
	Password          string   `json:"password,omitempty" jsonschema:"password of the user, also enables ssh password login"`
	SSHAuthorizedKeys []string `json:"ssh_authorized_keys,omitempty" jsonschema:"extra public keys allowed to log in; vmh always adds a key it generates for vm_exec"`
	Hostname          string   `json:"hostname,omitempty" jsonschema:"guest hostname, default the VM name"`
	Packages          []string `json:"packages,omitempty" jsonschema:"packages to install on first boot"`
	RunCmd            []string `json:"runcmd,omitempty" jsonschema:"shell commands to run on first boot"`
	UserData          string   `json:"user_data,omitempty" jsonschema:"complete user-data document starting with #cloud-config or #!, replaces the fields above; vmh then adds no ssh key and records no user, so ssh needs ssh.user and ssh.key_path or ssh.password"`
	NetworkConfig     string   `json:"network_config,omitempty" jsonschema:"cloud-init network-config document"`
}

type unattended struct {
	User             string `json:"user,omitempty" jsonschema:"account to create"`
	Password         string `json:"password,omitempty" jsonschema:"password of the account"`
	FullName         string `json:"full_name,omitempty" jsonschema:"full name of the account"`
	Hostname         string `json:"hostname,omitempty" jsonschema:"guest hostname"`
	Locale           string `json:"locale,omitempty" jsonschema:"locale such as en_US"`
	TimeZone         string `json:"time_zone,omitempty" jsonschema:"time zone such as UTC"`
	ProductKey       string `json:"product_key,omitempty" jsonschema:"Windows product key"`
	InstallAdditions bool   `json:"install_additions,omitempty" jsonschema:"install VirtualBox Guest Additions so the guest transport works"`
	PostInstall      string `json:"post_install,omitempty" jsonschema:"command to run at the end of the installation"`
}

func (s *server) create(ctx context.Context, in createInput) (vm.Machine, error) {
	return s.m.Create(ctx, in.spec())
}

func (in createInput) spec() vm.Spec {
	spec := vm.Spec{
		Name:       in.Name,
		Provider:   in.Provider,
		OSType:     in.OSType,
		CPUs:       in.CPUs,
		MemoryMB:   in.MemoryMB,
		DiskGB:     in.DiskGB,
		Firmware:   in.Firmware,
		ISO:        in.ISO,
		DiskImage:  in.DiskImage,
		Appliance:  in.Appliance,
		CloudInit:  (*vm.CloudInit)(in.CloudInit),
		Unattended: (*vm.Unattended)(in.Unattended),
		Labels:     in.Labels,
		Start:      in.Start,
	}
	for _, n := range in.NICs {
		spec.NICs = append(spec.NICs, vm.NIC(n))
	}
	for _, pf := range in.PortForwards {
		spec.PortForwards = append(spec.PortForwards, vm.PortForward(pf))
	}
	for _, sf := range in.SharedFolders {
		spec.SharedFolders = append(spec.SharedFolders, vm.SharedFolder(sf))
	}
	return spec
}
