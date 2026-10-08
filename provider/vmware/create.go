package vmware

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/fl4metf/vm-harness/cloudinit"
	"github.com/fl4metf/vm-harness/vm"
)

const (
	hardwareVersion = "20"
	seedFile        = "seed.iso"
	consoleFile     = "serial.log"
	maxNICs         = 10
	maxSATAUnits    = 30
	maxSerialPorts  = 4
)

var vmnetPattern = regexp.MustCompile(`^(/dev/)?vmnet\d+$`)

func (p *Provider) Create(ctx context.Context, spec vm.Spec) (vm.Machine, error) {
	if p.vmrun == "" {
		return vm.Machine{}, errNoVmrun
	}
	if err := p.checkSpec(spec); err != nil {
		return vm.Machine{}, err
	}
	dir, err := p.makeDir(spec.Name)
	if err != nil {
		return vm.Machine{}, err
	}
	path := filepath.Join(dir, spec.Name+".vmx")
	if err := p.build(ctx, spec, path); err != nil {
		os.RemoveAll(dir)
		return vm.Machine{}, err
	}
	return p.Get(ctx, path)
}

func (p *Provider) checkSpec(spec vm.Spec) error {
	generated := spec.Appliance == ""
	switch {
	case p.root == "":
		return fmt.Errorf("vmware root folder is not configured: %w", vm.ErrInvalid)
	case !isSafeDirName(spec.Name):
		return fmt.Errorf("vm name %q: %w", spec.Name, vm.ErrInvalid)
	case spec.Unattended != nil:
		return fmt.Errorf("unattended install on vmware: %w", vm.ErrUnsupported)
	case len(spec.PortForwards) > 0:
		return errPortForward
	case !generated && p.ovftool == "":
		return fmt.Errorf("importing an appliance needs ovftool: %w", vm.ErrUnsupported)
	case generated && p.vdiskmanager == "":
		return fmt.Errorf("vmware-vdiskmanager not found: %w", vm.ErrUnavailable)
	case generated && (spec.CPUs < 1 || spec.MemoryMB < 4):
		return fmt.Errorf("cpus and memory must be set: %w", vm.ErrInvalid)
	case generated && spec.DiskImage == "" && spec.DiskGB < 1:
		return fmt.Errorf("disk size must be set: %w", vm.ErrInvalid)
	case spec.MemoryMB%4 != 0:
		return fmt.Errorf("memory %d MB is not a multiple of 4: %w", spec.MemoryMB, vm.ErrInvalid)
	case spec.Firmware != "" && spec.Firmware != "bios" && spec.Firmware != "efi":
		return fmt.Errorf("firmware %q: %w", spec.Firmware, vm.ErrInvalid)
	}
	return nil
}

func (p *Provider) makeDir(name string) (string, error) {
	if err := os.MkdirAll(p.root, 0o700); err != nil {
		return "", err
	}
	dir := filepath.Join(p.root, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("vm %q: %w", name, vm.ErrExists)
		}
		return "", err
	}
	return dir, nil
}

func (p *Provider) build(ctx context.Context, spec vm.Spec, path string) error {
	var v *vmxFile
	if spec.Appliance == "" {
		v = newVMX(spec)
	} else {
		imported, err := p.importAppliance(ctx, spec, path)
		if err != nil {
			return err
		}
		v = imported
	}
	if err := configure(v, spec); err != nil {
		return err
	}
	if spec.Appliance == "" {
		if err := p.createDisk(ctx, spec, strings.TrimSuffix(path, ".vmx")+".vmdk"); err != nil {
			return err
		}
	}
	if ci := spec.CloudInit; ci != nil {
		seed := filepath.Join(filepath.Dir(path), seedFile)
		if err := cloudinit.WriteSeed(seed, *ci, "vmh-"+spec.Name, cmp.Or(ci.Hostname, spec.Name)); err != nil {
			return err
		}
		if err := attachSeed(v); err != nil {
			return err
		}
		addConsoleLog(v)
	}
	if err := v.write(path); err != nil {
		return err
	}
	return writeMeta(path, specMeta(spec))
}

func newVMX(spec vm.Spec) *vmxFile {
	cpus := strconv.Itoa(spec.CPUs)
	v := &vmxFile{}
	v.set(".encoding", "UTF-8")
	v.set("config.version", "8")
	v.set("virtualHW.version", hardwareVersion)
	v.set("virtualHW.productCompatibility", "hosted")
	v.set("displayName", spec.Name)
	v.set("guestOS", cmp.Or(spec.OSType, "other-64"))
	v.set("firmware", cmp.Or(spec.Firmware, "bios"))
	v.set("numvcpus", cpus)
	v.set("cpuid.coresPerSocket", cpus)
	v.set("memsize", strconv.Itoa(spec.MemoryMB))
	v.set("pciBridge0.present", "TRUE")
	for i := 4; i <= 7; i++ {
		prefix := fmt.Sprintf("pciBridge%d.", i)
		v.set(prefix+"present", "TRUE")
		v.set(prefix+"virtualDev", "pcieRootPort")
		v.set(prefix+"functions", "8")
	}
	v.set("vmci0.present", "TRUE")
	v.set("hpet0.present", "TRUE")
	v.set("usb.present", "FALSE")
	v.set("sound.present", "FALSE")
	v.set("floppy0.present", "FALSE")
	v.set("tools.syncTime", "TRUE")
	v.set("sata0.present", "TRUE")
	v.set("sata0:0.present", "TRUE")
	v.set("sata0:0.fileName", spec.Name+".vmdk")
	if spec.ISO != "" {
		attachCDROM(v, "sata0:1", spec.ISO)
	}
	return v
}

func configure(v *vmxFile, spec vm.Spec) error {
	if spec.Appliance == "" || len(spec.NICs) > 0 {
		v.removePrefix("ethernet")
		if err := addNICs(v, spec.NICs); err != nil {
			return err
		}
	}
	addSharedFolders(v, spec.SharedFolders)
	v.set("msg.autoAnswer", "TRUE")
	return nil
}

func specMeta(spec vm.Spec) map[string]string {
	values := make(map[string]string, len(spec.Meta)+len(spec.Labels))
	maps.Copy(values, spec.Meta)
	for name, value := range spec.Labels {
		values[vm.LabelKey(name)] = value
	}
	return values
}

func addNICs(v *vmxFile, nics []vm.NIC) error {
	if len(nics) == 0 {
		nics = []vm.NIC{{Mode: vm.NetNAT}}
	}
	if len(nics) > maxNICs {
		return fmt.Errorf("vmware supports at most %d network adapters: %w", maxNICs, vm.ErrInvalid)
	}
	for i, nic := range nics {
		prefix := fmt.Sprintf("ethernet%d.", i)
		if nic.Mode == vm.NetNone {
			v.set(prefix+"present", "FALSE")
			continue
		}
		connection, vnet, err := connectionType(nic)
		if err != nil {
			return err
		}
		v.set(prefix+"present", "TRUE")
		v.set(prefix+"connectionType", connection)
		if vnet != "" {
			v.set(prefix+"vnet", vnet)
		}
		v.set(prefix+"virtualDev", cmp.Or(nic.Model, "e1000e"))
		if nic.MAC == "" {
			v.set(prefix+"addressType", "generated")
			continue
		}
		mac, err := normalizeMAC(nic.MAC)
		if err != nil {
			return err
		}
		v.set(prefix+"addressType", "static")
		v.set(prefix+"address", mac)
	}
	return nil
}

func normalizeMAC(s string) (string, error) {
	if len(s) == 12 {
		s = s[0:2] + ":" + s[2:4] + ":" + s[4:6] + ":" + s[6:8] + ":" + s[8:10] + ":" + s[10:12]
	}
	mac, err := net.ParseMAC(s)
	if err != nil || len(mac) != 6 {
		return "", fmt.Errorf("mac address %q: %w", s, vm.ErrInvalid)
	}
	return mac.String(), nil
}

func connectionType(nic vm.NIC) (connection, vnet string, err error) {
	mode := cmp.Or(nic.Mode, vm.NetNAT)
	switch mode {
	case vm.NetNAT, vm.NetBridged, vm.NetHostOnly, vm.NetCustom:
	default:
		return "", "", fmt.Errorf("network mode %q on vmware: %w", mode, vm.ErrUnsupported)
	}
	if nic.Adapter == "" {
		if mode == vm.NetCustom {
			return "", "", fmt.Errorf("custom network needs a vmnet adapter such as vmnet2: %w", vm.ErrInvalid)
		}
		return mode, "", nil
	}
	if !vmnetPattern.MatchString(nic.Adapter) {
		return "", "", fmt.Errorf("adapter %q is not a vmware virtual network such as vmnet2: %w", nic.Adapter, vm.ErrInvalid)
	}
	return "custom", nic.Adapter, nil
}

func addSharedFolders(v *vmxFile, folders []vm.SharedFolder) {
	if len(folders) == 0 {
		return
	}
	v.set("isolation.tools.hgfs.disable", "FALSE")
	v.set("sharedFolder.maxNum", strconv.Itoa(len(folders)))
	for i, folder := range folders {
		prefix := fmt.Sprintf("sharedFolder%d.", i)
		writable := "TRUE"
		if folder.ReadOnly {
			writable = "FALSE"
		}
		v.set(prefix+"present", "TRUE")
		v.set(prefix+"enabled", "TRUE")
		v.set(prefix+"readAccess", "TRUE")
		v.set(prefix+"writeAccess", writable)
		v.set(prefix+"hostPath", folder.HostPath)
		v.set(prefix+"guestName", folder.Name)
		v.set(prefix+"expiration", "never")
	}
}

func attachCDROM(v *vmxFile, unit, file string) {
	v.set(unit+".present", "TRUE")
	v.set(unit+".deviceType", "cdrom-image")
	v.set(unit+".fileName", file)
	v.set(unit+".startConnected", "TRUE")
}

func attachSeed(v *vmxFile) error {
	unit, err := freeSATAUnit(v)
	if err != nil {
		return err
	}
	v.set("sata0.present", "TRUE")
	attachCDROM(v, unit, seedFile)
	return nil
}

func addConsoleLog(v *vmxFile) {
	v.set("serial0.present", "TRUE")
	v.set("serial0.fileType", "file")
	v.set("serial0.fileName", consoleFile)
	v.set("answer.msg.serial.file.open", "Replace")
}

func freeSATAUnit(v *vmxFile) (string, error) {
	for i := range maxSATAUnits {
		unit := fmt.Sprintf("sata0:%d", i)
		if !isTrue(v.get(unit + ".present")) {
			return unit, nil
		}
	}
	return "", fmt.Errorf("no free sata slot for the cloud-init seed: %w", vm.ErrInvalid)
}

func (p *Provider) createDisk(ctx context.Context, spec vm.Spec, disk string) error {
	if spec.DiskImage == "" {
		return p.runTool(ctx, p.vdiskmanager, "-c", "-s", fmt.Sprintf("%dGB", spec.DiskGB), "-a", "lsilogic", "-t", "0", disk)
	}
	if err := p.runTool(ctx, p.vdiskmanager, "-r", spec.DiskImage, "-t", "0", disk); err != nil {
		return err
	}
	if spec.DiskGB < 1 {
		return nil
	}
	capacity, err := diskCapacity(disk)
	if err != nil {
		return err
	}
	if int64(spec.DiskGB)<<30 <= capacity {
		return nil
	}
	return p.runTool(ctx, p.vdiskmanager, "-x", fmt.Sprintf("%dGB", spec.DiskGB), disk)
}

func diskCapacity(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var header [20]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	if string(header[:4]) != "KDMV" {
		return 0, fmt.Errorf("%s is not a sparse vmdk: %w", path, vm.ErrInvalid)
	}
	return int64(binary.LittleEndian.Uint64(header[12:20])) * 512, nil
}

func (p *Provider) importAppliance(ctx context.Context, spec vm.Spec, path string) (*vmxFile, error) {
	err := p.runTool(ctx, p.ovftool, "--acceptAllEulas", "--name="+spec.Name, spec.Appliance, path)
	if err != nil {
		return nil, err
	}
	v, err := readVMX(path)
	if err != nil {
		return nil, err
	}
	if err := sanitizeImport(v); err != nil {
		return nil, err
	}
	v.set("displayName", spec.Name)
	if spec.CPUs > 0 {
		v.set("numvcpus", strconv.Itoa(spec.CPUs))
		v.set("cpuid.coresPerSocket", strconv.Itoa(spec.CPUs))
	}
	if spec.MemoryMB > 0 {
		v.set("memsize", strconv.Itoa(spec.MemoryMB))
	}
	return v, nil
}

var (
	devicePattern    = regexp.MustCompile(`(?i)^((?:ide|scsi|sata|nvme)\d+:\d+|floppy\d+)\.`)
	hostPortPattern  = regexp.MustCompile(`(?i)^(serial|parallel|pcipassthru)\d+\.`)
	importedPrefixes = []string{
		"sharedFolder", "isolation.tools.", "sound.", "usb.autoConnect.", "usb.generic.",
		"RemoteDisplay.", "debugStub.", "checkpoint.",
	}
	importedPathKeys = []string{
		"nvram", "extendedConfigFile", "workingDir", "suspend.directory", "sched.swap.dir",
		"log.fileName", "vmxstats.filename", "fileSearchPath",
	}
)

func sanitizeImport(v *vmxFile) error {
	dropped := make(map[string]bool)
	for _, line := range v.lines {
		m := devicePattern.FindStringSubmatch(line.key)
		if m == nil {
			continue
		}
		keep, err := keepDevice(v, m[1])
		if err != nil {
			return err
		}
		dropped[strings.ToLower(m[1])] = !keep
	}
	v.removeFunc(func(key, value string) bool {
		if m := devicePattern.FindStringSubmatch(key); m != nil && dropped[strings.ToLower(m[1])] {
			return true
		}
		_, fileName := cutSuffixFold(key, ".fileName")
		hostPath := fileName || slices.ContainsFunc(importedPathKeys, func(k string) bool { return strings.EqualFold(key, k) })
		return hostPortPattern.MatchString(key) ||
			slices.ContainsFunc(importedPrefixes, func(prefix string) bool { return hasPrefixFold(key, prefix) }) ||
			hostPath && value != "" && !filepath.IsLocal(value)
	})
	if v.get("floppy0.present") == "" {
		v.set("floppy0.present", "FALSE")
	}
	v.set("sound.present", "FALSE")
	return nil
}

func keepDevice(v *vmxFile, device string) (bool, error) {
	file := v.get(device + ".fileName")
	if hasPrefixFold(device, "floppy") {
		return strings.EqualFold(v.get(device+".fileType"), "file") && filepath.IsLocal(file), nil
	}
	switch strings.ToLower(v.get(device + ".deviceType")) {
	case "", "disk", "scsi-harddisk", "ata-harddisk":
		if file != "" && !filepath.IsLocal(file) {
			return false, fmt.Errorf("appliance disk %q lies outside the vm folder: %w", file, vm.ErrInvalid)
		}
		return true, nil
	case "cdrom-image":
		return filepath.IsLocal(file), nil
	}
	return false, nil
}
