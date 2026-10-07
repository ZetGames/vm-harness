package harness

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"unicode/utf8"

	"github.com/fl4metf/vm-harness/internal/sshexec"
	"github.com/fl4metf/vm-harness/vm"
)

const (
	maxOutput   = 1 << 20
	maxReadFile = 32 << 20
)

func (m *Manager) Exec(ctx context.Context, ref Ref, req ExecRequest) (vm.ExecResult, error) {
	if err := checkExec(req.ExecRequest); err != nil {
		return vm.ExecResult{}, err
	}
	var res vm.ExecResult
	err := m.inGuest(ctx, "exec", ref, func(p vm.Provider, mach vm.Machine) error {
		transport, err := pickTransport(req.Transport, req.SSH, mach)
		if err != nil {
			return err
		}
		if transport == TransportGuest {
			res, err = p.Exec(ctx, mach.ID, req.ExecRequest)
		} else {
			res, err = m.execSSH(ctx, p, mach, req)
		}
		res.Transport = transport
		return err
	})
	if err != nil {
		return vm.ExecResult{}, err
	}
	var cutOut, cutErr bool
	res.Stdout, cutOut = truncate(res.Stdout, maxOutput)
	res.Stderr, cutErr = truncate(res.Stderr, maxOutput)
	res.Truncated = res.Truncated || cutOut || cutErr
	return res, nil
}

func (m *Manager) execSSH(ctx context.Context, p vm.Provider, mach vm.Machine, req ExecRequest) (vm.ExecResult, error) {
	cfg, err := m.sshConfig(ctx, p, mach, req.SSH)
	if err != nil {
		return vm.ExecResult{}, err
	}
	return m.ssh.run(ctx, cfg, req.ExecRequest)
}

func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

func (m *Manager) CopyTo(ctx context.Context, ref Ref, req CopyRequest) error {
	if req.GuestPath == "" || req.HostPath == "" {
		return fmt.Errorf("host path and guest path are required: %w", vm.ErrInvalid)
	}
	host, err := m.hostPath("host file", req.HostPath, false)
	if err != nil {
		return err
	}
	req.HostPath = host
	return m.inGuest(ctx, "copy_to", ref, func(p vm.Provider, mach vm.Machine) error {
		return m.upload(ctx, p, mach, req)
	})
}

func (m *Manager) CopyFrom(ctx context.Context, ref Ref, req CopyRequest) error {
	if req.GuestPath == "" || req.HostPath == "" {
		return fmt.Errorf("host path and guest path are required: %w", vm.ErrInvalid)
	}
	host, err := m.requestPath("host file", req.HostPath)
	if err != nil {
		return err
	}
	if err := m.checkHostDirs("host file", host); err != nil {
		return err
	}
	if fi, err := os.Stat(host); err == nil && fi.IsDir() {
		return fmt.Errorf("host path %s is a directory, name the file to write: %w", host, vm.ErrInvalid)
	}
	req.HostPath = host
	return m.inGuest(ctx, "copy_from", ref, func(p vm.Provider, mach vm.Machine) error {
		if err := m.checkHostWrite(ctx, host); err != nil {
			return err
		}
		transport, err := pickTransport(req.Transport, req.SSH, mach)
		if err != nil {
			return err
		}
		if transport == TransportGuest {
			return p.CopyFrom(ctx, mach.ID, req.CopyRequest)
		}
		cfg, err := m.sshConfig(ctx, p, mach, req.SSH)
		if err != nil {
			return err
		}
		return m.ssh.download(ctx, cfg, req.GuestPath, req.HostPath)
	})
}

func (m *Manager) upload(ctx context.Context, p vm.Provider, mach vm.Machine, req CopyRequest) error {
	transport, err := pickTransport(req.Transport, req.SSH, mach)
	if err != nil {
		return err
	}
	if transport == TransportGuest {
		return p.CopyTo(ctx, mach.ID, req.CopyRequest)
	}
	cfg, err := m.sshConfig(ctx, p, mach, req.SSH)
	if err != nil {
		return err
	}
	return m.ssh.upload(ctx, cfg, req.HostPath, req.GuestPath)
}

func (m *Manager) WriteFile(ctx context.Context, ref Ref, guestPath string, data []byte, acc Access) error {
	if guestPath == "" {
		return fmt.Errorf("guest path is required: %w", vm.ErrInvalid)
	}
	return m.inGuest(ctx, "write_file", ref, func(p vm.Provider, mach vm.Machine) error {
		dir, err := os.MkdirTemp("", "vmh-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		host := filepath.Join(dir, "data")
		if err := os.WriteFile(host, data, 0o600); err != nil {
			return err
		}
		return m.upload(ctx, p, mach, accessCopy(acc, host, guestPath))
	})
}

func (m *Manager) ReadFile(ctx context.Context, ref Ref, guestPath string, acc Access) ([]byte, error) {
	if guestPath == "" {
		return nil, fmt.Errorf("guest path is required: %w", vm.ErrInvalid)
	}
	var data []byte
	err := m.inGuest(ctx, "read_file", ref, func(p vm.Provider, mach vm.Machine) error {
		transport, err := pickTransport(acc.Transport, acc.SSH, mach)
		if err != nil {
			return err
		}
		if transport == TransportGuest {
			data, err = readWithGuestTools(ctx, p, mach, guestPath, acc.Credentials)
			return err
		}
		cfg, err := m.sshConfig(ctx, p, mach, acc.SSH)
		if err != nil {
			return err
		}
		data, err = m.ssh.readFile(ctx, cfg, guestPath, maxReadFile)
		return err
	})
	return data, err
}

func readWithGuestTools(ctx context.Context, p vm.Provider, mach vm.Machine, guestPath string, cred vm.Credentials) ([]byte, error) {
	dir, err := os.MkdirTemp("", "vmh-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	host := filepath.Join(dir, "data")
	if err := p.CopyFrom(ctx, mach.ID, vm.CopyRequest{HostPath: host, GuestPath: guestPath, Credentials: cred}); err != nil {
		return nil, err
	}
	fi, err := os.Stat(host)
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxReadFile {
		return nil, fmt.Errorf("guest file %s has %d bytes, the limit is %d: %w", guestPath, fi.Size(), maxReadFile, vm.ErrLimit)
	}
	return os.ReadFile(host)
}

func accessCopy(acc Access, hostPath, guestPath string) CopyRequest {
	return CopyRequest{
		CopyRequest: vm.CopyRequest{HostPath: hostPath, GuestPath: guestPath, Credentials: acc.Credentials},
		Transport:   acc.Transport,
		SSH:         acc.SSH,
	}
}

func (m *Manager) Screenshot(ctx context.Context, ref Ref, cred vm.Credentials) ([]byte, error) {
	p, mach, err := m.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := requireRunning(mach); err != nil {
		return nil, err
	}
	return p.Screenshot(ctx, mach.ID, cred)
}

func (m *Manager) GuestIP(ctx context.Context, ref Ref) (string, error) {
	p, mach, err := m.resolve(ctx, ref)
	if err != nil {
		return "", err
	}
	return p.GuestIP(ctx, mach.ID)
}

func pickTransport(requested string, opts SSHOptions, mach vm.Machine) (string, error) {
	switch requested {
	case TransportGuest:
		return TransportGuest, nil
	case TransportSSH:
		if mach.IsWindowsGuest() {
			return "", fmt.Errorf("ssh transport is not supported for windows guests, use the guest transport: %w", vm.ErrUnsupported)
		}
		return TransportSSH, nil
	case "", TransportAuto:
		if !mach.IsWindowsGuest() && (opts.KeyPath != "" || opts.Password != "" || mach.Meta[vm.MetaSSHKey] != "") {
			return TransportSSH, nil
		}
		return TransportGuest, nil
	}
	return "", fmt.Errorf("unknown transport %q, want auto, guest or ssh: %w", requested, vm.ErrInvalid)
}

func (m *Manager) sshConfig(ctx context.Context, p vm.Provider, mach vm.Machine, opts SSHOptions) (sshexec.Config, error) {
	cfg, err := m.sshAuth(mach, opts)
	if err != nil {
		return cfg, err
	}
	cfg.Host, cfg.Port, err = sshTarget(ctx, p, mach)
	return cfg, err
}

func (m *Manager) sshAuth(mach vm.Machine, opts SSHOptions) (sshexec.Config, error) {
	cfg := sshexec.Config{
		User:           cmp.Or(opts.User, mach.Meta[vm.MetaSSHUser]),
		Password:       opts.Password,
		KeyPath:        cmp.Or(opts.KeyPath, mach.Meta[vm.MetaSSHKey]),
		KnownHostsPath: m.knownHostsPath(mach),
	}
	if cfg.User == "" {
		return cfg, fmt.Errorf("vm %q has no ssh user on record, pass one: %w", mach.Name, vm.ErrInvalid)
	}
	if cfg.KeyPath == "" && cfg.Password == "" {
		return cfg, fmt.Errorf("vm %q has no ssh key on record, pass a key or a password: %w", mach.Name, vm.ErrInvalid)
	}
	if opts.KeyPath != "" {
		key, err := m.requestPath("ssh key", opts.KeyPath)
		if err != nil {
			return cfg, err
		}
		if err := m.checkHostDirs("ssh key", key, m.keysDir()); err != nil {
			return cfg, err
		}
		cfg.KeyPath = key
	}
	return cfg, nil
}

func sshForward(mach vm.Machine) (vm.PortForward, bool) {
	i := slices.IndexFunc(mach.PortForwards, func(pf vm.PortForward) bool {
		return pf.GuestPort == 22 && cmp.Or(pf.Protocol, "tcp") == "tcp" && pf.HostPort != 0 &&
			(pf.HostIP == "" || localHostIP(net.ParseIP(pf.HostIP)))
	})
	if i < 0 {
		return vm.PortForward{}, false
	}
	return mach.PortForwards[i], true
}

func sshAccess(mach vm.Machine) *vm.SSHAccess {
	user := mach.Meta[vm.MetaSSHUser]
	if user == "" {
		return nil
	}
	access := &vm.SSHAccess{User: user, KeyPath: mach.Meta[vm.MetaSSHKey]}
	if pf, ok := sshForward(mach); ok {
		access.Host, access.Port = forwardHost(pf.HostIP), pf.HostPort
	}
	return access
}

func sshTarget(ctx context.Context, p vm.Provider, mach vm.Machine) (string, int, error) {
	if pf, ok := sshForward(mach); ok {
		return forwardHost(pf.HostIP), pf.HostPort, nil
	}
	ip, err := p.GuestIP(ctx, mach.ID)
	if err != nil {
		return "", 0, fmt.Errorf("find ssh address of vm %q: %w", mach.Name, err)
	}
	return ip, 22, nil
}

func forwardHost(ip string) string {
	switch ip {
	case "", "0.0.0.0", "::":
		return "127.0.0.1"
	}
	return ip
}
