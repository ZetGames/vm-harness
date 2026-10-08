package vmware

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fl4metf/vm-harness/cloudinit"
	"github.com/fl4metf/vm-harness/vm"
)

func (p *Provider) Clone(ctx context.Context, ref string, opts vm.CloneOptions) (vm.Machine, error) {
	src, err := p.resolve(ctx, ref)
	if err != nil {
		return vm.Machine{}, err
	}
	if p.root == "" {
		return vm.Machine{}, fmt.Errorf("vmware root folder is not configured: %w", vm.ErrInvalid)
	}
	if !isSafeDirName(opts.Name) {
		return vm.Machine{}, fmt.Errorf("vm name %q: %w", opts.Name, vm.ErrInvalid)
	}
	snapshot, err := p.cloneSource(ctx, src, opts)
	if err != nil {
		return vm.Machine{}, err
	}
	dir, err := p.makeDir(opts.Name)
	if err != nil {
		return vm.Machine{}, err
	}
	dst := filepath.Join(dir, opts.Name+".vmx")
	mode := "full"
	if opts.Linked {
		mode = "linked"
	}
	args := []string{"clone", src, dst, mode}
	if snapshot != "" {
		args = append(args, "-snapshot="+snapshot)
	}
	args = append(args, "-cloneName="+opts.Name)
	if _, err := p.vmrunCmd(ctx, args...); err != nil {
		os.RemoveAll(dir)
		return vm.Machine{}, err
	}
	if err := finishClone(src, dst, opts.Name); err != nil {
		os.RemoveAll(dir)
		return vm.Machine{}, err
	}
	return p.Get(ctx, dst)
}

func (p *Provider) cloneSource(ctx context.Context, src string, opts vm.CloneOptions) (string, error) {
	v, err := readVMX(src)
	if err != nil {
		return "", err
	}
	if !p.inRoot(src) && v.get("extendedConfigFile") == "" {
		return "", fmt.Errorf("vm %s has never been opened in vmware and cloning it would rewrite its configuration, open it in vmware once first: %w", src, vm.ErrInvalidState)
	}
	name := opts.Snapshot
	if opts.Linked && name == "" {
		if name = currentSnapshot(src); name == "" {
			return "", fmt.Errorf("linked clone of %s needs a snapshot: %w", src, vm.ErrInvalidState)
		}
	}
	if name == "" {
		running, err := p.running(ctx)
		if err != nil {
			return "", err
		}
		if state := vmState(src, v, running); state == vm.StateRunning || state == vm.StatePaused {
			return "", fmt.Errorf("vm %s is %s and vmware clones only powered-off vms, stop it or clone a snapshot taken while it was off: %w", src, state, vm.ErrInvalidState)
		}
		return "", nil
	}
	d, err := readVMSD(src)
	if err != nil {
		return "", err
	}
	s, err := d.find(name)
	if err != nil {
		return "", err
	}
	if s.poweredOn {
		return "", fmt.Errorf("snapshot %q was taken while the vm was powered on and vmware cannot clone it, take one while the vm is off: %w", s.name, vm.ErrInvalidState)
	}
	return s.name, nil
}

func finishClone(src, dst, name string) error {
	v, err := readVMX(dst)
	if err != nil {
		return err
	}
	srcDir := filepath.Dir(src)
	for _, unit := range cdromUnits(v) {
		file := v.get(unit + ".fileName")
		if !strings.EqualFold(filepath.Base(file), seedFile) {
			continue
		}
		from := file
		if !filepath.IsAbs(from) {
			from = filepath.Join(srcDir, file)
		}
		if !samePath(filepath.Dir(from), srcDir) || !isFile(from) {
			continue
		}
		if err := cloudinit.Reseed(from, filepath.Join(filepath.Dir(dst), seedFile), "vmh-"+name, name); err != nil {
			return err
		}
		v.set(unit+".fileName", seedFile)
	}
	ports := serialFiles(v)
	for _, port := range ports {
		v.set(port+"fileName", filepath.Base(v.get(port+"fileName")))
	}
	if len(ports) > 0 {
		v.set("answer.msg.serial.file.open", "Replace")
	}
	v.set("msg.autoAnswer", "TRUE")
	v.set("displayName", name)
	return v.write(dst)
}

func cdromUnits(v *vmxFile) []string {
	var units []string
	for _, line := range v.lines {
		unit, ok := cutSuffixFold(line.key, ".deviceType")
		if ok && strings.EqualFold(line.value, "cdrom-image") {
			units = append(units, unit)
		}
	}
	return units
}

func cutSuffixFold(s, suffix string) (string, bool) {
	if len(s) < len(suffix) || !strings.EqualFold(s[len(s)-len(suffix):], suffix) {
		return s, false
	}
	return s[:len(s)-len(suffix)], true
}
