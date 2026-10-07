package vmware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/fl4metf/vm-harness/vm"
)

func (p *Provider) Get(ctx context.Context, ref string) (vm.Machine, error) {
	path, err := p.resolve(ctx, ref)
	if err != nil {
		return vm.Machine{}, err
	}
	running, err := p.running(ctx)
	if err != nil {
		return vm.Machine{}, err
	}
	return machineAt(path, running)
}

func (p *Provider) List(ctx context.Context) ([]vm.Machine, error) {
	listed, err := p.runningPaths(ctx)
	if err != nil {
		return nil, err
	}
	running := newRunningSet(listed)
	candidates := p.rootVMs()
	for _, entry := range readInventory(p.inventory) {
		candidates = append(candidates, entry.path)
	}
	candidates = append(candidates, listed...)
	seen := make(map[string]bool)
	var machines []vm.Machine
	for _, path := range candidates {
		path = canonicalPath(path)
		key := pathKey(path)
		if seen[key] {
			continue
		}
		seen[key] = true
		m, err := machineAt(path, running)
		if err != nil {
			continue
		}
		machines = append(machines, m)
	}
	return machines, nil
}

func (p *Provider) resolve(ctx context.Context, ref string) (string, error) {
	if p.vmrun == "" {
		return "", errNoVmrun
	}
	if filepath.IsAbs(ref) && strings.EqualFold(filepath.Ext(ref), ".vmx") {
		return p.resolvePath(ctx, ref)
	}
	if p.root != "" && isSafeDirName(ref) {
		if path := canonicalPath(filepath.Join(p.root, ref, ref+".vmx")); isFile(path) {
			return path, nil
		}
	}
	for _, entry := range readInventory(p.inventory) {
		if entry.name == ref && isFile(entry.path) {
			return canonicalPath(entry.path), nil
		}
	}
	listed, err := p.runningPaths(ctx)
	if err != nil {
		return "", err
	}
	for _, path := range listed {
		if v, err := readVMX(path); err == nil && displayName(path, v) == ref {
			return canonicalPath(path), nil
		}
	}
	return "", notFound(ref)
}

func (p *Provider) resolvePath(ctx context.Context, ref string) (string, error) {
	path := canonicalPath(ref)
	if !isFile(path) {
		return "", notFound(ref)
	}
	if p.inRoot(path) {
		return path, nil
	}
	for _, entry := range readInventory(p.inventory) {
		if samePath(canonicalPath(entry.path), path) {
			return path, nil
		}
	}
	running, err := p.running(ctx)
	if err != nil {
		return "", err
	}
	if running.has(path) {
		return path, nil
	}
	return "", notFound(ref)
}

func notFound(ref string) error {
	return fmt.Errorf("vm %q: %w", ref, vm.ErrNotFound)
}

func (p *Provider) rootVMs() []string {
	if p.root == "" {
		return nil
	}
	dirs, err := os.ReadDir(p.root)
	if err != nil {
		return nil
	}
	var paths []string
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		paths = append(paths, vmxFiles(filepath.Join(p.root, d.Name()))...)
	}
	return paths
}

func vmxFiles(dir string) []string {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, f := range files {
		if f.Type().IsRegular() && strings.EqualFold(filepath.Ext(f.Name()), ".vmx") {
			paths = append(paths, filepath.Join(dir, f.Name()))
		}
	}
	return paths
}

func (p *Provider) inRoot(path string) bool {
	return p.root != "" && samePath(filepath.Dir(filepath.Dir(path)), p.root)
}

type inventoryEntry struct {
	path string
	name string
}

func readInventory(path string) []inventoryEntry {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	f := parseVMX(data)
	var entries []inventoryEntry
	for _, line := range f.lines {
		id, ok := cutSuffixFold(line.key, ".config")
		if !ok || !hasPrefixFold(id, "vmlist") || !filepath.IsAbs(line.value) || !strings.EqualFold(filepath.Ext(line.value), ".vmx") {
			continue
		}
		entries = append(entries, inventoryEntry{path: line.value, name: f.get(id + ".DisplayName")})
	}
	return entries
}

func (p *Provider) runningPaths(ctx context.Context) ([]string, error) {
	out, err := p.vmrunCmd(ctx, "list")
	if err != nil {
		return nil, err
	}
	return parseList(out), nil
}

func (p *Provider) running(ctx context.Context) (runningSet, error) {
	paths, err := p.runningPaths(ctx)
	if err != nil {
		return runningSet{}, err
	}
	return newRunningSet(paths), nil
}

func (p *Provider) isRunning(ctx context.Context, path string) (bool, error) {
	running, err := p.running(ctx)
	return running.has(path), err
}

func parseList(out string) []string {
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Total running VMs:") {
			continue
		}
		paths = append(paths, line)
	}
	return paths
}

type runningSet struct {
	keys  map[string]bool
	files []os.FileInfo
}

func newRunningSet(paths []string) runningSet {
	s := runningSet{keys: make(map[string]bool, len(paths))}
	for _, path := range paths {
		s.keys[pathKey(path)] = true
		if fi, err := os.Stat(path); err == nil {
			s.files = append(s.files, fi)
		}
	}
	return s
}

func (s runningSet) has(path string) bool {
	if s.keys[pathKey(path)] {
		return true
	}
	if len(s.files) == 0 {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && slices.ContainsFunc(s.files, func(other os.FileInfo) bool { return os.SameFile(fi, other) })
}

func machineAt(path string, running runningSet) (vm.Machine, error) {
	v, err := readVMX(path)
	if err != nil {
		return vm.Machine{}, err
	}
	meta, err := readMeta(path)
	if err != nil {
		return vm.Machine{}, err
	}
	m := vm.Machine{
		ID:              path,
		Name:            displayName(path, v),
		Provider:        vm.VMware,
		State:           vmState(path, v, running),
		OSType:          v.get("guestOS"),
		CPUs:            atoi(v.get("numvcpus"), 1),
		MemoryMB:        atoi(v.get("memsize"), 0),
		Firmware:        firmware(v),
		ConfigPath:      path,
		Meta:            meta,
		NICs:            nics(v),
		CurrentSnapshot: currentSnapshot(path),
	}
	vm.ApplyMeta(&m)
	return m, nil
}

func displayName(path string, v *vmxFile) string {
	if name := v.get("displayName"); name != "" {
		return name
	}
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

func vmState(path string, v *vmxFile, running runningSet) vm.State {
	if running.has(path) {
		if isFile(pausedMarker(path)) {
			return vm.StatePaused
		}
		return vm.StateRunning
	}
	if v.get("checkpoint.vmState") != "" {
		return vm.StateSaved
	}
	return vm.StateStopped
}

func suspendFile(path string, v *vmxFile) string {
	name := v.get("checkpoint.vmState")
	if !filepath.IsLocal(name) || !strings.EqualFold(filepath.Ext(name), ".vmss") || isTrue(v.get("checkpoint.vmState.readOnly")) {
		return ""
	}
	return filepath.Join(filepath.Dir(path), name)
}

func discardSuspend(path string) error {
	v, err := readVMX(path)
	if err != nil {
		return err
	}
	if vmss := suspendFile(path, v); vmss != "" {
		for _, file := range []string{vmss, basePath(vmss) + ".vmem"} {
			if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	v.remove("checkpoint.vmState")
	v.remove("checkpoint.vmState.readOnly")
	return v.write(path)
}

func pausedMarker(path string) string {
	return basePath(path) + ".vmh-paused"
}

func setPaused(path string, paused bool) error {
	if paused {
		return os.WriteFile(pausedMarker(path), nil, 0o600)
	}
	if err := os.Remove(pausedMarker(path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func firmware(v *vmxFile) string {
	if strings.EqualFold(v.get("firmware"), "efi") {
		return "efi"
	}
	return "bios"
}

func nics(v *vmxFile) []vm.NIC {
	var nics []vm.NIC
	for i := range maxNICs {
		prefix := fmt.Sprintf("ethernet%d.", i)
		if !isTrue(v.get(prefix + "present")) {
			continue
		}
		nic := vm.NIC{Mode: v.get(prefix + "connectionType"), Model: v.get(prefix + "virtualDev")}
		switch nic.Mode {
		case "":
			nic.Mode = vm.NetBridged
		case "custom":
			nic.Mode = vm.NetCustom
			nic.Adapter = v.get(prefix + "vnet")
		}
		nic.MAC = v.get(prefix + "address")
		if nic.MAC == "" {
			nic.MAC = v.get(prefix + "generatedAddress")
		}
		nics = append(nics, nic)
	}
	return nics
}

func atoi(s string, fallback int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return fallback
}

const SidecarSuffix = ".vmh.json"

func metaPath(path string) string {
	return basePath(path) + SidecarSuffix
}

func readMeta(path string) (map[string]string, error) {
	values := make(map[string]string)
	data, err := os.ReadFile(metaPath(path))
	if errors.Is(err, fs.ErrNotExist) {
		return values, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("vm metadata %s: %w", metaPath(path), err)
	}
	maps.DeleteFunc(values, func(_, value string) bool { return value == "" })
	return values, nil
}

func writeMeta(path string, values map[string]string) error {
	stored, err := readMeta(path)
	if err != nil {
		return err
	}
	if stored == nil {
		stored = make(map[string]string)
	}
	for key, value := range values {
		if value == "" {
			delete(stored, key)
		} else {
			stored[key] = value
		}
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(metaPath(path), append(data, '\n'))
}

func (p *Provider) Update(ctx context.Context, ref string, ch vm.Changes) error {
	path, err := p.resolve(ctx, ref)
	if err != nil {
		return err
	}
	if ch.MemoryMB%4 != 0 {
		return fmt.Errorf("memory %d MB is not a multiple of 4: %w", ch.MemoryMB, vm.ErrInvalid)
	}
	if ch.CPUs <= 0 && ch.MemoryMB <= 0 {
		return nil
	}
	return p.updateHardware(ctx, path, ch)
}

func (p *Provider) updateHardware(ctx context.Context, path string, ch vm.Changes) error {
	v, err := readVMX(path)
	if err != nil {
		return err
	}
	running, err := p.running(ctx)
	if err != nil {
		return err
	}
	if state := vmState(path, v, running); state != vm.StateStopped {
		return fmt.Errorf("vm %s is %s, power it off first: %w", path, state, vm.ErrInvalidState)
	}
	if ch.CPUs > 0 {
		v.set("numvcpus", strconv.Itoa(ch.CPUs))
		v.set("cpuid.coresPerSocket", strconv.Itoa(ch.CPUs))
	}
	if ch.MemoryMB > 0 {
		v.set("memsize", strconv.Itoa(ch.MemoryMB))
	}
	return v.write(path)
}

func (p *Provider) SetMeta(ctx context.Context, ref string, values map[string]string) error {
	path, err := p.resolve(ctx, ref)
	if err != nil {
		return err
	}
	return writeMeta(path, values)
}

func (p *Provider) Delete(ctx context.Context, ref string) error {
	path, err := p.resolve(ctx, ref)
	if err != nil {
		return err
	}
	running, err := p.isRunning(ctx, path)
	if err != nil {
		return err
	}
	if running {
		return fmt.Errorf("vm %s is running: %w", path, vm.ErrInvalidState)
	}
	snapshots, err := readVMSD(path)
	if err != nil {
		return err
	}
	if clones := snapshots.liveClones(); len(clones) > 0 {
		return fmt.Errorf("vm %s is the base of linked clones %s: %w", path, strings.Join(clones, ", "), vm.ErrInvalidState)
	}
	if p.inRoot(path) {
		_, err = p.vmrunCmd(ctx, "deleteVM", path)
		if err != nil && !errors.Is(err, vm.ErrInvalid) {
			return err
		}
		return os.RemoveAll(filepath.Dir(path))
	}
	if others := slices.DeleteFunc(vmxFiles(filepath.Dir(path)), func(other string) bool { return samePath(other, path) }); len(others) > 0 {
		return fmt.Errorf("vm %s shares its folder with %s and deleting it could remove their disks: %w", path, strings.Join(others, ", "), vm.ErrForbidden)
	}
	if _, err := p.vmrunCmd(ctx, "deleteVM", path); err != nil {
		return err
	}
	for _, file := range []string{metaPath(path), pausedMarker(path)} {
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
