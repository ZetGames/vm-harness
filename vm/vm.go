package vm

type State string

const (
	StateRunning State = "running"
	StatePaused  State = "paused"
	StateSaved   State = "saved"
	StateStopped State = "stopped"
	StateBusy    State = "busy"
	StateUnknown State = "unknown"
)

type Machine struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Provider        string            `json:"provider"`
	State           State             `json:"state"`
	OSType          string            `json:"os_type,omitempty"`
	CPUs            int               `json:"cpus,omitempty"`
	MemoryMB        int               `json:"memory_mb,omitempty"`
	Firmware        string            `json:"firmware,omitempty"`
	ConfigPath      string            `json:"config_path,omitempty"`
	Managed         bool              `json:"managed"`
	Labels          map[string]string `json:"labels,omitempty"`
	Meta            map[string]string `json:"-"`
	NICs            []NIC             `json:"nics,omitempty"`
	PortForwards    []PortForward     `json:"port_forwards,omitempty"`
	CurrentSnapshot string            `json:"current_snapshot,omitempty"`
	SSH             *SSHAccess        `json:"ssh,omitempty"`
}

type SSHAccess struct {
	User    string `json:"user"`
	KeyPath string `json:"key_path,omitempty"`
	Host    string `json:"host,omitempty"`
	Port    int    `json:"port,omitempty"`
}

type NIC struct {
	Mode    string `json:"mode"`
	Adapter string `json:"adapter,omitempty"`
	Model   string `json:"model,omitempty"`
	MAC     string `json:"mac,omitempty"`
}

const (
	NetNAT        = "nat"
	NetBridged    = "bridged"
	NetHostOnly   = "hostonly"
	NetNATNetwork = "natnetwork"
	NetInternal   = "internal"
	NetCustom     = "custom"
	NetNone       = "none"
)

type PortForward struct {
	Name      string `json:"name"`
	Protocol  string `json:"protocol"`
	HostIP    string `json:"host_ip,omitempty"`
	HostPort  int    `json:"host_port"`
	GuestIP   string `json:"guest_ip,omitempty"`
	GuestPort int    `json:"guest_port"`
}

type SharedFolder struct {
	Name     string `json:"name"`
	HostPath string `json:"host_path"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

type Spec struct {
	Name          string            `json:"name"`
	Provider      string            `json:"provider,omitempty"`
	OSType        string            `json:"os_type,omitempty"`
	CPUs          int               `json:"cpus,omitempty"`
	MemoryMB      int               `json:"memory_mb,omitempty"`
	DiskGB        int               `json:"disk_gb,omitempty"`
	Firmware      string            `json:"firmware,omitempty"`
	ISO           string            `json:"iso,omitempty"`
	DiskImage     string            `json:"disk_image,omitempty"`
	Appliance     string            `json:"appliance,omitempty"`
	NICs          []NIC             `json:"nics,omitempty"`
	PortForwards  []PortForward     `json:"port_forwards,omitempty"`
	SharedFolders []SharedFolder    `json:"shared_folders,omitempty"`
	CloudInit     *CloudInit        `json:"cloud_init,omitempty"`
	Unattended    *Unattended       `json:"unattended,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	Meta          map[string]string `json:"-"`
	Start         bool              `json:"start,omitempty"`
}

type CloudInit struct {
	User              string   `json:"user,omitempty"`
	Password          string   `json:"password,omitempty"`
	SSHAuthorizedKeys []string `json:"ssh_authorized_keys,omitempty"`
	Hostname          string   `json:"hostname,omitempty"`
	Packages          []string `json:"packages,omitempty"`
	RunCmd            []string `json:"runcmd,omitempty"`
	UserData          string   `json:"user_data,omitempty"`
	NetworkConfig     string   `json:"network_config,omitempty"`
}

type Unattended struct {
	User             string `json:"user,omitempty"`
	Password         string `json:"password,omitempty"`
	FullName         string `json:"full_name,omitempty"`
	Hostname         string `json:"hostname,omitempty"`
	Locale           string `json:"locale,omitempty"`
	TimeZone         string `json:"time_zone,omitempty"`
	ProductKey       string `json:"product_key,omitempty"`
	InstallAdditions bool   `json:"install_additions,omitempty"`
	PostInstall      string `json:"post_install,omitempty"`
}

type Changes struct {
	CPUs     int               `json:"cpus,omitempty"`
	MemoryMB int               `json:"memory_mb,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
}

type CloneOptions struct {
	Name     string `json:"name"`
	Linked   bool   `json:"linked,omitempty"`
	Snapshot string `json:"snapshot,omitempty"`
}

type Snapshot struct {
	Name        string `json:"name"`
	ID          string `json:"id,omitempty"`
	Description string `json:"description,omitempty"`
	Parent      string `json:"parent,omitempty"`
	Current     bool   `json:"current,omitempty"`
}

type Credentials struct {
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
}

type ExecRequest struct {
	Command    []string          `json:"command,omitempty"`
	Script     string            `json:"script,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	WorkDir    string            `json:"workdir,omitempty"`
	TimeoutSec int               `json:"timeout_sec,omitempty"`
	Credentials
}

type ExecResult struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
	Transport  string `json:"transport,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type CopyRequest struct {
	HostPath  string `json:"host_path"`
	GuestPath string `json:"guest_path"`
	Credentials
}

type HostInfo struct {
	Provider string   `json:"provider"`
	Version  string   `json:"version,omitempty"`
	Binary   string   `json:"binary,omitempty"`
	Root     string   `json:"root,omitempty"`
	Features []string `json:"features"`
}

const (
	FeatureGuestExec     = "guest_exec"
	FeatureGuestCopy     = "guest_copy"
	FeatureScreenshot    = "screenshot"
	FeaturePortForward   = "port_forward"
	FeatureLinkedClone   = "linked_clone"
	FeatureSnapshots     = "snapshots"
	FeatureAppliance     = "appliance"
	FeatureDiskImage     = "disk_image"
	FeatureCloudInit     = "cloud_init"
	FeatureUnattended    = "unattended"
	FeatureSharedFolders = "shared_folders"
)
