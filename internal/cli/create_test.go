package cli

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ZetGames/vm-harness/vm"
)

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCreateFromFlags(t *testing.T) {
	e := newEnv(t)
	got := decode[vm.Machine](t, e.ok("create", "web",
		"--os", "ubuntu", "--cpus", "4", "--memory", "4096", "--disk", "30", "--firmware", "efi",
		"--net", "nat", "--net", "bridged:Intel Ethernet",
		"--forward", "http=8080:80", "--forward", "0:53/udp",
		"--label", "team=red", "--start",
	).stdout)

	if got.Name != "web" || got.Provider != vm.VirtualBox || got.State != vm.StateRunning || !got.Managed {
		t.Fatalf("machine = %+v", got)
	}
	if got.CPUs != 4 || got.MemoryMB != 4096 || got.Firmware != "efi" || got.OSType != "Ubuntu_64" {
		t.Errorf("hardware = %+v", got)
	}
	wantNICs := []vm.NIC{{Mode: vm.NetNAT}, {Mode: vm.NetBridged, Adapter: "Intel Ethernet"}}
	if !slices.Equal(got.NICs, wantNICs) {
		t.Errorf("nics = %+v, want %+v", got.NICs, wantNICs)
	}
	if len(got.PortForwards) != 2 {
		t.Fatalf("port forwards = %+v", got.PortForwards)
	}
	if pf := got.PortForwards[0]; pf != (vm.PortForward{Name: "http", Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 8080, GuestPort: 80}) {
		t.Errorf("first forward = %+v", pf)
	}
	if pf := got.PortForwards[1]; pf.Name != "udp-53" || pf.Protocol != "udp" || pf.HostPort == 0 {
		t.Errorf("second forward = %+v", pf)
	}
	if !maps.Equal(got.Labels, map[string]string{"team": "red"}) {
		t.Errorf("labels = %v", got.Labels)
	}
}

func TestCreateCloudInitGeneratesKey(t *testing.T) {
	e := newEnv(t)
	image := writeFile(t, filepath.Join(t.TempDir(), "cloud.vmdk"), "disk")
	got := decode[vm.Machine](t, e.ok("create", "ci", "--image", image, "--cloud-init").stdout)
	i := slices.IndexFunc(got.PortForwards, func(pf vm.PortForward) bool { return pf.Name == "vmh-ssh" })
	if i < 0 || got.PortForwards[i].GuestPort != 22 || got.PortForwards[i].HostPort == 0 {
		t.Fatalf("no ssh forward in %+v", got.PortForwards)
	}
	if got.SSH == nil || !strings.HasPrefix(got.SSH.KeyPath, filepath.Join(e.root, "keys", "virtualbox-ci-")) {
		t.Fatalf("ssh access = %+v", got.SSH)
	}
	key := got.SSH.KeyPath
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("generated key: %v", err)
	}

	e.ok("rm", "ci")
	if _, err := os.Stat(key); !os.IsNotExist(err) {
		t.Fatalf("key survived rm: %v", err)
	}
}

func TestCreateOnProvider(t *testing.T) {
	e := newEnv(t)
	got := decode[vm.Machine](t, e.ok("-p", "vmware", "create", "box", "--os", "windows11").stdout)
	if got.Provider != vm.VMware || got.OSType != "windows11-64" {
		t.Fatalf("machine = %+v", got)
	}
	e.fail(6, "unsupported", "-p", "vmware", "create", "fwd", "--forward", "8080:80")

	t.Setenv("VMH_PROVIDER", "vmware")
	if got := decode[vm.Machine](t, e.ok("create", "box2").stdout); got.Provider != vm.VMware {
		t.Fatalf("VMH_PROVIDER ignored: %+v", got)
	}
}

func TestCreateText(t *testing.T) {
	e := newEnv(t)
	out := e.ok("create", "web", "-o", "text").stdout
	if !strings.Contains(out, "name:") || !strings.Contains(out, "web") || !strings.Contains(out, "provider:") {
		t.Fatalf("text create printed:\n%s", out)
	}
}

func TestCreateSpecFile(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "media", "install.iso"), "iso")
	spec := writeFile(t, filepath.Join(dir, "spec.json"), `{
		"name": "fromfile",
		"cpus": 1,
		"memory_mb": 1024,
		"iso": "media/install.iso",
		"labels": {"a": "1", "b": "2"}
	}`)
	t.Chdir(t.TempDir())

	got := decode[vm.Machine](t, e.ok("create", "-f", spec, "--cpus", "3", "--label", "b=override").stdout)
	if got.Name != "fromfile" || got.CPUs != 3 || got.MemoryMB != 1024 {
		t.Fatalf("machine = %+v", got)
	}
	if want := map[string]string{"a": "1", "b": "override"}; !maps.Equal(got.Labels, want) {
		t.Errorf("labels = %v, want %v", got.Labels, want)
	}

	got = decode[vm.Machine](t, e.ok("create", "renamed", "-f", spec).stdout)
	if got.Name != "renamed" {
		t.Errorf("positional name did not override the spec: %q", got.Name)
	}
}

func TestCreateSpecFromStdin(t *testing.T) {
	e := newEnv(t)
	e.stdin = `{"name": "piped", "memory_mb": 512, "start": true}`
	got := decode[vm.Machine](t, e.ok("create", "-f", "-").stdout)
	if got.Name != "piped" || got.MemoryMB != 512 || got.State != vm.StateRunning {
		t.Fatalf("machine = %+v", got)
	}
}

func TestCreateErrors(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	unknown := writeFile(t, filepath.Join(dir, "unknown.json"), `{"name": "x", "cpu": 2}`)
	trailing := writeFile(t, filepath.Join(dir, "trailing.json"), `{"name": "x"} {}`)
	brace := writeFile(t, filepath.Join(dir, "brace.json"), `{"name": "x"}}`)
	bracket := writeFile(t, filepath.Join(dir, "bracket.json"), `{"name": "x"}]`)

	cases := [][]string{
		{"create"},
		{"create", "x", "--iso", filepath.Join(dir, "missing.iso")},
		{"create", "x", "--user", "admin"},
		{"create", "x", "--forward", "80"},
		{"create", "x", "--share", "nopath"},
		{"create", "x", "--label", "novalue"},
		{"create", "x", "--net", "wifi"},
		{"create", "x", "--ssh-key", filepath.Join(dir, "missing.pub")},
		{"create", "-f", unknown},
		{"create", "-f", trailing},
		{"create", "-f", brace},
		{"create", "-f", bracket},
		{"create", "-f", filepath.Join(dir, "missing.json")},
		{"create", "x", "--unattended", "--cloud-init"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			e.fail(9, "invalid_argument", args...)
		})
	}
	e.stdin = `{"name": "x"}]`
	e.fail(9, "invalid_argument", "create", "-f", "-")
	if vms := decode[[]vm.Machine](t, e.ok("ls").stdout); len(vms) != 0 {
		t.Fatalf("failed creates left vms behind: %v", names(vms))
	}
}

func TestCreateSpecBuilding(t *testing.T) {
	dir := t.TempDir()
	keyFile := writeFile(t, filepath.Join(dir, "id.pub"), "# laptop\nssh-ed25519 AAAAfile user@host\n\n")
	userData := writeFile(t, filepath.Join(dir, "user-data"), "#cloud-config\n")
	shared := filepath.Join(dir, "shared")
	t.Chdir(dir)

	cases := []struct {
		name string
		opts createOptions
		file string
		want vm.Spec
	}{
		{
			name: "cloud-init flags",
			opts: createOptions{
				image:    "disk.vmdk",
				sshKeys:  []string{keyFile, "ssh-rsa AAAAtext me"},
				packages: []string{"nginx"},
				runcmds:  []string{"echo a, b"},
				user:     "dev",
				hostname: "web01",
				shares:   []string{"src=shared:ro"},
			},
			want: vm.Spec{
				Name:          "web",
				DiskImage:     filepath.Join(dir, "disk.vmdk"),
				SharedFolders: []vm.SharedFolder{{Name: "src", HostPath: shared, ReadOnly: true}},
				CloudInit: &vm.CloudInit{
					User:              "dev",
					Hostname:          "web01",
					SSHAuthorizedKeys: []string{"ssh-ed25519 AAAAfile user@host", "ssh-rsa AAAAtext me"},
					Packages:          []string{"nginx"},
					RunCmd:            []string{"echo a, b"},
				},
			},
		},
		{
			name: "user data file",
			opts: createOptions{userData: userData, password: "pw"},
			want: vm.Spec{Name: "web", CloudInit: &vm.CloudInit{Password: "pw", UserData: "#cloud-config\n"}},
		},
		{
			name: "unattended",
			opts: createOptions{iso: "win.iso", user: "admin", password: "pw", timeZone: "UTC", installAdditions: true},
			want: vm.Spec{
				Name:       "web",
				ISO:        filepath.Join(dir, "win.iso"),
				Unattended: &vm.Unattended{User: "admin", Password: "pw", TimeZone: "UTC", InstallAdditions: true},
			},
		},
		{
			name: "spec file with flag overrides",
			file: `{"name": "ignored", "os_type": "debian", "disk_gb": 8, "appliance": "a.ova",
				"shared_folders": [{"name": "data", "host_path": "data"}],
				"cloud_init": {"user": "ops", "packages": ["git"]}}`,
			opts: createOptions{diskGB: 16, user: "dev"},
			want: vm.Spec{
				Name:          "web",
				OSType:        "debian",
				DiskGB:        16,
				Appliance:     filepath.Join(dir, "specs", "a.ova"),
				SharedFolders: []vm.SharedFolder{{Name: "data", HostPath: filepath.Join(dir, "specs", "data")}},
				CloudInit:     &vm.CloudInit{User: "dev", Packages: []string{"git"}},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.file != "" {
				c.opts.file = writeFile(t, filepath.Join("specs", "spec.json"), c.file)
			}
			got, err := c.opts.spec([]string{"web"}, strings.NewReader(""))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("spec:\n got %+v\nwant %+v", got, c.want)
				if got.CloudInit != nil && c.want.CloudInit != nil {
					t.Errorf("cloud-init:\n got %+v\nwant %+v", *got.CloudInit, *c.want.CloudInit)
				}
			}
		})
	}
}

func TestCreateRejectsPrivateKey(t *testing.T) {
	dir := t.TempDir()
	key := writeFile(t, filepath.Join(dir, "id_ed25519"), "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----\n")
	o := createOptions{sshKeys: []string{key}}
	if _, err := o.spec([]string{"web"}, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Fatalf("err = %v", err)
	}
}
