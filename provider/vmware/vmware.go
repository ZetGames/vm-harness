package vmware

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/fl4metf/vm-harness/runner"
	"github.com/fl4metf/vm-harness/vm"
)

type Options struct {
	Vmrun        string
	VDiskManager string
	OVFTool      string
	HostType     string
	Root         string
	Runner       runner.Runner
}

type Provider struct {
	vmrun        string
	vdiskmanager string
	ovftool      string
	hostType     string
	root         string
	inventory    string
	leases       []string
	runner       runner.Runner
}

var _ vm.Provider = (*Provider)(nil)

var errNoVmrun = fmt.Errorf("vmrun not found: %w", vm.ErrUnavailable)

func New(opts Options) *Provider {
	p := &Provider{
		vmrun:        opts.Vmrun,
		vdiskmanager: opts.VDiskManager,
		ovftool:      opts.OVFTool,
		hostType:     opts.HostType,
		root:         opts.Root,
		inventory:    inventoryPath(),
		leases:       leaseFiles(runtime.GOOS),
		runner:       opts.Runner,
	}
	if p.vmrun == "" || p.vdiskmanager == "" || p.ovftool == "" {
		dirs := installDirs()
		if p.vmrun == "" {
			p.vmrun = findTool("vmrun", dirs)
		}
		if p.vdiskmanager == "" {
			p.vdiskmanager = findTool("vmware-vdiskmanager", dirs)
		}
		if p.ovftool == "" {
			p.ovftool = findTool("ovftool", dirs)
		}
	}
	if p.hostType == "" {
		p.hostType = "ws"
		if runtime.GOOS == "darwin" {
			p.hostType = "fusion"
		}
	}
	if p.runner == nil {
		p.runner = runner.Exec{}
	}
	if p.root != "" {
		if abs, err := filepath.Abs(p.root); err == nil {
			p.root = canonicalPath(abs)
		}
	}
	return p
}

func (p *Provider) Name() string { return vm.VMware }

var versionPattern = regexp.MustCompile(`vmrun version (\S+(?: build-\d+)?)`)

func (p *Provider) Info(ctx context.Context) (vm.HostInfo, error) {
	info := vm.HostInfo{Provider: vm.VMware, Binary: p.vmrun, Root: p.root, Features: p.features()}
	if p.vmrun == "" {
		return info, errNoVmrun
	}
	res, err := p.invoke(ctx, p.vmrun, nil)
	if err != nil {
		return info, err
	}
	if m := versionPattern.FindStringSubmatch(string(res.Stdout)); m != nil {
		info.Version = m[1]
	}
	return info, nil
}

func (p *Provider) features() []string {
	features := []string{
		vm.FeatureGuestExec, vm.FeatureGuestCopy, vm.FeatureScreenshot,
		vm.FeatureDiskImage, vm.FeatureCloudInit, vm.FeatureSharedFolders,
	}
	if p.hostType != "player" {
		features = append(features, vm.FeatureSnapshots, vm.FeatureLinkedClone)
	}
	if p.ovftool != "" {
		features = append(features, vm.FeatureAppliance)
	}
	return features
}

func (p *Provider) vmrunCmd(ctx context.Context, args ...string) (string, error) {
	return p.runVmrun(ctx, nil, args)
}

func (p *Provider) guestCmd(ctx context.Context, cred vm.Credentials, args ...string) (string, error) {
	return p.runVmrun(ctx, &cred, args)
}

func (p *Provider) runVmrun(ctx context.Context, cred *vm.Credentials, args []string) (string, error) {
	if p.vmrun == "" {
		return "", errNoVmrun
	}
	full := []string{"-T", p.hostType}
	if cred != nil {
		full = append(full, "-gu", cred.User, "-gp", cred.Password)
	}
	res, err := p.invoke(ctx, p.vmrun, append(full, args...))
	if err != nil {
		return "", err
	}
	out := normalize(res.Stdout)
	if res.ExitCode != 0 {
		return out, commandError(p.vmrun, args, res)
	}
	return out, nil
}

func (p *Provider) runTool(ctx context.Context, bin string, args ...string) error {
	res, err := p.invoke(ctx, bin, args)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return commandError(bin, args, res)
	}
	return nil
}

func (p *Provider) invoke(ctx context.Context, bin string, args []string) (runner.Result, error) {
	res, err := p.runner.Run(ctx, bin, args...)
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("%v: %w", err, vm.ErrUnavailable)
	}
	return res, err
}

func commandError(bin string, args []string, res runner.Result) *vm.CommandError {
	stdout, stderr := normalize(res.Stdout), normalize(res.Stderr)
	return &vm.CommandError{
		Path:     bin,
		Args:     args,
		ExitCode: res.ExitCode,
		Stdout:   stdout,
		Stderr:   stderr,
		Kind:     classify(stdout + stderr),
	}
}

func normalize(b []byte) string {
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

var errorKinds = []struct {
	text string
	kind error
}{
	{"cannot be found", vm.ErrNotFound},
	{"does not exist", vm.ErrNotFound},
	{"Invalid snapshot name", vm.ErrNotFound},
	{"A file was not found", vm.ErrNotFound},
	{"already exists", vm.ErrExists},
	{"is not powered on", vm.ErrInvalidState},
	{"should not be powered on", vm.ErrInvalidState},
	{"is paused", vm.ErrInvalidState},
	{"has not changed since the last snapshot", vm.ErrInvalidState},
	{"already in use", vm.ErrInvalidState},
	{"VMware Tools are not running", vm.ErrNotReady},
	{"Unable to get the IP address", vm.ErrNotReady},
	{"Anonymous guest operations are not allowed", vm.ErrInvalid},
	{"Invalid user name or password", vm.ErrInvalid},
	{"Cannot read the virtual machine configuration file", vm.ErrInvalid},
	{"not a virtual disk", vm.ErrInvalid},
	{"does not uniquely identify", vm.ErrInvalid},
	{"A password is required", vm.ErrUnsupported},
	{"is not supported", vm.ErrUnsupported},
}

func classify(msg string) error {
	for _, e := range errorKinds {
		if strings.Contains(msg, e.text) {
			return e.kind
		}
	}
	return nil
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

func pathKey(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower(path)
	}
	return path
}

func samePath(a, b string) bool {
	return pathKey(a) == pathKey(b)
}

func canonicalPath(path string) string {
	path = filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	if dir := filepath.Dir(path); dir != path {
		return filepath.Join(canonicalPath(dir), filepath.Base(path))
	}
	return path
}

func basePath(path string) string {
	return strings.TrimSuffix(path, filepath.Ext(path))
}

func isSafeDirName(name string) bool {
	return name != "" && name != "." && filepath.IsLocal(name) && !strings.ContainsAny(name, `/\:`)
}
