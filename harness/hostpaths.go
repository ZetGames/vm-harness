package harness

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/ZetGames/vm-harness/provider/vmware"
	"github.com/ZetGames/vm-harness/vm"
)

var foldCase = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

var hypervisorExtensions = []string{
	".vbox", ".vbox-prev", ".vbox-tmp", ".vmx", ".vmxf", ".vmsd", ".vmdk", ".vdi", ".vhd", ".vhdx",
	".nvram", ".vmss", ".vmsn", ".vmem", ".sav", ".lck",
}

func pathKey(path string) string {
	path = filepath.Clean(path)
	if foldCase {
		return strings.ToLower(path)
	}
	return path
}

func within(dir, path string) bool {
	dir, path = pathKey(dir), pathKey(path)
	if path == dir {
		return true
	}
	if !strings.HasSuffix(dir, string(filepath.Separator)) {
		dir += string(filepath.Separator)
	}
	return strings.HasPrefix(path, dir)
}

func canonical(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("host path is empty: %w", vm.ErrInvalid)
	}
	if err := checkPathSyntax(path); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("host path %q: %v: %w", path, err, vm.ErrInvalid)
	}
	if err := checkPathSyntax(abs); err != nil {
		return "", err
	}
	dir, rest := abs, ""
	for {
		real, err := finalPath(dir)
		if err == nil {
			return filepath.Join(real, rest), nil
		}
		if _, lerr := os.Lstat(dir); lerr == nil || !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("host path %s cannot be resolved: %v: %w", path, err, vm.ErrInvalid)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

func protectedPath(path string) string {
	path = stripDevicePrefix(path)
	if real, err := canonical(path); err == nil {
		return real
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

func (m *Manager) requestPath(kind, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%s needs a host path: %w", kind, vm.ErrInvalid)
	}
	real, err := canonical(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", kind, err)
	}
	if files := m.cfg.FilesDir(); within(protectedPath(files), real) {
		if err := os.MkdirAll(files, 0o700); err != nil {
			return "", fmt.Errorf("create %s: %w", files, err)
		}
	}
	return real, nil
}

func (m *Manager) inHostDirs(real string, extra ...string) bool {
	dirs := append(slices.Clone(m.cfg.HostDirs), m.cfg.FilesDir())
	for _, dir := range append(dirs, extra...) {
		if dir != "" && within(protectedPath(dir), real) {
			return true
		}
	}
	return false
}

func (m *Manager) checkHostDirs(kind, real string, extra ...string) error {
	if len(m.cfg.HostDirs) == 0 || m.inHostDirs(real, extra...) {
		return nil
	}
	return fmt.Errorf("%s %s is outside the host directories vmh may use (%s): %w",
		kind, real, strings.Join(append(slices.Clone(m.cfg.HostDirs), m.cfg.FilesDir()), ", "), vm.ErrForbidden)
}

func (m *Manager) hostPath(kind, path string, wantDir bool) (string, error) {
	real, err := m.requestPath(kind, path)
	if err != nil {
		return "", err
	}
	if err := m.checkHostDirs(kind, real); err != nil {
		return "", err
	}
	fi, err := os.Stat(real)
	switch {
	case err != nil:
		return "", fmt.Errorf("%s: %v: %w", kind, err, vm.ErrInvalid)
	case wantDir && !fi.IsDir():
		return "", fmt.Errorf("%s %s is not a directory: %w", kind, real, vm.ErrInvalid)
	case !wantDir && fi.IsDir():
		return "", fmt.Errorf("%s %s is a directory: %w", kind, real, vm.ErrInvalid)
	}
	return real, nil
}

type vmFolder struct {
	name string
	dir  string
}

func (m *Manager) unmanagedFolders(ctx context.Context) ([]vmFolder, error) {
	machines, err := m.allMachines(ctx)
	if err != nil {
		return nil, fmt.Errorf("find the folders of vms vmh does not manage: %w", err)
	}
	var out []vmFolder
	for _, mach := range machines {
		if !mach.Managed && mach.ConfigPath != "" {
			out = append(out, vmFolder{name: mach.Name, dir: protectedPath(filepath.Dir(mach.ConfigPath))})
		}
	}
	return out, nil
}

func (m *Manager) checkHostWrite(ctx context.Context, real string) error {
	name := strings.ToLower(filepath.Base(real))
	if slices.Contains(hypervisorExtensions, filepath.Ext(name)) || strings.HasSuffix(name, vmware.SidecarSuffix) {
		return fmt.Errorf("host path %s is a virtual machine file, vmh does not write to it: %w", real, vm.ErrForbidden)
	}
	if err := m.checkRootWrite(real); err != nil {
		return err
	}
	folders, err := m.unmanagedFolders(ctx)
	if err != nil {
		return err
	}
	for _, f := range folders {
		if within(f.dir, real) {
			return fmt.Errorf("host path %s is inside the folder of vm %q, which vmh does not manage: %w", real, f.name, vm.ErrForbidden)
		}
	}
	return nil
}

func (m *Manager) checkRootWrite(real string) error {
	if within(protectedPath(m.cfg.Root), real) && !within(protectedPath(m.cfg.FilesDir()), real) {
		return fmt.Errorf("host path %s is inside the vmh root %s, where only %s is writable: %w", real, m.cfg.Root, m.cfg.FilesDir(), vm.ErrForbidden)
	}
	return nil
}

func (m *Manager) checkSharedFolders(ctx context.Context, folders []vm.SharedFolder) error {
	writable := slices.DeleteFunc(slices.Clone(folders), func(sf vm.SharedFolder) bool { return sf.ReadOnly })
	if len(writable) == 0 {
		return nil
	}
	root := protectedPath(m.cfg.Root)
	vmFolders, err := m.unmanagedFolders(ctx)
	if err != nil {
		return err
	}
	for _, sf := range writable {
		real, err := canonical(sf.HostPath)
		if err != nil {
			return fmt.Errorf("shared folder %s: %w", sf.Name, err)
		}
		if within(real, root) {
			return fmt.Errorf("shared folder %s: %s contains the vmh root, share it read-only: %w", sf.Name, real, vm.ErrForbidden)
		}
		if err := m.checkRootWrite(real); err != nil {
			return fmt.Errorf("shared folder %s: %w", sf.Name, err)
		}
		for _, f := range vmFolders {
			if within(f.dir, real) || within(real, f.dir) {
				return fmt.Errorf("shared folder %s: %s overlaps the folder of vm %q, which vmh does not manage; share it read-only: %w",
					sf.Name, real, f.name, vm.ErrForbidden)
			}
		}
	}
	return nil
}
