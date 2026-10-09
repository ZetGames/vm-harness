package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ZetGames/vm-harness/harness"
	"github.com/ZetGames/vm-harness/provider/virtualbox"
	"github.com/ZetGames/vm-harness/provider/vmware"
	"github.com/ZetGames/vm-harness/vm"
)

type ownedVMs struct {
	vm.Provider
	names []string
}

func (o ownedVMs) List(ctx context.Context) ([]vm.Machine, error) {
	var out []vm.Machine
	for _, name := range o.names {
		mach, err := o.Get(ctx, name)
		if errors.Is(err, vm.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, mach)
	}
	return out, nil
}

func TestIntegration(t *testing.T) {
	if os.Getenv("VMH_INTEGRATION") != "1" {
		t.Skip("set VMH_INTEGRATION=1 to drive the installed hypervisors")
	}
	root, err := os.MkdirTemp("", "vmh-it-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Logf("leftover files in %s: %v", root, err)
		}
	})
	providers := []vm.Provider{
		virtualbox.New(virtualbox.Options{Root: filepath.Join(root, vm.VirtualBox)}),
		vmware.New(vmware.Options{Root: filepath.Join(root, vm.VMware)}),
	}
	for _, p := range providers {
		t.Run(p.Name(), func(t *testing.T) {
			if _, err := p.Info(context.Background()); err != nil {
				t.Skipf("%s is not usable: %v", p.Name(), err)
			}
			suffix := make([]byte, 4)
			rand.Read(suffix)
			name := "vmh-it-" + hex.EncodeToString(suffix)
			e := newEnv(t)
			useManager(t, func(cfg harness.Config) *harness.Manager {
				return harness.New(cfg, ownedVMs{Provider: p, names: []string{name}})
			})
			lifecycle(t, e, p.Name(), name)
		})
	}
}

func lifecycle(t *testing.T, e *testEnv, provider, name string) {
	vmh := func(args ...string) string {
		t.Helper()
		return e.ok(append([]string{"-p", provider}, args...)...).stdout
	}
	t.Cleanup(func() {
		os.Setenv("VMH_ALLOW_UNMANAGED", "1")
		if r := e.run("-p", provider, "rm", name, "--force"); r.code != 0 && r.code != 3 {
			t.Errorf("cleanup of %s: exit %d: %s", name, r.code, r.stdout)
		}
	})

	created := decode[vm.Machine](t, vmh("create", name, "--cpus", "1", "--memory", "128", "--disk", "1", "--label", "suite=cli"))
	if created.Provider != provider || !created.Managed || created.State != vm.StateStopped || created.Labels["suite"] != "cli" {
		t.Fatalf("created = %+v", created)
	}
	listed := decode[[]vm.Machine](t, vmh("ls", "--managed"))
	if !slices.ContainsFunc(listed, func(m vm.Machine) bool { return m.Name == name }) {
		t.Fatalf("ls --managed lacks %s: %v", name, names(listed))
	}

	if got := decode[vm.Machine](t, vmh("start", name)); got.State != vm.StateRunning {
		t.Fatalf("start: state %s", got.State)
	}
	vmh("wait", name, "--for", "running", "--timeout", "1m")
	if got := decode[vm.Machine](t, vmh("set", name, "--label", "power=on")); got.Labels["power"] != "on" {
		t.Fatalf("label on a running vm: %+v", got.Labels)
	}
	if out := vmh("ls", "-o", "text"); !strings.Contains(out, name) || !strings.Contains(out, "running") {
		t.Fatalf("text ls:\n%s", out)
	}
	if got := decode[vm.Machine](t, vmh("stop", name, "--force")); got.State != vm.StateStopped {
		t.Fatalf("stop: state %s", got.State)
	}

	if snap := decode[vm.Snapshot](t, vmh("snap", "take", name, "base", "-d", "cli test")); snap.Name != "base" {
		t.Fatalf("snapshot = %+v", snap)
	}
	snapshots := decode[[]vm.Snapshot](t, vmh("snap", "ls", name))
	if len(snapshots) != 1 || snapshots[0].Name != "base" {
		t.Fatalf("snapshots = %+v", snapshots)
	}
	vmh("snap", "restore", name, "base")
	vmh("snap", "rm", name, "base")

	got := decode[vm.Machine](t, vmh("set", name, "--cpus", "2", "--label", "step=set"))
	if got.CPUs != 2 || got.Labels["step"] != "set" {
		t.Fatalf("set = %+v", got)
	}

	if got := decode[vm.Machine](t, vmh("release", name)); got.Managed {
		t.Fatalf("release left %s managed", name)
	}
	e.fail(4, "forbidden", "-p", provider, "adopt", name)
	e.fail(4, "forbidden", "-p", provider, "rm", name)
	fakeTerminal(t)
	e.stdin = name + "\n"
	if got := decode[vm.Machine](t, vmh("adopt", name)); !got.Managed {
		t.Fatalf("adopt left %s unmanaged", name)
	}

	vmh("rm", name)
	e.fail(3, "not_found", "-p", provider, "show", name)
}
