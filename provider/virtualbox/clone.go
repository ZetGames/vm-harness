package virtualbox

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ZetGames/vm-harness/cloudinit"
	"github.com/ZetGames/vm-harness/vm"
)

const maxUARTs = 4

func (p *Provider) Clone(ctx context.Context, ref string, opts vm.CloneOptions) (_ vm.Machine, err error) {
	if p.root == "" {
		return vm.Machine{}, errNoRoot
	}
	if err := checkNewName(opts.Name); err != nil {
		return vm.Machine{}, err
	}
	src, err := p.inspect(ctx, ref)
	if err != nil {
		return vm.Machine{}, err
	}
	existing, err := p.listVMs(ctx)
	if err != nil {
		return vm.Machine{}, err
	}
	if slices.ContainsFunc(existing, func(e vmEntry) bool { return e.name == opts.Name }) {
		return vm.Machine{}, fmt.Errorf("vm %s: %w", opts.Name, vm.ErrExists)
	}
	id := newUUID()
	args := []string{"clonevm", src.id(), "--name", opts.Name, "--uuid", id, "--register", "--basefolder", p.root}
	switch {
	case opts.Linked:
		snapshot := opts.Snapshot
		if snapshot == "" {
			snapshot = src.fields["CurrentSnapshotUUID"]
		}
		if snapshot == "" {
			return vm.Machine{}, fmt.Errorf("linked clone of %s needs a snapshot and it has none: %w", src.name(), vm.ErrInvalidState)
		}
		args = append(args, "--options", "link", "--snapshot", snapshot)
	case opts.Snapshot != "":
		args = append(args, "--snapshot", opts.Snapshot)
	}
	dir := filepath.Join(p.root, opts.Name)
	if err := reserve(dir); err != nil {
		return vm.Machine{}, err
	}
	defer func() {
		if err != nil {
			p.discard(ctx, id, dir, "")
		}
	}()
	if _, err = p.run(context.WithoutCancel(ctx), args...); err != nil {
		return vm.Machine{}, err
	}
	if err = p.detachFromSource(ctx, id, src.dir(), dir, opts.Name); err != nil {
		return vm.Machine{}, err
	}
	return p.Get(ctx, id)
}

func (p *Provider) detachFromSource(ctx context.Context, id, srcDir, dir, name string) error {
	clone, err := p.inspect(ctx, id)
	if err != nil {
		return err
	}
	var fixes [][]string
	for _, slot := range dvdSlots(clone.fields) {
		image := clone.fields[slot]
		if filepath.Base(image) != seedName || !samePath(filepath.Dir(image), srcDir) {
			continue
		}
		ctl, port, device, ok := parseSlot(slot)
		if !ok {
			return fmt.Errorf("unexpected storage slot %q", slot)
		}
		seed := filepath.Join(dir, seedName)
		if err := cloudinit.Reseed(image, seed, "vmh-"+name, name); err != nil {
			return err
		}
		fixes = append(fixes, []string{"storageattach", id, "--storagectl", ctl, "--port", port, "--device", device, "--type", "dvddrive", "--medium", seed})
	}
	for n := 1; n <= maxUARTs; n++ {
		file, ok := strings.CutPrefix(clone.fields["uartmode"+strconv.Itoa(n)], "file,")
		if ok && samePath(filepath.Dir(file), srcDir) {
			fixes = append(fixes, []string{"modifyvm", id, "--uartmode" + strconv.Itoa(n), "file", filepath.Join(dir, filepath.Base(file))})
		}
	}
	if len(fixes) > 0 && clone.state() == vm.StateSaved {
		if _, err := p.run(ctx, "discardstate", id); err != nil {
			return err
		}
	}
	for _, args := range fixes {
		if _, err := p.run(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

func parseSlot(slot string) (ctl, port, device string, ok bool) {
	i := strings.LastIndex(slot, "-")
	if i < 0 {
		return "", "", "", false
	}
	j := strings.LastIndex(slot[:i], "-")
	if j < 0 {
		return "", "", "", false
	}
	return slot[:j], slot[j+1 : i], slot[i+1:], true
}
