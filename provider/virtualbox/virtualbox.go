package virtualbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fl4metf/vm-harness/runner"
	"github.com/fl4metf/vm-harness/vm"
)

type Options struct {
	VBoxManage string
	Root       string
	Runner     runner.Runner
}

type Provider struct {
	bin       string
	root      string
	runner    runner.Runner
	retryWait time.Duration

	osTypesMu sync.Mutex
	osTypes   map[string]string
}

var _ vm.Provider = (*Provider)(nil)

const maxAttempts = 10

var errUnavailable = fmt.Errorf("VBoxManage not found: %w", vm.ErrUnavailable)

var installPaths = []string{
	`C:\Program Files\Oracle\VirtualBox\VBoxManage.exe`,
	"/usr/bin/VBoxManage",
	"/usr/local/bin/VBoxManage",
	"/Applications/VirtualBox.app/Contents/MacOS/VBoxManage",
}

var features = []string{
	vm.FeatureGuestExec, vm.FeatureGuestCopy, vm.FeatureScreenshot, vm.FeaturePortForward,
	vm.FeatureLinkedClone, vm.FeatureSnapshots, vm.FeatureAppliance, vm.FeatureDiskImage,
	vm.FeatureCloudInit, vm.FeatureUnattended, vm.FeatureSharedFolders,
}

func New(opts Options) *Provider {
	p := &Provider{bin: opts.VBoxManage, root: opts.Root, runner: opts.Runner, retryWait: 300 * time.Millisecond}
	if p.bin == "" {
		p.bin = discover()
	}
	if p.runner == nil {
		p.runner = runner.Exec{}
	}
	if p.root != "" {
		if abs, err := filepath.Abs(p.root); err == nil {
			p.root = abs
		}
	}
	return p
}

func discover() string {
	if path, err := exec.LookPath("VBoxManage"); err == nil {
		return path
	}
	exe := "VBoxManage"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	var candidates []string
	for _, env := range []string{"VBOX_MSI_INSTALL_PATH", "VBOX_INSTALL_PATH"} {
		for _, dir := range filepath.SplitList(os.Getenv(env)) {
			if dir != "" {
				candidates = append(candidates, filepath.Join(dir, exe))
			}
		}
	}
	for _, path := range append(candidates, installPaths...) {
		if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
			return path
		}
	}
	return ""
}

func (p *Provider) Name() string { return vm.VirtualBox }

func (p *Provider) Info(ctx context.Context) (vm.HostInfo, error) {
	info := vm.HostInfo{Provider: vm.VirtualBox, Binary: p.bin, Root: p.root, Features: slices.Clone(features)}
	out, err := p.run(ctx, "--version")
	if err != nil {
		return info, err
	}
	if ls := lines(out); len(ls) > 0 {
		info.Version = strings.TrimSpace(ls[len(ls)-1])
	}
	major, _, _ := strings.Cut(info.Version, ".")
	if n, err := strconv.Atoi(major); err != nil || n < 7 {
		return info, fmt.Errorf("VirtualBox 7.0 or newer required, found %q: %w", info.Version, vm.ErrUnavailable)
	}
	return info, nil
}

func (p *Provider) call(ctx context.Context, args []string) (runner.Result, error) {
	if p.bin == "" {
		return runner.Result{}, errUnavailable
	}
	res, err := p.runner.Run(ctx, p.bin, args...)
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("%v: %w", err, vm.ErrUnavailable)
	}
	return res, err
}

func (p *Provider) run(ctx context.Context, args ...string) (string, error) {
	for attempt := 1; ; attempt++ {
		res, err := p.call(ctx, args)
		if err != nil {
			return "", err
		}
		if res.ExitCode == 0 {
			return string(res.Stdout), nil
		}
		if attempt == maxAttempts || !transient(string(res.Stderr)) {
			return "", p.commandError(args, res)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(p.retryWait):
		}
	}
}

func (p *Provider) commandError(args []string, res runner.Result) error {
	msg := string(res.Stderr)
	if strings.TrimSpace(msg) == "" {
		msg = string(res.Stdout)
	}
	return &vm.CommandError{
		Path:     p.bin,
		Args:     redact(args),
		ExitCode: res.ExitCode,
		Stdout:   string(res.Stdout),
		Stderr:   string(res.Stderr),
		Kind:     classify(msg),
	}
}

var transientErrors = []string{
	"is already locked for a session (or being unlocked)",
	"is already locked by a session (or being locked or unlocked)",
	"while it is locked",
	"E_ACCESSDENIED",
}

func transient(stderr string) bool {
	if strings.Contains(stderr, "functionality is limited") {
		return false
	}
	return slices.ContainsFunc(transientErrors, func(s string) bool { return strings.Contains(stderr, s) })
}

var errorKinds = []struct {
	text string
	kind error
}{
	{"VBOX_E_OBJECT_NOT_FOUND", vm.ErrNotFound},
	{"Could not find", vm.ErrNotFound},
	{"VERR_FILE_NOT_FOUND", vm.ErrNotFound},
	{"VERR_PATH_NOT_FOUND", vm.ErrNotFound},
	{"already exists", vm.ErrExists},
	{"VERR_ALREADY_EXISTS", vm.ErrExists},
	{"same UUID as an existing", vm.ErrExists},
	{"Guest Additions are not installed or not ready", vm.ErrNotReady},
	{"Could not create guest session", vm.ErrNotReady},
	{"guest execution service is not", vm.ErrNotReady},
	{"VBOX_E_NOT_SUPPORTED", vm.ErrUnsupported},
	{"not able to logon", vm.ErrInvalid},
	{"VERR_AUTHENTICATION_FAILURE", vm.ErrInvalid},
	{"is not currently running", vm.ErrInvalidState},
	{"is not running", vm.ErrInvalidState},
	{"is already locked", vm.ErrInvalidState},
	{"while it is locked", vm.ErrInvalidState},
	{"E_ACCESSDENIED", vm.ErrInvalidState},
	{"VBOX_E_INVALID_VM_STATE", vm.ErrInvalidState},
	{"VBOX_E_INVALID_OBJECT_STATE", vm.ErrInvalidState},
	{"VBOX_E_OBJECT_IN_USE", vm.ErrInvalidState},
	{"Linked clone can only be created from a snapshot", vm.ErrInvalidState},
	{"E_INVALIDARG", vm.ErrInvalid},
	{"Invalid ", vm.ErrInvalid},
	{"Unknown ", vm.ErrInvalid},
	{"Missing ", vm.ErrInvalid},
	{"Incorrect parameters", vm.ErrInvalid},
}

func classify(msg string) error {
	for _, e := range errorKinds {
		if strings.Contains(msg, e.text) {
			return e.kind
		}
	}
	return nil
}

var secretFlags = []string{"--key", "--putenv"}

func redact(args []string) []string {
	out := slices.Clone(args)
	for i, arg := range out {
		for _, flag := range secretFlags {
			switch {
			case arg == flag && i+1 < len(out):
				out[i+1] = "***"
			case strings.HasPrefix(arg, flag+"="):
				out[i] = flag + "=***"
			}
		}
	}
	return out
}

func checkRef(ref string) error {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return fmt.Errorf("bad vm reference %q: %w", ref, vm.ErrInvalid)
	}
	return nil
}
