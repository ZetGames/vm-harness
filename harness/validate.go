package harness

import (
	"cmp"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/fl4metf/vm-harness/vm"
)

var (
	namePattern     = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,61}[A-Za-z0-9_-])?$`)
	labelPattern    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,63}$`)
	snapshotPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)
	envNamePattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

var nicModes = []string{vm.NetNAT, vm.NetBridged, vm.NetHostOnly, vm.NetNATNetwork, vm.NetInternal, vm.NetCustom, vm.NetNone}

const maxLabelValue = 256

func checkName(kind, name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid %s name %q: use up to 63 letters, digits, '.', '_' or '-', starting with a letter or digit and not ending with '.': %w", kind, name, vm.ErrInvalid)
	}
	if reservedName(name) {
		return fmt.Errorf("invalid %s name %q: it is a reserved device name on windows: %w", kind, name, vm.ErrInvalid)
	}
	return nil
}

func reservedName(name string) bool {
	base, _, _ := strings.Cut(strings.ToUpper(name), ".")
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	return len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9'
}

func checkSnapshotName(name string) error {
	if !snapshotPattern.MatchString(name) {
		return fmt.Errorf("invalid snapshot name %q: use up to 64 letters, digits, spaces, '.', '_' or '-', starting with a letter or digit: %w", name, vm.ErrInvalid)
	}
	return nil
}

func hasControl(s string) bool { return strings.ContainsFunc(s, unicode.IsControl) }

func checkLabels(labels map[string]string) error {
	for k, v := range labels {
		if !labelPattern.MatchString(k) {
			return fmt.Errorf("invalid label key %q: use 1-63 letters, digits, '.', '_' or '-': %w", k, vm.ErrInvalid)
		}
		if len(v) > maxLabelValue || hasControl(v) {
			return fmt.Errorf("invalid value for label %q: at most %d characters without control characters: %w", k, maxLabelValue, vm.ErrInvalid)
		}
	}
	return nil
}

func (l Limits) check(cpus, memoryMB, diskGB int) error {
	switch {
	case l.MaxCPUs > 0 && cpus > l.MaxCPUs:
		return fmt.Errorf("%d cpus requested, max_cpus is %d: %w", cpus, l.MaxCPUs, vm.ErrLimit)
	case l.MaxMemoryMB > 0 && memoryMB > l.MaxMemoryMB:
		return fmt.Errorf("%d MB of memory requested, max_memory_mb is %d: %w", memoryMB, l.MaxMemoryMB, vm.ErrLimit)
	case l.MaxDiskGB > 0 && diskGB > l.MaxDiskGB:
		return fmt.Errorf("%d GB disk requested, max_disk_gb is %d: %w", diskGB, l.MaxDiskGB, vm.ErrLimit)
	}
	return nil
}

func normalizeForward(pf vm.PortForward) (vm.PortForward, error) {
	pf.Protocol = cmp.Or(pf.Protocol, "tcp")
	if pf.Protocol != "tcp" && pf.Protocol != "udp" {
		return pf, fmt.Errorf("port forward protocol %q: want tcp or udp: %w", pf.Protocol, vm.ErrInvalid)
	}
	if pf.GuestPort < 1 || pf.GuestPort > 65535 {
		return pf, fmt.Errorf("port forward guest port %d: want 1-65535: %w", pf.GuestPort, vm.ErrInvalid)
	}
	if pf.HostPort < 0 || pf.HostPort > 65535 {
		return pf, fmt.Errorf("port forward host port %d: want 0-65535, 0 picks a free port: %w", pf.HostPort, vm.ErrInvalid)
	}
	pf.HostIP = cmp.Or(pf.HostIP, "127.0.0.1")
	ip := net.ParseIP(pf.HostIP)
	if ip == nil {
		return pf, fmt.Errorf("port forward host ip %q: %w", pf.HostIP, vm.ErrInvalid)
	}
	if !localHostIP(ip) {
		return pf, fmt.Errorf("port forward host ip %s is not an address of this host, use 127.0.0.1, 0.0.0.0 or the address of a local interface: %w", pf.HostIP, vm.ErrInvalid)
	}
	if pf.GuestIP != "" && net.ParseIP(pf.GuestIP) == nil {
		return pf, fmt.Errorf("port forward guest ip %q: %w", pf.GuestIP, vm.ErrInvalid)
	}
	if pf.Name == "" {
		pf.Name = fmt.Sprintf("%s-%d", pf.Protocol, pf.GuestPort)
	}
	if err := checkName("port forward", pf.Name); err != nil {
		return pf, err
	}
	return pf, nil
}

func localHostIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	return slices.ContainsFunc(addrs, func(addr net.Addr) bool {
		switch a := addr.(type) {
		case *net.IPNet:
			return a.IP.Equal(ip)
		case *net.IPAddr:
			return a.IP.Equal(ip)
		}
		return false
	})
}

func natFirst(nics []vm.NIC) bool {
	return len(nics) == 0 || nics[0].Mode == vm.NetNAT
}

func hasFeature(info vm.HostInfo, feature string) bool {
	return slices.Contains(info.Features, feature)
}

func checkExec(req vm.ExecRequest) error {
	switch {
	case len(req.Command) == 0 && req.Script == "":
		return fmt.Errorf("a command or a script is required: %w", vm.ErrInvalid)
	case len(req.Command) > 0 && req.Script != "":
		return fmt.Errorf("command and script are mutually exclusive: %w", vm.ErrInvalid)
	case req.TimeoutSec < 0:
		return fmt.Errorf("timeout must not be negative: %w", vm.ErrInvalid)
	}
	for k := range req.Env {
		if !envNamePattern.MatchString(k) {
			return fmt.Errorf("invalid environment variable name %q: %w", k, vm.ErrInvalid)
		}
	}
	return nil
}
