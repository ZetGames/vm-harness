package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ZetGames/vm-harness/vm"
)

func invalid(format string, args ...any) error {
	return fmt.Errorf(format+": %w", append(args, vm.ErrInvalid)...)
}

func parseForward(s string) (vm.PortForward, error) {
	var pf vm.PortForward
	spec := s
	if name, rest, ok := strings.Cut(spec, "="); ok {
		pf.Name, spec = name, rest
	}
	if rest, protocol, ok := strings.Cut(spec, "/"); ok {
		spec, pf.Protocol = rest, strings.ToLower(protocol)
	}
	host, guest, ok := cutLast(spec, ":")
	if !ok {
		return pf, invalid("port forward %q: want [name=][host-ip:]host-port:guest-port[/udp]", s)
	}
	if ip, port, ok := cutLast(host, ":"); ok {
		pf.HostIP = strings.TrimSuffix(strings.TrimPrefix(ip, "["), "]")
		host = port
	}
	var err error
	if pf.HostPort, err = strconv.Atoi(host); err != nil {
		return pf, invalid("port forward %q: host port %q is not a number", s, host)
	}
	if pf.GuestPort, err = strconv.Atoi(guest); err != nil {
		return pf, invalid("port forward %q: guest port %q is not a number", s, guest)
	}
	return pf, nil
}

func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

func parseNIC(s string) (vm.NIC, error) {
	mode, adapter, _ := strings.Cut(s, ":")
	if mode == "" {
		return vm.NIC{}, invalid("network %q: want mode[:adapter]", s)
	}
	return vm.NIC{Mode: strings.ToLower(mode), Adapter: adapter}, nil
}

func parseShare(s string) (vm.SharedFolder, error) {
	name, path, ok := strings.Cut(s, "=")
	if !ok || name == "" || path == "" {
		return vm.SharedFolder{}, invalid("shared folder %q: want name=host-path[:ro]", s)
	}
	sf := vm.SharedFolder{Name: name}
	if p, ok := strings.CutSuffix(path, ":ro"); ok {
		path, sf.ReadOnly = p, true
	} else if p, ok := strings.CutSuffix(path, ":rw"); ok {
		path = p
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return vm.SharedFolder{}, invalid("shared folder %q: %v", s, err)
	}
	sf.HostPath = abs
	return sf, nil
}

func parseLabels(items []string, allowRemove bool) (map[string]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	labels := make(map[string]string, len(items))
	for _, item := range items {
		key, value, ok := strings.Cut(item, "=")
		if !ok && allowRemove {
			key, ok = strings.CutSuffix(item, "-")
		}
		if !ok || key == "" {
			if allowRemove {
				return nil, invalid("label %q: want key=value, or key- to remove it", item)
			}
			return nil, invalid("label %q: want key=value", item)
		}
		labels[key] = value
	}
	return labels, nil
}

func parseEnv(items []string) (map[string]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(items))
	for _, item := range items {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			return nil, invalid("environment variable %q: want NAME=value", item)
		}
		env[key] = value
	}
	return env, nil
}

func parseDuration(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("duration %q must not be negative", s)
		}
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("duration %q: want a number of seconds or a value like 90s, 5m or 1h30m", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration %q must not be negative", s)
	}
	return d, nil
}

type durationValue struct {
	d *time.Duration
}

func (v durationValue) Set(s string) error {
	d, err := parseDuration(s)
	if err != nil {
		return err
	}
	*v.d = d
	return nil
}

func (v durationValue) String() string {
	if *v.d == 0 {
		return ""
	}
	return v.d.String()
}

func (v durationValue) Type() string { return "duration" }

func seconds(d time.Duration) int {
	return int((d + time.Second - 1) / time.Second)
}

func splitGuestPath(s string) (vmName, guestPath string, ok bool) {
	name, path, found := strings.Cut(s, ":")
	switch {
	case !found, name == "", strings.ContainsAny(name, `/\`):
		return "", s, false
	case len(name) == 1 && isLetter(name[0]) && (path == "" || path[0] == '/' || path[0] == '\\'):
		return "", s, false
	}
	return name, path, true
}

func isLetter(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

func guestBase(path string) string {
	return path[strings.LastIndexAny(path, `/\`)+1:]
}

func authorizedKeys(values []string) ([]string, error) {
	var keys []string
	for _, v := range values {
		if isPublicKey(v) {
			keys = append(keys, strings.TrimSpace(v))
			continue
		}
		data, err := os.ReadFile(v)
		if err != nil {
			return nil, invalid("ssh key: %v", err)
		}
		if strings.Contains(string(data), "PRIVATE KEY") {
			return nil, invalid("ssh key %s is a private key, pass the .pub file", v)
		}
		for line := range strings.Lines(string(data)) {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				keys = append(keys, line)
			}
		}
	}
	return keys, nil
}

func isPublicKey(s string) bool {
	if !strings.Contains(s, " ") {
		return false
	}
	for _, prefix := range []string{"ssh-", "ecdsa-", "sk-"} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
