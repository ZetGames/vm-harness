package virtualbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/fl4metf/vm-harness/cloudinit"
	"github.com/fl4metf/vm-harness/internal/fakerun"
	"github.com/fl4metf/vm-harness/runner"
	"github.com/fl4metf/vm-harness/vm"
)

const (
	importedID = "dbe76ac6-da70-4170-a062-1b5e513cd3ee"
	foreignID  = "99999999-8888-7777-6666-555555555555"
)

func setField(info, key, value string) string {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `="[^\r\n]*"`)
	return re.ReplaceAllLiteralString(info, key+`="`+esc(value)+`"`)
}

func echoInfo(info string) func(fakerun.Call) (runner.Result, error) {
	return func(c fakerun.Call) (runner.Result, error) {
		return runner.Result{Stdout: []byte(setField(info, "UUID", c.Args[1]))}, nil
	}
}

func createdID(t *testing.T, f *fakerun.Fake) string {
	t.Helper()
	calls := append(f.Find("createvm"), f.Find("clonevm")...)
	if len(calls) != 1 || argAfter(calls[0], "--uuid") == "" {
		t.Fatalf("machine creations = %v", calls)
	}
	return argAfter(calls[0], "--uuid")
}

func normalized(f *fakerun.Fake, id string) []string {
	var out []string
	for _, c := range commands(f) {
		out = append(out, strings.ReplaceAll(c, id, "ID"))
	}
	return out
}

type detachedCheck struct {
	runner.Runner
	t *testing.T
}

func (r detachedCheck) Run(ctx context.Context, name string, args ...string) (runner.Result, error) {
	switch args[0] {
	case "createvm", "clonevm", "import", "createmedium", "clonemedium":
		if ctx.Done() != nil {
			r.t.Errorf("%s runs under a context the caller can cancel", args[0])
		}
	}
	return r.Runner.Run(ctx, name, args...)
}

func detachedProvider(t *testing.T, f *fakerun.Fake) *Provider {
	p := New(Options{VBoxManage: "VBoxManage", Root: t.TempDir(), Runner: detachedCheck{f, t}})
	p.retryWait = 0
	return p
}

func cancellable(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

func createFake(t *testing.T) *fakerun.Fake {
	return newFake(t).
		On("list vms", fixture(t, "list-vms.txt")).
		On("list hdds", fixture(t, "list-hdds.txt")).
		On("list dvds", "").
		OnFunc("createvm", func(c fakerun.Call) (runner.Result, error) {
			name := argAfter(c, "--name")
			return runner.Result{Stdout: []byte(fixture(t, "createvm.txt"))}, os.WriteFile(filepath.Join(argAfter(c, "--basefolder"), name, name+".vbox"), nil, 0o644)
		}).
		On("modifyvm", "").
		On("storagectl", "").
		OnFunc("createmedium", func(c fakerun.Call) (runner.Result, error) {
			return runner.Result{Stdout: []byte(fixture(t, "createmedium.txt"))}, os.WriteFile(argAfter(c, "--filename"), nil, 0o644)
		}).
		On("clonemedium", fixture(t, "clonemedium.txt")).
		OnFunc("closemedium", func(c fakerun.Call) (runner.Result, error) {
			if slices.Contains(c.Args, "--delete") {
				return runner.Result{}, os.Remove(c.Args[2])
			}
			return runner.Result{}, nil
		}).
		On("showmediuminfo", fixture(t, "showmediuminfo.txt")).
		On("modifymedium", "").
		On("storageattach", "").
		On("sharedfolder", "").
		On("setextradata", "").
		On("unattended", "").
		On("unregistervm", "").
		OnFunc("showvminfo", echoInfo(fixture(t, "showvminfo-stopped.txt"))).
		On("getextradata", fixture(t, "extradata.txt"))
}

func argAfter(c fakerun.Call, flag string) string {
	if i := slices.Index(c.Args, flag); i >= 0 && i+1 < len(c.Args) {
		return c.Args[i+1]
	}
	return ""
}

func TestCreate(t *testing.T) {
	base := vm.Spec{
		Name: "web", OSType: "Ubuntu_64", CPUs: 2, MemoryMB: 1024, DiskGB: 10,
		Meta: map[string]string{"os_type": "ubuntu"},
	}
	cases := []struct {
		name string
		spec func(vm.Spec) vm.Spec
		want func(root, disk string) []string
	}{
		{"blank", func(s vm.Spec) vm.Spec { return s }, func(root, disk string) []string {
			return []string{
				"list vms",
				"createvm --name web --uuid ID --ostype Ubuntu_64 --register --basefolder " + root,
				"modifyvm ID --cpus 2 --memory 1024 --vram 16 --graphicscontroller vmsvga --firmware bios --audio-enabled off --boot1 dvd --boot2 disk --boot3 none --boot4 none --rtc-use-utc on --nic1 nat",
				"storagectl ID --name SATA --add sata --controller IntelAhci --portcount 4 --bootable on",
				"createmedium disk --filename " + disk + " --size 10240 --format VDI",
				"storageattach ID --storagectl SATA --port 0 --device 0 --type hdd --medium " + disk,
				"setextradata ID vmh/os_type ubuntu",
				"showvminfo ID --machinereadable",
				"getextradata ID enumerate",
			}
		}},
		{"iso labels forwards shares", func(s vm.Spec) vm.Spec {
			s.ISO = `C:\isos\ubuntu.iso`
			s.Labels = map[string]string{"team": "red"}
			s.PortForwards = []vm.PortForward{{Name: "ssh", Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 2222, GuestPort: 22}}
			s.SharedFolders = []vm.SharedFolder{{Name: "src", HostPath: `C:\src`, ReadOnly: true}}
			return s
		}, func(root, disk string) []string {
			return []string{
				"list vms",
				"list dvds",
				"createvm --name web --uuid ID --ostype Ubuntu_64 --register --basefolder " + root,
				"modifyvm ID --cpus 2 --memory 1024 --vram 16 --graphicscontroller vmsvga --firmware bios --audio-enabled off --boot1 dvd --boot2 disk --boot3 none --boot4 none --rtc-use-utc on --nic1 nat",
				"storagectl ID --name SATA --add sata --controller IntelAhci --portcount 4 --bootable on",
				"createmedium disk --filename " + disk + " --size 10240 --format VDI",
				"storageattach ID --storagectl SATA --port 0 --device 0 --type hdd --medium " + disk,
				`storageattach ID --storagectl SATA --port 1 --device 0 --type dvddrive --medium C:\isos\ubuntu.iso`,
				"modifyvm ID --natpf1 ssh,tcp,127.0.0.1,2222,,22",
				`sharedfolder add ID --name src --hostpath C:\src --readonly --automount`,
				"setextradata ID vmh/label.team red",
				"setextradata ID vmh/os_type ubuntu",
				`setextradata ID vmh/registered_iso C:\isos\ubuntu.iso`,
				"showvminfo ID --machinereadable",
				"getextradata ID enumerate",
			}
		}},
		{"windows11 nics", func(s vm.Spec) vm.Spec {
			s.OSType = "windows11"
			s.Meta = nil
			s.NICs = []vm.NIC{{Mode: vm.NetBridged, Adapter: "Intel(R) Ethernet", Model: "82545EM", MAC: "08:00:27:aa:bb:cc"}, {Mode: vm.NetInternal}}
			return s
		}, func(root, disk string) []string {
			return []string{
				"list vms",
				"createvm --name web --uuid ID --ostype Windows11_64 --register --basefolder " + root,
				"modifyvm ID --cpus 2 --memory 1024 --vram 128 --graphicscontroller vboxsvga --firmware efi --audio-enabled off --boot1 dvd --boot2 disk --boot3 none --boot4 none --tpm-type 2.0" +
					" --nic1 bridged --bridge-adapter1 Intel(R) Ethernet --nic-type1 82545EM --mac-address1 080027aabbcc --nic2 intnet --intnet2 intnet",
				"storagectl ID --name SATA --add sata --controller IntelAhci --portcount 4 --bootable on",
				"createmedium disk --filename " + disk + " --size 10240 --format VDI",
				"storageattach ID --storagectl SATA --port 0 --device 0 --type hdd --medium " + disk,
				"showvminfo ID --machinereadable",
				"getextradata ID enumerate",
			}
		}},
		{"disk image", func(s vm.Spec) vm.Spec {
			s.DiskImage = `C:\images\noble.vmdk`
			s.DiskGB = 2
			return s
		}, func(root, disk string) []string {
			return []string{
				"list vms",
				"createvm --name web --uuid ID --ostype Ubuntu_64 --register --basefolder " + root,
				"modifyvm ID --cpus 2 --memory 1024 --vram 16 --graphicscontroller vmsvga --firmware bios --audio-enabled off --boot1 dvd --boot2 disk --boot3 none --boot4 none --rtc-use-utc on --nic1 nat",
				"storagectl ID --name SATA --add sata --controller IntelAhci --portcount 4 --bootable on",
				"list hdds",
				`clonemedium disk C:\images\noble.vmdk ` + disk + " --format VDI",
				`closemedium disk C:\images\noble.vmdk`,
				"showmediuminfo disk " + disk,
				"modifymedium disk " + disk + " --resize 2048",
				"storageattach ID --storagectl SATA --port 0 --device 0 --type hdd --medium " + disk,
				"setextradata ID vmh/os_type ubuntu",
				"showvminfo ID --machinereadable",
				"getextradata ID enumerate",
			}
		}},
		{"registered disk image", func(s vm.Spec) vm.Spec {
			s.DiskImage = `C:\vms\demo-1\demo-1-disk0.vdi`
			s.DiskGB = 0
			return s
		}, func(root, disk string) []string {
			return []string{
				"list vms",
				"createvm --name web --uuid ID --ostype Ubuntu_64 --register --basefolder " + root,
				"modifyvm ID --cpus 2 --memory 1024 --vram 16 --graphicscontroller vmsvga --firmware bios --audio-enabled off --boot1 dvd --boot2 disk --boot3 none --boot4 none --rtc-use-utc on --nic1 nat",
				"storagectl ID --name SATA --add sata --controller IntelAhci --portcount 4 --bootable on",
				"list hdds",
				`clonemedium disk C:\vms\demo-1\demo-1-disk0.vdi ` + disk + " --format VDI",
				"storageattach ID --storagectl SATA --port 0 --device 0 --type hdd --medium " + disk,
				"setextradata ID vmh/os_type ubuntu",
				"showvminfo ID --machinereadable",
				"getextradata ID enumerate",
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := createFake(t)
			p := detachedProvider(t, f)
			m, err := p.Create(cancellable(t), c.spec(base))
			if err != nil {
				t.Fatal(err)
			}
			id := createdID(t, f)
			if m.ID != id {
				t.Fatalf("machine = %+v, created %s", m, id)
			}
			want := c.want(p.root, filepath.Join(p.root, "web", "web.vdi"))
			if got := normalized(f, id); !reflect.DeepEqual(got, want) {
				t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

func TestCreateCloudInit(t *testing.T) {
	f := createFake(t)
	p := newProvider(t, f)
	dir := filepath.Join(p.root, "ci")
	seed := filepath.Join(dir, "seed.iso")
	spec := vm.Spec{
		Name: "ci", OSType: "Ubuntu_64", CPUs: 1, MemoryMB: 512, DiskGB: 1,
		CloudInit: &vm.CloudInit{User: "vmh", SSHAuthorizedKeys: []string{"ssh-ed25519 AAAA test"}},
	}
	if _, err := p.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	id := createdID(t, f)
	modify := f.Find("modifyvm " + id + " --cpus")
	want := "modifyvm " + id + " --cpus 1 --memory 512 --vram 16 --graphicscontroller vmsvga --firmware bios --audio-enabled off" +
		" --boot1 dvd --boot2 disk --boot3 none --boot4 none --rtc-use-utc on --nic1 nat --nic-type1 virtio" +
		" --uart1 0x3F8 4 --uartmode1 file " + filepath.Join(dir, "serial.log")
	if len(modify) != 1 || modify[0].String() != want {
		t.Fatalf("modifyvm = %v", modify)
	}
	if !f.Called("storageattach " + id + " --storagectl SATA --port 2 --device 0 --type dvddrive --medium " + seed) {
		t.Fatalf("seed not attached: %q", commands(f))
	}
	if _, err := os.Stat(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestCloudInitNICModel(t *testing.T) {
	ci := &vm.CloudInit{User: "vmh"}
	cases := []struct {
		name string
		spec vm.Spec
		want string
	}{
		{"default nic", vm.Spec{OSType: "Ubuntu_64", CloudInit: ci}, " --nic1 nat --nic-type1 virtio"},
		{"nat nic without model", vm.Spec{OSType: "Ubuntu_64", CloudInit: ci, NICs: []vm.NIC{{Mode: vm.NetNAT}}}, " --nic1 nat --nic-type1 virtio"},
		{"only nic 1", vm.Spec{CloudInit: ci, NICs: []vm.NIC{{}, {Mode: vm.NetInternal}}}, " --nic1 nat --nic2 intnet --intnet2 intnet --nic-type1 virtio"},
		{"explicit model", vm.Spec{OSType: "Ubuntu_64", CloudInit: ci, NICs: []vm.NIC{{Mode: vm.NetNAT, Model: "82540EM"}}}, " --nic1 nat --nic-type1 82540EM"},
		{"nic 1 off", vm.Spec{OSType: "Ubuntu_64", CloudInit: ci, NICs: []vm.NIC{{Mode: vm.NetNone}}}, " --nic1 none"},
		{"windows", vm.Spec{OSType: "Windows11_64", CloudInit: ci}, " --nic1 nat"},
		{"without cloud-init", vm.Spec{OSType: "Ubuntu_64"}, " --nic1 nat"},
		{"unattended", vm.Spec{OSType: "Ubuntu_64", ISO: "ubuntu.iso", Unattended: &vm.Unattended{User: "vmh"}}, " --nic1 nat"},
	}
	for _, c := range cases {
		nics, err := nicArgs(c.spec.NICs)
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Join(hardwareArgs("ID", "dir", c.spec, "", nics), " ")
		args = strings.Split(args, " --uart1 ")[0]
		if got := args[strings.Index(args, " --nic1 "):]; got != c.want {
			t.Errorf("%s: nic args %q, want %q", c.name, got, c.want)
		}
	}
}

const staleExtradata = "Key: GUI/LastCloseAction, Value: PowerOff\r\n" +
	"Key: vmh/label.old, Value: x\r\n" +
	"Key: vmh/linked_from, Value: 0b3e4233-7102-4087-bbd8-2ea3a0f123a4\r\n" +
	"Key: vmh/managed, Value: 0b3e4233-7102-4087-bbd8-2ea3a0f123a4\r\n" +
	"Key: vmh/ssh_key, Value: C:\\vmh\\keys\\virtualbox-old\r\n"

const settingsXML = `<?xml version="1.0"?>
<VirtualBox xmlns="http://www.virtualbox.org/" version="1.19-windows">
  <Machine uuid="{dbe76ac6-da70-4170-a062-1b5e513cd3ee}" name="app">
    <Hardware>%s</Hardware>
  </Machine>
</VirtualBox>
`

func fakeImport(t *testing.T, hardware string) func(fakerun.Call) (runner.Result, error) {
	return func(c fakerun.Call) (runner.Result, error) {
		if !slices.Contains(c.Args, "--dry-run") {
			if err := os.WriteFile(argAfter(c, "--settingsfile"), []byte(fmt.Sprintf(settingsXML, hardware)), 0o644); err != nil {
				return runner.Result{}, err
			}
		}
		return runner.Result{Stdout: []byte(fixture(t, "import.txt"))}, nil
	}
}

var importedLeftovers = regexp.MustCompile(`(?m)^(Forwarding\(|USBFilter|SharedFolder).*\r?\n`)

func applianceFake(t *testing.T, root string) *fakerun.Fake {
	return importFake(t, root, fixture(t, "showvminfo-imported.txt"), staleExtradata)
}

func importFake(t *testing.T, root, info, extradata string) *fakerun.Fake {
	dir := filepath.Join(root, "app")
	for _, folder := range []string{`C:\vms\demo-3\`, `C:\vms\app\`} {
		info = strings.ReplaceAll(info, esc(folder), esc(dir+string(filepath.Separator)))
	}
	info = setField(setField(info, "UUID", importedID), "CfgFile", filepath.Join(dir, "app.vbox"))
	var f *fakerun.Fake
	f = createFake(t).
		OnResult("list vms",
			runner.Result{Stdout: []byte(fixture(t, "list-vms.txt"))},
			runner.Result{Stdout: []byte(`"app" {` + foreignID + "}\r\n" + fixture(t, "list-vms.txt") + `"app" {` + importedID + "}\r\n")}).
		OnFunc("import", fakeImport(t, "")).
		On("usbfilter", "").
		On("showvminfo "+foreignID, setField(fixture(t, "showvminfo-imported.txt"), "UUID", foreignID)).
		OnFunc("showvminfo "+importedID, func(fakerun.Call) (runner.Result, error) {
			if f.Called("modifyvm " + importedID + " --vrde off") {
				return runner.Result{Stdout: []byte(importedLeftovers.ReplaceAllString(info, ""))}, nil
			}
			return runner.Result{Stdout: []byte(info)}, nil
		}).
		OnFunc("getextradata "+importedID+" enumerate", func(fakerun.Call) (runner.Result, error) {
			if f.Called("setextradata " + importedID) {
				return runner.Result{}, nil
			}
			return runner.Result{Stdout: []byte(extradata)}, nil
		})
	return f
}

func isReset(call string) bool { return strings.HasPrefix(call, "modifyvm "+importedID+" --vrde off ") }

func TestCreateAppliance(t *testing.T) {
	root := t.TempDir()
	f := applianceFake(t, root)
	p := detachedProvider(t, f)
	p.root = root
	spec := vm.Spec{
		Name: "app", Appliance: `C:\ova\app.ova`, CPUs: 2, MemoryMB: 2048,
		PortForwards: []vm.PortForward{{Name: "ssh", Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 2223, GuestPort: 22}},
		Meta:         map[string]string{"os_type": "ubuntu"},
	}
	m, err := p.Create(cancellable(t), spec)
	if err != nil {
		t.Fatal(err)
	}
	args := `C:\ova\app.ova --vsys 0 --vmname app --settingsfile ` + filepath.Join(root, "app", "app.vbox") + " --basefolder " + root + " --cpus 2 --memory 2048"
	want := []string{
		"list vms",
		"import " + strings.Replace(args, " ", " --dry-run ", 1),
		"import " + args,
		"list vms",
		"showvminfo " + foreignID + " --machinereadable",
		"showvminfo " + importedID + " --machinereadable",
		"showvminfo " + importedID + " --machinereadable",
		"reset",
		"getextradata " + importedID + " enumerate",
		"setextradata " + importedID + " GUI/LastCloseAction",
		"setextradata " + importedID + " vmh/label.old",
		"setextradata " + importedID + " vmh/linked_from",
		"setextradata " + importedID + " vmh/managed",
		"setextradata " + importedID + " vmh/ssh_key",
		"showvminfo " + importedID + " --machinereadable",
		"getextradata " + importedID + " enumerate",
		"showvminfo " + importedID + " --machinereadable",
		"modifyvm " + importedID + " --natpf1 ssh,tcp,127.0.0.1,2223,,22",
		"setextradata " + importedID + " vmh/os_type ubuntu",
		"showvminfo " + importedID + " --machinereadable",
		"getextradata " + importedID + " enumerate",
	}
	got := commands(f)
	if i := slices.IndexFunc(got, isReset); i >= 0 {
		for _, rule := range []string{"persist", "ssh", "udp_5353_53"} {
			if !strings.Contains(got[i], " --natpf1 delete "+rule+" ") {
				t.Errorf("imported forward %s kept: %s", rule, got[i])
			}
		}
		got[i] = "reset"
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
	}
	if m.ID != importedID {
		t.Fatalf("machine = %+v", m)
	}
}

func TestCreateApplianceResetsHostSettings(t *testing.T) {
	root := t.TempDir()
	f := importFake(t, root, fixture(t, "showvminfo-imported-evil.txt"), fixture(t, "extradata-imported.txt")).
		OnFunc("import", fakeImport(t, `<TrustedPlatformModule type="Host" location=""/>`))
	p := newProvider(t, f)
	p.root = root
	spec := vm.Spec{Name: "app", Appliance: `C:\ova\app.ova`, PortForwards: []vm.PortForward{{Name: "ssh", HostIP: "127.0.0.1", HostPort: 2222, GuestPort: 22}}}
	if _, err := p.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	got := commands(f)
	reset := slices.IndexFunc(got, isReset)
	configured := slices.Index(got, "modifyvm "+importedID+" --natpf1 ssh,tcp,127.0.0.1,2222,,22")
	if reset < 0 || configured < reset {
		t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
	}
	for _, flag := range []string{"--vrde off", "--recording off", "--teleporter off", "--snapshot-folder default", "--clipboard-mode disabled",
		"--uart1 off", "--uart4 off", "--lpt1 off", "--nic-trace1 off", "--nat-localhostreachable2 off",
		"--natpf1 delete evil", "--natpf2 delete lan", "--tpm-type none"} {
		if !strings.Contains(got[reset]+" ", " "+flag+" ") {
			t.Errorf("reset lacks %s: %s", flag, got[reset])
		}
	}
	call := f.Find(got[reset])[0]
	if !slices.Contains(call.Args, "--nat-tftp-prefix8") || argAfter(call, "--nat-tftp-prefix8") != "" {
		t.Errorf("tftp prefix not reset: %q", call.Args)
	}
	for _, want := range []string{
		"usbfilter remove 0 --target " + importedID,
		"sharedfolder remove " + importedID + " --name host_share",
		"setextradata " + importedID + " VBoxInternal/Devices/serial/0/LUN#0/AttachedDriver/Config/Location",
		"setextradata " + importedID + " VBoxInternal/Devices/serial/0/LUN#0/AttachedDriver/Driver",
		"setextradata " + importedID + " VBoxInternal/Devices/serial/0/LUN#0/Driver",
		"setextradata " + importedID + " VBoxInternal2/SharedFoldersEnableSymlinksCreate/hostshare",
		"setextradata " + importedID + " vmh/managed",
	} {
		if i := slices.Index(got, want); i < reset || i > configured {
			t.Errorf("%q missing between the import and the configuration:\n%s", want, strings.Join(got, "\n"))
		}
	}
	if n := len(f.Find("usbfilter remove")); n != 2 {
		t.Errorf("usb filters removed = %d, want 2", n)
	}
}

func TestCreateApplianceEjectsForeignMedia(t *testing.T) {
	root := t.TempDir()
	iso := filepath.Join(t.TempDir(), "user.iso")
	f := importFake(t, root, setField(fixture(t, "showvminfo-imported.txt"), `"SATA-1-0"`, iso), "")
	p := newProvider(t, f)
	p.root = root
	if _, err := p.Create(context.Background(), vm.Spec{Name: "app", Appliance: `C:\ova\app.ova`}); err != nil {
		t.Fatal(err)
	}
	if !f.Called("storageattach " + importedID + " --storagectl SATA --port 1 --device 0 --medium emptydrive") {
		t.Fatalf("foreign dvd image left attached: %q", commands(f))
	}
}

func TestCreateApplianceRefuses(t *testing.T) {
	imported := fixture(t, "showvminfo-imported.txt")
	cases := []struct {
		name      string
		info      string
		extradata string
		dryRun    string
		discarded string
	}{
		{"two vms", imported, "", "import-dryrun-two.txt", ""},
		{"snapshots", imported + `CurrentSnapshotName="evil"` + "\r\n" + `CurrentSnapshotUUID="11111111-2222-4333-8444-555555555555"` + "\r\n", "", "", "unregistervm " + importedID},
		{"foreign disk", setField(imported, `"SATA-0-0"`, filepath.Join(t.TempDir(), "user.vdi")), "", "", "unregistervm " + importedID},
		{"extradata that cannot be removed", imported, "Key: VBoxInternal/PDM/Devices/x, Value: y/Path, Value: C:\\x.dll\r\n", "", "unregistervm " + importedID + " --delete"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			f := importFake(t, root, c.info, "")
			f.OnFunc("getextradata "+importedID+" enumerate", func(fakerun.Call) (runner.Result, error) {
				return runner.Result{Stdout: []byte(c.extradata)}, nil
			})
			if c.dryRun != "" {
				f.On("import C:\\ova\\app.ova --dry-run", fixture(t, c.dryRun))
			}
			p := newProvider(t, f)
			p.root = root
			if _, err := p.Create(context.Background(), vm.Spec{Name: "app", Appliance: `C:\ova\app.ova`}); !errors.Is(err, vm.ErrInvalid) {
				t.Fatalf("err = %v", err)
			}
			var unregistered []string
			for _, call := range commands(f) {
				if strings.HasPrefix(call, "unregistervm") {
					unregistered = append(unregistered, call)
				}
			}
			if want := slices.DeleteFunc([]string{c.discarded}, func(s string) bool { return s == "" }); !slices.Equal(unregistered, want) {
				t.Fatalf("unregistered %q, want %q", unregistered, want)
			}
			if c.dryRun != "" && len(f.Find("import")) != 1 {
				t.Fatalf("appliance imported: %q", commands(f))
			}
			if _, err := os.Stat(filepath.Join(root, "app")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("vm dir left behind: %v", err)
			}
		})
	}
}

func TestCreateApplianceCloudInit(t *testing.T) {
	cases := []struct {
		name     string
		ostype   string
		nics     []vm.NIC
		hardware string
	}{
		{"appliance nics kept", "Ubuntu (64-bit)", nil, " --firmware efi"},
		{"nat nic becomes virtio", "Ubuntu (64-bit)", []vm.NIC{{Mode: vm.NetNAT}}, " --firmware efi --nic1 nat --nic-type1 virtio"},
		{"windows appliance keeps its nic model", "Windows 10 (64-bit)", []vm.NIC{{Mode: vm.NetNAT}}, " --firmware efi --nic1 nat"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			info := setField(fixture(t, "showvminfo-imported.txt"), "ostype", c.ostype)
			f := importFake(t, root, info, staleExtradata).On("getextradata "+importedID+" enumerate", "")
			p := newProvider(t, f)
			p.root = root
			dir := filepath.Join(root, "app")
			spec := vm.Spec{Name: "app", Appliance: `C:\ova\app.ova`, Firmware: "EFI", NICs: c.nics, CloudInit: &vm.CloudInit{User: "vmh", Password: "pw"}}
			if _, err := p.Create(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			want := []string{
				"showvminfo " + importedID + " --machinereadable",
				"modifyvm " + importedID + c.hardware + " --uart1 0x3F8 4 --uartmode1 file " + filepath.Join(dir, "serial.log"),
				"storageattach " + importedID + " --storagectl SATA --port 3 --device 0 --type dvddrive --medium " + filepath.Join(dir, "seed.iso"),
				"showvminfo " + importedID + " --machinereadable",
				"getextradata " + importedID + " enumerate",
			}
			got := commands(f)
			if len(got) < len(want) || !reflect.DeepEqual(got[len(got)-len(want):], want) {
				t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
			}
		})
	}
}

func TestSeedSlot(t *testing.T) {
	imported := parseVMInfo(fixture(t, "showvminfo-imported.txt")).fields
	if ctl, port, setup := seedSlot(imported); ctl != "SATA" || port != 3 || setup != nil {
		t.Errorf("imported: %s %d %v", ctl, port, setup)
	}
	full := map[string]string{
		"storagecontrollername0": "IDE", "storagecontrollertype0": "PIIX4", "storagecontrollerportcount0": "2",
		"storagecontrollername1": "SATA Controller", "storagecontrollertype1": "IntelAhci", "storagecontrollerportcount1": "1",
		"SATA Controller-0-0": `C:\vms\a\disk.vmdk`,
	}
	ctl, port, setup := seedSlot(full)
	if ctl != "SATA Controller" || port != 1 || !reflect.DeepEqual(setup, []string{"--name", "SATA Controller", "--portcount", "2"}) {
		t.Errorf("full: %s %d %v", ctl, port, setup)
	}
	ctl, port, setup = seedSlot(map[string]string{"storagecontrollername0": "IDE", "storagecontrollertype0": "PIIX4"})
	if ctl != "SATA" || port != 0 || len(setup) == 0 || setup[2] != "--add" {
		t.Errorf("none: %s %d %v", ctl, port, setup)
	}
}

func TestCreateUnattended(t *testing.T) {
	var secret, password string
	f := createFake(t).OnFunc("unattended", func(c fakerun.Call) (runner.Result, error) {
		for _, a := range c.Args {
			if v, ok := strings.CutPrefix(a, "--user-password-file="); ok {
				secret = v
				data, err := os.ReadFile(v)
				password = string(data)
				return runner.Result{}, err
			}
		}
		return runner.Result{}, nil
	})
	p := newProvider(t, f)
	spec := vm.Spec{
		Name: "win", OSType: "Windows11_64", CPUs: 2, MemoryMB: 4096, DiskGB: 64, ISO: `C:\isos\win11.iso`,
		Unattended: &vm.Unattended{User: "agent", Password: "s3cret pass", FullName: "Agent Smith", Locale: "en_US", TimeZone: "UTC", ProductKey: "AAAAA-BBBBB", InstallAdditions: true, PostInstall: "shutdown /r"},
	}
	if _, err := p.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	id := createdID(t, f)
	if password != "s3cret pass" {
		t.Fatalf("password file held %q", password)
	}
	if _, err := os.Stat(secret); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("password file not removed: %v", err)
	}
	if f.Called("storageattach " + id + " --storagectl SATA --port 1") {
		t.Fatal("unattended install attaches the iso itself")
	}
	calls := f.Find("unattended")
	want := "unattended install " + id + " --iso=C:\\isos\\win11.iso --user=agent --user-password-file=" + secret +
		" --full-user-name=Agent Smith --hostname=win.local --locale=en_US --time-zone=UTC --key=AAAAA-BBBBB --install-additions --post-install-command=shutdown /r"
	if len(calls) != 1 || calls[0].String() != want {
		t.Fatalf("unattended = %v", calls)
	}
	if !f.Called("setextradata " + id + ` vmh/registered_iso C:\isos\win11.iso`) {
		t.Fatalf("iso registration not recorded: %q", commands(f))
	}
	for _, c := range f.Calls() {
		if strings.Contains(c.String(), "s3cret") {
			t.Fatalf("password leaked into %q", c)
		}
	}
}

func TestCreateRejects(t *testing.T) {
	cases := []struct {
		name string
		spec vm.Spec
		err  error
	}{
		{"name taken", vm.Spec{Name: "demo-1", DiskGB: 1}, vm.ErrExists},
		{"no disk size", vm.Spec{Name: "x"}, vm.ErrInvalid},
		{"bad name", vm.Spec{Name: "--x", DiskGB: 1}, vm.ErrInvalid},
		{"name vbox would rewrite", vm.Spec{Name: "a{b}", DiskGB: 1}, vm.ErrInvalid},
		{"name with a path", vm.Spec{Name: `..\x`, DiskGB: 1}, vm.ErrInvalid},
		{"forward without nat", vm.Spec{Name: "x", DiskGB: 1, NICs: []vm.NIC{{Mode: vm.NetHostOnly, Adapter: "vboxnet0"}},
			PortForwards: []vm.PortForward{{Name: "ssh", HostPort: 2222, GuestPort: 22}}}, vm.ErrInvalid},
		{"bridged without adapter", vm.Spec{Name: "x", DiskGB: 1, NICs: []vm.NIC{{Mode: vm.NetBridged}}}, vm.ErrInvalid},
		{"custom network", vm.Spec{Name: "x", DiskGB: 1, NICs: []vm.NIC{{Mode: vm.NetCustom, Adapter: "vmnet8"}}}, vm.ErrUnsupported},
		{"too many nics", vm.Spec{Name: "x", DiskGB: 1, NICs: make([]vm.NIC, 9)}, vm.ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := createFake(t)
			p := newProvider(t, f)
			if _, err := p.Create(context.Background(), c.spec); !errors.Is(err, c.err) {
				t.Fatalf("err = %v, want %v", err, c.err)
			}
			if f.Called("createvm") {
				t.Fatal("vm was created")
			}
			if entries, _ := os.ReadDir(p.root); len(entries) != 0 {
				t.Fatalf("root was touched: %v", entries)
			}
		})
	}

	p := New(Options{VBoxManage: "VBoxManage", Runner: createFake(t)})
	if _, err := p.Create(context.Background(), vm.Spec{Name: "x", DiskGB: 1}); !errors.Is(err, vm.ErrInvalid) {
		t.Fatalf("no root: err = %v", err)
	}
}

func TestCreateKeepsExistingFolder(t *testing.T) {
	f := createFake(t)
	p := newProvider(t, f)
	writeFiles(t, filepath.Join(p.root, "web"), "web.vdi", "notes.txt")
	if _, err := p.Create(context.Background(), vm.Spec{Name: "web", DiskGB: 1}); !errors.Is(err, vm.ErrExists) {
		t.Fatalf("err = %v", err)
	}
	if f.Called("createvm") || f.Called("closemedium") {
		t.Fatalf("calls = %q", commands(f))
	}
	for _, name := range []string{"web.vdi", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(p.root, "web", name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestCreateCleanupOnFailure(t *testing.T) {
	f := createFake(t).OnResult("storageattach", failure(t, "err-medium-attached.txt"))
	p := newProvider(t, f)
	dir := filepath.Join(p.root, "web")
	disk := filepath.Join(dir, "web.vdi")
	_, err := p.Create(context.Background(), vm.Spec{Name: "web", OSType: "Ubuntu_64", DiskGB: 1})
	if !errors.Is(err, vm.ErrInvalidState) {
		t.Fatalf("err = %v", err)
	}
	id := createdID(t, f)
	for _, want := range []string{"unregistervm " + id + " --delete", "closemedium disk " + disk + " --delete"} {
		if !f.Called(want) {
			t.Errorf("missing %q in %q", want, commands(f))
		}
	}
	if f.Called("setextradata") {
		t.Error("configuration continued after a failure")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("vm dir left behind: %v", err)
	}
}

func TestCreateCleanupRemovesSeed(t *testing.T) {
	f := createFake(t).
		OnFunc("createvm", func(c fakerun.Call) (runner.Result, error) {
			dir := filepath.Join(argAfter(c, "--basefolder"), argAfter(c, "--name"))
			writeFiles(t, dir, "ci.vbox", "Logs/VBox.log")
			return runner.Result{}, nil
		}).
		OnResult("setextradata", failure(t, "err-not-found.txt"))
	p := newProvider(t, f)
	dir := filepath.Join(p.root, "ci")
	spec := vm.Spec{Name: "ci", DiskGB: 1, CloudInit: &vm.CloudInit{User: "vmh", Password: "pw"}, Meta: map[string]string{"os_type": "ubuntu"}}
	if _, err := p.Create(context.Background(), spec); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	id := createdID(t, f)
	for _, want := range []string{"unregistervm " + id + " --delete", "closemedium dvd " + filepath.Join(dir, "seed.iso")} {
		if !f.Called(want) {
			t.Errorf("missing %q in %q", want, commands(f))
		}
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("vm dir left behind: %v", err)
	}
}

func TestCreateCancelledWhileCreating(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := createFake(t).OnFunc("createvm", func(fakerun.Call) (runner.Result, error) {
		cancel()
		return runner.Result{}, nil
	})
	p := detachedProvider(t, f)
	if _, err := p.Create(ctx, vm.Spec{Name: "web", DiskGB: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if !f.Called("unregistervm " + createdID(t, f) + " --delete") {
		t.Fatalf("vm not discarded: %q", commands(f))
	}
	if _, err := os.Stat(filepath.Join(p.root, "web")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("vm dir left behind: %v", err)
	}
}

func TestCreateCancelledWhileImporting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	f := applianceFake(t, root).OnFunc("import", func(c fakerun.Call) (runner.Result, error) {
		cancel()
		return fakeImport(t, "")(c)
	})
	p := detachedProvider(t, f)
	p.root = root
	if _, err := p.Create(ctx, vm.Spec{Name: "app", Appliance: `C:\ova\app.ova`}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if !f.Called("unregistervm " + importedID + " --delete") {
		t.Fatalf("imported vm not discarded: %q", commands(f))
	}
	for _, c := range commands(f) {
		if strings.Contains(c, foreignID) && !strings.HasPrefix(c, "showvminfo ") {
			t.Fatalf("foreign vm touched: %q", c)
		}
	}
}

func TestCreateISORegistration(t *testing.T) {
	const iso = `C:\isos\ubuntu.iso`
	known := "UUID:           a7539242-e73d-45f9-ab85-d206c9746be3\r\nState:          created\r\nType:           readonly\r\nLocation:       " + iso + "\r\nStorage format: RAW\r\n\r\n"
	cases := []struct {
		name, dvds string
		fail       bool
		ours       bool
	}{
		{"new iso", "", false, true},
		{"iso the user registered", known, false, false},
		{"new iso, failed create", "", true, true},
		{"iso the user registered, failed create", known, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := createFake(t).On("list dvds", c.dvds)
			if c.fail {
				f.OnResult("setextradata", failure(t, "err-not-found.txt"))
			}
			spec := vm.Spec{Name: "web", DiskGB: 1, ISO: iso, Meta: map[string]string{"os_type": "ubuntu"}}
			_, err := newProvider(t, f).Create(context.Background(), spec)
			if (err != nil) != c.fail {
				t.Fatalf("err = %v", err)
			}
			id := createdID(t, f)
			if got := f.Called("setextradata " + id + " vmh/registered_iso " + iso); got != (c.ours && !c.fail) {
				t.Fatalf("recorded = %v: %q", got, commands(f))
			}
			if got := f.Called("closemedium dvd " + iso); got != (c.ours && c.fail) {
				t.Fatalf("iso closed = %v: %q", got, commands(f))
			}
		})
	}
}

func seededInfo(id, name, dir, state string) string {
	return vmInfoText(
		`name="`+name+`"`,
		`UUID="`+id+`"`,
		`CfgFile="`+esc(filepath.Join(dir, name+".vbox"))+`"`,
		`VMState="`+state+`"`,
		`storagecontrollername0="SATA"`,
		`storagecontrollertype0="IntelAhci"`,
		`storagecontrollerportcount0="4"`,
		`"SATA-0-0"="`+esc(filepath.Join(dir, name+".vdi"))+`"`,
		`"SATA-ImageUUID-0-0"="09dd50f1-6f1f-4b2e-ab11-86b57ee9fe1e"`,
		`"SATA-1-0"="emptydrive"`,
		`"SATA-IsEjected-1-0"="off"`,
		`"SATA-2-0"="`+esc(filepath.Join(dir, "seed.iso"))+`"`,
		`"SATA-ImageUUID-2-0"="a6f3de49-75cc-4ec1-967d-998836efc591"`,
		`"SATA-IsEjected-2-0"="off"`,
		`"SATA-3-0"="none"`,
		`nic1="nat"`,
		`uart1="0x03f8,4"`,
		`uartmode1="file,`+filepath.Join(dir, "serial.log")+`"`,
	)
}

func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const noValue = "No value set!\r\n"

func TestDelete(t *testing.T) {
	f := newFake(t)
	p := newProvider(t, f)
	dir := filepath.Join(p.root, "web")
	writeFiles(t, dir, "seed.iso", "serial.log", "Logs/VBox.log", "Unattended-x-aux-iso.viso")
	f.On("showvminfo", seededInfo(demoID, "web", dir, "poweroff")).On("getextradata", noValue).On("unregistervm", "").On("closemedium", "")
	if err := p.Delete(context.Background(), "web"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"showvminfo web --machinereadable",
		"getextradata " + demoID + " vmh/registered_iso",
		"unregistervm " + demoID + " --delete",
		"closemedium dvd " + filepath.Join(dir, "seed.iso"),
	}
	if got := commands(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %q", got)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("vm dir left behind: %v", err)
	}
}

func TestDeleteClosesOnlyImagesItRegistered(t *testing.T) {
	iso := filepath.Join(t.TempDir(), "isos", "ubuntu.iso")
	for _, registered := range []bool{false, true} {
		f := newFake(t)
		p := newProvider(t, f)
		dir := filepath.Join(p.root, "web")
		info := setField(seededInfo(demoID, "web", dir, "poweroff"), `"SATA-1-0"`, iso)
		extradata := noValue
		if registered {
			extradata = "Value: " + iso + "\r\n"
		}
		f.On("showvminfo", info).On("getextradata", extradata).On("unregistervm", "").On("closemedium", "")
		if err := p.Delete(context.Background(), "web"); err != nil {
			t.Fatal(err)
		}
		if !f.Called("closemedium dvd " + filepath.Join(dir, "seed.iso")) {
			t.Fatalf("seed not closed: %q", commands(f))
		}
		if got := f.Called("closemedium dvd " + iso); got != registered {
			t.Fatalf("registered %v: iso closed = %v", registered, got)
		}
	}
}

func TestDeleteKeepsUnknownFilesAndForeignDirs(t *testing.T) {
	f := newFake(t)
	p := newProvider(t, f)
	inside := filepath.Join(p.root, "web")
	writeFiles(t, inside, "seed.iso", "notes.txt")
	outside := filepath.Join(t.TempDir(), "web")
	writeFiles(t, outside, "seed.iso", "serial.log")
	f.On("getextradata", noValue).On("unregistervm", "").On("closemedium", "")

	f.On("showvminfo", seededInfo(demoID, "web", inside, "poweroff"))
	if err := p.Delete(context.Background(), "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(inside, "notes.txt")); err != nil {
		t.Fatalf("unknown file removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inside, "seed.iso")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("seed left behind: %v", err)
	}

	f.On("showvminfo", seededInfo(demoID, "web", outside, "poweroff"))
	if err := p.Delete(context.Background(), "web"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"seed.iso", "serial.log"} {
		if _, err := os.Stat(filepath.Join(outside, name)); err != nil {
			t.Fatalf("file outside root touched: %v", err)
		}
	}
}

func TestDeleteWaitsForSessionUnlock(t *testing.T) {
	f := newFake(t).
		On("showvminfo", fixture(t, "showvminfo-stopped.txt")).
		On("getextradata", noValue).
		OnResult("unregistervm", failure(t, "err-unregister-locked.txt"), runner.Result{}).
		On("closemedium", "")
	if err := newProvider(t, f).Delete(context.Background(), "demo-1"); err != nil {
		t.Fatal(err)
	}
	if got := len(f.Find("unregistervm")); got != 2 {
		t.Fatalf("unregistervm attempts = %d", got)
	}
}

func TestDeleteRefusesRunning(t *testing.T) {
	running := fixture(t, "showvminfo-running.txt")
	for _, info := range []string{running, fixture(t, "showvminfo-paused.txt"), strings.Replace(running, `VMState="running"`, `VMState="gurumeditation"`, 1)} {
		f := newFake(t).On("showvminfo", info).On("getextradata", noValue).On("unregistervm", "")
		if err := newProvider(t, f).Delete(context.Background(), "demo-1"); !errors.Is(err, vm.ErrInvalidState) {
			t.Fatalf("err = %v", err)
		}
		if f.Called("unregistervm") {
			t.Fatalf("unregistervm called: %q", commands(f))
		}
	}
}

func TestDeleteParentOfLinkedClone(t *testing.T) {
	f := newFake(t).On("showvminfo", fixture(t, "showvminfo-snapshots.txt")).On("getextradata", noValue).OnResult("unregistervm", failure(t, "err-parent-delete.txt"))
	err := newProvider(t, f).Delete(context.Background(), "demo-1")
	if !errors.Is(err, vm.ErrInvalidState) || !strings.Contains(err.Error(), "linked clones") || !strings.Contains(err.Error(), "child media") {
		t.Fatalf("err = %v", err)
	}
}

func cloneFake(t *testing.T, srcInfo, cloneInfo string) *fakerun.Fake {
	var cloneID string
	return newFake(t).
		On("list vms", fixture(t, "list-vms-special.txt")).
		OnFunc("showvminfo", func(c fakerun.Call) (runner.Result, error) {
			if c.Args[1] == cloneID {
				return runner.Result{Stdout: []byte(setField(cloneInfo, "UUID", cloneID))}, nil
			}
			return runner.Result{Stdout: []byte(srcInfo)}, nil
		}).
		OnFunc("clonevm", func(c fakerun.Call) (runner.Result, error) {
			cloneID = argAfter(c, "--uuid")
			return runner.Result{Stdout: []byte(fixture(t, "clonevm.txt"))}, nil
		}).
		On("storageattach", "").
		On("modifyvm", "").
		On("unregistervm", "").
		On("closemedium", "").
		On("getextradata", "")
}

func TestClone(t *testing.T) {
	cases := []struct {
		name string
		opts vm.CloneOptions
		want string
	}{
		{"full", vm.CloneOptions{Name: "copy"}, ""},
		{"full from snapshot", vm.CloneOptions{Name: "copy", Snapshot: "s2"}, " --snapshot s2"},
		{"linked current", vm.CloneOptions{Name: "copy", Linked: true}, " --options link --snapshot 4edf60df-5090-4366-9eca-7c6268cc8fe3"},
		{"linked named", vm.CloneOptions{Name: "copy", Linked: true, Snapshot: "s1"}, " --options link --snapshot s1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := cloneFake(t, fixture(t, "showvminfo-snapshots.txt"), fixture(t, "showvminfo-linked.txt"))
			p := detachedProvider(t, f)
			m, err := p.Clone(cancellable(t), "demo-1", c.opts)
			if err != nil {
				t.Fatal(err)
			}
			id := createdID(t, f)
			if m.ID != id {
				t.Fatalf("machine = %+v, created %s", m, id)
			}
			want := "clonevm " + demoID + " --name copy --uuid " + id + " --register --basefolder " + p.root + c.want
			if got := f.Find("clonevm"); len(got) != 1 || got[0].String() != want {
				t.Fatalf("clonevm = %v, want %q", got, want)
			}
			if f.Called("storageattach") || f.Called("modifyvm") || len(f.Find("list vms")) != 1 {
				t.Fatalf("calls = %q", commands(f))
			}
		})
	}
}

func TestCloneRejects(t *testing.T) {
	f := cloneFake(t, fixture(t, "showvminfo-stopped.txt"), "")
	p := newProvider(t, f)
	if _, err := p.Clone(context.Background(), "demo-1", vm.CloneOptions{Name: "copy", Linked: true}); !errors.Is(err, vm.ErrInvalidState) {
		t.Fatalf("no snapshot: err = %v", err)
	}
	if _, err := p.Clone(context.Background(), "demo-1", vm.CloneOptions{Name: "demo-3"}); !errors.Is(err, vm.ErrExists) {
		t.Fatalf("name taken: err = %v", err)
	}
	if _, err := p.Clone(context.Background(), "demo-1", vm.CloneOptions{Name: "a/b"}); !errors.Is(err, vm.ErrInvalid) {
		t.Fatalf("bad name: err = %v", err)
	}
	if f.Called("clonevm") {
		t.Fatal("clonevm called")
	}
	if entries, _ := os.ReadDir(p.root); len(entries) != 0 {
		t.Fatalf("root was touched: %v", entries)
	}
}

func writeSeed(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cloudinit.WriteSeed(filepath.Join(dir, seedName), vm.CloudInit{User: "vmh"}, "vmh-"+name, name); err != nil {
		t.Fatal(err)
	}
}

func clonedInfo(id, srcDir, dir, state string) string {
	return setField(seededInfo(id, "copy", srcDir, state), "CfgFile", filepath.Join(dir, "copy.vbox"))
}

func TestCloneReseedsAndMovesSerial(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "demo-1")
	dir := filepath.Join(root, "copy")
	writeSeed(t, srcDir, "demo-1")
	f := cloneFake(t, seededInfo(demoID, "demo-1", srcDir, "poweroff"), clonedInfo(linkedID, srcDir, dir, "poweroff"))
	p := newProvider(t, f)
	p.root = root
	if _, err := p.Clone(context.Background(), "demo-1", vm.CloneOptions{Name: "copy"}); err != nil {
		t.Fatal(err)
	}
	id := createdID(t, f)
	seed := filepath.Join(dir, "seed.iso")
	want := []string{
		"storageattach " + id + " --storagectl SATA --port 2 --device 0 --type dvddrive --medium " + seed,
		"modifyvm " + id + " --uartmode1 file " + filepath.Join(dir, "serial.log"),
	}
	got := append(f.Find("storageattach"), f.Find("modifyvm")...)
	if len(got) != 2 || got[0].String() != want[0] || got[1].String() != want[1] {
		t.Fatalf("fixups = %v", got)
	}
	data, err := os.ReadFile(seed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"vmh-copy"`)) || bytes.Contains(data, []byte("vmh-demo-1")) {
		t.Fatal("clone seed keeps the source instance id")
	}
}

func TestCloneOfOnlineSnapshotDropsSavedState(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "demo-1")
	writeSeed(t, srcDir, "demo-1")
	f := cloneFake(t, seededInfo(demoID, "demo-1", srcDir, "running"), clonedInfo(linkedID, srcDir, filepath.Join(root, "copy"), "saved")).On("discardstate", "")
	p := newProvider(t, f)
	p.root = root
	if _, err := p.Clone(context.Background(), "demo-1", vm.CloneOptions{Name: "copy", Snapshot: "s1"}); err != nil {
		t.Fatal(err)
	}
	got := commands(f)
	i := slices.Index(got, "discardstate "+createdID(t, f))
	if i < 0 || i+2 >= len(got) || !strings.HasPrefix(got[i+1], "storageattach") || !strings.HasPrefix(got[i+2], "modifyvm") {
		t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
	}
}

func TestCloneCleanupOnFailure(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "demo-1")
	writeSeed(t, srcDir, "demo-1")
	source, err := os.ReadFile(filepath.Join(srcDir, seedName))
	if err != nil {
		t.Fatal(err)
	}
	f := cloneFake(t, seededInfo(demoID, "demo-1", srcDir, "poweroff"), clonedInfo(linkedID, srcDir, filepath.Join(root, "copy"), "poweroff")).
		OnResult("storageattach", failure(t, "err-medium-attached.txt"))
	p := newProvider(t, f)
	p.root = root
	if _, err := p.Clone(context.Background(), "demo-1", vm.CloneOptions{Name: "copy"}); !errors.Is(err, vm.ErrInvalidState) {
		t.Fatalf("err = %v", err)
	}
	if !f.Called("unregistervm " + createdID(t, f) + " --delete") {
		t.Fatalf("clone not discarded: %q", commands(f))
	}
	if after, err := os.ReadFile(filepath.Join(srcDir, seedName)); err != nil || !bytes.Equal(after, source) {
		t.Fatalf("source seed touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "copy")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clone dir left behind: %v", err)
	}
}

func TestCloneCancelledWhileCopying(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := cloneFake(t, fixture(t, "showvminfo-stopped.txt"), fixture(t, "showvminfo-linked.txt"))
	f.OnFunc("clonevm", func(c fakerun.Call) (runner.Result, error) {
		cancel()
		return runner.Result{Stdout: []byte(fixture(t, "clonevm.txt"))}, nil
	})
	p := detachedProvider(t, f)
	if _, err := p.Clone(ctx, "demo-1", vm.CloneOptions{Name: "copy"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if !f.Called("unregistervm " + createdID(t, f) + " --delete") {
		t.Fatalf("clone not discarded: %q", commands(f))
	}
	if _, err := os.Stat(filepath.Join(p.root, "copy")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clone dir left behind: %v", err)
	}
}
