package vmware

import (
	"context"
	"encoding/binary"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/fl4metf/vm-harness/internal/fakerun"
	"github.com/fl4metf/vm-harness/runner"
	"github.com/fl4metf/vm-harness/vm"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata")

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		writeFile(t, path, got)
		return
	}
	if want := readFile(t, path); got != want {
		t.Errorf("%s mismatch, got:\n%s", name, got)
	}
}

func TestCreateGolden(t *testing.T) {
	cases := []struct {
		golden string
		spec   vm.Spec
	}{
		{"linux.vmx", vm.Spec{
			Name: "web", OSType: "ubuntu-64", CPUs: 2, MemoryMB: 2048, DiskGB: 20,
			Meta:   map[string]string{"os_type": "ubuntu"},
			Labels: map[string]string{"team": `red "core" | ops`},
		}},
		{"windows.vmx", vm.Spec{
			Name: "win", OSType: "windows11-64", CPUs: 4, MemoryMB: 8192, DiskGB: 64, Firmware: "efi",
			ISO: `C:\iso\win11.iso`,
			NICs: []vm.NIC{
				{Mode: vm.NetNAT},
				{Mode: vm.NetBridged},
				{Mode: vm.NetHostOnly, Adapter: "vmnet2", Model: "vmxnet3", MAC: "00:50:56:00:00:01"},
				{Mode: vm.NetNone},
			},
			SharedFolders: []vm.SharedFolder{{Name: "src", HostPath: `C:\src`}, {Name: "docs", HostPath: `D:\docs`, ReadOnly: true}},
			Meta:          map[string]string{"os_type": "windows11"},
		}},
	}
	for _, c := range cases {
		t.Run(c.golden, func(t *testing.T) {
			e := newTestEnv(t)
			e.fake.On("-c ", fixture(t, "vdiskmanager/create.txt"))
			m, err := e.Create(context.Background(), c.spec)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(e.root, c.spec.Name)
			path := filepath.Join(dir, c.spec.Name+".vmx")
			disk := filepath.Join(dir, c.spec.Name+".vmdk")
			want := []string{"-c -s " + strconv.Itoa(c.spec.DiskGB) + "GB -a lsilogic -t 0 " + disk}
			if got := e.commands(); !slices.Equal(got, want) {
				t.Fatalf("commands = %q, want %q", got, want)
			}
			checkGolden(t, c.golden, readFile(t, path))
			if m.ID != path || m.Name != c.spec.Name || m.State != vm.StateStopped || m.Managed || m.ConsoleLog != "" {
				t.Fatalf("machine = %+v", m)
			}
			meta := specMeta(c.spec)
			if got := storedMeta(t, path); !maps.Equal(got, meta) || !maps.Equal(m.Meta, meta) {
				t.Fatalf("meta = %v, want %v", got, meta)
			}
		})
	}
}

func TestCreateWithCloudInit(t *testing.T) {
	e := newTestEnv(t)
	e.fake.On("-c ", fixture(t, "vdiskmanager/create.txt"))
	spec := vm.Spec{
		Name: "cloud", OSType: "ubuntu-64", CPUs: 2, MemoryMB: 2048, DiskGB: 10,
		CloudInit: &vm.CloudInit{User: "vmh", SSHAuthorizedKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample test"}},
	}
	m, err := e.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.root, "cloud")
	checkGolden(t, "cloud-init.vmx", readFile(t, filepath.Join(dir, "cloud.vmx")))
	if want := filepath.Join(dir, consoleFile); m.ConsoleLog != want {
		t.Errorf("console log = %q, want %q", m.ConsoleLog, want)
	}
	iso := readFile(t, filepath.Join(dir, seedFile))
	if len(iso) < 0x8006 || iso[0x8001:0x8006] != "CD001" {
		t.Fatalf("seed is not an iso9660 image (%d bytes)", len(iso))
	}
}

func sparseDisk(capacityGB int64) func(fakerun.Call) (runner.Result, error) {
	return func(c fakerun.Call) (runner.Result, error) {
		header := make([]byte, 512)
		copy(header, "KDMV")
		binary.LittleEndian.PutUint64(header[12:], uint64(capacityGB<<30/512))
		err := os.WriteFile(c.Args[len(c.Args)-1], header, 0o644)
		return runner.Result{Stdout: []byte("Virtual disk conversion successful.\r\n")}, err
	}
}

func TestCreateFromDiskImage(t *testing.T) {
	cases := []struct {
		diskGB int
		expand bool
	}{{20, true}, {10, false}, {5, false}, {0, false}}
	for _, c := range cases {
		e := newTestEnv(t)
		e.fake.OnFunc("-r ", sparseDisk(10))
		e.fake.On("-x ", fixture(t, "vdiskmanager/expand.txt"))
		spec := vm.Spec{Name: "img", CPUs: 1, MemoryMB: 1024, DiskGB: c.diskGB, DiskImage: `C:\images\noble.vmdk`}
		if _, err := e.Create(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		disk := filepath.Join(e.root, "img", "img.vmdk")
		want := []string{`-r C:\images\noble.vmdk -t 0 ` + disk}
		if c.expand {
			want = append(want, "-x "+strconv.Itoa(c.diskGB)+"GB "+disk)
		}
		if got := e.commands(); !slices.Equal(got, want) {
			t.Errorf("disk %d GB: commands = %q, want %q", c.diskGB, got, want)
		}
	}
}

func TestCreateFromAppliance(t *testing.T) {
	e := newTestEnv(t)
	var args []string
	planted := "sharedFolder0.present = \"TRUE\"\r\nsharedFolder0.hostPath = \"C:\\\"\r\nsharedFolder0.writeAccess = \"TRUE\"\r\n" +
		"sharedFolder.maxNum = \"1\"\r\nisolation.tools.hgfs.disable = \"FALSE\"\r\nisolation.tools.hgfsServerSet.disable = \"FALSE\"\r\n"
	e.fake.OnFunc("--acceptAllEulas", func(c fakerun.Call) (runner.Result, error) {
		args = c.Args
		err := os.WriteFile(c.Args[len(c.Args)-1], []byte(fixture(t, "vmx/ovftool-imported.vmx")+planted), 0o644)
		return runner.Result{Stdout: []byte(fixture(t, "ovftool/import.txt"))}, err
	})
	spec := vm.Spec{
		Name: "app", Appliance: `C:\images\web.ova`, CPUs: 2, MemoryMB: 1024,
		Meta: map[string]string{"ssh_user": "vmh"}, Labels: map[string]string{"role": "db"},
	}
	m, err := e.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.root, "app", "app.vmx")
	if want := []string{"--acceptAllEulas", "--name=app", `C:\images\web.ova`, path}; !slices.Equal(args, want) {
		t.Fatalf("ovftool args = %q", args)
	}
	if got := e.commands(); len(got) != 1 {
		t.Fatalf("no disk should be created for an appliance: %q", got)
	}
	vmx := readFile(t, path)
	for _, line := range []string{
		"displayname = \"app\"\r\n",
		"numvcpus = \"2\"\r\n",
		"memsize = \"1024\"\r\n",
		"ethernet0.wakeonpcktrcv = \"true\"\r\n",
		"msg.autoAnswer = \"TRUE\"\r\n",
	} {
		if !strings.Contains(vmx, line) {
			t.Errorf("vmx lacks %q:\n%s", line, vmx)
		}
	}
	if strings.Contains(strings.ToLower(vmx), "sharedfolder") || strings.Contains(vmx, "hgfs") {
		t.Errorf("shared folders from the appliance survived:\n%s", vmx)
	}
	if m.Name != "app" || m.CPUs != 2 || m.MemoryMB != 1024 || m.Meta["ssh_user"] != "vmh" || m.Labels["role"] != "db" {
		t.Fatalf("machine = %+v", m)
	}
}

func importWrites(vmx string) func(fakerun.Call) (runner.Result, error) {
	return func(c fakerun.Call) (runner.Result, error) {
		return runner.Result{}, os.WriteFile(c.Args[len(c.Args)-1], []byte(vmx), 0o644)
	}
}

func TestCreateFromApplianceDropsHostDevices(t *testing.T) {
	e := newTestEnv(t)
	outside := t.TempDir()
	planted := []string{
		`nvram = "` + filepath.Join(outside, "app.nvram") + `"`,
		`workingDir = "` + outside + `"`,
		`log.fileName = "` + filepath.Join(outside, "vmware.log") + `"`,
		`extendedConfigFile = "` + filepath.Join(outside, "app.vmxf") + `"`,
		`checkpoint.vmState = "app-5cf61512.vmss"`,
		`RemoteDisplay.vnc.enabled = "TRUE"`,
		`isolation.tools.ghi.launchmenu.change = "FALSE"`,
		`usb.autoConnect.device0 = "0x0781:0x5567"`,
		`ide0:0.present = "TRUE"`,
		`ide0:0.deviceType = "cdrom-image"`,
		`ide0:0.fileName = "` + filepath.Join(outside, "tools.iso") + `"`,
		`ide0:1.present = "TRUE"`,
		`ide0:1.deviceType = "cdrom-raw"`,
		`ide0:1.fileName = "auto detect"`,
		"",
	}
	e.fake.OnFunc("--acceptAllEulas", importWrites(fixture(t, "vmx/ovftool-host-devices.vmx")+strings.Join(planted, "\r\n")))
	m, err := e.Create(context.Background(), vm.Spec{Name: "app", Appliance: `C:\images\web.ova`})
	if err != nil {
		t.Fatal(err)
	}
	v, err := readVMX(m.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range v.lines {
		if line.key == "" {
			continue
		}
		if hostPortPattern.MatchString(line.key) || strings.Contains(line.value, outside) || strings.Contains(line.value, `C:\outside`) || strings.Contains(line.value, "pipe") {
			t.Errorf("host device or path survived the import: %s = %q", line.key, line.value)
		}
		for _, prefix := range []string{"floppy2.", "sata0:2.", "ide0:", "sound.", "checkpoint.", "RemoteDisplay.", "isolation.tools.", "usb.autoConnect.", "workingDir", "log.", "extendedConfigFile"} {
			if hasPrefixFold(line.key, prefix) && !strings.EqualFold(line.key, "sound.present") {
				t.Errorf("imported key %s = %q survived", line.key, line.value)
			}
		}
	}
	kept := map[string]string{
		"sata0:0.fileName":  "app-disk1.vmdk",
		"sata0:1.fileName":  "app-file2.iso",
		"floppy0.fileName":  "app-file1.flp",
		"nvram":             "app-file3.nvram",
		"sound.present":     "FALSE",
		"ethernet0.present": "TRUE",
	}
	for key, want := range kept {
		if got := v.get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if m.State != vm.StateStopped {
		t.Errorf("state = %s", m.State)
	}
}

func TestCreateApplianceWithCloudInitGetsOwnConsole(t *testing.T) {
	e := newTestEnv(t)
	e.fake.OnFunc("--acceptAllEulas", importWrites(fixture(t, "vmx/ovftool-host-devices.vmx")))
	m, err := e.Create(context.Background(), vm.Spec{Name: "app", Appliance: `C:\images\web.ova`, CloudInit: &vm.CloudInit{User: "vmh"}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.root, "app")
	if want := filepath.Join(dir, consoleFile); m.ConsoleLog != want {
		t.Errorf("console log = %q, want %q", m.ConsoleLog, want)
	}
	v, err := readVMX(m.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"serial0.present":             "TRUE",
		"serial0.fileType":            "file",
		"serial0.fileName":            consoleFile,
		"serial0.startConnected":      "",
		"serial0.yieldOnMsrRead":      "",
		"serial1.present":             "",
		"serial1.fileName":            "",
		"answer.msg.serial.file.open": "Replace",
	}
	for key, value := range want {
		if got := v.get(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	if !isFile(filepath.Join(dir, seedFile)) {
		t.Error("seed not written")
	}
}

func TestCreateRefusesApplianceDiskOutsideFolder(t *testing.T) {
	for _, disk := range []string{filepath.Join(t.TempDir(), "host.vmdk"), "../other/other.vmdk"} {
		e := newTestEnv(t)
		vmx := strings.Replace(fixture(t, "vmx/ovftool-imported.vmx"), `"app-disk1.vmdk"`, `"`+disk+`"`, 1)
		e.fake.OnFunc("--acceptAllEulas", importWrites(vmx))
		_, err := e.Create(context.Background(), vm.Spec{Name: "app", Appliance: `C:\images\web.ova`})
		wantKind(t, err, vm.ErrInvalid)
		if _, err := os.Stat(filepath.Join(e.root, "app")); !os.IsNotExist(err) {
			t.Fatalf("disk %s: vm folder left behind: %v", disk, err)
		}
	}
}

func TestCreateAppliesRequestedSharedFoldersToAppliance(t *testing.T) {
	e := newTestEnv(t)
	e.fake.OnFunc("--acceptAllEulas", func(c fakerun.Call) (runner.Result, error) {
		vmx := fixture(t, "vmx/ovftool-imported.vmx") + "sharedFolder0.hostPath = \"C:\\\"\r\nsharedFolder0.writeAccess = \"TRUE\"\r\n"
		return runner.Result{}, os.WriteFile(c.Args[len(c.Args)-1], []byte(vmx), 0o644)
	})
	spec := vm.Spec{Name: "app", Appliance: `C:\images\web.ova`, SharedFolders: []vm.SharedFolder{{Name: "src", HostPath: `D:\src`, ReadOnly: true}}}
	if _, err := e.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	v, err := readVMX(filepath.Join(e.root, "app", "app.vmx"))
	if err != nil {
		t.Fatal(err)
	}
	if v.get("sharedFolder0.hostPath") != `D:\src` || v.get("sharedFolder0.writeAccess") != "FALSE" || v.get("isolation.tools.hgfs.disable") != "FALSE" {
		t.Fatalf("requested folder not applied:\n%s", v.encode())
	}
}

func TestCreateNormalizesMAC(t *testing.T) {
	for in, want := range map[string]string{"52-54-00-12-34-56": "52:54:00:12:34:56", "525400ABCDEF": "52:54:00:ab:cd:ef", "00:50:56:00:00:01": "00:50:56:00:00:01"} {
		e := newTestEnv(t)
		e.fake.On("-c ", fixture(t, "vdiskmanager/create.txt"))
		spec := vm.Spec{Name: "web", CPUs: 1, MemoryMB: 64, DiskGB: 1, NICs: []vm.NIC{{Mode: vm.NetNAT, MAC: in}}}
		if _, err := e.Create(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		v, err := readVMX(filepath.Join(e.root, "web", "web.vmx"))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.get("ethernet0.address"); got != want || v.get("ethernet0.addressType") != "static" {
			t.Errorf("mac %q written as %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"zz", "52:54:00:12:34", "02:00:5e:10:00:00:00:01", "525400ABCDEZ"} {
		e := newTestEnv(t)
		_, err := e.Create(context.Background(), vm.Spec{Name: "web", CPUs: 1, MemoryMB: 64, DiskGB: 1, NICs: []vm.NIC{{MAC: bad}}})
		wantKind(t, err, vm.ErrInvalid)
	}
}

func TestAttachSeedToImportedVM(t *testing.T) {
	v := parseVMX([]byte(fixture(t, "vmx/ovftool-imported.vmx")))
	if err := attachSeed(v); err != nil {
		t.Fatal(err)
	}
	if v.get("sata0:1.fileName") != seedFile || v.get("sata0:1.deviceType") != "cdrom-image" {
		t.Fatalf("seed not on the first free sata unit:\n%s", v.encode())
	}
}

func TestCreateRejects(t *testing.T) {
	base := vm.Spec{Name: "web", CPUs: 1, MemoryMB: 1024, DiskGB: 8}
	with := func(change func(*vm.Spec)) vm.Spec {
		spec := base
		change(&spec)
		return spec
	}
	cases := []struct {
		name string
		spec vm.Spec
		kind error
	}{
		{"port forwards", with(func(s *vm.Spec) { s.PortForwards = []vm.PortForward{{Protocol: "tcp", HostPort: 2222, GuestPort: 22}} }), vm.ErrUnsupported},
		{"unattended", with(func(s *vm.Spec) { s.Unattended = &vm.Unattended{User: "u"} }), vm.ErrUnsupported},
		{"bad name", with(func(s *vm.Spec) { s.Name = `..\web` }), vm.ErrInvalid},
		{"memory", with(func(s *vm.Spec) { s.MemoryMB = 1001 }), vm.ErrInvalid},
		{"no disk", with(func(s *vm.Spec) { s.DiskGB = 0 }), vm.ErrInvalid},
		{"firmware", with(func(s *vm.Spec) { s.Firmware = "uefi" }), vm.ErrInvalid},
		{"internal network", with(func(s *vm.Spec) { s.NICs = []vm.NIC{{Mode: vm.NetInternal, Adapter: "lan"}} }), vm.ErrUnsupported},
		{"custom without vmnet", with(func(s *vm.Spec) { s.NICs = []vm.NIC{{Mode: vm.NetCustom}} }), vm.ErrInvalid},
		{"bridged to host adapter", with(func(s *vm.Spec) { s.NICs = []vm.NIC{{Mode: vm.NetBridged, Adapter: "Intel(R) Ethernet"}} }), vm.ErrInvalid},
		{"too many nics", with(func(s *vm.Spec) { s.NICs = make([]vm.NIC, 11) }), vm.ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			_, err := e.Create(context.Background(), c.spec)
			wantKind(t, err, c.kind)
			if got := e.commands(); len(got) != 0 {
				t.Fatalf("nothing should run: %q", got)
			}
			if entries, _ := os.ReadDir(e.root); len(entries) != 0 {
				t.Fatalf("root not clean: %v", entries)
			}
		})
	}

	t.Run("appliance without ovftool", func(t *testing.T) {
		e := newTestEnv(t)
		e.ovftool = ""
		_, err := e.Create(context.Background(), vm.Spec{Name: "app", Appliance: `C:\images\web.ova`})
		wantKind(t, err, vm.ErrUnsupported)
	})

	t.Run("no root", func(t *testing.T) {
		e := newTestEnv(t)
		e.root = ""
		_, err := e.Create(context.Background(), base)
		wantKind(t, err, vm.ErrInvalid)
	})

	t.Run("existing folder", func(t *testing.T) {
		e := newTestEnv(t)
		writeFile(t, filepath.Join(e.root, "web", "notes.txt"), "keep me")
		_, err := e.Create(context.Background(), base)
		wantKind(t, err, vm.ErrExists)
		if readFile(t, filepath.Join(e.root, "web", "notes.txt")) != "keep me" {
			t.Fatal("existing folder was touched")
		}
	})
}

func TestCreateFailureRemovesFolder(t *testing.T) {
	e := newTestEnv(t)
	e.fake.OnResult("-c ", runner.Result{ExitCode: 1, Stdout: []byte("Creating disk 'x'\r\n"), Stderr: []byte(fixture(t, "vdiskmanager/create-exists.txt"))})
	_, err := e.Create(context.Background(), vm.Spec{Name: "web", CPUs: 1, MemoryMB: 1024, DiskGB: 8})
	wantKind(t, err, vm.ErrExists)
	if _, err := os.Stat(filepath.Join(e.root, "web")); !os.IsNotExist(err) {
		t.Fatalf("vm folder left behind: %v", err)
	}
}

func TestDiskCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.vmdk")
	if _, err := sparseDisk(3)(fakerun.Call{Args: []string{path}}); err != nil {
		t.Fatal(err)
	}
	got, err := diskCapacity(path)
	if err != nil || got != 3<<30 {
		t.Fatalf("capacity = %d, %v", got, err)
	}
	writeFile(t, path, "# Disk DescriptorFile\nversion=1\n")
	_, err = diskCapacity(path)
	wantKind(t, err, vm.ErrInvalid)
}
