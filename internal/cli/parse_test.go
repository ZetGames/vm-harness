package cli

import (
	"errors"
	"maps"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ZetGames/vm-harness/vm"
)

func TestParseForward(t *testing.T) {
	cases := []struct {
		in   string
		want vm.PortForward
	}{
		{"8080:80", vm.PortForward{HostPort: 8080, GuestPort: 80}},
		{"web=8080:80", vm.PortForward{Name: "web", HostPort: 8080, GuestPort: 80}},
		{"0:53/udp", vm.PortForward{Protocol: "udp", GuestPort: 53}},
		{"dns=5353:53/UDP", vm.PortForward{Name: "dns", Protocol: "udp", HostPort: 5353, GuestPort: 53}},
		{"ssh=0.0.0.0:2222:22/tcp", vm.PortForward{Name: "ssh", Protocol: "tcp", HostIP: "0.0.0.0", HostPort: 2222, GuestPort: 22}},
		{"[::1]:8080:80", vm.PortForward{HostIP: "::1", HostPort: 8080, GuestPort: 80}},
	}
	for _, c := range cases {
		got, err := parseForward(c.in)
		if err != nil || got != c.want {
			t.Errorf("parseForward(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"", "80", "web=80", "a:80", "8080:b", "x=:80"} {
		if _, err := parseForward(in); !errors.Is(err, vm.ErrInvalid) {
			t.Errorf("parseForward(%q) error = %v, want invalid argument", in, err)
		}
	}
}

func TestParseNIC(t *testing.T) {
	cases := []struct {
		in   string
		want vm.NIC
	}{
		{"nat", vm.NIC{Mode: "nat"}},
		{"NAT", vm.NIC{Mode: "nat"}},
		{"bridged:Intel(R) Ethernet I219-V", vm.NIC{Mode: "bridged", Adapter: "Intel(R) Ethernet I219-V"}},
		{"hostonly:VirtualBox Host-Only Ethernet Adapter #2", vm.NIC{Mode: "hostonly", Adapter: "VirtualBox Host-Only Ethernet Adapter #2"}},
		{"custom:vmnet2", vm.NIC{Mode: "custom", Adapter: "vmnet2"}},
	}
	for _, c := range cases {
		got, err := parseNIC(c.in)
		if err != nil || got != c.want {
			t.Errorf("parseNIC(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"", ":eth0"} {
		if _, err := parseNIC(in); !errors.Is(err, vm.ErrInvalid) {
			t.Errorf("parseNIC(%q) error = %v", in, err)
		}
	}
}

func TestParseShare(t *testing.T) {
	cwd, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(t.TempDir(), "data")
	cases := []struct {
		in   string
		want vm.SharedFolder
	}{
		{"src=" + abs, vm.SharedFolder{Name: "src", HostPath: abs}},
		{"src=" + abs + ":ro", vm.SharedFolder{Name: "src", HostPath: abs, ReadOnly: true}},
		{"src=" + abs + ":rw", vm.SharedFolder{Name: "src", HostPath: abs}},
		{"rel=sub/dir:ro", vm.SharedFolder{Name: "rel", HostPath: filepath.Join(cwd, "sub", "dir"), ReadOnly: true}},
	}
	for _, c := range cases {
		got, err := parseShare(c.in)
		if err != nil || got != c.want {
			t.Errorf("parseShare(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"", "src", "=path", "src="} {
		if _, err := parseShare(in); !errors.Is(err, vm.ErrInvalid) {
			t.Errorf("parseShare(%q) error = %v", in, err)
		}
	}
}

func TestParseShareWindowsDrive(t *testing.T) {
	got, err := parseShare(`work=C:\Users\dev\src:ro`)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ReadOnly || got.Name != "work" || filepath.Base(got.HostPath) != filepath.Base(`C:\Users\dev\src`) {
		t.Errorf("parseShare = %+v", got)
	}
}

func TestParseLabels(t *testing.T) {
	got, err := parseLabels([]string{"team=red", "empty=", "expr=a=b", "team=blue"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"team": "blue", "empty": "", "expr": "a=b"}; !maps.Equal(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}

	got, err = parseLabels([]string{"keep=1", "drop-", "with-dash-"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"keep": "1", "drop": "", "with-dash": ""}; !maps.Equal(got, want) {
		t.Errorf("labels with removal = %v, want %v", got, want)
	}

	if got, err := parseLabels(nil, true); got != nil || err != nil {
		t.Errorf("no labels = %v, %v", got, err)
	}
	for _, c := range []struct {
		items       []string
		allowRemove bool
	}{
		{[]string{"drop-"}, false},
		{[]string{"novalue"}, true},
		{[]string{"=x"}, false},
		{[]string{"-"}, true},
	} {
		if _, err := parseLabels(c.items, c.allowRemove); !errors.Is(err, vm.ErrInvalid) {
			t.Errorf("parseLabels(%q, %v) error = %v", c.items, c.allowRemove, err)
		}
	}
}

func TestParseEnv(t *testing.T) {
	got, err := parseEnv([]string{"A=1", "PATH=/bin:/usr/bin", "EMPTY=", "EQ=a=b"})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"A": "1", "PATH": "/bin:/usr/bin", "EMPTY": "", "EQ": "a=b"}; !maps.Equal(got, want) {
		t.Errorf("env = %v, want %v", got, want)
	}
	for _, in := range []string{"A", "=1"} {
		if _, err := parseEnv([]string{in}); !errors.Is(err, vm.ErrInvalid) {
			t.Errorf("parseEnv(%q) error = %v", in, err)
		}
	}
}

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"60":     time.Minute,
		"0":      0,
		"60s":    time.Minute,
		"5m":     5 * time.Minute,
		"1h30m":  90 * time.Minute,
		"1500ms": 1500 * time.Millisecond,
	}
	for in, want := range cases {
		if got, err := parseDuration(in); err != nil || got != want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "soon", "-1", "-5s", "5 m"} {
		if _, err := parseDuration(in); err == nil {
			t.Errorf("parseDuration(%q) succeeded", in)
		}
	}
}

func TestSeconds(t *testing.T) {
	cases := map[time.Duration]int{0: 0, time.Millisecond: 1, time.Second: 1, 1500 * time.Millisecond: 2, 5 * time.Minute: 300}
	for in, want := range cases {
		if got := seconds(in); got != want {
			t.Errorf("seconds(%v) = %d, want %d", in, got, want)
		}
	}
}

func TestSplitGuestPath(t *testing.T) {
	cases := []struct {
		in      string
		vm      string
		path    string
		inGuest bool
	}{
		{"web:/tmp/x", "web", "/tmp/x", true},
		{"web:relative/x", "web", "relative/x", true},
		{`win11:C:\Users\Public\x.txt`, "win11", `C:\Users\Public\x.txt`, true},
		{"web.example-1:/x", "web.example-1", "/x", true},
		{"b:/data", "", "b:/data", false},
		{`C:\Users\dev\file.txt`, "", `C:\Users\dev\file.txt`, false},
		{"C:/Users/dev/file.txt", "", "C:/Users/dev/file.txt", false},
		{`d:\x`, "", `d:\x`, false},
		{"C:", "", "C:", false},
		{"/tmp/a:b", "", "/tmp/a:b", false},
		{"./a:b", "", "./a:b", false},
		{`dir\a:b`, "", `dir\a:b`, false},
		{"plain.txt", "", "plain.txt", false},
		{":x", "", ":x", false},
		{`\\server\share\a:b`, "", `\\server\share\a:b`, false},
	}
	for _, c := range cases {
		name, path, inGuest := splitGuestPath(c.in)
		if name != c.vm || path != c.path || inGuest != c.inGuest {
			t.Errorf("splitGuestPath(%q) = %q, %q, %v; want %q, %q, %v", c.in, name, path, inGuest, c.vm, c.path, c.inGuest)
		}
	}
}

func TestPlanCopyWindowsHostPaths(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive letter paths are absolute only on windows")
	}
	plan, err := planCopy(`C:\build\app.zip`, `win11:C:\Users\Public\`)
	if err != nil {
		t.Fatal(err)
	}
	if plan.VM != "win11" || plan.Direction != toGuest || plan.GuestPath != `C:\Users\Public\app.zip` {
		t.Errorf("to guest = %+v", plan)
	}

	plan, err = planCopy(`win11:C:\Windows\win.ini`, `D:\out\win.ini`)
	if err != nil {
		t.Fatal(err)
	}
	if plan.VM != "win11" || plan.Direction != fromGuest || plan.GuestPath != `C:\Windows\win.ini` {
		t.Errorf("from guest = %+v", plan)
	}

	if _, err := planCopy(`C:\a`, `D:\b`); !errors.Is(err, vm.ErrInvalid) {
		t.Errorf("two host paths: %v", err)
	}
}

func TestGuestBase(t *testing.T) {
	cases := map[string]string{
		"/var/log/syslog":       "syslog",
		`C:\Windows\win.ini`:    "win.ini",
		"C:/Windows/system.ini": "system.ini",
		"relative":              "relative",
	}
	for in, want := range cases {
		if got := guestBase(in); got != want {
			t.Errorf("guestBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAuthorizedKeys(t *testing.T) {
	dir := t.TempDir()
	file := writeFile(t, filepath.Join(dir, "keys.pub"), "ssh-ed25519 AAAA1 a@b\r\n# comment\n\necdsa-sha2-nistp256 AAAA2 c@d\n")
	got, err := authorizedKeys([]string{file, "sk-ssh-ed25519@openssh.com AAAA3 key"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ssh-ed25519 AAAA1 a@b", "ecdsa-sha2-nistp256 AAAA2 c@d", "sk-ssh-ed25519@openssh.com AAAA3 key"}
	if len(got) != len(want) {
		t.Fatalf("keys = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d = %q, want %q", i, got[i], want[i])
		}
	}
}
