package vmware

import "golang.org/x/sys/windows/registry"

var registryKeys = []string{
	`SOFTWARE\WOW6432Node\VMware, Inc.\VMware Workstation`,
	`SOFTWARE\WOW6432Node\VMware, Inc.\VMware Player`,
	`SOFTWARE\VMware, Inc.\VMware Workstation`,
	`SOFTWARE\VMware, Inc.\VMware Player`,
}

func registryInstallDirs() []string {
	var dirs []string
	for _, name := range registryKeys {
		key, err := registry.OpenKey(registry.LOCAL_MACHINE, name, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		dir, _, err := key.GetStringValue("InstallPath")
		key.Close()
		if err == nil && dir != "" {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}
