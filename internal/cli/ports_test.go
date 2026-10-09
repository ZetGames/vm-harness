package cli

import (
	"testing"

	"github.com/ZetGames/vm-harness/vm"
)

func TestPorts(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)

	if out := e.ok("port", "ls", "web").stdout; out != "[]\n" {
		t.Fatalf("empty port list printed %q", out)
	}
	pf := decode[vm.PortForward](t, e.ok("port", "add", "web", "8080:80").stdout)
	if pf != (vm.PortForward{Name: "tcp-80", Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 8080, GuestPort: 80}) {
		t.Fatalf("add = %+v", pf)
	}
	pf = decode[vm.PortForward](t, e.ok("port", "add", "web", "dns=0:53/udp").stdout)
	if pf.Name != "dns" || pf.Protocol != "udp" || pf.HostPort == 0 {
		t.Fatalf("add with free port = %+v", pf)
	}
	e.fail(8, "already_exists", "port", "add", "web", "dns=5353:53/udp")
	e.fail(9, "invalid_argument", "port", "add", "web", "80")
	e.fail(9, "invalid_argument", "port", "add", "web", "8080:70000")

	forwards := decode[[]vm.PortForward](t, e.ok("port", "ls", "web").stdout)
	if len(forwards) != 2 {
		t.Fatalf("list = %+v", forwards)
	}

	got := decode[removal](t, e.ok("port", "rm", "web", "tcp-80").stdout)
	if got != (removal{VM: "web", PortForward: "tcp-80", Deleted: true}) {
		t.Fatalf("rm = %+v", got)
	}
	e.fail(3, "not_found", "port", "rm", "web", "tcp-80")
	e.ok("port", "rm", "web", "dns")

	if out := e.ok("port", "add", "web", "[::1]:9090:90", "-o", "text").stdout; out != "tcp-90 tcp [::1]:9090 -> 90\n" {
		t.Fatalf("text add printed %q", out)
	}
	out := e.ok("port", "ls", "web", "-o", "text").stdout
	want := "NAME    PROTOCOL  HOST        GUEST\n" +
		"tcp-90  tcp       [::1]:9090  90\n"
	if out != want {
		t.Fatalf("text list:\n%q\nwant:\n%q", out, want)
	}
}
