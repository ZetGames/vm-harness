package cli

import (
	"bytes"
	"context"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZetGames/vm-harness/vm"
)

func names(machines []vm.Machine) []string {
	out := make([]string, len(machines))
	for i, m := range machines {
		out[i] = m.Provider + "/" + m.Name
	}
	return out
}

func TestList(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)
	e.put(e.vbox, "legacy", vm.StateStopped, false)
	e.put(e.vmw, "box", vm.StateStopped, true)

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"ls"}, "virtualbox/legacy virtualbox/web vmware/box"},
		{[]string{"list", "--managed"}, "virtualbox/web vmware/box"},
		{[]string{"ls", "-p", "vmware"}, "vmware/box"},
	}
	for _, c := range cases {
		got := strings.Join(names(decode[[]vm.Machine](t, e.ok(c.args...).stdout)), " ")
		if got != c.want {
			t.Errorf("vmh %v = %s, want %s", c.args, got, c.want)
		}
	}
}

func TestListEmpty(t *testing.T) {
	e := newEnv(t)
	if out := e.ok("ls").stdout; out != "[]\n" {
		t.Fatalf("empty list printed %q", out)
	}
}

func TestListText(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)
	e.put(e.vmw, "build-box", vm.StateStopped, false)
	e.ok("set", "web", "--label", "team=red", "--label", "env=ci")

	out := e.ok("ls", "-o", "text").stdout
	want := "" +
		"NAME       PROVIDER    STATE    MANAGED  CPUS  MEMORY   OS         LABELS\n" +
		"web        virtualbox  running  yes      2     2048 MB  Ubuntu_64  env=ci,team=red\n" +
		"build-box  vmware      stopped  no       2     2048 MB  Ubuntu_64  \n"
	if out != want {
		t.Fatalf("text table:\n%s\nwant:\n%s", out, want)
	}
}

func TestShow(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(vm.Machine{
		Name:         "web",
		State:        vm.StateRunning,
		CPUs:         2,
		MemoryMB:     1024,
		Managed:      true,
		Meta:         map[string]string{vm.LabelKey("team"): "red"},
		NICs:         []vm.NIC{{Mode: vm.NetNAT, Model: "virtio"}},
		PortForwards: []vm.PortForward{{Name: "vmh-ssh", Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 2222, GuestPort: 22}},
		ConsoleLog:   "/vms/web/serial.log",
	})

	got := decode[vm.Machine](t, e.ok("show", "web").stdout)
	if got.Name != "web" || got.ID != "virtualbox-1" || !got.Managed || got.Labels["team"] != "red" || len(got.PortForwards) != 1 || got.ConsoleLog != "/vms/web/serial.log" {
		t.Fatalf("show = %+v", got)
	}

	out := e.ok("show", "web", "-o", "text").stdout
	for _, line := range []string{
		"name:      web\n",
		"state:     running\n",
		"managed:   yes\n",
		"memory:    1024 MB\n",
		"labels:    team=red\n",
		"nic1:      nat virtio\n",
		"forward:   vmh-ssh tcp 127.0.0.1:2222 -> 22\n",
		"console:   /vms/web/serial.log\n",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("text show lacks %q:\n%s", line, out)
		}
	}
}

func TestSet(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateStopped, true)

	got := decode[vm.Machine](t, e.ok("set", "web", "--cpus", "4", "--memory", "8192", "--label", "a=1", "--label", "b=x=y").stdout)
	if got.CPUs != 4 || got.MemoryMB != 8192 {
		t.Errorf("hardware = %d cpus %d MB", got.CPUs, got.MemoryMB)
	}
	if want := map[string]string{"a": "1", "b": "x=y"}; !maps.Equal(got.Labels, want) {
		t.Errorf("labels = %v, want %v", got.Labels, want)
	}

	got = decode[vm.Machine](t, e.ok("set", "web", "--label", "a-").stdout)
	if want := map[string]string{"b": "x=y"}; !maps.Equal(got.Labels, want) {
		t.Errorf("labels after removal = %v, want %v", got.Labels, want)
	}

	e.fail(9, "invalid_argument", "set", "web")
	e.fail(9, "invalid_argument", "set", "web", "--label", "novalue")
	e.ok("start", "web")
	e.fail(5, "invalid_state", "set", "web", "--cpus", "1")
	e.ok("set", "web", "--label", "c=3")
}

func TestRemove(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)

	e.fail(5, "invalid_state", "rm", "web")
	got := decode[removal](t, e.ok("rm", "web", "--force").stdout)
	if got != (removal{VM: "web", Deleted: true}) {
		t.Fatalf("rm printed %+v", got)
	}
	e.fail(3, "not_found", "show", "web")

	e.put(e.vbox, "old", vm.StateStopped, true)
	if r := e.ok("delete", "old", "-o", "text"); r.stdout != "" || r.stderr != "" {
		t.Fatalf("text rm printed %q / %q", r.stdout, r.stderr)
	}
}

func TestClone(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "base", vm.StateStopped, true)
	e.ok("set", "base", "--label", "role=db")

	got := decode[vm.Machine](t, e.ok("clone", "base", "copy", "--linked").stdout)
	if got.Name != "copy" || !got.Managed || got.Labels["role"] != "db" {
		t.Fatalf("clone = %+v", got)
	}
	snapshots := decode[[]vm.Snapshot](t, e.ok("snap", "ls", "base").stdout)
	if len(snapshots) != 1 || snapshots[0].Name != "vmh-clone-base" {
		t.Fatalf("source snapshots = %+v", snapshots)
	}

	e.ok("clone", "base", "full")
	e.fail(8, "already_exists", "clone", "base", "full")
	e.fail(9, "invalid_argument", "clone", "base", "no/slash")
}

func fakeTerminal(t *testing.T) {
	saved := interactive
	interactive = func(io.Reader, io.Writer) bool { return true }
	t.Cleanup(func() { interactive = saved })
}

func (e *testEnv) wantManaged(name string, want bool) {
	e.t.Helper()
	if got := decode[vm.Machine](e.t, e.ok("show", name).stdout); got.Managed != want {
		e.t.Fatalf("vm %s managed = %v, want %v", name, got.Managed, want)
	}
}

func TestAdoptRelease(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "legacy", vm.StateStopped, false)
	fakeTerminal(t)

	e.stdin = "legacy\n"
	r := e.ok("adopt", "virtualbox-1")
	if !strings.HasSuffix(r.stderr, "type the VM name to adopt it: ") {
		t.Errorf("prompt = %q", r.stderr)
	}
	if got := decode[vm.Machine](t, r.stdout); !got.Managed {
		t.Fatal("adopt did not mark the vm managed")
	}
	e.ok("start", "legacy")
	if got := decode[vm.Machine](t, e.ok("release", "legacy").stdout); got.Managed {
		t.Fatal("release left the vm managed")
	}
	e.fail(4, "forbidden", "stop", "legacy")
}

func TestAdoptNeedsTerminal(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "legacy", vm.StateStopped, false)

	e.stdin = "legacy\n"
	msg := e.fail(4, "forbidden", "adopt", "legacy")
	if !strings.Contains(msg.Message, "interactive terminal") {
		t.Errorf("message = %q", msg.Message)
	}
	e.wantManaged("legacy", false)

	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"adopt", "legacy"}, null, null, &stderr); code != 4 {
		t.Errorf("adopt with %s as stdin and stdout: exit %d, stderr %q", os.DevNull, code, stderr.String())
	}
	if strings.Contains(stderr.String(), "type the VM name") {
		t.Errorf("adopt prompted on %s: %q", os.DevNull, stderr.String())
	}
	e.wantManaged("legacy", false)
}

func TestAdoptWrongName(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "legacy", vm.StateStopped, false)
	fakeTerminal(t)

	for _, typed := range []string{"", "Legacy", "legacy-2", "y"} {
		e.stdin = typed + "\n"
		e.fail(4, "forbidden", "adopt", "legacy")
		e.wantManaged("legacy", false)
	}
}

func TestAdoptPromptCanceled(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "legacy", vm.StateStopped, false)
	fakeTerminal(t)

	stdin, typing := io.Pipe()
	defer typing.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	cancel()
	go func() { done <- run(ctx, []string{"adopt", "legacy"}, stdin, &stdout, &stderr) }()
	select {
	case code := <-done:
		if code != 130 {
			t.Errorf("exit %d, want 130", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("adopt kept waiting for input after cancellation")
	}
	e.wantManaged("legacy", false)
}

func TestIsTerminal(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	file, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, stream := range []any{null, file, &bytes.Buffer{}, strings.NewReader("")} {
		if isTerminal(stream) {
			t.Errorf("isTerminal(%T %v) = true", stream, stream)
		}
	}
}
