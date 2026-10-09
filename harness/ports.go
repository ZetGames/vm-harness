package harness

import (
	"context"
	"fmt"
	"net"
	"slices"

	"github.com/ZetGames/vm-harness/vm"
)

func (m *Manager) AddPortForward(ctx context.Context, ref Ref, pf vm.PortForward) (vm.PortForward, error) {
	pf, err := normalizeForward(pf)
	if err != nil {
		return vm.PortForward{}, err
	}
	err = m.mutate(ctx, "port_add", ref, func(p vm.Provider, mach vm.Machine) error {
		info, err := p.Info(ctx)
		if err != nil {
			return err
		}
		if !hasFeature(info, vm.FeaturePortForward) {
			return fmt.Errorf("provider %s does not support port forwarding: %w", p.Name(), vm.ErrUnsupported)
		}
		if !natFirst(mach.NICs) {
			return fmt.Errorf("port forwarding needs nic 1 of vm %q in nat mode: %w", mach.Name, vm.ErrInvalid)
		}
		if slices.ContainsFunc(mach.PortForwards, func(x vm.PortForward) bool { return x.Name == pf.Name }) {
			return fmt.Errorf("vm %q already has a port forward named %q: %w", mach.Name, pf.Name, vm.ErrExists)
		}
		if pf.HostPort == 0 {
			picker, err := m.newPortPicker(ctx)
			if err != nil {
				return err
			}
			if pf.HostPort, err = picker.pick(pf.Protocol, pf.HostIP); err != nil {
				return err
			}
		}
		return p.AddPortForward(ctx, mach.ID, pf)
	})
	if err != nil {
		return vm.PortForward{}, err
	}
	return pf, nil
}

func (m *Manager) RemovePortForward(ctx context.Context, ref Ref, name string) error {
	if name == "" {
		return fmt.Errorf("port forward name is required: %w", vm.ErrInvalid)
	}
	return m.mutate(ctx, "port_remove", ref, func(p vm.Provider, mach vm.Machine) error {
		return p.RemovePortForward(ctx, mach.ID, name)
	})
}

type portPicker struct {
	used map[int]bool
}

func (m *Manager) newPortPicker(ctx context.Context) (*portPicker, error) {
	machines, err := m.List(ctx, ListOptions{})
	if err != nil {
		return nil, err
	}
	used := make(map[int]bool)
	for _, mach := range machines {
		for _, pf := range mach.PortForwards {
			used[pf.HostPort] = true
		}
	}
	return &portPicker{used: used}, nil
}

func (pp *portPicker) pick(protocol, hostIP string) (int, error) {
	for range 100 {
		port, err := freePort(protocol, hostIP)
		if err != nil {
			return 0, err
		}
		if !pp.used[port] {
			pp.used[port] = true
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free %s port found on %s: %w", protocol, hostIP, vm.ErrLimit)
}

func freePort(protocol, hostIP string) (int, error) {
	addr := net.JoinHostPort(hostIP, "0")
	if protocol == "udp" {
		conn, err := net.ListenPacket("udp", addr)
		if err != nil {
			return 0, unusableHostIP(hostIP, err)
		}
		defer conn.Close()
		return conn.LocalAddr().(*net.UDPAddr).Port, nil
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return 0, unusableHostIP(hostIP, err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func unusableHostIP(hostIP string, err error) error {
	return fmt.Errorf("host ip %s cannot be used for port forwarding: %v: %w", hostIP, err, vm.ErrInvalid)
}

func (m *Manager) assignHostPorts(ctx context.Context, forwards []vm.PortForward) error {
	if !slices.ContainsFunc(forwards, func(pf vm.PortForward) bool { return pf.HostPort == 0 }) {
		return nil
	}
	picker, err := m.newPortPicker(ctx)
	if err != nil {
		return err
	}
	for _, pf := range forwards {
		picker.used[pf.HostPort] = true
	}
	for i := range forwards {
		if forwards[i].HostPort != 0 {
			continue
		}
		if forwards[i].HostPort, err = picker.pick(forwards[i].Protocol, forwards[i].HostIP); err != nil {
			return err
		}
	}
	return nil
}
