package harness

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/fl4metf/vm-harness/internal/strictjson"
	"github.com/fl4metf/vm-harness/vm"
)

type Config struct {
	Root            string           `json:"root"`
	DefaultProvider string           `json:"default_provider,omitempty"`
	AllowUnmanaged  bool             `json:"allow_unmanaged,omitempty"`
	HostDirs        []string         `json:"host_dirs,omitempty"`
	Limits          Limits           `json:"limits"`
	Defaults        Defaults         `json:"defaults"`
	VirtualBox      VirtualBoxConfig `json:"virtualbox"`
	VMware          VMwareConfig     `json:"vmware"`
	Logger          *slog.Logger     `json:"-"`
}

type Limits struct {
	MaxVMs      int `json:"max_vms,omitempty"`
	MaxCPUs     int `json:"max_cpus,omitempty"`
	MaxMemoryMB int `json:"max_memory_mb,omitempty"`
	MaxDiskGB   int `json:"max_disk_gb,omitempty"`
}

type Defaults struct {
	CPUs     int    `json:"cpus,omitempty"`
	MemoryMB int    `json:"memory_mb,omitempty"`
	DiskGB   int    `json:"disk_gb,omitempty"`
	OSType   string `json:"os_type,omitempty"`
}

type VirtualBoxConfig struct {
	VBoxManage string `json:"vboxmanage,omitempty"`
	Disabled   bool   `json:"disabled,omitempty"`
}

type VMwareConfig struct {
	Vmrun        string `json:"vmrun,omitempty"`
	VDiskManager string `json:"vdiskmanager,omitempty"`
	OVFTool      string `json:"ovftool,omitempty"`
	HostType     string `json:"host_type,omitempty"`
	Disabled     bool   `json:"disabled,omitempty"`
}

var builtinDefaults = Defaults{CPUs: 2, MemoryMB: 2048, DiskGB: 20, OSType: "linux"}

func DefaultConfig() Config {
	return Config{Root: defaultRoot(), Defaults: builtinDefaults}
}

func (c Config) FilesDir() string { return filepath.Join(resolveRoot(c.Root), "files") }

func defaultRoot() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".vmh")
	}
	return filepath.Join(os.TempDir(), "vmh")
}

func resolveRoot(root string) string {
	root = cmp.Or(root, defaultRoot())
	if abs, err := filepath.Abs(root); err == nil {
		return abs
	}
	return root
}

func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	envRoot := os.Getenv("VMH_ROOT")
	explicit := path != ""
	if !explicit {
		path = os.Getenv("VMH_CONFIG")
		explicit = path != ""
	}
	if !explicit {
		path = filepath.Join(resolveRoot(envRoot), "config.json")
	}
	f, err := os.Open(path)
	switch {
	case err == nil:
		err = strictjson.Decode(f, &cfg)
		f.Close()
		if err != nil {
			return Config{}, fmt.Errorf("config %s: %w", path, err)
		}
		cfg.Root = besideConfig(path, cfg.Root)
		for i, dir := range cfg.HostDirs {
			cfg.HostDirs[i] = besideConfig(path, dir)
		}
	case errors.Is(err, fs.ErrNotExist) && !explicit:
	default:
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	cfg.Root = resolveRoot(cmp.Or(envRoot, cfg.Root))
	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func besideConfig(configPath, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(filepath.Dir(configPath), path)
}

func applyEnv(cfg *Config) error {
	if v := os.Getenv("VMH_PROVIDER"); v != "" {
		cfg.DefaultProvider = v
	}
	if v := os.Getenv("VMH_VBOXMANAGE"); v != "" {
		cfg.VirtualBox.VBoxManage = v
	}
	if v := os.Getenv("VMH_VMRUN"); v != "" {
		cfg.VMware.Vmrun = v
	}
	if v := os.Getenv("VMH_ALLOW_UNMANAGED"); v != "" {
		allow, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("VMH_ALLOW_UNMANAGED=%q is not a boolean: %w", v, vm.ErrInvalid)
		}
		cfg.AllowUnmanaged = allow
	}
	if v := os.Getenv("VMH_MAX_VMS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("VMH_MAX_VMS=%q is not a non-negative number: %w", v, vm.ErrInvalid)
		}
		cfg.Limits.MaxVMs = n
	}
	return nil
}
