package vm

import (
	"cmp"
	"maps"
	"runtime"
	"slices"
	"strings"
)

const (
	VirtualBox = "virtualbox"
	VMware     = "vmware"

	WindowsShell = `C:\Windows\System32\cmd.exe`
)

type osType struct {
	virtualbox string
	vmware     string
	windows    bool
}

var armOSTypes = map[string]osType{
	"linux":     {"Other_arm64", "arm-other6xlinux-64", false},
	"ubuntu":    {"Ubuntu_arm64", "arm-ubuntu-64", false},
	"debian":    {"Debian12_arm64", "arm-debian12-64", false},
	"fedora":    {"Fedora_arm64", "arm-fedora-64", false},
	"rhel":      {"RedHat9_arm64", "arm-rhel9-64", false},
	"oracle":    {"Oracle9_arm64", "arm-other6xlinux-64", false},
	"arch":      {"ArchLinux_arm64", "arm-other6xlinux-64", false},
	"freebsd":   {"FreeBSD_arm64", "arm-freebsd14-64", false},
	"windows11": {"Windows11_arm64", "arm-windows11-64", true},
	"other":     {"Other_arm64", "arm-other-64", false},
}

var osTypes = map[string]osType{
	"linux":       {"Linux26_64", "other5xlinux-64", false},
	"ubuntu":      {"Ubuntu_64", "ubuntu-64", false},
	"debian":      {"Debian12_64", "debian12-64", false},
	"fedora":      {"Fedora_64", "fedora-64", false},
	"rhel":        {"RedHat9_64", "rhel9-64", false},
	"oracle":      {"Oracle9_64", "oraclelinux9-64", false},
	"arch":        {"ArchLinux_64", "other5xlinux-64", false},
	"freebsd":     {"FreeBSD_64", "freebsd-64", false},
	"windows10":   {"Windows10_64", "windows9-64", true},
	"windows11":   {"Windows11_64", "windows11-64", true},
	"windows2019": {"Windows2019_64", "windows2019srv-64", true},
	"windows2022": {"Windows2022_64", "windows2019srvNext-64", true},
	"windows2025": {"Windows2025_64", "windows2022srvNext-64", true},
	"other":       {"Other_64", "other-64", false},
}

func NativeOSType(provider, os string) string {
	return nativeOSType(provider, os, runtime.GOARCH)
}

func nativeOSType(provider, os, arch string) string {
	key := strings.ToLower(os)
	t, ok := osTypes[key]
	if arm, found := armOSTypes[key]; found && arch == "arm64" {
		t = arm
	}
	if !ok {
		return os
	}
	switch provider {
	case VirtualBox:
		return t.virtualbox
	case VMware:
		return t.vmware
	}
	return os
}

func (m Machine) IsWindowsGuest() bool {
	return IsWindows(cmp.Or(m.Meta[MetaOSType], m.OSType))
}

func IsWindows(os string) bool {
	if t, ok := osTypes[strings.ToLower(os)]; ok {
		return t.windows
	}
	return strings.HasPrefix(strings.ToLower(os), "win")
}

func OSTypes() []string {
	return slices.Sorted(maps.Keys(osTypes))
}
