package harness

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ZetGames/vm-harness/internal/memprovider"
	"github.com/ZetGames/vm-harness/vm"
)

type gated struct {
	*memprovider.Provider
	entered chan string
	release chan struct{}
}

func newGated() *gated {
	return &gated{
		Provider: memprovider.New(vm.VirtualBox),
		entered:  make(chan string, 4),
		release:  make(chan struct{}),
	}
}

func (g *gated) TakeSnapshot(ctx context.Context, ref, name, description string) error {
	g.entered <- ref + "/" + name
	<-g.release
	return g.Provider.TakeSnapshot(ctx, ref, name, description)
}

func (g *gated) waitEntered(t *testing.T) string {
	t.Helper()
	select {
	case op := <-g.entered:
		return op
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not reach the provider")
		return ""
	}
}

func (g *gated) noneEntered(t *testing.T) {
	t.Helper()
	select {
	case op := <-g.entered:
		t.Fatalf("%s ran while another operation held the vm", op)
	case <-time.After(100 * time.Millisecond):
	}
}

func snapshotAsync(ctx context.Context, m *Manager, vmName, snap string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := m.TakeSnapshot(ctx, Ref{VM: vmName}, snap, "")
		done <- err
	}()
	return done
}

func awaitDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not finish")
	}
}

func TestMutationsOnOneVMDoNotOverlap(t *testing.T) {
	g := newGated()
	g.Put(managedVM("a", vm.StateRunning))
	m := newManager(t, Config{}, g)

	first := snapshotAsync(t.Context(), m, "a", "s1")
	second := snapshotAsync(t.Context(), m, "a", "s2")
	g.waitEntered(t)
	g.noneEntered(t)
	g.release <- struct{}{}
	g.waitEntered(t)
	g.release <- struct{}{}
	awaitDone(t, first)
	awaitDone(t, second)
	if snaps, _ := g.Snapshots(t.Context(), "a"); len(snaps) != 2 {
		t.Fatalf("snapshots = %+v", snaps)
	}
}

func TestDifferentOperationsOnOneVMDoNotOverlap(t *testing.T) {
	g := newGated()
	g.Put(managedVM("a", vm.StateStopped))
	m := newManager(t, Config{}, g)

	snap := snapshotAsync(t.Context(), m, "a", "s1")
	g.waitEntered(t)
	started := make(chan error, 1)
	go func() {
		_, err := m.Start(t.Context(), Ref{VM: "a"}, false)
		started <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if calls := callsOf(g.Provider, "start"); len(calls) > 0 {
		t.Fatalf("start ran during the snapshot: %v", calls)
	}
	g.release <- struct{}{}
	awaitDone(t, snap)
	awaitDone(t, started)
	if state := mustGet(t, g, "a").State; state != vm.StateRunning {
		t.Fatalf("state = %s", state)
	}
}

func TestMutationsOnDifferentVMsRunConcurrently(t *testing.T) {
	g := newGated()
	g.Put(managedVM("a", vm.StateRunning))
	g.Put(managedVM("b", vm.StateRunning))
	m := newManager(t, Config{}, g)

	first := snapshotAsync(t.Context(), m, "a", "s1")
	second := snapshotAsync(t.Context(), m, "b", "s1")
	g.waitEntered(t)
	g.waitEntered(t)
	close(g.release)
	awaitDone(t, first)
	awaitDone(t, second)
}

func TestLockWaitHonoursContext(t *testing.T) {
	g := newGated()
	g.Put(managedVM("a", vm.StateStopped))
	m := newManager(t, Config{}, g)

	snap := snapshotAsync(t.Context(), m, "a", "s1")
	g.waitEntered(t)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := m.Start(ctx, Ref{VM: "a"}, false)
	wantErr(t, err, context.DeadlineExceeded)
	close(g.release)
	awaitDone(t, snap)
	if calls := callsOf(g.Provider, "start"); len(calls) > 0 {
		t.Fatalf("start ran after its context expired: %v", calls)
	}
}

type slowCreate struct {
	*memprovider.Provider
	entered chan string
	release chan struct{}
}

func (s slowCreate) Create(ctx context.Context, spec vm.Spec) (vm.Machine, error) {
	s.entered <- spec.Name
	<-s.release
	return s.Provider.Create(ctx, spec)
}

func TestVMLimitHoldsAcrossManagers(t *testing.T) {
	p := slowCreate{memprovider.New(vm.VirtualBox), make(chan string, 2), make(chan struct{})}
	root := t.TempDir()
	results := make(chan error, 2)
	for _, name := range []string{"first", "second"} {
		m := New(Config{Root: root, Limits: Limits{MaxVMs: 1}}, p)
		go func() {
			_, err := m.Create(context.Background(), vm.Spec{Name: name})
			results <- err
		}()
	}
	<-p.entered
	select {
	case name := <-p.entered:
		t.Errorf("%s was created while another manager was creating", name)
	case <-time.After(200 * time.Millisecond):
	}
	close(p.release)
	var limited, created int
	for range 2 {
		select {
		case err := <-results:
			switch {
			case err == nil:
				created++
			case errors.Is(err, vm.ErrLimit):
				limited++
			default:
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("create did not finish")
		}
	}
	if created != 1 || limited != 1 {
		t.Fatalf("created %d, limited %d", created, limited)
	}
}

func TestMutationsAcrossManagersDoNotOverlap(t *testing.T) {
	g := newGated()
	g.Put(managedVM("a", vm.StateRunning))
	root := t.TempDir()
	first := snapshotAsync(t.Context(), New(Config{Root: root}, g), "a", "s1")
	g.waitEntered(t)
	second := snapshotAsync(t.Context(), New(Config{Root: root}, g), "a", "s2")
	g.noneEntered(t)
	g.release <- struct{}{}
	g.waitEntered(t)
	g.release <- struct{}{}
	awaitDone(t, first)
	awaitDone(t, second)
}

type caseInsensitive struct {
	*gated
}

func (c caseInsensitive) Get(ctx context.Context, ref string) (vm.Machine, error) {
	mach, err := c.gated.Get(ctx, strings.ToLower(ref))
	mach.ID = ref
	return mach, err
}

func (c caseInsensitive) Snapshots(ctx context.Context, ref string) ([]vm.Snapshot, error) {
	return c.gated.Snapshots(ctx, strings.ToLower(ref))
}

func (c caseInsensitive) TakeSnapshot(ctx context.Context, ref, name, description string) error {
	return c.gated.TakeSnapshot(ctx, strings.ToLower(ref), name, description)
}

func TestLockKeyIgnoresHowTheIDIsSpelled(t *testing.T) {
	g := newGated()
	g.Put(vm.Machine{ID: "c:/vms/web/web.vmx", Name: "web", State: vm.StateRunning, Managed: true})
	m := newManager(t, Config{}, caseInsensitive{g})
	first := snapshotAsync(t.Context(), m, "C:/VMs/Web/Web.vmx", "s1")
	g.waitEntered(t)
	second := snapshotAsync(t.Context(), m, "c:/vms/web/WEB.vmx", "s2")
	g.noneEntered(t)
	g.release <- struct{}{}
	g.waitEntered(t)
	g.release <- struct{}{}
	awaitDone(t, first)
	awaitDone(t, second)
}
