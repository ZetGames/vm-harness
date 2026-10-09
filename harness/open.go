package harness

import (
	"path/filepath"

	"github.com/ZetGames/vm-harness/provider/virtualbox"
	"github.com/ZetGames/vm-harness/provider/vmware"
	"github.com/ZetGames/vm-harness/vm"
)

func Open(cfg Config) *Manager {
	cfg.Root = resolveRoot(cfg.Root)
	var providers []vm.Provider
	if !cfg.VirtualBox.Disabled {
		providers = append(providers, virtualbox.New(virtualbox.Options{
			VBoxManage: cfg.VirtualBox.VBoxManage,
			Root:       filepath.Join(cfg.Root, vm.VirtualBox),
		}))
	}
	if !cfg.VMware.Disabled {
		providers = append(providers, vmware.New(vmware.Options{
			Vmrun:        cfg.VMware.Vmrun,
			VDiskManager: cfg.VMware.VDiskManager,
			OVFTool:      cfg.VMware.OVFTool,
			HostType:     cfg.VMware.HostType,
			Root:         filepath.Join(cfg.Root, vm.VMware),
		}))
	}
	return New(cfg, providers...)
}
