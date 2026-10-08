package memprovider

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/fl4metf/vm-harness/vm"
)

var PNG = []byte("\x89PNG\r\n\x1a\nfake")

type machine struct {
	m         vm.Machine
	snapshots []vm.Snapshot
	files     map[string][]byte
	ip        string
}

type Provider struct {
	ProviderName    string
	Features        []string
	Warnings        []string
	MaxReliableCPUs int
	ExecFunc        func(m vm.Machine, req vm.ExecRequest) (vm.ExecResult, error)
	SoftStopFunc    func(m vm.Machine) error
	ResetFunc       func(m vm.Machine)

	mu       sync.Mutex
	machines map[string]*machine
	seq      int
	calls    []string
}

func New(name string) *Provider {
	return &Provider{ProviderName: name, machines: make(map[string]*machine)}
}

func (p *Provider) Name() string { return p.ProviderName }

var allFeatures = []string{
	vm.FeatureGuestExec, vm.FeatureGuestCopy, vm.FeatureScreenshot, vm.FeaturePortForward,
	vm.FeatureLinkedClone, vm.FeatureSnapshots, vm.FeatureAppliance, vm.FeatureDiskImage,
	vm.FeatureCloudInit, vm.FeatureUnattended, vm.FeatureSharedFolders,
}

func (p *Provider) Info(context.Context) (vm.HostInfo, error) {
	features := p.Features
	if features == nil {
		features = allFeatures
	}
	return vm.HostInfo{
		Provider:        p.ProviderName,
		Version:         "mem",
		Features:        slices.Clone(features),
		Warnings:        slices.Clone(p.Warnings),
		MaxReliableCPUs: p.MaxReliableCPUs,
	}, nil
}

func (p *Provider) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func (p *Provider) record(op, ref string) {
	p.calls = append(p.calls, op+" "+ref)
}

func (p *Provider) find(ref string) (*machine, error) {
	if m, ok := p.machines[ref]; ok {
		return m, nil
	}
	for _, m := range p.machines {
		if m.m.ID == ref {
			return m, nil
		}
	}
	return nil, fmt.Errorf("machine %q: %w", ref, vm.ErrNotFound)
}

func snapshotOf(m *machine) vm.Machine {
	out := m.m
	out.Meta = maps.Clone(m.m.Meta)
	out.NICs = slices.Clone(m.m.NICs)
	out.PortForwards = slices.Clone(m.m.PortForwards)
	vm.ApplyMeta(&out)
	return out
}

func (p *Provider) Put(m vm.Machine) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m.ID == "" {
		p.seq++
		m.ID = fmt.Sprintf("%s-%d", p.ProviderName, p.seq)
	}
	m.Provider = p.ProviderName
	if m.State == "" {
		m.State = vm.StateStopped
	}
	m.Meta = maps.Clone(m.Meta)
	if m.Meta == nil {
		m.Meta = map[string]string{}
	}
	if m.Managed {
		m.Meta[vm.MetaManaged] = m.ID
	}
	p.machines[m.Name] = &machine{m: m, files: map[string][]byte{}}
}

func (p *Provider) SetState(ref string, s vm.State) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, err := p.find(ref); err == nil {
		m.m.State = s
	}
}

func (p *Provider) SetIP(ref, ip string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, err := p.find(ref); err == nil {
		m.ip = ip
	}
}

func (p *Provider) GuestFile(ref, path string) ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, err := p.find(ref)
	if err != nil {
		return nil, false
	}
	b, ok := m.files[path]
	return b, ok
}

func (p *Provider) List(context.Context) ([]vm.Machine, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]vm.Machine, 0, len(p.machines))
	for _, m := range p.machines {
		out = append(out, snapshotOf(m))
	}
	slices.SortFunc(out, func(a, b vm.Machine) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (p *Provider) Get(_ context.Context, ref string) (vm.Machine, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, err := p.find(ref)
	if err != nil {
		return vm.Machine{}, err
	}
	return snapshotOf(m), nil
}

func (p *Provider) Create(_ context.Context, spec vm.Spec) (vm.Machine, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("create", spec.Name)
	if _, ok := p.machines[spec.Name]; ok {
		return vm.Machine{}, fmt.Errorf("machine %q: %w", spec.Name, vm.ErrExists)
	}
	p.seq++
	meta := maps.Clone(spec.Meta)
	if meta == nil {
		meta = map[string]string{}
	}
	for k, v := range spec.Labels {
		meta[vm.LabelKey(k)] = v
	}
	nics := slices.Clone(spec.NICs)
	if len(nics) == 0 {
		nics = []vm.NIC{{Mode: vm.NetNAT}}
	}
	m := &machine{
		m: vm.Machine{
			ID:           fmt.Sprintf("%s-%d", p.ProviderName, p.seq),
			Name:         spec.Name,
			Provider:     p.ProviderName,
			State:        vm.StateStopped,
			OSType:       spec.OSType,
			CPUs:         spec.CPUs,
			MemoryMB:     spec.MemoryMB,
			Firmware:     spec.Firmware,
			Meta:         meta,
			NICs:         nics,
			PortForwards: slices.Clone(spec.PortForwards),
		},
		files: map[string][]byte{},
	}
	p.machines[spec.Name] = m
	return snapshotOf(m), nil
}

func (p *Provider) Delete(_ context.Context, ref string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("delete", ref)
	m, err := p.find(ref)
	if err != nil {
		return err
	}
	if m.m.State == vm.StateRunning || m.m.State == vm.StatePaused {
		return fmt.Errorf("machine %q is %s: %w", ref, m.m.State, vm.ErrInvalidState)
	}
	delete(p.machines, m.m.Name)
	return nil
}

func (p *Provider) Update(_ context.Context, ref string, ch vm.Changes) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("update", ref)
	m, err := p.find(ref)
	if err != nil {
		return err
	}
	if (ch.CPUs > 0 || ch.MemoryMB > 0) && m.m.State != vm.StateStopped {
		return fmt.Errorf("machine %q must be stopped: %w", ref, vm.ErrInvalidState)
	}
	if ch.CPUs > 0 {
		m.m.CPUs = ch.CPUs
	}
	if ch.MemoryMB > 0 {
		m.m.MemoryMB = ch.MemoryMB
	}
	return nil
}

func (p *Provider) SetMeta(_ context.Context, ref string, meta map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("setmeta", ref)
	m, err := p.find(ref)
	if err != nil {
		return err
	}
	for k, v := range meta {
		if v == "" {
			delete(m.m.Meta, k)
		} else {
			m.m.Meta[k] = v
		}
	}
	return nil
}

func (p *Provider) Clone(_ context.Context, ref string, opts vm.CloneOptions) (vm.Machine, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("clone", ref)
	src, err := p.find(ref)
	if err != nil {
		return vm.Machine{}, err
	}
	if _, ok := p.machines[opts.Name]; ok {
		return vm.Machine{}, fmt.Errorf("machine %q: %w", opts.Name, vm.ErrExists)
	}
	if opts.Linked {
		snap := opts.Snapshot
		if snap == "" {
			snap = src.m.CurrentSnapshot
		}
		if !slices.ContainsFunc(src.snapshots, func(s vm.Snapshot) bool { return s.Name == snap }) {
			return vm.Machine{}, fmt.Errorf("linked clone needs a snapshot: %w", vm.ErrInvalidState)
		}
	}
	p.seq++
	c := snapshotOf(src)
	c.ID = fmt.Sprintf("%s-%d", p.ProviderName, p.seq)
	c.Name = opts.Name
	c.State = vm.StateStopped
	c.CurrentSnapshot = ""
	p.machines[opts.Name] = &machine{m: c, files: maps.Clone(src.files)}
	return snapshotOf(p.machines[opts.Name]), nil
}

func (p *Provider) transition(op, ref string, from []vm.State, to vm.State) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record(op, ref)
	m, err := p.find(ref)
	if err != nil {
		return err
	}
	if !slices.Contains(from, m.m.State) {
		return fmt.Errorf("%s %q: machine is %s: %w", op, ref, m.m.State, vm.ErrInvalidState)
	}
	m.m.State = to
	return nil
}

func (p *Provider) Start(_ context.Context, ref string, _ bool) error {
	return p.transition("start", ref, []vm.State{vm.StateStopped, vm.StateSaved}, vm.StateRunning)
}

func (p *Provider) Stop(_ context.Context, ref string, force bool) error {
	if force {
		return p.transition("kill", ref, []vm.State{vm.StateRunning, vm.StatePaused, vm.StateSaved, vm.StateBusy, vm.StateUnknown}, vm.StateStopped)
	}
	if p.SoftStopFunc != nil {
		return p.softStop(ref)
	}
	return p.transition("stop", ref, []vm.State{vm.StateRunning, vm.StatePaused}, vm.StateStopped)
}

func (p *Provider) softStop(ref string) error {
	p.mu.Lock()
	p.record("stop", ref)
	m, err := p.running(ref)
	if err != nil {
		p.mu.Unlock()
		return err
	}
	snap := snapshotOf(m)
	fn := p.SoftStopFunc
	p.mu.Unlock()
	return fn(snap)
}

func (p *Provider) Pause(_ context.Context, ref string) error {
	return p.transition("pause", ref, []vm.State{vm.StateRunning}, vm.StatePaused)
}

func (p *Provider) Resume(_ context.Context, ref string) error {
	return p.transition("resume", ref, []vm.State{vm.StatePaused}, vm.StateRunning)
}

func (p *Provider) Reset(_ context.Context, ref string) error {
	if err := p.transition("reset", ref, []vm.State{vm.StateRunning}, vm.StateRunning); err != nil {
		return err
	}
	p.mu.Lock()
	m, err := p.find(ref)
	if err != nil {
		p.mu.Unlock()
		return err
	}
	snap := snapshotOf(m)
	fn := p.ResetFunc
	p.mu.Unlock()
	if fn != nil {
		fn(snap)
	}
	return nil
}

func (p *Provider) Suspend(_ context.Context, ref string) error {
	return p.transition("suspend", ref, []vm.State{vm.StateRunning, vm.StatePaused}, vm.StateSaved)
}

func (p *Provider) Snapshots(_ context.Context, ref string) ([]vm.Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, err := p.find(ref)
	if err != nil {
		return nil, err
	}
	out := slices.Clone(m.snapshots)
	for i := range out {
		out[i].Current = out[i].Name == m.m.CurrentSnapshot
	}
	return out, nil
}

func (p *Provider) TakeSnapshot(_ context.Context, ref, name, description string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("snapshot", ref)
	m, err := p.find(ref)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(m.snapshots, func(s vm.Snapshot) bool { return s.Name == name }) {
		return fmt.Errorf("snapshot %q: %w", name, vm.ErrExists)
	}
	m.snapshots = append(m.snapshots, vm.Snapshot{
		Name:        name,
		ID:          fmt.Sprintf("snap-%d", len(m.snapshots)+1),
		Description: description,
		Parent:      m.m.CurrentSnapshot,
	})
	m.m.CurrentSnapshot = name
	return nil
}

func (p *Provider) RestoreSnapshot(_ context.Context, ref, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("restore", ref)
	m, err := p.find(ref)
	if err != nil {
		return err
	}
	if m.m.State == vm.StateRunning || m.m.State == vm.StatePaused {
		return fmt.Errorf("restore %q: machine is %s: %w", ref, m.m.State, vm.ErrInvalidState)
	}
	if !slices.ContainsFunc(m.snapshots, func(s vm.Snapshot) bool { return s.Name == name }) {
		return fmt.Errorf("snapshot %q: %w", name, vm.ErrNotFound)
	}
	m.m.CurrentSnapshot = name
	return nil
}

func (p *Provider) DeleteSnapshot(_ context.Context, ref, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("delsnapshot", ref)
	m, err := p.find(ref)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(m.snapshots, func(s vm.Snapshot) bool { return s.Name == name })
	if i < 0 {
		return fmt.Errorf("snapshot %q: %w", name, vm.ErrNotFound)
	}
	m.snapshots = slices.Delete(m.snapshots, i, i+1)
	if m.m.CurrentSnapshot == name {
		m.m.CurrentSnapshot = ""
	}
	return nil
}

func (p *Provider) running(ref string) (*machine, error) {
	m, err := p.find(ref)
	if err != nil {
		return nil, err
	}
	if m.m.State != vm.StateRunning {
		return nil, fmt.Errorf("machine %q is %s: %w", ref, m.m.State, vm.ErrInvalidState)
	}
	return m, nil
}

func (p *Provider) GuestIP(_ context.Context, ref string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, err := p.running(ref)
	if err != nil {
		return "", err
	}
	if m.ip == "" {
		return "", fmt.Errorf("guest ip: %w", vm.ErrNotReady)
	}
	return m.ip, nil
}

func (p *Provider) Exec(_ context.Context, ref string, req vm.ExecRequest) (vm.ExecResult, error) {
	p.mu.Lock()
	p.record("exec", ref)
	m, err := p.running(ref)
	if err != nil {
		p.mu.Unlock()
		return vm.ExecResult{}, err
	}
	snap := snapshotOf(m)
	fn := p.ExecFunc
	p.mu.Unlock()
	if fn != nil {
		return fn(snap, req)
	}
	out := req.Script
	if out == "" {
		out = strings.Join(req.Command, " ")
	}
	return vm.ExecResult{Stdout: out + "\n"}, nil
}

func (p *Provider) CopyTo(_ context.Context, ref string, req vm.CopyRequest) error {
	data, err := os.ReadFile(req.HostPath)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("copyto", ref)
	m, err := p.running(ref)
	if err != nil {
		return err
	}
	m.files[req.GuestPath] = data
	return nil
}

func (p *Provider) CopyFrom(_ context.Context, ref string, req vm.CopyRequest) error {
	p.mu.Lock()
	p.record("copyfrom", ref)
	m, err := p.running(ref)
	if err != nil {
		p.mu.Unlock()
		return err
	}
	data, ok := m.files[req.GuestPath]
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("guest file %q: %w", req.GuestPath, vm.ErrNotFound)
	}
	return os.WriteFile(req.HostPath, data, 0o644)
}

func (p *Provider) Screenshot(_ context.Context, ref string, _ vm.Credentials) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.running(ref); err != nil {
		return nil, err
	}
	return slices.Clone(PNG), nil
}

func (p *Provider) AddPortForward(_ context.Context, ref string, pf vm.PortForward) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("addpf", ref)
	m, err := p.find(ref)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(m.m.PortForwards, func(x vm.PortForward) bool { return x.Name == pf.Name }) {
		return fmt.Errorf("port forward %q: %w", pf.Name, vm.ErrExists)
	}
	m.m.PortForwards = append(m.m.PortForwards, pf)
	return nil
}

func (p *Provider) RemovePortForward(_ context.Context, ref, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("rmpf", ref)
	m, err := p.find(ref)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(m.m.PortForwards, func(x vm.PortForward) bool { return x.Name == name })
	if i < 0 {
		return fmt.Errorf("port forward %q: %w", name, vm.ErrNotFound)
	}
	m.m.PortForwards = slices.Delete(m.m.PortForwards, i, i+1)
	return nil
}
