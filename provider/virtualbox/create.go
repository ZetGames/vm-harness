package virtualbox

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fl4metf/vm-harness/cloudinit"
	"github.com/fl4metf/vm-harness/vm"
)

const (
	controller = "SATA"
	seedName   = "seed.iso"
	serialName = "serial.log"
	maxNICs    = 8
	metaISO    = "registered_iso"
)

var errNoRoot = fmt.Errorf("virtualbox root folder is not configured: %w", vm.ErrInvalid)

func (p *Provider) Create(ctx context.Context, spec vm.Spec) (_ vm.Machine, err error) {
	if p.root == "" {
		return vm.Machine{}, errNoRoot
	}
	if err := checkNewName(spec.Name); err != nil {
		return vm.Machine{}, err
	}
	spec.OSType = vm.NativeOSType(vm.VirtualBox, spec.OSType)
	if spec.Appliance == "" && spec.DiskImage == "" && spec.DiskGB <= 0 {
		return vm.Machine{}, fmt.Errorf("disk size must be positive: %w", vm.ErrInvalid)
	}
	nics, err := nicArgs(spec.NICs)
	if err != nil {
		return vm.Machine{}, err
	}
	rules, err := natRules(spec.PortForwards)
	if err != nil {
		return vm.Machine{}, err
	}
	if len(rules) > 0 && (spec.Appliance == "" || len(spec.NICs) > 0) && !firstNICIsNAT(spec.NICs) {
		return vm.Machine{}, errForwardNeedsNAT
	}
	existing, err := p.listVMs(ctx)
	if err != nil {
		return vm.Machine{}, err
	}
	if slices.ContainsFunc(existing, func(e vmEntry) bool { return e.name == spec.Name }) {
		return vm.Machine{}, fmt.Errorf("vm %s: %w", spec.Name, vm.ErrExists)
	}
	iso, err := p.unregisteredISO(ctx, spec.ISO)
	if err != nil {
		return vm.Machine{}, err
	}
	dir := filepath.Join(p.root, spec.Name)
	if err := reserve(dir); err != nil {
		return vm.Machine{}, err
	}
	var id string
	defer func() {
		if err != nil {
			p.discard(ctx, id, dir, iso)
		}
	}()
	if spec.Appliance != "" {
		id, err = p.importAppliance(context.WithoutCancel(ctx), spec, dir, existing)
	} else {
		id, err = p.createVM(context.WithoutCancel(ctx), spec)
	}
	if err != nil {
		return vm.Machine{}, err
	}
	if err = p.configure(ctx, id, dir, iso, spec, nics, rules); err != nil {
		return vm.Machine{}, err
	}
	return p.Get(ctx, id)
}

func (p *Provider) createVM(ctx context.Context, spec vm.Spec) (string, error) {
	id := newUUID()
	args := []string{"createvm", "--name", spec.Name, "--uuid", id}
	if spec.OSType != "" {
		args = append(args, "--ostype", spec.OSType)
	}
	_, err := p.run(ctx, append(args, "--register", "--basefolder", p.root)...)
	return id, err
}

var virtualSystem = regexp.MustCompile(`(?m)^Virtual system \d+:`)

func (p *Provider) importAppliance(ctx context.Context, spec vm.Spec, dir string, known []vmEntry) (string, error) {
	settings := filepath.Join(dir, spec.Name+".vbox")
	args := []string{"--vsys", "0", "--vmname", spec.Name, "--settingsfile", settings, "--basefolder", p.root}
	if spec.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(spec.CPUs))
	}
	if spec.MemoryMB > 0 {
		args = append(args, "--memory", strconv.Itoa(spec.MemoryMB))
	}
	out, err := p.run(ctx, append([]string{"import", spec.Appliance, "--dry-run"}, args...)...)
	if err != nil {
		return "", err
	}
	if n := len(virtualSystem.FindAllString(out, -1)); n > 1 {
		return "", fmt.Errorf("appliance %s holds %d virtual machines and vmh imports single-vm appliances only: %w", spec.Appliance, n, vm.ErrInvalid)
	}
	_, err = p.run(ctx, append([]string{"import", spec.Appliance}, args...)...)
	id, lookupErr := p.findImported(ctx, settings, known)
	if id == "" {
		return "", cmp.Or(err, lookupErr)
	}
	info, vetErr := p.inspect(ctx, id)
	if vetErr == nil {
		vetErr = importRefusal(info, spec.Appliance)
	}
	if vetErr != nil {
		_, unregisterErr := p.run(ctx, "unregistervm", id)
		return "", errors.Join(cmp.Or(err, vetErr), unregisterErr)
	}
	if err != nil {
		return id, err
	}
	return id, p.resetImported(ctx, info)
}

func importRefusal(info vmInfo, appliance string) error {
	if info.fields["CurrentSnapshotUUID"] != "" {
		return fmt.Errorf("appliance %s carries snapshots and vmh imports only the current state: %w", appliance, vm.ErrInvalid)
	}
	removable := dvdSlots(info.fields)
	for _, slot := range info.mediaSlots() {
		if !slices.Contains(removable, slot) && !within(info.dir(), info.fields[slot]) {
			return fmt.Errorf("appliance %s attaches disks that it does not contain: %w", appliance, vm.ErrInvalid)
		}
	}
	return nil
}

var importResets = []string{
	"--vrde", "off", "--recording", "off", "--teleporter", "off", "--autostart-enabled", "off",
	"--guest-debug-provider", "none", "--snapshot-folder", "default", "--firmware-logo-image-path", "",
	"--clipboard-mode", "disabled", "--drag-and-drop", "disabled", "--audio-enabled", "off", "--usb-card-reader", "off",
	"--uart1", "off", "--uart2", "off", "--uart3", "off", "--uart4", "off", "--lpt1", "off", "--lpt2", "off",
}

func (p *Provider) resetImported(ctx context.Context, info vmInfo) error {
	id := info.id()
	args := append([]string{"modifyvm", id}, importResets...)
	for n := 1; n <= maxNICs; n++ {
		nic := strconv.Itoa(n)
		args = append(args, "--nic-trace"+nic, "off", "--nat-tftp-prefix"+nic, "", "--nat-localhostreachable"+nic, "off")
		for _, name := range info.natRules[n] {
			args = append(args, "--natpf"+nic, "delete", name)
		}
	}
	passthrough, err := hostTPM(info.fields["CfgFile"])
	if err != nil {
		return err
	}
	if passthrough {
		args = append(args, "--tpm-type", "none")
	}
	if _, err := p.run(ctx, args...); err != nil {
		return err
	}
	for _, slot := range dvdSlots(info.fields) {
		if info.fields[slot] == "emptydrive" {
			continue
		}
		ctl, port, device, ok := parseSlot(slot)
		if !ok {
			return fmt.Errorf("unexpected storage slot %q", slot)
		}
		if _, err := p.run(ctx, "storageattach", id, "--storagectl", ctl, "--port", port, "--device", device, "--medium", "emptydrive"); err != nil {
			return err
		}
	}
	for range info.numbered("USBFilterActive") {
		if _, err := p.run(ctx, "usbfilter", "remove", "0", "--target", id); err != nil {
			return err
		}
	}
	for _, name := range info.numbered("SharedFolderNameMachineMapping") {
		if _, err := p.run(ctx, "sharedfolder", "remove", id, "--name", name); err != nil {
			return err
		}
	}
	out, err := p.run(ctx, "getextradata", id, "enumerate")
	if err != nil {
		return err
	}
	for _, key := range slices.Sorted(maps.Keys(parseExtradata(out))) {
		if _, err := p.run(ctx, "setextradata", id, key); err != nil {
			return err
		}
	}
	info, err = p.inspect(ctx, id)
	if err != nil {
		return err
	}
	if out, err = p.run(ctx, "getextradata", id, "enumerate"); err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" || len(info.natRules) > 0 {
		return fmt.Errorf("could not reset the host settings that imported vm %s brought along: %w", info.name(), vm.ErrInvalid)
	}
	return nil
}

var emulatedTPMs = []string{"None", "v1_2", "v2_0"}

func hostTPM(settings string) (bool, error) {
	f, err := os.Open(settings)
	if err != nil {
		return false, err
	}
	defer f.Close()
	d := xml.NewDecoder(f)
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read %s: %w", settings, err)
		}
		if e, ok := tok.(xml.StartElement); ok && e.Name.Local == "TrustedPlatformModule" {
			i := slices.IndexFunc(e.Attr, func(a xml.Attr) bool { return a.Name.Local == "type" })
			return i >= 0 && !slices.Contains(emulatedTPMs, e.Attr[i].Value), nil
		}
	}
}

func (p *Provider) findImported(ctx context.Context, settings string, known []vmEntry) (string, error) {
	entries, err := p.listVMs(ctx)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if slices.Contains(known, e) {
			continue
		}
		if info, err := p.inspect(ctx, e.id); err == nil && samePath(info.fields["CfgFile"], settings) {
			return info.id(), nil
		}
	}
	return "", fmt.Errorf("no vm is registered at %s after the import: %w", settings, vm.ErrNotFound)
}

func (p *Provider) unregisteredISO(ctx context.Context, iso string) (string, error) {
	if iso == "" {
		return "", nil
	}
	out, err := p.run(ctx, "list", "dvds")
	if err != nil {
		return "", err
	}
	if slices.ContainsFunc(parseMediumLocations(out), func(loc string) bool { return samePath(loc, iso) }) {
		return "", nil
	}
	return iso, nil
}

func (p *Provider) configure(ctx context.Context, id, dir, iso string, spec vm.Spec, nics, rules []string) error {
	if args := hardwareArgs(id, dir, spec, nics); len(args) > 2 {
		if _, err := p.run(ctx, args...); err != nil {
			return err
		}
	}
	var imported vmInfo
	if spec.Appliance != "" {
		var err error
		if imported, err = p.inspect(ctx, id); err != nil {
			return err
		}
		if len(rules) > 0 && len(spec.NICs) == 0 && imported.fields["nic1"] != "nat" {
			return errForwardNeedsNAT
		}
	} else if err := p.addDisks(ctx, id, dir, spec); err != nil {
		return err
	}
	if spec.CloudInit != nil {
		if err := p.attachSeed(ctx, id, dir, spec, imported); err != nil {
			return err
		}
	}
	if len(rules) > 0 {
		args := []string{"modifyvm", id}
		for _, r := range rules {
			args = append(args, "--natpf1", r)
		}
		if _, err := p.run(ctx, args...); err != nil {
			return err
		}
	}
	for _, sf := range spec.SharedFolders {
		args := []string{"sharedfolder", "add", id, "--name", sf.Name, "--hostpath", sf.HostPath}
		if sf.ReadOnly {
			args = append(args, "--readonly")
		}
		if _, err := p.run(ctx, append(args, "--automount")...); err != nil {
			return err
		}
	}
	if err := p.writeMeta(ctx, id, iso, spec); err != nil {
		return err
	}
	if spec.Unattended != nil {
		return p.unattended(ctx, id, spec)
	}
	return nil
}

func (p *Provider) writeMeta(ctx context.Context, id, iso string, spec vm.Spec) error {
	meta := make(map[string]string)
	for k, v := range spec.Meta {
		if v != "" {
			meta[k] = v
		}
	}
	for k, v := range spec.Labels {
		if v != "" {
			meta[vm.LabelKey(k)] = v
		}
	}
	if iso != "" {
		meta[metaISO] = iso
	}
	return p.SetMeta(ctx, id, meta)
}

func hardwareArgs(id, dir string, spec vm.Spec, nics []string) []string {
	args := []string{"modifyvm", id}
	if spec.Appliance == "" {
		windows := vm.IsWindows(spec.OSType)
		windows11 := strings.HasPrefix(strings.ToLower(spec.OSType), "windows11")
		vram, graphics, firmware := "16", "vmsvga", "bios"
		if windows {
			vram, graphics = "128", "vboxsvga"
		}
		if windows11 {
			firmware = "efi"
		}
		firmware = cmp.Or(strings.ToLower(spec.Firmware), firmware)
		if spec.CPUs > 0 {
			args = append(args, "--cpus", strconv.Itoa(spec.CPUs))
		}
		if spec.MemoryMB > 0 {
			args = append(args, "--memory", strconv.Itoa(spec.MemoryMB))
		}
		args = append(args, "--vram", vram, "--graphicscontroller", graphics, "--firmware", firmware, "--audio-enabled", "off",
			"--boot1", "dvd", "--boot2", "disk", "--boot3", "none", "--boot4", "none")
		if windows11 {
			args = append(args, "--tpm-type", "2.0")
		}
		if !windows {
			args = append(args, "--rtc-use-utc", "on")
		}
		if len(spec.NICs) == 0 {
			args = append(args, "--nic1", "nat")
		}
	} else if spec.Firmware != "" {
		args = append(args, "--firmware", strings.ToLower(spec.Firmware))
	}
	args = append(args, "--paravirt-provider", "hyperv")
	args = append(args, nics...)
	if spec.CloudInit != nil {
		args = append(args, "--uart1", "0x3F8", "4", "--uartmode1", "file", filepath.Join(dir, serialName))
	}
	return args
}

func nicArgs(nics []vm.NIC) ([]string, error) {
	if len(nics) > maxNICs {
		return nil, fmt.Errorf("at most %d nics are supported: %w", maxNICs, vm.ErrInvalid)
	}
	var args []string
	for i, nic := range nics {
		n := strconv.Itoa(i + 1)
		switch nic.Mode {
		case vm.NetNAT, "":
			args = append(args, "--nic"+n, "nat")
		case vm.NetBridged, vm.NetHostOnly:
			if nic.Adapter == "" {
				return nil, fmt.Errorf("nic %s: %s mode needs a host adapter: %w", n, nic.Mode, vm.ErrInvalid)
			}
			flag := "--bridge-adapter"
			if nic.Mode == vm.NetHostOnly {
				flag = "--host-only-adapter"
			}
			args = append(args, "--nic"+n, nic.Mode, flag+n, nic.Adapter)
		case vm.NetNATNetwork:
			args = append(args, "--nic"+n, "natnetwork", "--nat-network"+n, cmp.Or(nic.Adapter, "NatNetwork"))
		case vm.NetInternal:
			args = append(args, "--nic"+n, "intnet", "--intnet"+n, cmp.Or(nic.Adapter, "intnet"))
		case vm.NetNone:
			args = append(args, "--nic"+n, "none")
		case vm.NetCustom:
			return nil, fmt.Errorf("nic %s: custom networks are vmware only: %w", n, vm.ErrUnsupported)
		default:
			return nil, fmt.Errorf("nic %s: unknown mode %q: %w", n, nic.Mode, vm.ErrInvalid)
		}
		if nic.Model != "" {
			args = append(args, "--nic-type"+n, nic.Model)
		}
		if nic.MAC != "" {
			args = append(args, "--mac-address"+n, strings.NewReplacer(":", "", "-", "").Replace(nic.MAC))
		}
	}
	return args, nil
}

var errForwardNeedsNAT = fmt.Errorf("port forwarding needs nic 1 in nat mode: %w", vm.ErrInvalid)

func firstNICIsNAT(nics []vm.NIC) bool {
	return len(nics) == 0 || nics[0].Mode == vm.NetNAT || nics[0].Mode == ""
}

func natRule(pf vm.PortForward) (string, error) {
	proto := cmp.Or(strings.ToLower(pf.Protocol), "tcp")
	switch {
	case proto != "tcp" && proto != "udp":
		return "", fmt.Errorf("port forward %s: protocol must be tcp or udp: %w", pf.Name, vm.ErrInvalid)
	case strings.ContainsAny(pf.Name+pf.HostIP+pf.GuestIP, ",\""):
		return "", fmt.Errorf("port forward %s: name and addresses must not contain commas or quotes: %w", pf.Name, vm.ErrInvalid)
	case pf.HostPort < 0 || pf.HostPort > 65535 || pf.GuestPort <= 0 || pf.GuestPort > 65535:
		return "", fmt.Errorf("port forward %s: port out of range: %w", pf.Name, vm.ErrInvalid)
	}
	return strings.Join([]string{pf.Name, proto, pf.HostIP, strconv.Itoa(pf.HostPort), pf.GuestIP, strconv.Itoa(pf.GuestPort)}, ","), nil
}

func natRules(pfs []vm.PortForward) ([]string, error) {
	rules := make([]string, 0, len(pfs))
	for _, pf := range pfs {
		r, err := natRule(pf)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, nil
}

func (p *Provider) addDisks(ctx context.Context, id, dir string, spec vm.Spec) error {
	if _, err := p.run(ctx, "storagectl", id, "--name", controller, "--add", "sata", "--controller", "IntelAhci", "--portcount", "4", "--bootable", "on"); err != nil {
		return err
	}
	disk := filepath.Join(dir, spec.Name+".vdi")
	work := context.WithoutCancel(ctx)
	if spec.DiskImage != "" {
		if err := p.cloneDisk(work, spec.DiskImage, disk, spec.DiskGB); err != nil {
			return err
		}
	} else if _, err := p.run(work, "createmedium", "disk", "--filename", disk, "--size", strconv.Itoa(spec.DiskGB*1024), "--format", "VDI"); err != nil {
		return err
	}
	if _, err := p.run(ctx, "storageattach", id, "--storagectl", controller, "--port", "0", "--device", "0", "--type", "hdd", "--medium", disk); err != nil {
		return err
	}
	if spec.ISO != "" && spec.Unattended == nil {
		return p.attachDVD(ctx, id, controller, 1, spec.ISO)
	}
	return nil
}

func (p *Provider) cloneDisk(ctx context.Context, src, dst string, sizeGB int) error {
	out, err := p.run(ctx, "list", "hdds")
	if err != nil {
		return err
	}
	known := slices.ContainsFunc(parseMediumLocations(out), func(loc string) bool { return samePath(loc, src) })
	if _, err := p.run(ctx, "clonemedium", "disk", src, dst, "--format", "VDI"); err != nil {
		return err
	}
	if !known {
		p.run(ctx, "closemedium", "disk", src)
	}
	if sizeGB <= 0 {
		return nil
	}
	out, err = p.run(ctx, "showmediuminfo", "disk", dst)
	if err != nil {
		return err
	}
	if parseCapacityMB(out) >= sizeGB*1024 {
		return nil
	}
	_, err = p.run(ctx, "modifymedium", "disk", dst, "--resize", strconv.Itoa(sizeGB*1024))
	return err
}

func (p *Provider) attachDVD(ctx context.Context, id, ctl string, port int, image string) error {
	_, err := p.run(ctx, "storageattach", id, "--storagectl", ctl, "--port", strconv.Itoa(port), "--device", "0", "--type", "dvddrive", "--medium", image)
	return err
}

func (p *Provider) attachSeed(ctx context.Context, id, dir string, spec vm.Spec, imported vmInfo) error {
	seed := filepath.Join(dir, seedName)
	hostname := cmp.Or(spec.CloudInit.Hostname, spec.Name)
	if err := cloudinit.WriteSeed(seed, *spec.CloudInit, "vmh-"+spec.Name, hostname); err != nil {
		return err
	}
	if spec.Appliance == "" {
		return p.attachDVD(ctx, id, controller, 2, seed)
	}
	ctl, port, setup := seedSlot(imported.fields)
	if setup != nil {
		if _, err := p.run(ctx, append([]string{"storagectl", id}, setup...)...); err != nil {
			return err
		}
	}
	return p.attachDVD(ctx, id, ctl, port, seed)
}

func seedSlot(f map[string]string) (string, int, []string) {
	for i := 0; ; i++ {
		name, ok := f["storagecontrollername"+strconv.Itoa(i)]
		if !ok {
			break
		}
		if f["storagecontrollertype"+strconv.Itoa(i)] != "IntelAhci" {
			continue
		}
		count, _ := strconv.Atoi(f["storagecontrollerportcount"+strconv.Itoa(i)])
		for port := range count {
			if v, ok := f[fmt.Sprintf("%s-%d-0", name, port)]; !ok || v == "none" {
				return name, port, nil
			}
		}
		return name, count, []string{"--name", name, "--portcount", strconv.Itoa(count + 1)}
	}
	return controller, 0, []string{"--name", controller, "--add", "sata", "--controller", "IntelAhci", "--portcount", "4"}
}

func (p *Provider) unattended(ctx context.Context, id string, spec vm.Spec) error {
	u := spec.Unattended
	args := []string{"unattended", "install", id, "--iso=" + spec.ISO}
	if u.User != "" {
		args = append(args, "--user="+u.User)
	}
	if u.Password != "" {
		file, err := writeSecret(u.Password)
		if err != nil {
			return err
		}
		defer os.Remove(file)
		args = append(args, "--user-password-file="+file)
	}
	if u.FullName != "" {
		args = append(args, "--full-user-name="+u.FullName)
	}
	hostname := cmp.Or(u.Hostname, spec.Name)
	if !strings.Contains(hostname, ".") {
		hostname += ".local"
	}
	args = append(args, "--hostname="+hostname)
	if u.Locale != "" {
		args = append(args, "--locale="+u.Locale)
	}
	if u.TimeZone != "" {
		args = append(args, "--time-zone="+u.TimeZone)
	}
	if u.ProductKey != "" {
		args = append(args, "--key="+u.ProductKey)
	}
	if u.InstallAdditions {
		args = append(args, "--install-additions")
	}
	if u.PostInstall != "" {
		args = append(args, "--post-install-command="+u.PostInstall)
	}
	_, err := p.run(ctx, args...)
	return err
}

func (p *Provider) Delete(ctx context.Context, ref string) error {
	info, err := p.inspect(ctx, ref)
	if err != nil {
		return err
	}
	switch s := info.state(); s {
	case vm.StateRunning, vm.StatePaused, vm.StateBusy:
		return fmt.Errorf("vm %s is %s: %w", info.name(), s, vm.ErrInvalidState)
	}
	out, err := p.run(ctx, "getextradata", info.id(), metaPrefix+metaISO)
	if err != nil {
		return err
	}
	iso, _ := lineValue(out, "Value:")
	if _, err := p.run(ctx, "unregistervm", info.id(), "--delete"); err != nil {
		var cmdErr *vm.CommandError
		if errors.As(err, &cmdErr) && strings.Contains(cmdErr.Stderr, "child media") {
			return fmt.Errorf("delete vm %s: its disks have child media from linked clones or an interrupted snapshot; delete the clones or close the leftover media first: %w", info.name(), err)
		}
		return fmt.Errorf("delete vm %s: %w", info.name(), err)
	}
	for _, image := range info.ownImages(iso) {
		p.run(ctx, "closemedium", "dvd", image)
	}
	p.removeLeftovers(info.dir())
	return nil
}

func (p *Provider) discard(ctx context.Context, id, dir, iso string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if id != "" {
		if _, err := p.run(ctx, "unregistervm", id, "--delete"); err != nil && !errors.Is(err, vm.ErrNotFound) {
			return
		}
	}
	if iso != "" {
		p.run(ctx, "closemedium", "dvd", iso)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		switch strings.ToLower(filepath.Ext(path)) {
		case ".vdi", ".vmdk", ".vhd":
			p.run(ctx, "closemedium", "disk", path, "--delete")
		case ".iso", ".viso":
			p.run(ctx, "closemedium", "dvd", path)
		}
	}
	os.RemoveAll(dir)
}

func (p *Provider) removeLeftovers(dir string) {
	if !p.inRoot(dir) {
		return
	}
	os.Remove(filepath.Join(dir, seedName))
	os.Remove(filepath.Join(dir, serialName))
	os.RemoveAll(filepath.Join(dir, "Logs"))
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "Unattended-") && !e.IsDir() {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	os.Remove(dir)
}

func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsAbs(path) && filepath.IsLocal(rel)
}

func (p *Provider) inRoot(dir string) bool {
	if p.root == "" || dir == "" {
		return false
	}
	rel, err := filepath.Rel(p.root, dir)
	return err == nil && rel != "." && rel != ".." && filepath.Dir(rel) == "."
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func checkNewName(name string) error {
	if err := checkRef(name); err != nil {
		return err
	}
	if name == "." || name == ".." || strings.ContainsAny(name, `/\:*?"<>|{}`) {
		return fmt.Errorf("vm name %q must not contain any of / \\ : * ? \" < > | { }: %w", name, vm.ErrInvalid)
	}
	return nil
}

func reserve(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	err := os.Mkdir(dir, 0o755)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("folder %s already exists, remove it or pick another name: %w", dir, vm.ErrExists)
	}
	return err
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
