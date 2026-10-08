package vmware

import (
	"cmp"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func installDirs() []string {
	dirs := registryInstallDirs()
	switch runtime.GOOS {
	case "windows":
		dirs = append(dirs,
			`C:\Program Files (x86)\VMware\VMware Workstation`,
			`C:\Program Files\VMware\VMware Workstation`,
		)
	case "darwin":
		dirs = append(dirs,
			"/Applications/VMware Fusion.app/Contents/Library",
			"/Applications/VMware Fusion.app/Contents/Library/VMware OVF Tool",
		)
	default:
		dirs = append(dirs, "/usr/bin")
	}
	return dirs
}

func findTool(name string, dirs []string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	for _, dir := range dirs {
		for _, path := range []string{filepath.Join(dir, name), filepath.Join(dir, "OVFTool", name)} {
			if isFile(path) {
				return path
			}
		}
	}
	return ""
}

func inventoryPath() string {
	switch runtime.GOOS {
	case "windows":
		if dir := os.Getenv("APPDATA"); dir != "" {
			return filepath.Join(dir, "VMware", "inventory.vmls")
		}
		return ""
	case "darwin":
		if dir, err := os.UserConfigDir(); err == nil {
			return filepath.Join(dir, "VMware Fusion", "vmInventory")
		}
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".vmware", "inventory.vmls")
	}
	return ""
}

func leaseFiles(goos string) []string {
	switch goos {
	case "windows":
		return []string{filepath.Join(cmp.Or(os.Getenv("ProgramData"), `C:\ProgramData`), "VMware", "vmnetdhcp.leases")}
	case "darwin":
		return []string{"/var/db/vmware/*.leases"}
	}
	return []string{"/etc/vmware/vmnet*/dhcpd/dhcpd.leases"}
}
