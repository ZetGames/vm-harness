package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ZetGames/vm-harness/internal/memprovider"
	"github.com/ZetGames/vm-harness/vm"
)

func TestResolve(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("alpha", vm.StateStopped))
	e.vbox.Put(managedVM("shared", vm.StateStopped))
	e.vmw.Put(managedVM("beta", vm.StateRunning))
	e.vmw.Put(managedVM("shared", vm.StateStopped))
	alphaID := mustGet(t, e.vbox, "alpha").ID

	cases := []struct {
		name     string
		ref      Ref
		provider string
		vm       string
		err      error
		message  string
	}{
		{name: "name in virtualbox", ref: Ref{VM: "alpha"}, provider: vm.VirtualBox, vm: "alpha"},
		{name: "name in vmware", ref: Ref{VM: "beta"}, provider: vm.VMware, vm: "beta"},
		{name: "by id", ref: Ref{VM: alphaID}, provider: vm.VirtualBox, vm: "alpha"},
		{name: "explicit provider", ref: Ref{Provider: vm.VMware, VM: "shared"}, provider: vm.VMware, vm: "shared"},
		{name: "provider name is case insensitive", ref: Ref{Provider: "VirtualBox", VM: "shared"}, provider: vm.VirtualBox, vm: "shared"},
		{name: "ambiguous", ref: Ref{VM: "shared"}, err: vm.ErrInvalid, message: "exists in virtualbox and vmware"},
		{name: "missing", ref: Ref{VM: "gamma"}, err: vm.ErrNotFound},
		{name: "missing in the given provider", ref: Ref{Provider: vm.VirtualBox, VM: "beta"}, err: vm.ErrNotFound},
		{name: "unknown provider", ref: Ref{Provider: "hyperv", VM: "alpha"}, err: vm.ErrInvalid, message: "virtualbox, vmware"},
		{name: "empty ref", ref: Ref{}, err: vm.ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, mach, err := e.m.resolve(t.Context(), c.ref)
			if c.err != nil {
				wantErr(t, err, c.err)
				if !strings.Contains(err.Error(), c.message) {
					t.Fatalf("error %q does not mention %q", err, c.message)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.Name() != c.provider || mach.Name != c.vm || mach.Provider != c.provider {
				t.Fatalf("resolved %s/%s (machine provider %s), want %s/%s", p.Name(), mach.Name, mach.Provider, c.provider, c.vm)
			}
		})
	}
}

func TestResolveSkipsUnavailableProviders(t *testing.T) {
	vbox := memprovider.New(vm.VirtualBox)
	vbox.Put(managedVM("shared", vm.StateStopped))
	vmw := newOffline(vm.VMware)
	vmw.Put(managedVM("shared", vm.StateStopped))
	vmw.Put(managedVM("only-vmware", vm.StateStopped))
	m := newManager(t, Config{}, vbox, vmw)

	got, err := m.Get(t.Context(), Ref{VM: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != vm.VirtualBox {
		t.Fatalf("resolved to %s", got.Provider)
	}
	_, err = m.Get(t.Context(), Ref{VM: "only-vmware"})
	wantErr(t, err, vm.ErrNotFound)
	_, err = m.Get(t.Context(), Ref{Provider: vm.VMware, VM: "shared"})
	wantErr(t, err, vm.ErrUnavailable)
}

func TestResolveWithoutAnyProvider(t *testing.T) {
	m := newManager(t, Config{}, newOffline(vm.VirtualBox), newOffline(vm.VMware))
	_, err := m.Get(t.Context(), Ref{VM: "x"})
	wantErr(t, err, vm.ErrUnavailable)
	_, err = m.List(t.Context(), ListOptions{})
	wantErr(t, err, vm.ErrUnavailable)
}

func TestInfoFailureMeansUnavailable(t *testing.T) {
	broken := newOffline(vm.VMware)
	broken.err = errors.New("vmrun crashed")
	m := newManager(t, Config{}, broken)
	_, err := m.Get(t.Context(), Ref{Provider: vm.VMware, VM: "x"})
	wantErr(t, err, vm.ErrUnavailable)
	if !strings.Contains(err.Error(), "vmrun crashed") {
		t.Fatalf("error %q lost the cause", err)
	}
}

func TestProviders(t *testing.T) {
	cases := []struct {
		name        string
		cfg         Config
		providers   []vm.Provider
		want        []string
		available   []bool
		defaultName string
	}{
		{
			name:        "virtualbox preferred",
			providers:   []vm.Provider{memprovider.New(vm.VMware), memprovider.New(vm.VirtualBox)},
			want:        []string{vm.VirtualBox, vm.VMware},
			available:   []bool{true, true},
			defaultName: vm.VirtualBox,
		},
		{
			name:        "first available",
			providers:   []vm.Provider{memprovider.New(vm.VMware), newOffline(vm.VirtualBox)},
			want:        []string{vm.VirtualBox, vm.VMware},
			available:   []bool{false, true},
			defaultName: vm.VMware,
		},
		{
			name:        "configured default",
			cfg:         Config{DefaultProvider: vm.VMware},
			providers:   []vm.Provider{memprovider.New(vm.VirtualBox), memprovider.New(vm.VMware)},
			want:        []string{vm.VirtualBox, vm.VMware},
			available:   []bool{true, true},
			defaultName: vm.VMware,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newManager(t, c.cfg, c.providers...)
			got := m.Providers(t.Context())
			if len(got) != len(c.want) {
				t.Fatalf("got %d providers", len(got))
			}
			for i, st := range got {
				if st.Name != c.want[i] || st.Available != c.available[i] || st.Default != (st.Name == c.defaultName) {
					t.Errorf("provider %d = %+v", i, st)
				}
				if !st.Available && !strings.Contains(st.Error, "not found") {
					t.Errorf("unavailable provider %s has error %q", st.Name, st.Error)
				}
				if st.Available && st.Info.Provider != st.Name {
					t.Errorf("provider %s info = %+v", st.Name, st.Info)
				}
			}
		})
	}
}

func TestList(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("a", vm.StateStopped))
	e.vbox.Put(foreignVM("legacy", vm.StateRunning))
	e.vmw.Put(managedVM("b", vm.StateStopped))

	names := func(ms []vm.Machine) string {
		var out []string
		for _, m := range ms {
			out = append(out, m.Provider+"/"+m.Name)
		}
		return strings.Join(out, " ")
	}
	cases := []struct {
		opts ListOptions
		want string
		err  error
	}{
		{opts: ListOptions{}, want: "virtualbox/a virtualbox/legacy vmware/b"},
		{opts: ListOptions{ManagedOnly: true}, want: "virtualbox/a vmware/b"},
		{opts: ListOptions{Provider: vm.VMware}, want: "vmware/b"},
		{opts: ListOptions{Provider: "xen"}, err: vm.ErrInvalid},
	}
	for _, c := range cases {
		got, err := e.m.List(t.Context(), c.opts)
		if c.err != nil {
			wantErr(t, err, c.err)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if names(got) != c.want {
			t.Errorf("List(%+v) = %s, want %s", c.opts, names(got), c.want)
		}
	}
}

func TestListIsNeverNil(t *testing.T) {
	e := newEnv(t)
	got, err := e.m.List(t.Context(), ListOptions{})
	if err != nil || got == nil {
		t.Fatalf("List = %v, %v", got, err)
	}
}

type brokenListing struct {
	*memprovider.Provider
}

func (b brokenListing) Get(context.Context, string) (vm.Machine, error) {
	return vm.Machine{}, fmt.Errorf("vmrun list: %w", vm.ErrUnsupported)
}

func (b brokenListing) List(context.Context) ([]vm.Machine, error) {
	return nil, fmt.Errorf("vmrun list: %w", vm.ErrUnsupported)
}

func TestOneBrokenProviderDoesNotBlockTheOther(t *testing.T) {
	vbox := memprovider.New(vm.VirtualBox)
	vbox.Put(managedVM("web", vm.StateRunning))
	m := newManager(t, Config{}, vbox, brokenListing{memprovider.New(vm.VMware)})

	got, err := m.Get(t.Context(), Ref{VM: "web"})
	if err != nil || got.Provider != vm.VirtualBox {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if stopped, err := m.Stop(t.Context(), Ref{VM: "web"}, StopOptions{Force: true}); err != nil || stopped.State != vm.StateStopped {
		t.Fatalf("Stop = %s, %v", stopped.State, err)
	}
	all, err := m.List(t.Context(), ListOptions{})
	if err != nil || len(all) != 1 || all[0].Name != "web" {
		t.Fatalf("List = %v, %v", all, err)
	}
	created, err := m.Create(t.Context(), vm.Spec{Name: "ci", Provider: vm.VirtualBox, DiskImage: cloudImage(t), CloudInit: &vm.CloudInit{}})
	if err != nil {
		t.Fatal(err)
	}
	if pf, ok := forwardNamed(created, sshForwardName); !ok || pf.HostPort == 0 {
		t.Fatalf("forwards = %+v", created.PortForwards)
	}

	_, err = m.Get(t.Context(), Ref{VM: "missing"})
	wantErr(t, err, vm.ErrUnsupported)
	_, err = m.List(t.Context(), ListOptions{Provider: vm.VMware})
	wantErr(t, err, vm.ErrUnsupported)

	limited := newManager(t, Config{Limits: Limits{MaxVMs: 10}}, vbox, brokenListing{memprovider.New(vm.VMware)})
	_, err = limited.Create(t.Context(), vm.Spec{Name: "counted", Provider: vm.VirtualBox})
	wantErr(t, err, vm.ErrUnsupported)
}

func TestListFailsWhenEveryProviderFails(t *testing.T) {
	m := newManager(t, Config{}, brokenListing{memprovider.New(vm.VirtualBox)}, brokenListing{memprovider.New(vm.VMware)})
	_, err := m.List(t.Context(), ListOptions{})
	wantErr(t, err, vm.ErrUnsupported)
}

func TestMachinesCarrySSHAccess(t *testing.T) {
	e := newEnv(t)
	web := sshVM("web", vm.PortForward{Name: sshForwardName, Protocol: "tcp", HostIP: "0.0.0.0", HostPort: 40022, GuestPort: 22})
	e.vbox.Put(web)
	direct := sshVM("direct")
	e.vmw.Put(direct)
	e.vbox.Put(managedVM("plain", vm.StateRunning))

	got, err := e.m.Get(t.Context(), vboxRef("web"))
	if err != nil {
		t.Fatal(err)
	}
	want := vm.SSHAccess{User: "vmh", KeyPath: web.Meta[vm.MetaSSHKey], Host: "127.0.0.1", Port: 40022}
	if got.SSH == nil || *got.SSH != want {
		t.Fatalf("ssh = %+v, want %+v", got.SSH, want)
	}
	all, err := e.m.List(t.Context(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, mach := range all {
		switch mach.Name {
		case "web":
			if mach.SSH == nil || *mach.SSH != want {
				t.Fatalf("listed ssh = %+v", mach.SSH)
			}
		case "direct":
			if mach.SSH == nil || mach.SSH.Host != "" || mach.SSH.Port != 0 || mach.SSH.KeyPath != direct.Meta[vm.MetaSSHKey] {
				t.Fatalf("direct ssh = %+v", mach.SSH)
			}
		case "plain":
			if mach.SSH != nil {
				t.Fatalf("plain ssh = %+v", mach.SSH)
			}
		}
	}
}
