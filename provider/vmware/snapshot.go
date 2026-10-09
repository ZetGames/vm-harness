package vmware

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"

	"github.com/ZetGames/vm-harness/vm"
)

func (p *Provider) Snapshots(ctx context.Context, ref string) ([]vm.Snapshot, error) {
	path, err := p.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	out, err := p.vmrunCmd(ctx, "listSnapshots", path, "showTree")
	if err != nil {
		return nil, err
	}
	snapshots := parseSnapshotTree(out)
	d, err := readVMSD(path)
	if err != nil {
		return nil, err
	}
	for i := range snapshots {
		if s, ok := d.byName(snapshots[i].Name); ok {
			snapshots[i].ID = s.uid
			snapshots[i].Description = s.description
			snapshots[i].Current = s.uid == d.current
		}
	}
	return snapshots, nil
}

func parseSnapshotTree(out string) []vm.Snapshot {
	var snapshots []vm.Snapshot
	var branch []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "Total snapshots:") {
			continue
		}
		name := strings.TrimLeft(line, "\t")
		depth := min(len(line)-len(name), len(branch))
		branch = append(branch[:depth], name)
		s := vm.Snapshot{Name: name}
		if depth > 0 {
			s.Parent = branch[depth-1]
		}
		snapshots = append(snapshots, s)
	}
	return snapshots
}

func (p *Provider) TakeSnapshot(ctx context.Context, ref, name, description string) error {
	if err := checkSnapshotName(name); err != nil {
		return err
	}
	path, err := p.resolve(ctx, ref)
	if err != nil {
		return err
	}
	if _, err := p.vmrunCmd(ctx, "snapshot", path, name); err != nil {
		return err
	}
	if description == "" {
		return nil
	}
	d, err := readVMSD(path)
	if err != nil {
		return err
	}
	s, ok := d.byName(name)
	if !ok {
		return fmt.Errorf("snapshot %q is missing from %s", name, d.path)
	}
	d.file.set(s.prefix+"description", description)
	return d.file.write(d.path)
}

func (p *Provider) RestoreSnapshot(ctx context.Context, ref, name string) error {
	return p.power(ctx, ref, false, "revertToSnapshot", name)
}

func (p *Provider) DeleteSnapshot(ctx context.Context, ref, name string) error {
	if err := checkSnapshotName(name); err != nil {
		return err
	}
	path, err := p.resolve(ctx, ref)
	if err != nil {
		return err
	}
	d, err := readVMSD(path)
	if err != nil {
		return err
	}
	target, err := d.find(name)
	if err != nil {
		return err
	}
	if clones := d.clonesBelow(target); len(clones) > 0 {
		return fmt.Errorf("snapshot %q is the base of linked clones %s: %w", target.name, strings.Join(clones, ", "), vm.ErrInvalidState)
	}
	_, err = p.vmrunCmd(ctx, "deleteSnapshot", path, target.name)
	return err
}

func checkSnapshotName(name string) error {
	if name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("snapshot name %q: %w", name, vm.ErrInvalid)
	}
	return nil
}

var snapshotUIDKey = regexp.MustCompile(`(?i)^(snapshot\d+\.)uid$`)

type vmsd struct {
	path      string
	file      *vmxFile
	current   string
	snapshots []vmsdSnapshot
}

type vmsdSnapshot struct {
	prefix      string
	uid         string
	parent      string
	name        string
	description string
	poweredOn   bool
	clones      []string
}

func readVMSD(vmxPath string) (*vmsd, error) {
	path := basePath(vmxPath) + ".vmsd"
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &vmsd{path: path, file: &vmxFile{}}, nil
	}
	if err != nil {
		return nil, err
	}
	f := parseVMX(data)
	d := &vmsd{path: path, file: f, current: f.get("snapshot.current")}
	for _, line := range f.lines {
		m := snapshotUIDKey.FindStringSubmatch(line.key)
		if m == nil {
			continue
		}
		prefix := m[1]
		s := vmsdSnapshot{
			prefix:      prefix,
			uid:         line.value,
			parent:      f.get(prefix + "parent"),
			name:        f.get(prefix + "displayName"),
			description: f.get(prefix + "description"),
			poweredOn:   atoi(f.get(prefix+"type"), 0) != 0,
		}
		for i := range atoi(f.get(prefix+"numClones"), 0) {
			if clone := f.get(fmt.Sprintf("%sclone%d", prefix, i)); clone != "" {
				s.clones = append(s.clones, clone)
			}
		}
		d.snapshots = append(d.snapshots, s)
	}
	return d, nil
}

func currentSnapshot(vmxPath string) string {
	d, err := readVMSD(vmxPath)
	if err != nil {
		return ""
	}
	if s, ok := d.byUID(d.current); ok {
		return s.name
	}
	return ""
}

func (d *vmsd) byName(name string) (vmsdSnapshot, bool) {
	for _, s := range d.snapshots {
		if s.name == name {
			return s, true
		}
	}
	return vmsdSnapshot{}, false
}

func (d *vmsd) find(name string) (vmsdSnapshot, error) {
	var found []vmsdSnapshot
	for _, s := range d.snapshots {
		if strings.EqualFold(s.name, name) {
			found = append(found, s)
		}
	}
	switch len(found) {
	case 0:
		return vmsdSnapshot{}, fmt.Errorf("snapshot %q: %w", name, vm.ErrNotFound)
	case 1:
		return found[0], nil
	}
	return vmsdSnapshot{}, fmt.Errorf("snapshot name %q matches %d snapshots: %w", name, len(found), vm.ErrInvalid)
}

func (d *vmsd) byUID(uid string) (vmsdSnapshot, bool) {
	for _, s := range d.snapshots {
		if uid != "" && s.uid == uid {
			return s, true
		}
	}
	return vmsdSnapshot{}, false
}

func (d *vmsd) liveClones() []string {
	var clones []string
	for _, s := range d.snapshots {
		clones = append(clones, existingFiles(s.clones)...)
	}
	return clones
}

func (d *vmsd) clonesBelow(target vmsdSnapshot) []string {
	var clones []string
	for _, s := range d.snapshots {
		if d.descendsFrom(s, target.uid) {
			clones = append(clones, existingFiles(s.clones)...)
		}
	}
	return clones
}

func (d *vmsd) descendsFrom(s vmsdSnapshot, uid string) bool {
	for range len(d.snapshots) {
		if s.uid == uid {
			return true
		}
		parent, ok := d.byUID(s.parent)
		if !ok {
			return false
		}
		s = parent
	}
	return false
}

func existingFiles(paths []string) []string {
	var found []string
	for _, path := range paths {
		if isFile(path) {
			found = append(found, path)
		}
	}
	return found
}
