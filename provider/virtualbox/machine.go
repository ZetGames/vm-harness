package virtualbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/fl4metf/vm-harness/vm"
)

const (
	listWorkers  = 4
	inaccessible = "<inaccessible>"
)

func stateOf(s string) vm.State {
	switch s {
	case "running":
		return vm.StateRunning
	case "paused", "gurumeditation":
		return vm.StatePaused
	case "saved":
		return vm.StateSaved
	case "poweroff", "aborted", "aborted-saved":
		return vm.StateStopped
	case "starting", "stopping", "saving", "restoring", "settingup", "snapshotting", "livesnapshotting",
		"onlinesnapshotting", "restoringsnapshot", "deletingsnapshot", "deletingsnapshotlive",
		"deletingsnapshotlivepaused", "teleporting", "teleportingpausedvm", "teleportingin":
		return vm.StateBusy
	}
	return vm.StateUnknown
}

func firmwareOf(s string) string {
	s = strings.ToLower(s)
	if strings.HasPrefix(s, "efi") {
		return "efi"
	}
	return s
}

var nicModes = map[string]string{
	"nat":        vm.NetNAT,
	"bridged":    vm.NetBridged,
	"hostonly":   vm.NetHostOnly,
	"natnetwork": vm.NetNATNetwork,
	"intnet":     vm.NetInternal,
	"null":       vm.NetNone,
}

var nicAdapterKeys = map[string]string{
	"bridged":    "bridgeadapter",
	"hostonly":   "hostonlyadapter",
	"natnetwork": "nat-network",
	"intnet":     "intnet",
}

func nicsOf(f map[string]string) []vm.NIC {
	var nics []vm.NIC
	for n := 1; ; n++ {
		suffix := strconv.Itoa(n)
		mode, ok := f["nic"+suffix]
		if !ok {
			return nics
		}
		if mode == "none" {
			continue
		}
		nic := vm.NIC{Mode: cmp.Or(nicModes[mode], mode), Model: f["nictype"+suffix], MAC: formatMAC(f["macaddress"+suffix])}
		if key, ok := nicAdapterKeys[mode]; ok {
			nic.Adapter = f[key+suffix]
		}
		nics = append(nics, nic)
	}
}

func formatMAC(s string) string {
	if len(s) != 12 {
		return s
	}
	parts := make([]string, 0, 6)
	for i := 0; i < 12; i += 2 {
		parts = append(parts, s[i:i+2])
	}
	return strings.Join(parts, ":")
}

func (p *Provider) inspect(ctx context.Context, ref string) (vmInfo, error) {
	if err := checkRef(ref); err != nil {
		return vmInfo{}, err
	}
	out, err := p.run(ctx, "showvminfo", ref, "--machinereadable")
	if err != nil {
		return vmInfo{}, err
	}
	info := parseVMInfo(out)
	if info.id() == "" {
		return vmInfo{}, fmt.Errorf("showvminfo %s printed no uuid", ref)
	}
	return info, nil
}

func (p *Provider) machine(ctx context.Context, info vmInfo) vm.Machine {
	f := info.fields
	cpus, _ := strconv.Atoi(f["cpus"])
	memory, _ := strconv.Atoi(f["memory"])
	return vm.Machine{
		ID:              info.id(),
		Name:            info.name(),
		Provider:        vm.VirtualBox,
		State:           info.state(),
		OSType:          p.osTypeID(ctx, f["ostype"]),
		CPUs:            cpus,
		MemoryMB:        memory,
		Firmware:        firmwareOf(f["firmware"]),
		ConfigPath:      f["CfgFile"],
		NICs:            nicsOf(f),
		PortForwards:    info.forwards,
		CurrentSnapshot: f["CurrentSnapshotName"],
		ConsoleLog:      info.consoleLog(),
	}
}

func (p *Provider) osTypeID(ctx context.Context, description string) string {
	p.osTypesMu.Lock()
	defer p.osTypesMu.Unlock()
	if p.osTypes == nil {
		out, err := p.run(ctx, "list", "ostypes")
		if err != nil {
			return description
		}
		p.osTypes = parseOSTypes(out)
	}
	return cmp.Or(p.osTypes[description], description)
}

func (p *Provider) Get(ctx context.Context, ref string) (vm.Machine, error) {
	info, err := p.inspect(ctx, ref)
	if err != nil {
		return vm.Machine{}, err
	}
	m := p.machine(ctx, info)
	out, err := p.run(ctx, "getextradata", m.ID, "enumerate")
	if err != nil {
		return vm.Machine{}, err
	}
	m.Meta = parseMeta(out)
	vm.ApplyMeta(&m)
	return m, nil
}

func (p *Provider) listVMs(ctx context.Context) ([]vmEntry, error) {
	out, err := p.run(ctx, "list", "vms")
	if err != nil {
		return nil, err
	}
	return parseVMList(out), nil
}

func (p *Provider) List(ctx context.Context) ([]vm.Machine, error) {
	entries, err := p.listVMs(ctx)
	if err != nil {
		return nil, err
	}
	machines := make([]vm.Machine, len(entries))
	errs := make([]error, len(entries))
	sem := make(chan struct{}, listWorkers)
	var wg sync.WaitGroup
	for i, e := range entries {
		machines[i] = vm.Machine{ID: e.id, Name: e.name, Provider: vm.VirtualBox, State: vm.StateUnknown}
		if e.name == inaccessible {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			m, err := p.Get(ctx, e.id)
			if err == nil {
				machines[i] = m
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := machines[:0]
	for i, m := range machines {
		if !errors.Is(errs[i], vm.ErrNotFound) {
			out = append(out, m)
		}
	}
	return out, nil
}

func (p *Provider) SetMeta(ctx context.Context, ref string, meta map[string]string) error {
	if err := checkRef(ref); err != nil {
		return err
	}
	for _, key := range slices.Sorted(maps.Keys(meta)) {
		if key == "" {
			return fmt.Errorf("empty meta key: %w", vm.ErrInvalid)
		}
		args := []string{"setextradata", ref, metaPrefix + key}
		if v := meta[key]; v != "" {
			args = append(args, v)
		}
		if _, err := p.run(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) Update(ctx context.Context, ref string, ch vm.Changes) error {
	if ch.CPUs <= 0 && ch.MemoryMB <= 0 {
		return nil
	}
	info, err := p.inspect(ctx, ref)
	if err != nil {
		return err
	}
	if s := info.state(); s != vm.StateStopped {
		return fmt.Errorf("vm %s is %s, stop it first: %w", info.name(), s, vm.ErrInvalidState)
	}
	args := []string{"modifyvm", info.id()}
	if ch.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(ch.CPUs))
	}
	if ch.MemoryMB > 0 {
		args = append(args, "--memory", strconv.Itoa(ch.MemoryMB))
	}
	_, err = p.run(ctx, args...)
	return err
}
