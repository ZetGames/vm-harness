package harness

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ZetGames/vm-harness/vm"
)

var configEnv = []string{
	"VMH_ROOT", "VMH_CONFIG", "VMH_PROVIDER", "VMH_ALLOW_UNMANAGED", "VMH_VBOXMANAGE", "VMH_VMRUN", "VMH_MAX_VMS",
}

func clearConfigEnv(t *testing.T) {
	for _, name := range configEnv {
		t.Setenv(name, "")
	}
}

func TestDefaultConfig(t *testing.T) {
	clearConfigEnv(t)
	cfg := DefaultConfig()
	if filepath.Base(cfg.Root) != ".vmh" {
		t.Fatalf("root = %q", cfg.Root)
	}
	if cfg.Defaults != (Defaults{CPUs: 2, MemoryMB: 2048, DiskGB: 20, OSType: "linux"}) {
		t.Fatalf("defaults = %+v", cfg.Defaults)
	}
	if cfg.DefaultProvider != "" || cfg.AllowUnmanaged {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadConfigWithoutFile(t *testing.T) {
	clearConfigEnv(t)
	root := t.TempDir()
	t.Setenv("VMH_ROOT", root)
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Root != root || cfg.Defaults.CPUs != 2 {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadConfigFile(t *testing.T) {
	clearConfigEnv(t)
	root := t.TempDir()
	t.Setenv("VMH_ROOT", root)
	writeFile(t, filepath.Join(root, "config.json"), `{
		"default_provider": "vmware",
		"allow_unmanaged": true,
		"limits": {"max_vms": 5, "max_cpus": 8},
		"defaults": {"cpus": 4},
		"virtualbox": {"vboxmanage": "C:\\vbox\\VBoxManage.exe"},
		"vmware": {"vmrun": "/usr/bin/vmrun", "host_type": "player", "disabled": true}
	}`)
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultProvider != vm.VMware || !cfg.AllowUnmanaged || cfg.Limits != (Limits{MaxVMs: 5, MaxCPUs: 8}) {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.Defaults != (Defaults{CPUs: 4, MemoryMB: 2048, DiskGB: 20, OSType: "linux"}) {
		t.Fatalf("defaults = %+v", cfg.Defaults)
	}
	if cfg.VirtualBox.VBoxManage != `C:\vbox\VBoxManage.exe` || cfg.VMware.Vmrun != "/usr/bin/vmrun" || cfg.VMware.HostType != "player" || !cfg.VMware.Disabled {
		t.Fatalf("provider config = %+v %+v", cfg.VirtualBox, cfg.VMware)
	}
	if cfg.Root != root {
		t.Fatalf("root = %q", cfg.Root)
	}
}

func TestLoadConfigPaths(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("VMH_ROOT", t.TempDir())
	dir := t.TempDir()
	viaEnv := writeFile(t, filepath.Join(dir, "env.json"), `{"default_provider": "vmware"}`)
	explicit := writeFile(t, filepath.Join(dir, "explicit.json"), `{"default_provider": "virtualbox"}`)
	t.Setenv("VMH_CONFIG", viaEnv)

	cfg, err := LoadConfig("")
	if err != nil || cfg.DefaultProvider != vm.VMware {
		t.Fatalf("VMH_CONFIG not used: %+v, %v", cfg, err)
	}
	cfg, err = LoadConfig(explicit)
	if err != nil || cfg.DefaultProvider != vm.VirtualBox {
		t.Fatalf("explicit path not used: %+v, %v", cfg, err)
	}
	if _, err := LoadConfig(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("a missing explicit config must be an error")
	}
	t.Setenv("VMH_CONFIG", filepath.Join(dir, "missing.json"))
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("a missing VMH_CONFIG file must be an error")
	}
}

func TestLoadConfigRejectsBadFiles(t *testing.T) {
	cases := map[string]string{
		"unknown field":    `{"root": "x", "max_vms": 3}`,
		"unknown nested":   `{"limits": {"max_vm": 3}}`,
		"wrong type":       `{"limits": {"max_vms": "3"}}`,
		"trailing data":    `{} {}`,
		"trailing brace":   `{"root": "x"}}`,
		"trailing bracket": `{"root": "x"}]`,
		"not json":         `root = "x"`,
		"unknown provider": `{"hyperv": {}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			clearConfigEnv(t)
			path := writeFile(t, filepath.Join(t.TempDir(), "config.json"), content)
			_, err := LoadConfig(path)
			wantErr(t, err, vm.ErrInvalid)
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("error does not name the file: %v", err)
			}
		})
	}
}

func TestLoadConfigEnvOverrides(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "config.json"), `{
		"root": "from-file",
		"default_provider": "virtualbox",
		"allow_unmanaged": true,
		"limits": {"max_vms": 9},
		"virtualbox": {"vboxmanage": "file-vbox"},
		"vmware": {"vmrun": "file-vmrun"}
	}`)
	root := t.TempDir()
	t.Setenv("VMH_ROOT", root)
	t.Setenv("VMH_PROVIDER", "vmware")
	t.Setenv("VMH_ALLOW_UNMANAGED", "0")
	t.Setenv("VMH_VBOXMANAGE", "env-vbox")
	t.Setenv("VMH_VMRUN", "env-vmrun")
	t.Setenv("VMH_MAX_VMS", "3")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Root:            root,
		DefaultProvider: vm.VMware,
		Limits:          Limits{MaxVMs: 3},
		Defaults:        builtinDefaults,
		VirtualBox:      VirtualBoxConfig{VBoxManage: "env-vbox"},
		VMware:          VMwareConfig{Vmrun: "env-vmrun"},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("config = %+v\nwant     %+v", cfg, want)
	}

	t.Setenv("VMH_ROOT", "")
	t.Setenv("VMH_ALLOW_UNMANAGED", "1")
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowUnmanaged || cfg.Root != filepath.Join(dir, "from-file") {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadConfigRelativePathsFollowTheFile(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	shared := t.TempDir()
	content, err := json.Marshal(map[string]any{"root": "vms", "host_dirs": []string{"files", shared}})
	if err != nil {
		t.Fatal(err)
	}
	path := writeFile(t, filepath.Join(dir, "config.json"), string(content))
	t.Chdir(t.TempDir())
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Root != filepath.Join(dir, "vms") {
		t.Fatalf("root = %q, want it next to the config file", cfg.Root)
	}
	if want := []string{filepath.Join(dir, "files"), shared}; !slices.Equal(cfg.HostDirs, want) {
		t.Fatalf("host dirs = %q, want %q", cfg.HostDirs, want)
	}
	if got := New(cfg).Config().Root; got != filepath.Join(dir, "vms") {
		t.Fatalf("manager root = %q", got)
	}
}

func TestLoadConfigBadEnv(t *testing.T) {
	cases := map[string]string{
		"VMH_MAX_VMS":         "many",
		"VMH_ALLOW_UNMANAGED": "sometimes",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("VMH_ROOT", t.TempDir())
			t.Setenv(name, value)
			_, err := LoadConfig("")
			wantErr(t, err, vm.ErrInvalid)
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("error does not name the variable: %v", err)
			}
		})
	}
	clearConfigEnv(t)
	t.Setenv("VMH_ROOT", t.TempDir())
	t.Setenv("VMH_MAX_VMS", "-1")
	_, err := LoadConfig("")
	wantErr(t, err, vm.ErrInvalid)
}

func TestNewFillsMissingSettings(t *testing.T) {
	t.Chdir(t.TempDir())
	m := New(Config{Root: "state", Defaults: Defaults{MemoryMB: 1024}})
	cfg := m.Config()
	if !filepath.IsAbs(cfg.Root) || filepath.Base(cfg.Root) != "state" {
		t.Fatalf("root = %q", cfg.Root)
	}
	if cfg.Defaults != (Defaults{CPUs: 2, MemoryMB: 1024, DiskGB: 20, OSType: "linux"}) {
		t.Fatalf("defaults = %+v", cfg.Defaults)
	}
}
