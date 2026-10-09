package harness

import (
	"cmp"
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/ZetGames/vm-harness/internal/sshexec"
	"github.com/ZetGames/vm-harness/vm"
)

type Ref struct {
	Provider string
	VM       string
}

type ProviderStatus struct {
	Name      string      `json:"name"`
	Available bool        `json:"available"`
	Default   bool        `json:"default,omitempty"`
	Error     string      `json:"error,omitempty"`
	Info      vm.HostInfo `json:"info"`
}

type ListOptions struct {
	Provider    string
	ManagedOnly bool
}

type StopOptions struct {
	Force   bool
	Timeout time.Duration
}

type SSHOptions struct {
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	KeyPath  string `json:"key_path,omitempty"`
}

const (
	TransportAuto  = "auto"
	TransportGuest = "guest"
	TransportSSH   = "ssh"
)

type ExecRequest struct {
	vm.ExecRequest
	Transport string     `json:"transport,omitempty"`
	SSH       SSHOptions `json:"ssh,omitempty"`
}

type CopyRequest struct {
	vm.CopyRequest
	Transport string     `json:"transport,omitempty"`
	SSH       SSHOptions `json:"ssh,omitempty"`
}

type Access struct {
	vm.Credentials
	Transport string     `json:"transport,omitempty"`
	SSH       SSHOptions `json:"ssh,omitempty"`
}

const (
	WaitRunning = "running"
	WaitStopped = "stopped"
	WaitPaused  = "paused"
	WaitSaved   = "saved"
	WaitIP      = "ip"
	WaitSSH     = "ssh"
	WaitGuest   = "guest"
)

type WaitRequest struct {
	For     string
	Timeout time.Duration
	Access  Access
}

type WaitResult struct {
	Machine    vm.Machine `json:"machine"`
	IP         string     `json:"ip,omitempty"`
	ElapsedMS  int64      `json:"elapsed_ms"`
	Recoveries []string   `json:"recoveries,omitempty"`
}

type sshClient struct {
	run      func(ctx context.Context, cfg sshexec.Config, req vm.ExecRequest) (vm.ExecResult, error)
	upload   func(ctx context.Context, cfg sshexec.Config, hostPath, guestPath string) error
	download func(ctx context.Context, cfg sshexec.Config, guestPath, hostPath string) error
	readFile func(ctx context.Context, cfg sshexec.Config, guestPath string, limit int64) ([]byte, error)
	probe    func(ctx context.Context, cfg sshexec.Config) error
}

type Manager struct {
	cfg        Config
	realRoot   string
	providers  []vm.Provider
	log        *slog.Logger
	poll       time.Duration
	bootStall  time.Duration
	bootResets int
	ssh        sshClient
	creating   chan struct{}
	locks      lockTable
}

func New(cfg Config, providers ...vm.Provider) *Manager {
	cfg.Root = resolveRoot(cfg.Root)
	cfg.Defaults = Defaults{
		CPUs:     cmp.Or(cfg.Defaults.CPUs, builtinDefaults.CPUs),
		MemoryMB: cmp.Or(cfg.Defaults.MemoryMB, builtinDefaults.MemoryMB),
		DiskGB:   cmp.Or(cfg.Defaults.DiskGB, builtinDefaults.DiskGB),
		OSType:   cmp.Or(cfg.Defaults.OSType, builtinDefaults.OSType),
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	ordered := slices.Clone(providers)
	slices.SortStableFunc(ordered, func(a, b vm.Provider) int {
		return cmp.Compare(preference(a.Name()), preference(b.Name()))
	})
	return &Manager{
		cfg:        cfg,
		realRoot:   protectedPath(cfg.Root),
		providers:  ordered,
		log:        logger,
		poll:       time.Second,
		bootStall:  bootStallWindow,
		bootResets: maxBootResets,
		ssh: sshClient{
			run:      sshexec.Run,
			upload:   sshexec.Upload,
			download: sshexec.Download,
			readFile: sshexec.ReadFile,
			probe:    sshexec.Probe,
		},
		creating: make(chan struct{}, 1),
		locks:    lockTable{held: make(map[string]chan struct{})},
	}
}

func (m *Manager) Config() Config { return m.cfg }

func (m *Manager) keysDir() string { return filepath.Join(m.cfg.Root, "keys") }

func preference(provider string) int {
	switch provider {
	case vm.VirtualBox:
		return 0
	case vm.VMware:
		return 1
	}
	return 2
}

func (m *Manager) logOp(ctx context.Context, op, provider, name string, elapsed time.Duration, err error) {
	attrs := []slog.Attr{
		slog.String("op", op),
		slog.String("provider", provider),
		slog.String("vm", name),
		slog.Duration("duration", elapsed),
	}
	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelWarn
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	m.log.LogAttrs(ctx, level, "vm operation", attrs...)
}
