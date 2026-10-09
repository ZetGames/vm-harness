package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ZetGames/vm-harness/vm"
)

var errNoProvider = fmt.Errorf("no hypervisor is available, install VirtualBox or VMware: %w", vm.ErrUnavailable)

func (m *Manager) Providers(ctx context.Context) []ProviderStatus {
	out := make([]ProviderStatus, 0, len(m.providers))
	for _, p := range m.providers {
		info, err := p.Info(ctx)
		st := ProviderStatus{Name: p.Name(), Available: err == nil, Info: info}
		if err != nil {
			st.Error = err.Error()
		}
		out = append(out, st)
	}
	def := m.cfg.DefaultProvider
	for i := range out {
		if def == "" && out[i].Available {
			def = out[i].Name
		}
		out[i].Default = strings.EqualFold(out[i].Name, def)
	}
	return out
}

func (m *Manager) lookup(name string) (vm.Provider, error) {
	names := make([]string, 0, len(m.providers))
	for _, p := range m.providers {
		if strings.EqualFold(p.Name(), name) {
			return p, nil
		}
		names = append(names, p.Name())
	}
	return nil, fmt.Errorf("unknown provider %q, known providers: %s: %w", name, strings.Join(names, ", "), vm.ErrInvalid)
}

func (m *Manager) provider(ctx context.Context, name string) (vm.Provider, vm.HostInfo, error) {
	p, err := m.lookup(name)
	if err != nil {
		return nil, vm.HostInfo{}, err
	}
	info, err := p.Info(ctx)
	if err != nil {
		if !errors.Is(err, vm.ErrUnavailable) {
			err = fmt.Errorf("provider %s: %w: %v", p.Name(), vm.ErrUnavailable, err)
		}
		return nil, vm.HostInfo{}, err
	}
	return p, info, nil
}

func (m *Manager) createProvider(ctx context.Context, name string) (vm.Provider, vm.HostInfo, error) {
	if name == "" {
		name = m.cfg.DefaultProvider
	}
	if name != "" {
		return m.provider(ctx, name)
	}
	for _, p := range m.providers {
		if info, err := p.Info(ctx); err == nil {
			return p, info, nil
		}
	}
	return nil, vm.HostInfo{}, errNoProvider
}

func (m *Manager) resolve(ctx context.Context, ref Ref) (vm.Provider, vm.Machine, error) {
	if ref.VM == "" {
		return nil, vm.Machine{}, fmt.Errorf("vm name or id is required: %w", vm.ErrInvalid)
	}
	if ref.Provider != "" {
		p, _, err := m.provider(ctx, ref.Provider)
		if err != nil {
			return nil, vm.Machine{}, err
		}
		mach, err := m.get(ctx, p, ref.VM)
		if err != nil {
			return nil, vm.Machine{}, err
		}
		return p, mach, nil
	}
	candidates, err := m.listProviders(ctx, "")
	if err != nil {
		return nil, vm.Machine{}, err
	}
	var (
		found     []vm.Machine
		providers []vm.Provider
		failures  []error
	)
	for _, p := range candidates {
		mach, err := m.get(ctx, p, ref.VM)
		switch {
		case err == nil:
			found = append(found, mach)
			providers = append(providers, p)
		case !errors.Is(err, vm.ErrNotFound):
			failures = append(failures, fmt.Errorf("look up vm %q in %s: %w", ref.VM, p.Name(), err))
		}
	}
	switch len(found) {
	case 0:
		if len(failures) > 0 {
			return nil, vm.Machine{}, errors.Join(failures...)
		}
		return nil, vm.Machine{}, fmt.Errorf("vm %q: %w", ref.VM, vm.ErrNotFound)
	case 1:
		return providers[0], found[0], nil
	}
	names := make([]string, len(providers))
	for i, p := range providers {
		names[i] = p.Name()
	}
	return nil, vm.Machine{}, fmt.Errorf("vm %q is ambiguous, it exists in %s; specify a provider: %w", ref.VM, strings.Join(names, " and "), vm.ErrInvalid)
}

func (m *Manager) listProviders(ctx context.Context, name string) ([]vm.Provider, error) {
	if name != "" {
		p, _, err := m.provider(ctx, name)
		if err != nil {
			return nil, err
		}
		return []vm.Provider{p}, nil
	}
	var providers []vm.Provider
	for _, p := range m.providers {
		if _, err := p.Info(ctx); err == nil {
			providers = append(providers, p)
		}
	}
	if len(providers) == 0 {
		return nil, errNoProvider
	}
	return providers, nil
}

func (m *Manager) list(ctx context.Context, p vm.Provider) ([]vm.Machine, error) {
	machines, err := p.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list %s vms: %w", p.Name(), err)
	}
	for i := range machines {
		machines[i] = m.annotate(machines[i])
	}
	return machines, nil
}

func (m *Manager) List(ctx context.Context, opts ListOptions) ([]vm.Machine, error) {
	providers, err := m.listProviders(ctx, opts.Provider)
	if err != nil {
		return nil, err
	}
	out := []vm.Machine{}
	var failures []error
	for _, p := range providers {
		machines, err := m.list(ctx, p)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, mach := range machines {
			if mach.Managed || !opts.ManagedOnly {
				out = append(out, mach)
			}
		}
	}
	if len(failures) == len(providers) {
		return nil, errors.Join(failures...)
	}
	for _, err := range failures {
		m.log.Warn("skipping a provider that failed to list its vms", "error", err)
	}
	return out, nil
}

func (m *Manager) allMachines(ctx context.Context) ([]vm.Machine, error) {
	providers, err := m.listProviders(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []vm.Machine
	for _, p := range providers {
		machines, err := m.list(ctx, p)
		if err != nil {
			return nil, err
		}
		out = append(out, machines...)
	}
	return out, nil
}

func (m *Manager) Get(ctx context.Context, ref Ref) (vm.Machine, error) {
	_, mach, err := m.resolve(ctx, ref)
	return mach, err
}
