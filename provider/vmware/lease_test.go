package vmware

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZetGames/vm-harness/vm"
)

func TestParseLeases(t *testing.T) {
	leases := parseLeases(fixture(t, "dhcp/vmnetdhcp.leases"))
	var ips []string
	for _, l := range leases {
		ips = append(ips, l.ip)
	}
	want := []string{"192.168.80.128", "192.168.80.127", "192.168.80.129", "192.168.80.130", "192.168.80.131", "192.168.80.140", "192.168.80.150"}
	if !slices.Equal(ips, want) {
		t.Fatalf("ips = %q, want %q", ips, want)
	}
	first := leases[0]
	if first.mac != "00:0c:29:aa:bb:cc" || first.unused || first.endless ||
		!first.starts.Equal(time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)) || !first.ends.Equal(time.Date(2026, 10, 5, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("first lease = %+v", first)
	}
	if !leases[3].unused || !leases[4].unused {
		t.Errorf("abandoned and free leases must be marked unused: %+v %+v", leases[3], leases[4])
	}
	quoted := leases[5]
	if quoted.mac != "00:0c:29:dd:ee:ff" || !quoted.endless || quoted.unused {
		t.Errorf("lease with quoted strings and a nested block = %+v", quoted)
	}
	if leases[6].mac != "00:0c:29:44:55:66" {
		t.Errorf("the last record of an address must win: %+v", leases[6])
	}
}

func TestParseLeasesToleratesDamage(t *testing.T) {
	cases := map[string]int{
		"":                          0,
		"} } lease 10.0.0.1 { }":    1,
		"lease 10.0.0.2 { uid \"\\": 0,
		"lease 10.0.0.3 { ends never; } # trailing comment":                1,
		"lease 10.0.0.4 { starts 1 2026/13/45 99:00:00; ends 1 garbage; }": 1,
	}
	for text, want := range cases {
		if got := parseLeases(text); len(got) != want {
			t.Errorf("%q: %d leases, want %d", text, len(got), want)
		}
	}
	l := parseLeases("lease 10.0.0.4 { starts 1 2026/13/45 99:00:00; ends 1 garbage; }")[0]
	if !l.starts.IsZero() || l.current(time.Now()) {
		t.Errorf("unreadable times must count as old and ended: %+v", l)
	}
}

func TestNewestLease(t *testing.T) {
	leases := parseLeases(fixture(t, "dhcp/vmnetdhcp.leases"))
	at := func(hour, minute int) time.Time { return time.Date(2026, 10, 5, hour, minute, 0, 0, time.UTC) }
	cases := []struct {
		mac   string
		since time.Time
		now   time.Time
		want  string
	}{
		{"00:0c:29:aa:bb:cc", at(8, 0), at(10, 15), "192.168.80.129"},
		{"00:0c:29:aa:bb:cc", at(8, 0), at(11, 0), "192.168.80.127"},
		{"00:0c:29:aa:bb:cc", at(8, 0), at(14, 0), ""},
		{"00:0c:29:aa:bb:cc", at(9, 55), at(10, 15), "192.168.80.129"},
		{"00:0c:29:aa:bb:cc", at(9, 55), at(11, 0), ""},
		{"00:0c:29:aa:bb:cc", at(10, 1), at(10, 15), ""},
		{"00:0C:29:DD:EE:FF", at(7, 0), at(23, 0), "192.168.80.140"},
		{"00:0C:29:DD:EE:FF", at(9, 0), at(23, 0), ""},
		{"00:0c:29:11:22:33", at(7, 0), at(7, 10), ""},
		{"00:0c:29:44:55:66", at(7, 0), at(8, 15), "192.168.80.150"},
		{"00:0c:29:44:55:66", at(7, 0), at(9, 0), ""},
		{"00:50:56:00:00:01", at(7, 0), at(10, 15), ""},
	}
	for _, c := range cases {
		if got := newestLease(leases, c.mac, c.since, c.now); got != c.want {
			t.Errorf("%s since %s at %s: ip = %q, want %q", c.mac, c.since.Format(time.Kitchen), c.now.Format(time.Kitchen), got, c.want)
		}
	}
}

func TestPowerOnTime(t *testing.T) {
	cases := []struct {
		log  string
		want time.Time
	}{
		{"2026-10-08T13:54:39.211Z In(05) vmx Log for VMware Workstation pid=27348 version=17.6.3 build=build-24583834 option=Release\r\n" +
			"2026-10-08T13:54:39.211Z In(05) vmx The host is x86_64.\r\n", time.Date(2026, 10, 8, 13, 54, 39, 211e6, time.UTC)},
		{"2020-05-01T10:00:00.500+03:00| vmx| I005: Log for VMware Workstation pid=4321 version=15.5.6\n", time.Date(2020, 5, 1, 7, 0, 0, 500e6, time.UTC)},
		{"2026-10-08T13:54:39Z\n", time.Date(2026, 10, 8, 13, 54, 39, 0, time.UTC)},
		{"", time.Time{}},
		{"Log for VMware Workstation\n", time.Time{}},
		{"2026-10-08 13:54:39 In(05) vmx\n", time.Time{}},
	}
	for _, c := range cases {
		vmx := filepath.Join(t.TempDir(), "web.vmx")
		writeFile(t, filepath.Join(filepath.Dir(vmx), vmwareLog), c.log)
		if got := powerOnTime(vmx); !got.Equal(c.want) {
			t.Errorf("%q: power-on = %v, want %v", c.log, got, c.want)
		}
	}
	if got := powerOnTime(filepath.Join(t.TempDir(), "web.vmx")); !got.IsZero() {
		t.Errorf("power-on without vmware.log = %v", got)
	}
}

func TestLeaseFiles(t *testing.T) {
	cases := map[string][]string{
		"darwin": {"/var/db/vmware/vmnet-dhcpd-vmnet8.leases", "/var/db/vmware/vmnet8.leases"},
		"linux":  {"/etc/vmware/vmnet8/dhcpd/dhcpd.leases"},
	}
	for goos, files := range cases {
		patterns := leaseFiles(goos)
		for _, file := range files {
			if !slices.ContainsFunc(patterns, func(pattern string) bool { ok, _ := path.Match(pattern, file); return ok }) {
				t.Errorf("%s: %s is not read by %q", goos, file, patterns)
			}
		}
	}
	t.Setenv("ProgramData", `D:\Data`)
	if got := leaseFiles("windows"); len(got) != 1 || got[0] != filepath.Join(`D:\Data`, "VMware", "vmnetdhcp.leases") {
		t.Errorf("windows lease files = %q", got)
	}
}

func leaseEntry(ip, mac string, starts, ends time.Time) string {
	starts, ends = starts.UTC(), ends.UTC()
	return fmt.Sprintf("lease %s {\n\tstarts %d %s;\n\tends %d %s;\n\thardware ethernet %s;\n}\n",
		ip, starts.Weekday(), starts.Format(leaseTimeLayout), ends.Weekday(), ends.Format(leaseTimeLayout), mac)
}

func TestGuestIPFallsBackToDHCPLease(t *testing.T) {
	nics := strings.Join([]string{
		`ethernet0.present = "TRUE"`,
		`ethernet0.connectionType = "nat"`,
		`ethernet0.generatedAddress = "00:0c:29:aa:bb:cc"`,
		`ethernet1.present = "TRUE"`,
		`ethernet1.connectionType = "bridged"`,
		`ethernet1.address = "00:50:56:00:00:02"`,
		`ethernet2.present = "TRUE"`,
		`ethernet2.connectionType = "hostonly"`,
		`ethernet2.addressType = "static"`,
		`ethernet2.address = "00:50:56:00:00:03"`,
		"",
	}, "\r\n")
	now := time.Now().Truncate(time.Second)
	booted := now.Add(-2 * time.Minute)
	active := func(ip, mac string) string {
		return leaseEntry(ip, mac, now.Add(-time.Minute), now.Add(29*time.Minute))
	}
	nat, bridged, hostonly := active("192.168.80.128", "00:0c:29:aa:bb:cc"), active("10.1.1.20", "00:50:56:00:00:02"), active("192.168.56.130", "00:50:56:00:00:03")
	newer := leaseEntry("192.168.56.131", "00:50:56:00:00:03", now, now.Add(30*time.Minute))
	ended := leaseEntry("192.168.80.127", "00:0c:29:aa:bb:cc", now.Add(-40*time.Minute), now.Add(-10*time.Minute))
	toolsDown := failed(fixture(t, "vmrun/tools-not-running.txt"))
	cases := []struct {
		name      string
		vmrun     string
		failed    bool
		poweredOn time.Time
		leases    string
		want      string
		kind      error
	}{
		{"first nic", "", true, booted, hostonly + bridged + nat, "192.168.80.128", nil},
		{"next nic", "", true, booted, newer + bridged, "192.168.56.131", nil},
		{"tools without an address", "unknown\r\n", false, booted, nat, "192.168.80.128", nil},
		{"bridged nics have no vmware lease", "", true, booted, bridged, "", vm.ErrNotReady},
		{"no lease", "", true, booted, "", "", vm.ErrNotReady},
		{"tools answer first", "192.168.80.200\r\n", false, booted, nat, "192.168.80.200", nil},
		{"ended lease", "", true, now.Add(-time.Hour), ended, "", vm.ErrNotReady},
		{"ended lease on the first nic", "", true, now.Add(-time.Hour), ended + hostonly, "192.168.56.130", nil},
		{"lease from an earlier power-on", "", true, now, nat, "", vm.ErrNotReady},
		{"lease within the clock skew", "", true, now.Add(-time.Minute + leaseSkew - time.Second), nat, "192.168.80.128", nil},
		{"power-on time unknown", "", true, time.Time{}, nat, "", vm.ErrNotReady},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			path := e.addVM(t, "web", simpleVMX("web")+nics)
			if !c.poweredOn.IsZero() {
				writeVMwareLog(t, path, c.poweredOn)
			}
			e.leases = []string{writeFile(t, filepath.Join(t.TempDir(), "vmnetdhcp.leases"), c.leases)}
			if c.failed {
				e.fake.OnResult("-T ws getGuestIPAddress "+path, toolsDown)
			} else {
				e.fake.On("-T ws getGuestIPAddress "+path, c.vmrun)
			}
			ip, err := e.GuestIP(context.Background(), "web")
			if c.kind != nil {
				wantKind(t, err, c.kind)
				return
			}
			if err != nil || ip != c.want {
				t.Fatalf("ip = %q, %v, want %q", ip, err, c.want)
			}
		})
	}
}

func writeVMwareLog(t *testing.T, path string, poweredOn time.Time) {
	t.Helper()
	line := poweredOn.UTC().Format("2006-01-02T15:04:05.000Z07:00") + " In(05) vmx Log for VMware Workstation pid=1 version=17.6.3 build=build-1 option=Release\r\n"
	writeFile(t, filepath.Join(filepath.Dir(path), vmwareLog), line)
}

func TestGuestIPIgnoresLeasesOfStoppedVM(t *testing.T) {
	e := newTestEnv(t)
	path := e.addVM(t, "web", simpleVMX("web")+"ethernet0.present = \"TRUE\"\r\nethernet0.generatedAddress = \"00:0c:29:aa:bb:cc\"\r\n")
	now := time.Now()
	writeVMwareLog(t, path, now.Add(-time.Minute))
	e.leases = []string{writeFile(t, filepath.Join(t.TempDir(), "vmnetdhcp.leases"), leaseEntry("192.168.80.128", "00:0c:29:aa:bb:cc", now, now.Add(time.Hour)))}
	e.fake.OnResult("-T ws getGuestIPAddress "+path, failed(fixture(t, "vmrun/not-powered-on.txt")))
	_, err := e.GuestIP(context.Background(), "web")
	wantKind(t, err, vm.ErrInvalidState)
}
