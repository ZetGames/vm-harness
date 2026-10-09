package harness

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ZetGames/vm-harness/vm"
)

const (
	defaultSSHUser = "vmh"
	sshForwardName = "vmh-ssh"
)

func (m *Manager) Create(ctx context.Context, spec vm.Spec) (created vm.Machine, err error) {
	start := time.Now()
	provider := spec.Provider
	defer func() { m.logOp(ctx, "create", provider, spec.Name, time.Since(start), err) }()

	p, info, err := m.createProvider(ctx, spec.Provider)
	if err != nil {
		return vm.Machine{}, err
	}
	provider = p.Name()
	if spec, err = m.prepareSpec(spec, provider, info); err != nil {
		return vm.Machine{}, err
	}
	if err := m.checkSharedFolders(ctx, spec.SharedFolders); err != nil {
		return vm.Machine{}, err
	}
	err = m.creatingVM(ctx, func() error {
		if err := m.checkVMLimit(ctx); err != nil {
			return err
		}
		if err := ensureAbsent(ctx, p, spec.Name); err != nil {
			return err
		}
		if err := m.assignHostPorts(ctx, spec.PortForwards); err != nil {
			return err
		}
		key, err := m.generateSSHKey(&spec)
		if err != nil {
			return err
		}
		if created, err = m.provision(ctx, p, spec); err != nil {
			m.removeKey(key)
		}
		return err
	})
	if err != nil {
		return vm.Machine{}, err
	}
	if spec.Start {
		if err := p.Start(ctx, created.ID, false); err != nil {
			mach, _ := m.get(ctx, p, created.ID)
			return mach, fmt.Errorf("vm %q was created but did not start: %w", spec.Name, err)
		}
	}
	return m.get(ctx, p, created.ID)
}

func (m *Manager) provision(ctx context.Context, p vm.Provider, spec vm.Spec) (vm.Machine, error) {
	created, err := p.Create(ctx, spec)
	if err != nil {
		return vm.Machine{}, err
	}
	if err := m.cfg.Limits.check(created.CPUs, created.MemoryMB, 0); err != nil {
		m.discard(ctx, p, created)
		return vm.Machine{}, fmt.Errorf("vm %q was removed again because it exceeds the limits: %w", created.Name, err)
	}
	if err := p.SetMeta(ctx, created.ID, map[string]string{vm.MetaManaged: created.ID}); err != nil {
		m.discard(ctx, p, created)
		return vm.Machine{}, fmt.Errorf("mark vm %q as managed: %w", created.Name, err)
	}
	m.forgetHostKeys(created)
	return created, nil
}

func (m *Manager) discard(ctx context.Context, p vm.Provider, mach vm.Machine) {
	if err := p.Delete(context.WithoutCancel(ctx), mach.ID); err != nil {
		m.log.Warn("remove vm after a failed operation", "provider", p.Name(), "vm", mach.Name, "error", err)
	}
}

func ensureAbsent(ctx context.Context, p vm.Provider, name string) error {
	_, err := p.Get(ctx, name)
	switch {
	case err == nil:
		return fmt.Errorf("vm %q already exists in %s: %w", name, p.Name(), vm.ErrExists)
	case !errors.Is(err, vm.ErrNotFound):
		return err
	}
	machines, err := p.List(ctx)
	if err != nil {
		return fmt.Errorf("list %s vms: %w", p.Name(), err)
	}
	for _, mach := range machines {
		if strings.EqualFold(mach.Name, name) {
			return fmt.Errorf("vm %q differs from the existing vm %q in %s only by case: %w", name, mach.Name, p.Name(), vm.ErrExists)
		}
	}
	return nil
}

func (m *Manager) prepareSpec(spec vm.Spec, provider string, info vm.HostInfo) (vm.Spec, error) {
	if err := checkName("vm", spec.Name); err != nil {
		return spec, err
	}
	spec.Provider = provider
	if err := m.applySpecDefaults(&spec, info); err != nil {
		return spec, err
	}
	if err := m.resolveSpecPaths(&spec); err != nil {
		return spec, err
	}
	if err := checkSpec(spec, info); err != nil {
		return spec, err
	}
	forwards, err := specForwards(spec, info)
	if err != nil {
		return spec, err
	}
	spec.PortForwards = forwards

	spec.Meta = map[string]string{}
	if spec.OSType != "" {
		spec.Meta[vm.MetaOSType] = spec.OSType
		spec.OSType = vm.NativeOSType(provider, spec.OSType)
	}
	if spec.CloudInit != nil {
		ci := *spec.CloudInit
		ci.SSHAuthorizedKeys = slices.Clone(ci.SSHAuthorizedKeys)
		if ci.UserData == "" {
			ci.User = cmp.Or(ci.User, defaultSSHUser)
			spec.Meta[vm.MetaSSHUser] = ci.User
		}
		spec.CloudInit = &ci
	}
	return spec, nil
}

func (m *Manager) applySpecDefaults(spec *vm.Spec, info vm.HostInfo) error {
	if spec.CPUs < 0 || spec.MemoryMB < 0 || spec.DiskGB < 0 {
		return fmt.Errorf("cpus, memory and disk size must not be negative: %w", vm.ErrInvalid)
	}
	if spec.Appliance == "" {
		d := m.cfg.Defaults
		if info.MaxReliableCPUs > 0 {
			d.CPUs = min(d.CPUs, info.MaxReliableCPUs)
		}
		spec.CPUs = cmp.Or(spec.CPUs, d.CPUs)
		spec.MemoryMB = cmp.Or(spec.MemoryMB, d.MemoryMB)
		spec.DiskGB = cmp.Or(spec.DiskGB, d.DiskGB)
		spec.OSType = cmp.Or(spec.OSType, d.OSType)
	} else if info.MaxReliableCPUs > 0 {
		spec.CPUs = cmp.Or(spec.CPUs, info.MaxReliableCPUs)
	}
	if len(spec.NICs) == 0 {
		spec.NICs = []vm.NIC{{Mode: vm.NetNAT}}
	}
	return m.cfg.Limits.check(spec.CPUs, spec.MemoryMB, spec.DiskGB)
}

func (m *Manager) resolveSpecPaths(spec *vm.Spec) error {
	sources := 0
	for _, source := range []string{spec.ISO, spec.DiskImage, spec.Appliance} {
		if source != "" {
			sources++
		}
	}
	if sources > 1 {
		return fmt.Errorf("iso, disk_image and appliance are mutually exclusive: %w", vm.ErrInvalid)
	}
	var err error
	if spec.ISO != "" {
		if spec.ISO, err = m.sourcePath("iso", spec.ISO, checkISO); err != nil {
			return err
		}
	}
	if spec.DiskImage != "" {
		if spec.DiskImage, err = m.sourcePath("disk image", spec.DiskImage, m.checkDiskImage); err != nil {
			return err
		}
	}
	if spec.Appliance != "" {
		if spec.Appliance, err = m.sourcePath("appliance", spec.Appliance, checkAppliance); err != nil {
			return err
		}
	}
	spec.SharedFolders = slices.Clone(spec.SharedFolders)
	for i, sf := range spec.SharedFolders {
		if !labelPattern.MatchString(sf.Name) {
			return fmt.Errorf("invalid shared folder name %q: %w", sf.Name, vm.ErrInvalid)
		}
		if spec.SharedFolders[i].HostPath, err = m.hostPath("shared folder "+sf.Name, sf.HostPath, true); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) sourcePath(kind, path string, check func(string) error) (string, error) {
	real, err := m.hostPath(kind, path, false)
	if err != nil {
		return "", err
	}
	if len(m.cfg.HostDirs) > 0 {
		if err := check(real); err != nil {
			return "", err
		}
	}
	return real, nil
}

func checkSpec(spec vm.Spec, info vm.HostInfo) error {
	if spec.Unattended != nil {
		if spec.ISO == "" {
			return fmt.Errorf("unattended installation needs an iso: %w", vm.ErrInvalid)
		}
		if spec.CloudInit != nil {
			return fmt.Errorf("unattended installation and cloud-init are mutually exclusive: %w", vm.ErrInvalid)
		}
	}
	if spec.CloudInit != nil && spec.ISO == "" && spec.DiskImage == "" && spec.Appliance == "" {
		return fmt.Errorf("cloud-init needs a cloud disk image (disk_image), an appliance or an installer iso to boot from: %w", vm.ErrInvalid)
	}
	if spec.Firmware != "" && spec.Firmware != "bios" && spec.Firmware != "efi" {
		return fmt.Errorf("firmware %q: want bios or efi: %w", spec.Firmware, vm.ErrInvalid)
	}
	for i, nic := range spec.NICs {
		if !slices.Contains(nicModes, nic.Mode) {
			return fmt.Errorf("nic %d: unknown mode %q, want one of %s: %w", i+1, nic.Mode, strings.Join(nicModes, ", "), vm.ErrInvalid)
		}
	}
	if err := checkLabels(spec.Labels); err != nil {
		return err
	}
	needs := []struct {
		used    bool
		feature string
	}{
		{spec.Appliance != "", vm.FeatureAppliance},
		{spec.DiskImage != "", vm.FeatureDiskImage},
		{spec.CloudInit != nil, vm.FeatureCloudInit},
		{spec.Unattended != nil, vm.FeatureUnattended},
		{len(spec.SharedFolders) > 0, vm.FeatureSharedFolders},
		{len(spec.PortForwards) > 0, vm.FeaturePortForward},
	}
	for _, n := range needs {
		if n.used && !hasFeature(info, n.feature) {
			return fmt.Errorf("provider %s does not support %s: %w", spec.Provider, n.feature, vm.ErrUnsupported)
		}
	}
	return nil
}

func specForwards(spec vm.Spec, info vm.HostInfo) ([]vm.PortForward, error) {
	var forwards []vm.PortForward
	sshForwarded := false
	for _, pf := range spec.PortForwards {
		pf, err := normalizeForward(pf)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(forwards, func(x vm.PortForward) bool { return x.Name == pf.Name }) {
			return nil, fmt.Errorf("duplicate port forward name %q: %w", pf.Name, vm.ErrInvalid)
		}
		sshForwarded = sshForwarded || pf.Protocol == "tcp" && pf.GuestPort == 22
		forwards = append(forwards, pf)
	}
	nat := natFirst(spec.NICs)
	if len(forwards) > 0 && !nat {
		return nil, fmt.Errorf("port forwarding needs nic 1 in nat mode: %w", vm.ErrInvalid)
	}
	if spec.CloudInit != nil && nat && !sshForwarded && hasFeature(info, vm.FeaturePortForward) {
		forwards = append(forwards, vm.PortForward{Name: sshForwardName, Protocol: "tcp", HostIP: "127.0.0.1", GuestPort: 22})
	}
	return forwards, nil
}

func (m *Manager) checkVMLimit(ctx context.Context) error {
	limit := m.cfg.Limits.MaxVMs
	if limit <= 0 {
		return nil
	}
	machines, err := m.allMachines(ctx)
	if err != nil {
		return fmt.Errorf("count managed vms: %w", err)
	}
	managed := 0
	for _, mach := range machines {
		if mach.Managed {
			managed++
		}
	}
	if managed >= limit {
		return fmt.Errorf("%d managed vms exist, max_vms is %d: %w", managed, limit, vm.ErrLimit)
	}
	return nil
}
