package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/fl4metf/vm-harness/harness"
	"github.com/fl4metf/vm-harness/internal/memprovider"
	"github.com/fl4metf/vm-harness/vm"
)

var vmwareFeatures = []string{
	vm.FeatureGuestExec, vm.FeatureGuestCopy, vm.FeatureScreenshot, vm.FeatureLinkedClone,
	vm.FeatureSnapshots, vm.FeatureDiskImage, vm.FeatureCloudInit, vm.FeatureSharedFolders,
}

type testEnv struct {
	t     *testing.T
	vbox  *memprovider.Provider
	vmw   *memprovider.Provider
	root  string
	stdin string
}

type result struct {
	stdout string
	stderr string
	code   int
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	for _, name := range []string{"VMH_CONFIG", "VMH_PROVIDER", "VMH_OUTPUT", "VMH_ALLOW_UNMANAGED", "VMH_MAX_VMS", "VMH_TOKEN", "VMH_VBOXMANAGE", "VMH_VMRUN"} {
		t.Setenv(name, "")
	}
	e := &testEnv{
		t:    t,
		vbox: memprovider.New(vm.VirtualBox),
		vmw:  memprovider.New(vm.VMware),
		root: t.TempDir(),
	}
	e.vmw.Features = vmwareFeatures
	t.Setenv("VMH_ROOT", e.root)
	useManager(t, func(cfg harness.Config) *harness.Manager { return harness.New(cfg, e.vbox, e.vmw) })
	return e
}

func useManager(t *testing.T, fn func(harness.Config) *harness.Manager) {
	saved := newManager
	newManager = fn
	t.Cleanup(func() { newManager = saved })
}

func (e *testEnv) run(args ...string) result {
	return e.runContext(context.Background(), args...)
}

func (e *testEnv) runContext(ctx context.Context, args ...string) result {
	var stdout, stderr bytes.Buffer
	code := run(ctx, append([]string{}, args...), strings.NewReader(e.stdin), &stdout, &stderr)
	return result{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

func (e *testEnv) ok(args ...string) result {
	e.t.Helper()
	r := e.run(args...)
	if r.code != 0 {
		e.t.Fatalf("vmh %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r
}

func (e *testEnv) fail(exit int, code string, args ...string) errorBody {
	e.t.Helper()
	r := e.run(args...)
	if r.code != exit {
		e.t.Fatalf("vmh %s: exit %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, exit, r.stdout, r.stderr)
	}
	doc := decode[errorDocument](e.t, r.stdout)
	if doc.Error.Code != code {
		e.t.Fatalf("vmh %s: error code %q, want %q (%s)", strings.Join(args, " "), doc.Error.Code, code, doc.Error.Message)
	}
	if doc.Error.Message == "" {
		e.t.Fatalf("vmh %s: empty error message", strings.Join(args, " "))
	}
	return doc.Error
}

func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(strings.NewReader(s))
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %T from %q: %v", v, s, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("output has more than one json document: %q", s)
	}
	return v
}

func (e *testEnv) put(p *memprovider.Provider, name string, state vm.State, managed bool) {
	p.Put(vm.Machine{Name: name, State: state, CPUs: 2, MemoryMB: 2048, OSType: "Ubuntu_64", Managed: managed})
}

func TestVersion(t *testing.T) {
	e := newEnv(t)
	got := decode[versionInfo](t, e.ok("version").stdout)
	if got.Version != version {
		t.Fatalf("version = %q, want %q", got.Version, version)
	}
	if out := e.ok("version", "-o", "text").stdout; out != "vmh dev\n" {
		t.Fatalf("text version = %q", out)
	}
}

func TestUsageErrors(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)
	cases := [][]string{
		{"bogus"},
		{"start"},
		{"show", "web", "extra"},
		{"ls", "--bogus"},
		{"-o", "yaml", "ls"},
		{"stop", "web", "--timeout", "soon"},
		{"stop", "web", "--timeout", "-5"},
		{"create", "x", "--cpus", "many"},
		{"snap", "bogus"},
		{"port", "ls"},
		{"snap"},
		{"port"},
		{"start", "web", "--allow-unmanaged"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			msg := e.fail(2, "usage", args...)
			if !strings.Contains(msg.Message, "--help") {
				t.Errorf("message %q does not point to --help", msg.Message)
			}
		})
	}
}

func TestHelpExitsZero(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{{"--help"}, {}, {"snap", "--help"}, {"exec", "--help"}, {"rm", "--help"}} {
		r := e.run(args...)
		if r.code != 0 || !strings.Contains(r.stdout, "Usage:") {
			t.Errorf("vmh %v: exit %d, stdout %q", args, r.code, r.stdout)
		}
		if strings.Contains(r.stdout, "allow-unmanaged") || strings.Contains(r.stdout, "vmh adopt") {
			t.Errorf("vmh %v help tells how to lift the unmanaged guard:\n%s", args, r.stdout)
		}
	}
}

func TestHelpDescribesCurrentRules(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		args    []string
		want    []string
		notWant []string
	}{
		{
			args: []string{"create"},
			want: []string{
				"Unless --user-data is given, vmh also generates an SSH key",
				"in addition to any --ssh-key or --password",
				"With --user-data you manage users and keys yourself",
				"holds a single VM", "serial and parallel ports, shared folders and remote display",
				"no VMDK descriptor or differencing disk", "plain ISO 9660 image", "--appliance an .ova file",
				"only <root>/files may be shared writable", "loopback or local address",
			},
			notWant: []string{"Without --ssh-key or --password"},
		},
		{args: []string{"cp"}, want: []string{"never writes hypervisor files", "vmh root other than <root>/files", "host_dirs"}},
		{args: []string{"port", "add"}, want: []string{"must be a loopback address or an address of this host"}},
		{args: []string{"serve"}, want: []string{"self-contained", "plain ISO 9660 images", "appliances .ova files", "Inside <root> only <root>/files is writable"}},
		{args: []string{"mcp"}, want: []string{"exits with status 0 when the client closes stdin"}},
		{args: []string{"wait"}, want: []string{"A rejected guest login is retried", "vmh hard-resets the VM and keeps waiting", "fails the wait at once with not_ready", "VMs that vmh does not manage are never reset; adopted VMs are managed", "paused or busy", "In text mode each reset is printed on stderr; JSON results list them", "another wait would reset the VM again", "start by vmh that succeeded"}},
		{args: []string{"providers"}, want: []string{"info.max_reliable_cpus is the most vCPUs", "Hyper-V"}},
		{args: []string{"create"}, want: []string{"capped at the provider's max_reliable_cpus", "an --appliance gets that many CPUs instead of its own", "virtio unless a -f spec sets its model"}},
		{args: []string{"ip"}, want: []string{"the address VMware's DHCP server leased", "since the VM was powered on, if that lease has not ended", "a reset or a reboot inside the guest is not a new power-on", "--wait resets a stuck boot"}},
	}
	for _, c := range cases {
		help := strings.Join(strings.Fields(e.ok(append(c.args, "--help")...).stdout), " ")
		for _, phrase := range c.want {
			if !strings.Contains(help, phrase) {
				t.Errorf("vmh %s --help does not say %q", strings.Join(c.args, " "), phrase)
			}
		}
		for _, phrase := range c.notWant {
			if strings.Contains(help, phrase) {
				t.Errorf("vmh %s --help still says %q", strings.Join(c.args, " "), phrase)
			}
		}
	}
}

func TestMissingSubcommand(t *testing.T) {
	e := newEnv(t)
	msg := e.fail(2, "usage", "snap")
	if !strings.Contains(msg.Message, "ls, take, restore, rm") {
		t.Errorf("message %q does not list the subcommands", msg.Message)
	}
	r := e.run("-o", "text", "port")
	if r.code != 2 || r.stdout != "" || !strings.Contains(r.stderr, "missing subcommand") {
		t.Errorf("text port: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestExitCodesDocumented(t *testing.T) {
	help := strings.Join(strings.Fields(rootHelp), " ")
	errs := []error{
		vm.ErrNotFound, vm.ErrExists, vm.ErrInvalidState, vm.ErrInvalid, vm.ErrUnsupported, vm.ErrNotReady,
		vm.ErrUnavailable, vm.ErrForbidden, vm.ErrLimit, context.DeadlineExceeded, context.Canceled, io.ErrUnexpectedEOF,
	}
	seen := map[int]string{exitCode(codeUsage): codeUsage}
	if !strings.Contains(help, "2 usage") {
		t.Error("root help does not document the usage exit code")
	}
	for _, err := range errs {
		code := vm.Code(err)
		n := exitCode(code)
		if n == 1 && code != vm.CodeInternal {
			t.Errorf("code %s has no exit code of its own", code)
		}
		if other, ok := seen[n]; ok {
			t.Errorf("codes %s and %s share exit code %d", other, code, n)
		}
		seen[n] = code
		if doc := fmt.Sprintf("%d %s", n, code); !strings.Contains(help, doc) {
			t.Errorf("root help does not document %q", doc)
		}
	}
}

func TestOutputFormatSelection(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateStopped, true)

	t.Setenv("VMH_OUTPUT", "text")
	if out := e.ok("ls").stdout; !strings.HasPrefix(out, "NAME") {
		t.Fatalf("VMH_OUTPUT=text printed %q", out)
	}
	if out := e.ok("ls", "-o", "json").stdout; !strings.HasPrefix(out, "[") {
		t.Fatalf("-o json did not override VMH_OUTPUT: %q", out)
	}

	t.Setenv("VMH_OUTPUT", "xml")
	r := e.run("ls")
	if r.code != 2 {
		t.Fatalf("VMH_OUTPUT=xml: exit %d", r.code)
	}
}

func TestErrorsInTextMode(t *testing.T) {
	e := newEnv(t)
	r := e.run("-o", "text", "show", "missing")
	if r.code != 3 {
		t.Fatalf("exit %d, want 3", r.code)
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q, want nothing", r.stdout)
	}
	if want := "vmh: vm \"missing\": not found\n"; r.stderr != want {
		t.Errorf("stderr = %q, want %q", r.stderr, want)
	}
}

func TestExitCodes(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "web", vm.StateRunning, true)
	e.put(e.vbox, "foreign", vm.StateStopped, false)
	e.put(e.vmw, "box", vm.StateStopped, true)

	e.fail(3, "not_found", "show", "missing")
	e.fail(4, "forbidden", "start", "foreign")
	e.fail(5, "invalid_state", "resume", "web")
	e.fail(6, "unsupported", "port", "add", "box", "8080:80")
	e.fail(7, "timeout", "wait", "web", "--for", "stopped", "--timeout", "50ms")
	e.fail(8, "already_exists", "create", "web")
	e.fail(9, "invalid_argument", "create", "bad name")
	e.fail(10, "not_ready", "ip", "web")
	e.fail(1, "internal", "screenshot", "web", t.TempDir()+"/missing/dir/shot.png")

	t.Setenv("VMH_MAX_VMS", "2")
	e.fail(12, "limit_exceeded", "create", "fourth")
}

type unavailableProvider struct {
	*memprovider.Provider
}

func (p unavailableProvider) Info(context.Context) (vm.HostInfo, error) {
	return vm.HostInfo{}, fmt.Errorf("VBoxManage not found: %w", vm.ErrUnavailable)
}

func TestNoHypervisor(t *testing.T) {
	e := newEnv(t)
	useManager(t, func(cfg harness.Config) *harness.Manager {
		return harness.New(cfg, unavailableProvider{memprovider.New(vm.VirtualBox)})
	})
	e.fail(11, "unavailable", "ls")

	providers := decode[[]harness.ProviderStatus](t, e.ok("providers").stdout)
	if len(providers) != 1 || providers[0].Available || !strings.Contains(providers[0].Error, "not found") {
		t.Fatalf("providers = %+v", providers)
	}
}

func TestProviders(t *testing.T) {
	e := newEnv(t)
	providers := decode[[]harness.ProviderStatus](t, e.ok("providers").stdout)
	if len(providers) != 2 {
		t.Fatalf("got %d providers", len(providers))
	}
	if providers[0].Name != vm.VirtualBox || !providers[0].Default || !providers[0].Available {
		t.Errorf("first provider = %+v", providers[0])
	}
	if providers[1].Name != vm.VMware || providers[1].Default {
		t.Errorf("second provider = %+v", providers[1])
	}

	providers = decode[[]harness.ProviderStatus](t, e.ok("providers", "-p", "vmware").stdout)
	if providers[0].Default || !providers[1].Default {
		t.Errorf("--provider vmware did not become the default: %+v", providers)
	}

	out := e.ok("providers", "-o", "text").stdout
	want := "NAME        AVAILABLE  DEFAULT  VERSION  DETAIL\n" +
		"virtualbox  yes        yes      mem      \n" +
		"vmware      yes                 mem      \n"
	if out != want {
		t.Errorf("text providers:\n%s\nwant:\n%s", out, want)
	}
}

func TestAmbiguousName(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "dup", vm.StateStopped, true)
	e.put(e.vmw, "dup", vm.StateStopped, true)

	msg := e.fail(9, "invalid_argument", "show", "dup")
	if !strings.Contains(msg.Message, "provider") {
		t.Errorf("message %q does not suggest a provider", msg.Message)
	}
	got := decode[vm.Machine](t, e.ok("show", "dup", "-p", "vmware").stdout)
	if got.Provider != vm.VMware {
		t.Errorf("provider = %q", got.Provider)
	}
	e.fail(9, "invalid_argument", "show", "dup", "-p", "hyperv")
}

func TestUnmanagedGuard(t *testing.T) {
	e := newEnv(t)
	e.put(e.vbox, "legacy", vm.StateStopped, false)

	msg := e.fail(4, "forbidden", "start", "legacy")
	if strings.Contains(msg.Message, "vmh adopt") {
		t.Errorf("message %q hands out the command that lifts the guard", msg.Message)
	}
	e.fail(4, "forbidden", "rm", "legacy")
	e.fail(2, "usage", "start", "legacy", "--allow-unmanaged")
	if got := decode[vm.Machine](t, e.ok("show", "legacy").stdout); got.State != vm.StateStopped {
		t.Fatalf("state = %s", got.State)
	}

	t.Setenv("VMH_ALLOW_UNMANAGED", "1")
	e.ok("start", "legacy")
}
