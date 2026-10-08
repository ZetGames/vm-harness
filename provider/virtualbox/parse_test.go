package virtualbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fl4metf/vm-harness/vm"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestParseVMInfoQuirks(t *testing.T) {
	running := parseVMInfo(fixture(t, "showvminfo-running.txt")).fields
	if got := running["VideoMode"]; got != "1024,768,32" {
		t.Errorf("VideoMode = %q", got)
	}
	if got := running["VRDEClients"]; got != "=0" {
		t.Errorf("VRDEClients = %q", got)
	}
	if got := running["memory"]; got != "512" {
		t.Errorf("memory = %q", got)
	}
	if got := running["SATA-1-0"]; got != "emptydrive" {
		t.Errorf("quoted storage key not parsed: %q", got)
	}
	if _, ok := running["rec_screen0"]; ok {
		t.Error("line without '=' must be skipped")
	}

	multiline := parseVMInfo(fixture(t, "showvminfo-multiline.txt")).fields
	if got, want := multiline["description"], "desc1\ndesc2 \"q\" \\ b"; got != want {
		t.Errorf("description = %q, want %q", got, want)
	}
	if got := multiline["GuestMemoryBalloon"]; got != "0" {
		t.Errorf("key after a multi-line value = %q", got)
	}

	special := parseVMInfo(fixture(t, "showvminfo-special.txt")).fields
	if got, want := special["name"], `demo-sp ace "q" {b}/x`; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}

	saved := parseVMInfo(fixture(t, "showvminfo-saved.txt")).fields
	if got, want := saved["VMStateFile"], `C:\vms\demo-1\Snapshots\2026-10-07T11-23-12-392812900Z.sav`; got != want {
		t.Errorf("unescaped VMStateFile = %q, want %q", got, want)
	}
	if got, want := saved["CfgFile"], `C:\vms\demo-1\demo-1.vbox`; got != want {
		t.Errorf("escaped CfgFile = %q, want %q", got, want)
	}
}

func TestParseVMInfoIgnoresKeysInsideDescription(t *testing.T) {
	stopped := fixture(t, "showvminfo-stopped.txt")
	injected := strings.Join([]string{
		"Imported appliance",
		"name=web2",
		`UUID=00000000-0000-0000-0000-000000000001`,
		`VMState="running"`,
		`CfgFile="C:\vms\web2\web2.vbox"`,
		`nic1="nat"`,
		`Forwarding(0)="evil,tcp,,2200,,22"`,
		`"SATA-9-0"="C:\vms\web2\seed.iso"`,
		`"SATA-IsEjected-9-0"="off"`,
	}, "\r\n")
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(injected)
	out := strings.Replace(stopped, `description="demo vm"`, `description="`+escaped+`"`, 1)
	if out == stopped {
		t.Fatal("fixture has no description line")
	}
	info := parseVMInfo(out)
	want := parseVMInfo(stopped)
	for _, key := range []string{"name", "UUID", "VMState", "CfgFile", "nic1"} {
		if info.fields[key] != want.fields[key] {
			t.Errorf("%s = %q, want %q", key, info.fields[key], want.fields[key])
		}
	}
	if got, wantDesc := info.fields["description"], strings.ReplaceAll(injected, "\r\n", "\n"); got != wantDesc {
		t.Errorf("description = %q, want %q", got, wantDesc)
	}
	if !reflect.DeepEqual(info.forwards, want.forwards) || len(info.dvdImages()) != len(want.dvdImages()) {
		t.Errorf("forwards = %+v, dvds = %q", info.forwards, info.dvdImages())
	}
	if after := info.fields["GuestMemoryBalloon"]; after != want.fields["GuestMemoryBalloon"] {
		t.Errorf("key after the description = %q", after)
	}
}

func TestParseVMInfoKeepsFirstValue(t *testing.T) {
	out := vmInfoText(`name="real"`, `UUID="`+demoID+`"`, `uartmode1="file,C:\vms\real\serial.log"`, `name="fake"`, `UUID="00000000-0000-0000-0000-000000000001"`)
	info := parseVMInfo(out)
	if info.name() != "real" || info.id() != demoID {
		t.Fatalf("name = %q, id = %q", info.name(), info.id())
	}
}

func TestConsoleLog(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "serial.log"), filepath.Join(dir, "com2,x.log")
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"file", []string{`uart1="0x03f8,4"`, `uartmode1="file,` + first + `"`, `uart2="off"`}, first},
		{"prefers uart1", []string{`uart1="0x03f8,4"`, `uartmode1="file,` + first + `"`, `uart2="0x02f8,3"`, `uartmode2="file,` + second + `"`}, first},
		{"later uart", []string{`uart1="0x03f8,4"`, `uartmode1="tcpserver,28998"`, `uart2="off"`, `uart3="0x03e8,4"`, `uartmode3="file,` + second + `"`}, second},
		{"disabled uart", []string{`uart1="off"`, `uartmode1="file,` + first + `"`}, ""},
		{"host device", []string{`uart1="0x03f8,4"`, `uartmode1="` + first + `"`}, ""},
		{"relative file", []string{`uart1="0x03f8,4"`, `uartmode1="file,serial.log"`}, ""},
		{"empty file", []string{`uart1="0x03f8,4"`, `uartmode1="file,"`}, ""},
		{"disconnected", []string{`uart1="0x03f8,4"`, `uartmode1="disconnected"`}, ""},
		{"no uarts", []string{`name="demo"`}, ""},
	}
	for _, c := range cases {
		if got := parseVMInfo(vmInfoText(c.lines...)).consoleLog(); got != c.want {
			t.Errorf("%s: console log = %q, want %q", c.name, got, c.want)
		}
	}
	if got := parseVMInfo(fixture(t, "showvminfo-imported-evil.txt")).consoleLog(); got != "" {
		t.Errorf("tcp server uart gave console log %q", got)
	}
}

func TestParseForwardsOfFirstNICOnly(t *testing.T) {
	out := strings.Join([]string{
		`natnet1="nat"`,
		`nic1="nat"`,
		`Forwarding(0)="ssh,tcp,127.0.0.1,2222,,22"`,
		`Forwarding(1)="udp_5353_53,udp,,5353,10.0.2.15,53"`,
		`Forwarding(2)="broken"`,
		`Forwarding(3)="a,b,tcp,0.0.0.0,2200,,22"`,
		`natnet2="nat"`,
		`nic2="nat"`,
		`Forwarding(0)="other,tcp,,8080,,80"`,
	}, "\r\n")
	want := []vm.PortForward{
		{Name: "ssh", Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 2222, GuestPort: 22},
		{Name: "udp_5353_53", Protocol: "udp", HostPort: 5353, GuestIP: "10.0.2.15", GuestPort: 53},
	}
	info := parseVMInfo(out)
	if !reflect.DeepEqual(info.forwards, want) {
		t.Fatalf("forwards = %+v", info.forwards)
	}
	if rules := map[int][]string{1: {"ssh", "udp_5353_53", "broken", "a,b"}, 2: {"other"}}; !reflect.DeepEqual(info.natRules, rules) {
		t.Fatalf("rules = %q", info.natRules)
	}
}

func TestMediaSlots(t *testing.T) {
	info := parseVMInfo(fixture(t, "showvminfo-imported.txt"))
	if got := info.mediaSlots(); !reflect.DeepEqual(got, []string{"SATA-0-0", "SATA-2-0"}) {
		t.Fatalf("media slots = %q", got)
	}
	info.fields["SATA-ImageUUID-0-0"] = "not a slot"
	info.fields["IDE-1-1"] = "host:D:"
	if got := info.mediaSlots(); !reflect.DeepEqual(got, []string{"IDE-1-1", "SATA-0-0", "SATA-2-0"}) {
		t.Fatalf("media slots = %q", got)
	}
}

func TestHostTPM(t *testing.T) {
	cases := map[string]bool{
		"":                                     false,
		`<TrustedPlatformModule type="v2_0"/>`: false,
		`<TrustedPlatformModule type="Host" location=""/>`:                true,
		`<TrustedPlatformModule type="Swtpm" location="127.0.0.1:2321"/>`: true,
	}
	for hardware, want := range cases {
		file := filepath.Join(t.TempDir(), "x.vbox")
		if err := os.WriteFile(file, []byte(fmt.Sprintf(settingsXML, hardware)), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := hostTPM(file); err != nil || got != want {
			t.Errorf("%s: host tpm = %v, %v", hardware, got, err)
		}
	}
	if _, err := hostTPM(filepath.Join(t.TempDir(), "missing.vbox")); err == nil {
		t.Error("missing settings file accepted")
	}
}

func TestParseVMList(t *testing.T) {
	got := parseVMList(fixture(t, "list-vms-special.txt"))
	want := []vmEntry{
		{"demo-2", "0b3e4233-7102-4087-bbd8-2ea3a0f123a4"},
		{"demo-3", "dbe76ac6-da70-4170-a062-1b5e513cd3ee"},
		{`demo-sp ace "q" {b}/x`, "72ab9325-9311-4e92-8b95-95275d30e185"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %+v", got)
	}
	if got := parseVMList(""); got != nil {
		t.Fatalf("empty list = %+v", got)
	}
}

func TestParseMeta(t *testing.T) {
	got := parseMeta(fixture(t, "extradata.txt") + "Key: GUI/LastWindowPosition, Value: 1,2\r\n")
	want := map[string]string{"managed": "1", "meta": `{"a":"b c","n":1}`, "owner": "agent one"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("meta = %v", got)
	}
	all := parseExtradata(fixture(t, "extradata-imported.txt"))
	if len(all) != 6 || all["VBoxInternal/Devices/serial/0/LUN#0/AttachedDriver/Driver"] != "RawFile" || all["GUI/LastCloseAction"] != "PowerOff" {
		t.Fatalf("extradata = %q", all)
	}
}

func TestParseSnapshots(t *testing.T) {
	cases := []struct {
		fixture string
		want    []vm.Snapshot
	}{
		{"snapshot-list-nested.txt", []vm.Snapshot{
			{Name: "s1", ID: "fd5d2e6b-be95-45e8-9760-4f28f551cbf2"},
			{Name: "s2", ID: "7fbb1912-7270-43b2-b91e-351dd3584762", Description: "second snapshot", Parent: "s1"},
			{Name: "s1", ID: "2ebf852c-6583-4ea9-8897-3c5c58fa8147", Parent: "s2", Current: true},
		}},
		{"snapshot-list-current-middle.txt", []vm.Snapshot{
			{Name: "s1", ID: "fd5d2e6b-be95-45e8-9760-4f28f551cbf2", Current: true},
			{Name: "s2", ID: "7fbb1912-7270-43b2-b91e-351dd3584762", Description: "second snapshot", Parent: "s1"},
			{Name: "s1", ID: "2ebf852c-6583-4ea9-8897-3c5c58fa8147", Parent: "s2"},
		}},
		{"snapshot-list-branched.txt", []vm.Snapshot{
			{Name: "s1", ID: "fd5d2e6b-be95-45e8-9760-4f28f551cbf2"},
			{Name: "s2", ID: "7fbb1912-7270-43b2-b91e-351dd3584762", Description: "second snapshot", Parent: "s1"},
			{Name: "s3", ID: "4edf60df-5090-4366-9eca-7c6268cc8fe3", Description: "line1", Parent: "s1", Current: true},
		}},
		{"snapshot-list-multiline.txt", []vm.Snapshot{
			{Name: "s1", ID: "fd5d2e6b-be95-45e8-9760-4f28f551cbf2"},
			{Name: "s2", ID: "7fbb1912-7270-43b2-b91e-351dd3584762", Description: "second snapshot", Parent: "s1"},
			{Name: "s3", ID: "4edf60df-5090-4366-9eca-7c6268cc8fe3", Description: "line1\nline2 \"quoted\" \\ back", Parent: "s1", Current: true},
		}},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			if got := parseSnapshots(fixture(t, c.fixture)); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("snapshots =\n%+v\nwant\n%+v", got, c.want)
			}
		})
	}
}

func TestStateOf(t *testing.T) {
	cases := map[string]vm.State{
		"running":          vm.StateRunning,
		"paused":           vm.StatePaused,
		"saved":            vm.StateSaved,
		"poweroff":         vm.StateStopped,
		"aborted":          vm.StateStopped,
		"aborted-saved":    vm.StateStopped,
		"starting":         vm.StateBusy,
		"stopping":         vm.StateBusy,
		"livesnapshotting": vm.StateBusy,
		"restoring":        vm.StateBusy,
		"gurumeditation":   vm.StatePaused,
		"":                 vm.StateUnknown,
	}
	for in, want := range cases {
		if got := stateOf(in); got != want {
			t.Errorf("stateOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseOSTypes(t *testing.T) {
	types := parseOSTypes(fixture(t, "list-ostypes.txt"))
	if got := types["Ubuntu (64-bit)"]; got != "Ubuntu_64" {
		t.Errorf("Ubuntu = %q", got)
	}
	if got := types["Other Windows (64-bit)"]; got != "WindowsNT_64" {
		t.Errorf("Other Windows = %q", got)
	}
}

func TestParseMediumOutput(t *testing.T) {
	if got := parseCapacityMB(fixture(t, "showmediuminfo.txt")); got != 64 {
		t.Errorf("capacity = %d", got)
	}
	want := []string{`C:\vms\demo-1\demo-1-disk0.vdi`, `C:\vms\demo-1\demo-1-disk1.vmdk`}
	if got := parseMediumLocations(fixture(t, "list-hdds.txt")); !reflect.DeepEqual(got, want) {
		t.Errorf("locations = %q", got)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		fixture string
		want    error
	}{
		{"err-not-found.txt", vm.ErrNotFound},
		{"err-not-found-uuid.txt", vm.ErrNotFound},
		{"err-snapshot-missing.txt", vm.ErrNotFound},
		{"err-settings-exists.txt", vm.ErrExists},
		{"err-nat-rule-exists.txt", vm.ErrExists},
		{"err-already-running.txt", vm.ErrInvalidState},
		{"err-locked.txt", vm.ErrInvalidState},
		{"err-lock-pending.txt", vm.ErrInvalidState},
		{"err-not-running.txt", vm.ErrInvalidState},
		{"err-guest-not-running.txt", vm.ErrInvalidState},
		{"err-already-paused.txt", vm.ErrInvalidState},
		{"err-not-mutable.txt", vm.ErrInvalidState},
		{"err-restore-running.txt", vm.ErrInvalidState},
		{"err-unregister-locked.txt", vm.ErrInvalidState},
		{"err-parent-delete.txt", vm.ErrInvalidState},
		{"err-access-denied.txt", vm.ErrInvalidState},
		{"err-linked-no-snapshot.txt", vm.ErrInvalidState},
		{"err-medium-attached.txt", vm.ErrInvalidState},
		{"err-no-additions.txt", vm.ErrNotReady},
		{"err-no-guest-session.txt", vm.ErrNotReady},
		{"err-unknown-option.txt", vm.ErrInvalid},
		{"err-bad-cpus.txt", vm.ErrInvalid},
		{"err-bad-ostype.txt", vm.ErrInvalid},
		{"err-invalidarg.txt", vm.ErrInvalid},
		{"err-shrink.txt", vm.ErrUnsupported},
	}
	for _, c := range cases {
		if got := classify(fixture(t, c.fixture)); !errors.Is(got, c.want) {
			t.Errorf("%s: kind = %v, want %v", c.fixture, got, c.want)
		}
	}
	if got := classify("VBoxManage.exe: error: something odd happened\r\n"); got != nil {
		t.Errorf("unknown error classified as %v", got)
	}
}

func TestRedact(t *testing.T) {
	got := redact([]string{"unattended", "install", "x", "--key=AAAA-BBBB", "--user=root"})
	want := []string{"unattended", "install", "x", "--key=***", "--user=root"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("redact = %q", got)
	}
	got = redact([]string{"guestcontrol", "x", "run", "--username", "root", "--putenv", "TOKEN=s3cret", "--", "/bin/true"})
	want = []string{"guestcontrol", "x", "run", "--username", "root", "--putenv", "***", "--", "/bin/true"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("redact = %q", got)
	}
}

func TestTransient(t *testing.T) {
	cases := map[string]bool{
		fixture(t, "err-access-denied.txt"):     true,
		fixture(t, "err-locked.txt"):            true,
		fixture(t, "err-lock-pending.txt"):      true,
		fixture(t, "err-unregister-locked.txt"): true,
		"VBoxManage.exe: error: The object is not ready\r\nVBoxManage.exe: error: Details: code E_ACCESSDENIED (0x80070005)\r\n":             true,
		"VBoxManage.exe: error: The object functionality is limited\r\nVBoxManage.exe: error: Details: code E_ACCESSDENIED (0x80070005)\r\n": false,
		fixture(t, "err-not-found.txt"): false,
	}
	for stderr, want := range cases {
		if got := transient(stderr); got != want {
			t.Errorf("transient(%q) = %v", stderr, got)
		}
	}
}
