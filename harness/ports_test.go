package harness

import (
	"net"
	"strconv"
	"testing"

	"github.com/fl4metf/vm-harness/vm"
)

func TestAddPortForward(t *testing.T) {
	cases := []struct {
		name string
		in   vm.PortForward
		want vm.PortForward
	}{
		{
			name: "defaults",
			in:   vm.PortForward{GuestPort: 80},
			want: vm.PortForward{Name: "tcp-80", Protocol: "tcp", HostIP: "127.0.0.1", GuestPort: 80},
		},
		{
			name: "udp auto port",
			in:   vm.PortForward{Protocol: "udp", GuestPort: 53},
			want: vm.PortForward{Name: "udp-53", Protocol: "udp", HostIP: "127.0.0.1", GuestPort: 53},
		},
		{
			name: "explicit",
			in:   vm.PortForward{Name: "web", Protocol: "tcp", HostIP: "0.0.0.0", HostPort: 18080, GuestIP: "10.0.2.15", GuestPort: 8080},
			want: vm.PortForward{Name: "web", Protocol: "tcp", HostIP: "0.0.0.0", HostPort: 18080, GuestIP: "10.0.2.15", GuestPort: 8080},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.vbox.Put(managedVM("a", vm.StateRunning))
			got, err := e.m.AddPortForward(t.Context(), vboxRef("a"), c.in)
			if err != nil {
				t.Fatal(err)
			}
			if c.want.HostPort == 0 {
				if got.HostPort <= 0 || got.HostPort > 65535 {
					t.Fatalf("host port = %d", got.HostPort)
				}
				c.want.HostPort = got.HostPort
			}
			if got != c.want {
				t.Fatalf("forward = %+v, want %+v", got, c.want)
			}
			stored, ok := forwardNamed(mustGet(t, e.vbox, "a"), c.want.Name)
			if !ok || stored != got {
				t.Fatalf("stored forward = %+v", stored)
			}
		})
	}
}

func TestAddPortForwardPicksAFreePort(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("a", vm.StateRunning))
	got, err := e.m.AddPortForward(t.Context(), vboxRef("a"), vm.PortForward{GuestPort: 22})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(got.HostPort)))
	if err != nil {
		t.Fatalf("picked port %d is not free: %v", got.HostPort, err)
	}
	l.Close()
}

func TestPortPickerSkipsForwardedPorts(t *testing.T) {
	e := newEnv(t)
	busy := managedVM("busy", vm.StateStopped)
	for port := range 64 {
		busy.PortForwards = append(busy.PortForwards, vm.PortForward{Name: "p" + strconv.Itoa(port), HostPort: 50000 + port, GuestPort: 80})
	}
	e.vbox.Put(busy)
	picker, err := e.m.newPortPicker(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for port := 50000; port < 50064; port++ {
		if !picker.used[port] {
			t.Fatalf("forwarded port %d not treated as used", port)
		}
	}
	seen := map[int]bool{}
	for range 20 {
		port, err := picker.pick("tcp", "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		if seen[port] || !picker.used[port] {
			t.Fatalf("port %d picked twice or not reserved", port)
		}
		seen[port] = true
	}
}

func TestAddPortForwardErrors(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		nics     []vm.NIC
		pf       vm.PortForward
		err      error
	}{
		{name: "duplicate name", pf: vm.PortForward{Name: "web", GuestPort: 80}, err: vm.ErrExists},
		{name: "bad protocol", pf: vm.PortForward{Protocol: "sctp", GuestPort: 80}, err: vm.ErrInvalid},
		{name: "guest port", pf: vm.PortForward{GuestPort: 65536}, err: vm.ErrInvalid},
		{name: "host port", pf: vm.PortForward{HostPort: -1, GuestPort: 80}, err: vm.ErrInvalid},
		{name: "host ip", pf: vm.PortForward{HostIP: "300.1.1.1", GuestPort: 80}, err: vm.ErrInvalid},
		{name: "unusable host ip", pf: vm.PortForward{HostIP: "192.0.2.1", GuestPort: 80}, err: vm.ErrInvalid},
		{name: "address of another host", pf: vm.PortForward{Name: "pivot", HostIP: "198.51.100.7", HostPort: 22, GuestPort: 22}, err: vm.ErrInvalid},
		{name: "guest ip", pf: vm.PortForward{GuestIP: "guest", GuestPort: 80}, err: vm.ErrInvalid},
		{name: "first nic bridged", nics: []vm.NIC{{Mode: vm.NetBridged}}, pf: vm.PortForward{GuestPort: 80}, err: vm.ErrInvalid},
		{name: "provider without forwarding", provider: vm.VMware, pf: vm.PortForward{GuestPort: 80}, err: vm.ErrUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			mach := managedVM("a", vm.StateRunning)
			mach.NICs = c.nics
			mach.PortForwards = []vm.PortForward{{Name: "web", Protocol: "tcp", HostPort: 18080, GuestPort: 80}}
			e.vbox.Put(mach)
			e.vmw.Put(mach)
			ref := vboxRef("a")
			if c.provider != "" {
				ref.Provider = c.provider
			}
			_, err := e.m.AddPortForward(t.Context(), ref, c.pf)
			wantErr(t, err, c.err)
			if calls := append(callsOf(e.vbox, "addpf"), callsOf(e.vmw, "addpf")...); len(calls) > 0 {
				t.Fatalf("provider called: %v", calls)
			}
		})
	}
}

func TestRemovePortForward(t *testing.T) {
	e := newEnv(t)
	mach := managedVM("a", vm.StateRunning)
	mach.PortForwards = []vm.PortForward{{Name: "web", Protocol: "tcp", HostPort: 18080, GuestPort: 80}}
	e.vbox.Put(mach)
	if err := e.m.RemovePortForward(t.Context(), vboxRef("a"), "web"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, e.vbox, "a"); len(got.PortForwards) != 0 {
		t.Fatalf("forwards = %+v", got.PortForwards)
	}
	wantErr(t, e.m.RemovePortForward(t.Context(), vboxRef("a"), "web"), vm.ErrNotFound)
	wantErr(t, e.m.RemovePortForward(t.Context(), vboxRef("a"), ""), vm.ErrInvalid)
}

func TestAddPortForwardOnALocalInterface(t *testing.T) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip(err)
	}
	var local string
	for _, addr := range addrs {
		if n, ok := addr.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			local = n.IP.String()
			break
		}
	}
	if local == "" {
		t.Skip("no local ipv4 interface address")
	}
	e := newEnv(t)
	e.vbox.Put(managedVM("a", vm.StateRunning))
	got, err := e.m.AddPortForward(t.Context(), vboxRef("a"), vm.PortForward{Name: "lan", HostIP: local, HostPort: 18081, GuestPort: 80})
	if err != nil || got.HostIP != local {
		t.Fatalf("forward = %+v, %v", got, err)
	}
}
