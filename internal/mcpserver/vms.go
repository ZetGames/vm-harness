package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/ZetGames/vm-harness/harness"
	"github.com/ZetGames/vm-harness/vm"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type target struct {
	VM       string `json:"vm" jsonschema:"name or ID of the VM"`
	Provider string `json:"provider,omitempty" jsonschema:"virtualbox or vmware, needed only when the name exists on both"`
}

func (t target) ref() harness.Ref { return harness.Ref{Provider: t.Provider, VM: t.VM} }

type providersOutput struct {
	Providers []harness.ProviderStatus `json:"providers"`
}

func (s *server) providers(ctx context.Context, _ struct{}) (providersOutput, error) {
	return providersOutput{Providers: s.m.Providers(ctx)}, nil
}

type listInput struct {
	Provider    string `json:"provider,omitempty" jsonschema:"only list VMs of this provider: virtualbox or vmware"`
	ManagedOnly bool   `json:"managed_only,omitempty" jsonschema:"only list VMs managed by vmh"`
}

type listOutput struct {
	VMs []vm.Machine `json:"vms"`
}

func (s *server) list(ctx context.Context, in listInput) (listOutput, error) {
	machines, err := s.m.List(ctx, harness.ListOptions(in))
	return listOutput{VMs: machines}, err
}

func (s *server) get(ctx context.Context, in target) (vm.Machine, error) {
	return s.m.Get(ctx, in.ref())
}

type powerInput struct {
	target
	Action     string `json:"action" jsonschema:"start, stop, kill, pause, resume, reset or suspend"`
	GUI        bool   `json:"gui,omitempty" jsonschema:"start only: open a console window instead of running headless"`
	TimeoutSec int    `json:"timeout_sec,omitempty" jsonschema:"stop only: seconds to wait for a clean shutdown before forcing power-off, default 60"`
}

func (s *server) power(ctx context.Context, in powerInput) (vm.Machine, error) {
	ref := in.ref()
	switch in.Action {
	case "start":
		return s.m.Start(ctx, ref, in.GUI)
	case "stop":
		return s.m.Stop(ctx, ref, harness.StopOptions{Timeout: time.Duration(in.TimeoutSec) * time.Second})
	case "kill":
		return s.m.Stop(ctx, ref, harness.StopOptions{Force: true})
	case "pause":
		return s.m.Pause(ctx, ref)
	case "resume":
		return s.m.Resume(ctx, ref)
	case "reset":
		return s.m.Reset(ctx, ref)
	case "suspend":
		return s.m.Suspend(ctx, ref)
	}
	return vm.Machine{}, unknownAction(in.Action, "start, stop, kill, pause, resume, reset or suspend")
}

type deleteInput struct {
	target
	Force bool `json:"force,omitempty" jsonschema:"power the VM off first if it is running"`
}

func (s *server) deleteVM(ctx context.Context, in deleteInput) (*mcp.CallToolResult, error) {
	if err := s.m.Delete(ctx, in.ref(), in.Force); err != nil {
		return nil, err
	}
	return text(fmt.Sprintf("vm %q deleted", in.VM)), nil
}

type updateInput struct {
	target
	CPUs     int               `json:"cpus,omitempty" jsonschema:"new number of virtual CPUs, the VM must be stopped"`
	MemoryMB int               `json:"memory_mb,omitempty" jsonschema:"new memory size in MiB, the VM must be stopped"`
	Labels   map[string]string `json:"labels,omitempty" jsonschema:"labels to set; an empty value removes the label"`
}

func (s *server) update(ctx context.Context, in updateInput) (vm.Machine, error) {
	return s.m.Update(ctx, in.ref(), vm.Changes{CPUs: in.CPUs, MemoryMB: in.MemoryMB, Labels: in.Labels})
}

type cloneInput struct {
	target
	Name     string `json:"name" jsonschema:"name of the new VM: letters, digits, dot, underscore or dash, up to 63 characters"`
	Linked   bool   `json:"linked,omitempty" jsonschema:"make a linked clone that shares the source disks instead of a full copy"`
	Snapshot string `json:"snapshot,omitempty" jsonschema:"linked clones only: source snapshot to base the clone on"`
}

func (s *server) clone(ctx context.Context, in cloneInput) (vm.Machine, error) {
	return s.m.Clone(ctx, in.ref(), vm.CloneOptions{Name: in.Name, Linked: in.Linked, Snapshot: in.Snapshot})
}

type snapshotInput struct {
	target
	Action      string `json:"action" jsonschema:"list, take, restore or delete"`
	Name        string `json:"name,omitempty" jsonschema:"snapshot name, required for take, restore and delete"`
	Description string `json:"description,omitempty" jsonschema:"take only: one-line description of the snapshot"`
}

type snapshotOutput struct {
	Snapshots []vm.Snapshot `json:"snapshots,omitzero"`
	Snapshot  *vm.Snapshot  `json:"snapshot,omitempty"`
	Machine   *vm.Machine   `json:"machine,omitempty"`
}

func (s *server) snapshot(ctx context.Context, in snapshotInput) (snapshotOutput, error) {
	ref := in.ref()
	switch in.Action {
	case "list":
		return s.snapshots(ctx, ref)
	case "take":
		snap, err := s.m.TakeSnapshot(ctx, ref, in.Name, in.Description)
		return snapshotOutput{Snapshot: &snap}, err
	case "restore":
		mach, err := s.m.RestoreSnapshot(ctx, ref, in.Name)
		return snapshotOutput{Machine: &mach}, err
	case "delete":
		if err := s.m.DeleteSnapshot(ctx, ref, in.Name); err != nil {
			return snapshotOutput{}, err
		}
		return s.snapshots(ctx, ref)
	}
	return snapshotOutput{}, unknownAction(in.Action, "list, take, restore or delete")
}

func (s *server) snapshots(ctx context.Context, ref harness.Ref) (snapshotOutput, error) {
	snapshots, err := s.m.Snapshots(ctx, ref)
	return snapshotOutput{Snapshots: snapshots}, err
}

type portInput struct {
	target
	Action string `json:"action" jsonschema:"list, add or remove"`
	portForward
}

type portOutput struct {
	Forwards []vm.PortForward `json:"port_forwards,omitzero"`
	Forward  *vm.PortForward  `json:"port_forward,omitempty"`
}

func (s *server) port(ctx context.Context, in portInput) (portOutput, error) {
	ref := in.ref()
	switch in.Action {
	case "list":
		return s.ports(ctx, ref)
	case "add":
		pf, err := s.m.AddPortForward(ctx, ref, vm.PortForward(in.portForward))
		return portOutput{Forward: &pf}, err
	case "remove":
		if err := s.m.RemovePortForward(ctx, ref, in.Name); err != nil {
			return portOutput{}, err
		}
		return s.ports(ctx, ref)
	}
	return portOutput{}, unknownAction(in.Action, "list, add or remove")
}

func (s *server) ports(ctx context.Context, ref harness.Ref) (portOutput, error) {
	mach, err := s.m.Get(ctx, ref)
	if err != nil {
		return portOutput{}, err
	}
	forwards := mach.PortForwards
	if forwards == nil {
		forwards = []vm.PortForward{}
	}
	return portOutput{Forwards: forwards}, nil
}
