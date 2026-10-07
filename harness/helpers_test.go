package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fl4metf/vm-harness/internal/memprovider"
	"github.com/fl4metf/vm-harness/internal/sshexec"
	"github.com/fl4metf/vm-harness/vm"
)

var vmwareFeatures = []string{
	vm.FeatureGuestExec, vm.FeatureGuestCopy, vm.FeatureScreenshot, vm.FeatureLinkedClone,
	vm.FeatureSnapshots, vm.FeatureDiskImage, vm.FeatureCloudInit, vm.FeatureSharedFolders,
}

type testEnv struct {
	m    *Manager
	vbox *memprovider.Provider
	vmw  *memprovider.Provider
	root string
}

func newEnv(t *testing.T) *testEnv {
	return newEnvWith(t, Config{})
}

func newEnvWith(t *testing.T, cfg Config) *testEnv {
	t.Helper()
	vbox := memprovider.New(vm.VirtualBox)
	vmw := memprovider.New(vm.VMware)
	vmw.Features = vmwareFeatures
	cfg.Root = t.TempDir()
	m := New(cfg, vmw, vbox)
	m.poll = 5 * time.Millisecond
	return &testEnv{m: m, vbox: vbox, vmw: vmw, root: cfg.Root}
}

func newManager(t *testing.T, cfg Config, providers ...vm.Provider) *Manager {
	t.Helper()
	cfg.Root = t.TempDir()
	m := New(cfg, providers...)
	m.poll = 5 * time.Millisecond
	return m
}

func managedVM(name string, state vm.State) vm.Machine {
	return vm.Machine{Name: name, State: state, Managed: true, Meta: map[string]string{}}
}

func foreignVM(name string, state vm.State) vm.Machine {
	return vm.Machine{Name: name, State: state}
}

func vboxRef(name string) Ref { return Ref{Provider: vm.VirtualBox, VM: name} }

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want %v", err, target)
	}
}

func mustGet(t *testing.T, p vm.Provider, ref string) vm.Machine {
	t.Helper()
	mach, err := p.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return mach
}

func callsOf(p *memprovider.Provider, op string) []string {
	var out []string
	for _, c := range p.Calls() {
		if strings.HasPrefix(c, op+" ") {
			out = append(out, c)
		}
	}
	return out
}

func opsOn(id string, ops ...string) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = op + " " + id
	}
	return out
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

type offline struct {
	*memprovider.Provider
	err error
}

func newOffline(name string) offline {
	return offline{memprovider.New(name), fmt.Errorf("%s binary not found: %w", name, vm.ErrUnavailable)}
}

func (o offline) Info(context.Context) (vm.HostInfo, error) { return vm.HostInfo{}, o.err }

type recorder struct {
	*memprovider.Provider
	mu    sync.Mutex
	specs []vm.Spec
	fail  error
}

func (r *recorder) Create(ctx context.Context, spec vm.Spec) (vm.Machine, error) {
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	if r.fail != nil {
		return vm.Machine{}, r.fail
	}
	return r.Provider.Create(ctx, spec)
}

func (r *recorder) last(t *testing.T) vm.Spec {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.specs) == 0 {
		t.Fatal("provider Create was not called")
	}
	return r.specs[len(r.specs)-1]
}

type sshStub struct {
	mu      sync.Mutex
	configs []sshexec.Config
	files   map[string][]byte
	probe   []error
	limits  []int64
	run     func(vm.ExecRequest) (vm.ExecResult, error)
}

func stubSSH(m *Manager) *sshStub {
	s := &sshStub{files: map[string][]byte{}}
	m.ssh = sshClient{
		run: func(_ context.Context, cfg sshexec.Config, req vm.ExecRequest) (vm.ExecResult, error) {
			s.record(cfg)
			if s.run != nil {
				return s.run(req)
			}
			return vm.ExecResult{Stdout: "ssh " + strings.Join(req.Command, " ")}, nil
		},
		upload: func(_ context.Context, cfg sshexec.Config, hostPath, guestPath string) error {
			s.record(cfg)
			data, err := os.ReadFile(hostPath)
			if err != nil {
				return err
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			s.files[guestPath] = data
			return nil
		},
		download: func(ctx context.Context, cfg sshexec.Config, guestPath, hostPath string) error {
			data, err := s.read(cfg, guestPath, 0)
			if err != nil {
				return err
			}
			return os.WriteFile(hostPath, data, 0o600)
		},
		readFile: func(_ context.Context, cfg sshexec.Config, guestPath string, limit int64) ([]byte, error) {
			return s.read(cfg, guestPath, limit)
		},
		probe: func(_ context.Context, cfg sshexec.Config) error {
			s.record(cfg)
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.probe) == 0 {
				return nil
			}
			err := s.probe[0]
			s.probe = s.probe[1:]
			return err
		},
	}
	return s
}

func (s *sshStub) read(cfg sshexec.Config, guestPath string, limit int64) ([]byte, error) {
	s.record(cfg)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limits = append(s.limits, limit)
	data, ok := s.files[guestPath]
	switch {
	case !ok:
		return nil, fmt.Errorf("%s: %w", guestPath, vm.ErrNotFound)
	case limit > 0 && int64(len(data)) > limit:
		return nil, fmt.Errorf("%s is larger than %d bytes: %w", guestPath, limit, vm.ErrLimit)
	}
	return data, nil
}

func (s *sshStub) record(cfg sshexec.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configs = append(s.configs, cfg)
}

func (s *sshStub) lastConfig(t *testing.T) sshexec.Config {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.configs) == 0 {
		t.Fatal("ssh was not used")
	}
	return s.configs[len(s.configs)-1]
}

func (s *sshStub) used() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.configs) > 0
}

func forwardNamed(mach vm.Machine, name string) (vm.PortForward, bool) {
	i := slices.IndexFunc(mach.PortForwards, func(pf vm.PortForward) bool { return pf.Name == name })
	if i < 0 {
		return vm.PortForward{}, false
	}
	return mach.PortForwards[i], true
}

func mustCanonical(t *testing.T, path string) string {
	t.Helper()
	real, err := canonical(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func dirLink(t *testing.T, target, link string) {
	t.Helper()
	var err error
	if runtime.GOOS == "windows" {
		var out []byte
		out, err = exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
		if err != nil {
			err = fmt.Errorf("%v: %s", err, out)
		}
	} else {
		err = os.Symlink(target, link)
	}
	if err != nil {
		t.Skipf("cannot link %s to %s: %v", link, target, err)
	}
}

func copyFrom(m *Manager, ctx context.Context, ref Ref, host string) error {
	return m.CopyFrom(ctx, ref, CopyRequest{CopyRequest: vm.CopyRequest{HostPath: host, GuestPath: "/tmp/x"}})
}
