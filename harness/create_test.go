package harness

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/fl4metf/vm-harness/internal/memprovider"
	"github.com/fl4metf/vm-harness/vm"
)

func TestCreateProviderSelection(t *testing.T) {
	cases := []struct {
		name      string
		cfg       Config
		spec      string
		vboxDown  bool
		vmwDown   bool
		want      string
		err       error
		errSubstr string
	}{
		{name: "first available", want: vm.VirtualBox},
		{name: "spec provider", spec: vm.VMware, want: vm.VMware},
		{name: "configured default", cfg: Config{DefaultProvider: vm.VMware}, want: vm.VMware},
		{name: "spec wins over default", cfg: Config{DefaultProvider: vm.VMware}, spec: vm.VirtualBox, want: vm.VirtualBox},
		{name: "virtualbox down", vboxDown: true, want: vm.VMware},
		{name: "unknown provider", spec: "parallels", err: vm.ErrInvalid},
		{name: "requested provider down", spec: vm.VirtualBox, vboxDown: true, err: vm.ErrUnavailable},
		{name: "default provider down", cfg: Config{DefaultProvider: vm.VirtualBox}, vboxDown: true, err: vm.ErrUnavailable},
		{name: "nothing available", vboxDown: true, vmwDown: true, err: vm.ErrUnavailable, errSubstr: "no hypervisor"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var vbox, vmw vm.Provider = memprovider.New(vm.VirtualBox), memprovider.New(vm.VMware)
			if c.vboxDown {
				vbox = newOffline(vm.VirtualBox)
			}
			if c.vmwDown {
				vmw = newOffline(vm.VMware)
			}
			m := newManager(t, c.cfg, vmw, vbox)
			got, err := m.Create(t.Context(), vm.Spec{Name: "web", Provider: c.spec})
			if c.err != nil {
				wantErr(t, err, c.err)
				if !strings.Contains(err.Error(), c.errSubstr) {
					t.Fatalf("error %q does not mention %q", err, c.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Provider != c.want {
				t.Fatalf("created in %s, want %s", got.Provider, c.want)
			}
		})
	}
}

func TestCreateDefaults(t *testing.T) {
	cases := []struct {
		name     string
		cfg      Config
		spec     vm.Spec
		cpus     int
		memory   int
		disk     int
		osType   string
		metaType string
	}{
		{
			name: "built in", spec: vm.Spec{Name: "a"},
			cpus: 2, memory: 2048, disk: 20, osType: "Linux26_64", metaType: "linux",
		},
		{
			name: "configured", cfg: Config{Defaults: Defaults{CPUs: 4, MemoryMB: 4096, DiskGB: 40, OSType: "ubuntu"}},
			spec: vm.Spec{Name: "a"},
			cpus: 4, memory: 4096, disk: 40, osType: "Ubuntu_64", metaType: "ubuntu",
		},
		{
			name: "explicit values win", spec: vm.Spec{Name: "a", CPUs: 1, MemoryMB: 512, DiskGB: 8, OSType: "windows11"},
			cpus: 1, memory: 512, disk: 8, osType: "Windows11_64", metaType: "windows11",
		},
		{
			name: "native type passes through", spec: vm.Spec{Name: "a", OSType: "Gentoo_64"},
			cpus: 2, memory: 2048, disk: 20, osType: "Gentoo_64", metaType: "Gentoo_64",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recorder{Provider: memprovider.New(vm.VirtualBox)}
			m := newManager(t, c.cfg, rec)
			got, err := m.Create(t.Context(), c.spec)
			if err != nil {
				t.Fatal(err)
			}
			spec := rec.last(t)
			if spec.CPUs != c.cpus || spec.MemoryMB != c.memory || spec.DiskGB != c.disk || spec.OSType != c.osType {
				t.Fatalf("spec = cpus %d memory %d disk %d os %q", spec.CPUs, spec.MemoryMB, spec.DiskGB, spec.OSType)
			}
			if spec.Provider != vm.VirtualBox {
				t.Fatalf("spec provider = %q", spec.Provider)
			}
			if !got.Managed || got.Meta[vm.MetaOSType] != c.metaType {
				t.Fatalf("machine managed=%v meta=%v", got.Managed, got.Meta)
			}
			if got.State != vm.StateStopped {
				t.Fatalf("state = %s", got.State)
			}
		})
	}
}

func TestCreateApplianceKeepsItsOwnSizing(t *testing.T) {
	ova := writeFile(t, filepath.Join(t.TempDir(), "box.ova"), "ova")
	rec := &recorder{Provider: memprovider.New(vm.VirtualBox)}
	m := newManager(t, Config{}, rec)
	got, err := m.Create(t.Context(), vm.Spec{Name: "imported", Appliance: ova})
	if err != nil {
		t.Fatal(err)
	}
	spec := rec.last(t)
	if spec.CPUs != 0 || spec.MemoryMB != 0 || spec.DiskGB != 0 || spec.OSType != "" {
		t.Fatalf("appliance spec got defaults: %+v", spec)
	}
	if _, ok := got.Meta[vm.MetaOSType]; ok {
		t.Fatalf("os_type recorded without a request: %v", got.Meta)
	}
}

func TestCreateApplianceGetsTheDefaultNATNic(t *testing.T) {
	ova := writeFile(t, filepath.Join(t.TempDir(), "noble.ova"), "ova")
	rec := &recorder{Provider: memprovider.New(vm.VirtualBox)}
	m := newManager(t, Config{}, rec)
	got, err := m.Create(t.Context(), vm.Spec{Name: "imported", Appliance: ova, CloudInit: &vm.CloudInit{}})
	if err != nil {
		t.Fatal(err)
	}
	if nics := rec.last(t).NICs; len(nics) != 1 || nics[0].Mode != vm.NetNAT {
		t.Fatalf("appliance nics = %+v, want the documented single nat nic instead of the appliance's own network", nics)
	}
	if !slices.ContainsFunc(got.PortForwards, func(pf vm.PortForward) bool { return pf.GuestPort == 22 }) {
		t.Fatalf("no ssh forward for a cloud-init appliance: %+v", got.PortForwards)
	}
}

func TestCreateMakesHostPathsAbsolute(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "boot.iso"), "iso")
	share := filepath.Join(dir, "share")
	if err := os.Mkdir(share, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	rec := &recorder{Provider: memprovider.New(vm.VirtualBox)}
	m := newManager(t, Config{}, rec)
	_, err := m.Create(t.Context(), vm.Spec{
		Name:          "a",
		ISO:           "boot.iso",
		SharedFolders: []vm.SharedFolder{{Name: "work", HostPath: "share"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := rec.last(t)
	if spec.ISO != mustCanonical(t, filepath.Join(dir, "boot.iso")) || spec.SharedFolders[0].HostPath != mustCanonical(t, share) {
		t.Fatalf("paths not absolute: %q %q", spec.ISO, spec.SharedFolders[0].HostPath)
	}
}

func TestCreateValidation(t *testing.T) {
	dir := t.TempDir()
	iso := writeFile(t, filepath.Join(dir, "a.iso"), "iso")
	disk := writeFile(t, filepath.Join(dir, "a.vmdk"), "disk")
	ova := writeFile(t, filepath.Join(dir, "a.ova"), "ova")

	cases := []struct {
		name string
		spec vm.Spec
		err  error
	}{
		{"empty name", vm.Spec{}, vm.ErrInvalid},
		{"name starts with dash", vm.Spec{Name: "-x"}, vm.ErrInvalid},
		{"name with space", vm.Spec{Name: "a b"}, vm.ErrInvalid},
		{"name with slash", vm.Spec{Name: "a/b"}, vm.ErrInvalid},
		{"name too long", vm.Spec{Name: strings.Repeat("a", 64)}, vm.ErrInvalid},
		{"iso and disk image", vm.Spec{Name: "a", ISO: iso, DiskImage: disk}, vm.ErrInvalid},
		{"disk image and appliance", vm.Spec{Name: "a", DiskImage: disk, Appliance: ova}, vm.ErrInvalid},
		{"missing iso", vm.Spec{Name: "a", ISO: filepath.Join(dir, "nope.iso")}, vm.ErrInvalid},
		{"iso is a directory", vm.Spec{Name: "a", ISO: dir}, vm.ErrInvalid},
		{"missing disk image", vm.Spec{Name: "a", DiskImage: filepath.Join(dir, "nope.vmdk")}, vm.ErrInvalid},
		{"shared folder is a file", vm.Spec{Name: "a", SharedFolders: []vm.SharedFolder{{Name: "s", HostPath: iso}}}, vm.ErrInvalid},
		{"shared folder without name", vm.Spec{Name: "a", SharedFolders: []vm.SharedFolder{{HostPath: dir}}}, vm.ErrInvalid},
		{"shared folder without host path", vm.Spec{Name: "a", SharedFolders: []vm.SharedFolder{{Name: "s"}}}, vm.ErrInvalid},
		{"unattended without iso", vm.Spec{Name: "a", Unattended: &vm.Unattended{User: "u", Password: "p"}}, vm.ErrInvalid},
		{"unattended with cloud-init", vm.Spec{Name: "a", ISO: iso, Unattended: &vm.Unattended{}, CloudInit: &vm.CloudInit{}}, vm.ErrInvalid},
		{"cloud-init on a blank disk", vm.Spec{Name: "a", CloudInit: &vm.CloudInit{}}, vm.ErrInvalid},
		{"name ending in a dot", vm.Spec{Name: "web.", DiskImage: disk, CloudInit: &vm.CloudInit{}}, vm.ErrInvalid},
		{"reserved device name", vm.Spec{Name: "Nul.vm", DiskImage: disk, CloudInit: &vm.CloudInit{}}, vm.ErrInvalid},
		{"name differs only by case", vm.Spec{Name: "TAKEN", DiskImage: disk, CloudInit: &vm.CloudInit{}}, vm.ErrExists},
		{"negative cpus", vm.Spec{Name: "a", CPUs: -1}, vm.ErrInvalid},
		{"negative memory", vm.Spec{Name: "a", MemoryMB: -1}, vm.ErrInvalid},
		{"negative disk", vm.Spec{Name: "a", DiskGB: -5}, vm.ErrInvalid},
		{"unknown firmware", vm.Spec{Name: "a", Firmware: "uefi"}, vm.ErrInvalid},
		{"unknown nic mode", vm.Spec{Name: "a", NICs: []vm.NIC{{Mode: "wifi"}}}, vm.ErrInvalid},
		{"bad label key", vm.Spec{Name: "a", Labels: map[string]string{"bad key": "v"}}, vm.ErrInvalid},
		{"long label value", vm.Spec{Name: "a", Labels: map[string]string{"k": strings.Repeat("v", 257)}}, vm.ErrInvalid},
		{"label value with newline", vm.Spec{Name: "a", Labels: map[string]string{"k": "a\nb"}}, vm.ErrInvalid},
		{"forward protocol", vm.Spec{Name: "a", PortForwards: []vm.PortForward{{Protocol: "icmp", GuestPort: 1}}}, vm.ErrInvalid},
		{"forward guest port zero", vm.Spec{Name: "a", PortForwards: []vm.PortForward{{HostPort: 80}}}, vm.ErrInvalid},
		{"forward host port too big", vm.Spec{Name: "a", PortForwards: []vm.PortForward{{HostPort: 70000, GuestPort: 80}}}, vm.ErrInvalid},
		{"forward host ip", vm.Spec{Name: "a", PortForwards: []vm.PortForward{{HostIP: "localhost", GuestPort: 80}}}, vm.ErrInvalid},
		{"forward on another host", vm.Spec{Name: "a", PortForwards: []vm.PortForward{{HostIP: "192.0.2.10", HostPort: 2222, GuestPort: 22}}}, vm.ErrInvalid},
		{"forward name", vm.Spec{Name: "a", PortForwards: []vm.PortForward{{Name: "a,b", GuestPort: 80}}}, vm.ErrInvalid},
		{"duplicate forward names", vm.Spec{Name: "a", PortForwards: []vm.PortForward{{GuestPort: 80}, {GuestPort: 80, HostPort: 8080}}}, vm.ErrInvalid},
		{"forward without nat", vm.Spec{Name: "a", NICs: []vm.NIC{{Mode: vm.NetBridged}}, PortForwards: []vm.PortForward{{GuestPort: 80}}}, vm.ErrInvalid},
		{"vmware port forward", vm.Spec{Name: "a", Provider: vm.VMware, PortForwards: []vm.PortForward{{GuestPort: 80}}}, vm.ErrUnsupported},
		{"vmware unattended", vm.Spec{Name: "a", Provider: vm.VMware, ISO: iso, Unattended: &vm.Unattended{}}, vm.ErrUnsupported},
		{"vmware appliance", vm.Spec{Name: "a", Provider: vm.VMware, Appliance: ova}, vm.ErrUnsupported},
		{"existing name", vm.Spec{Name: "taken", DiskImage: disk, CloudInit: &vm.CloudInit{}}, vm.ErrExists},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.vbox.Put(foreignVM("taken", vm.StateStopped))
			_, err := e.m.Create(t.Context(), c.spec)
			wantErr(t, err, c.err)
			if calls := append(callsOf(e.vbox, "create"), callsOf(e.vmw, "create")...); len(calls) > 0 {
				t.Fatalf("provider was asked to create: %v", calls)
			}
			if exists(filepath.Join(e.root, "keys")) {
				t.Fatal("keys were generated for a rejected spec")
			}
		})
	}
}

func TestCreateLimits(t *testing.T) {
	cases := []struct {
		name   string
		limits Limits
		spec   vm.Spec
		err    error
	}{
		{"cpus", Limits{MaxCPUs: 4}, vm.Spec{Name: "a", CPUs: 8}, vm.ErrLimit},
		{"cpus at cap", Limits{MaxCPUs: 4}, vm.Spec{Name: "a", CPUs: 4}, nil},
		{"memory", Limits{MaxMemoryMB: 4096}, vm.Spec{Name: "a", MemoryMB: 8192}, vm.ErrLimit},
		{"default memory over cap", Limits{MaxMemoryMB: 1024}, vm.Spec{Name: "a"}, vm.ErrLimit},
		{"disk", Limits{MaxDiskGB: 10}, vm.Spec{Name: "a", DiskGB: 11}, vm.ErrLimit},
		{"vm count", Limits{MaxVMs: 2}, vm.Spec{Name: "a"}, vm.ErrLimit},
		{"vm count ignores foreign vms", Limits{MaxVMs: 3}, vm.Spec{Name: "a"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnvWith(t, Config{Limits: c.limits})
			e.vbox.Put(managedVM("one", vm.StateStopped))
			e.vmw.Put(managedVM("two", vm.StateStopped))
			e.vbox.Put(foreignVM("legacy", vm.StateStopped))
			_, err := e.m.Create(t.Context(), c.spec)
			if c.err == nil {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			wantErr(t, err, c.err)
			if vm.Code(err) != "limit_exceeded" {
				t.Fatalf("code = %s", vm.Code(err))
			}
		})
	}
}

func TestCreateStart(t *testing.T) {
	e := newEnv(t)
	got, err := e.m.Create(t.Context(), vm.Spec{Name: "a", Start: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != vm.StateRunning {
		t.Fatalf("state = %s", got.State)
	}
	got, err = e.m.Create(t.Context(), vm.Spec{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != vm.StateStopped {
		t.Fatalf("state = %s", got.State)
	}
}

func TestCreateForwards(t *testing.T) {
	e := newEnv(t)
	got, err := e.m.Create(t.Context(), vm.Spec{
		Name: "web",
		PortForwards: []vm.PortForward{
			{GuestPort: 80},
			{Protocol: "udp", GuestPort: 53},
			{Name: "fixed", HostIP: "0.0.0.0", HostPort: 18080, GuestPort: 8080},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PortForwards) != 3 {
		t.Fatalf("forwards = %+v", got.PortForwards)
	}
	web, _ := forwardNamed(got, "tcp-80")
	dns, _ := forwardNamed(got, "udp-53")
	fixed, _ := forwardNamed(got, "fixed")
	if web.HostIP != "127.0.0.1" || web.HostPort == 0 || web.Protocol != "tcp" {
		t.Fatalf("tcp-80 = %+v", web)
	}
	if dns.HostPort == 0 || dns.Protocol != "udp" {
		t.Fatalf("udp-53 = %+v", dns)
	}
	if fixed.HostIP != "0.0.0.0" || fixed.HostPort != 18080 {
		t.Fatalf("fixed = %+v", fixed)
	}
	if _, ok := forwardNamed(got, sshForwardName); ok {
		t.Fatal("ssh forward added without cloud-init")
	}
}

func cloudImage(t *testing.T) string {
	t.Helper()
	return writeFile(t, filepath.Join(t.TempDir(), "cloud.vmdk"), "disk")
}

var keyName = regexp.MustCompile(`^(virtualbox|vmware)-ci-[0-9a-f]{8}$`)

func TestCreateCloudInit(t *testing.T) {
	cases := []struct {
		name       string
		provider   string
		cloudInit  vm.CloudInit
		nics       []vm.NIC
		forwards   []vm.PortForward
		generated  bool
		sshUser    string
		sshForward bool
	}{
		{name: "generated key", cloudInit: vm.CloudInit{}, generated: true, sshUser: "vmh", sshForward: true},
		{name: "custom user", cloudInit: vm.CloudInit{User: "dev"}, generated: true, sshUser: "dev", sshForward: true},
		{name: "own key", cloudInit: vm.CloudInit{SSHAuthorizedKeys: []string{"ssh-ed25519 AAAA me"}}, generated: true, sshUser: "vmh", sshForward: true},
		{name: "password", cloudInit: vm.CloudInit{Password: "secret"}, generated: true, sshUser: "vmh", sshForward: true},
		{name: "raw user data", cloudInit: vm.CloudInit{UserData: "#cloud-config\n"}, sshForward: true},
		{name: "no port forwarding feature", provider: vm.VMware, generated: true, sshUser: "vmh"},
		{name: "first nic not nat", nics: []vm.NIC{{Mode: vm.NetBridged, Adapter: "eth0"}}, generated: true, sshUser: "vmh"},
		{name: "explicit ssh forward", forwards: []vm.PortForward{{Name: "ssh", GuestPort: 22}}, generated: true, sshUser: "vmh"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vbox := &recorder{Provider: memprovider.New(vm.VirtualBox)}
			vmw := &recorder{Provider: memprovider.New(vm.VMware)}
			vmw.Features = vmwareFeatures
			m := newManager(t, Config{}, vbox, vmw)
			ci := c.cloudInit
			got, err := m.Create(t.Context(), vm.Spec{Name: "ci", Provider: c.provider, DiskImage: cloudImage(t), CloudInit: &ci, NICs: c.nics, PortForwards: c.forwards})
			if err != nil {
				t.Fatal(err)
			}
			rec := vbox
			if got.Provider == vm.VMware {
				rec = vmw
			}
			spec := rec.last(t)
			if ci.User != c.cloudInit.User || len(ci.SSHAuthorizedKeys) != len(c.cloudInit.SSHAuthorizedKeys) {
				t.Fatalf("caller's cloud-init was modified: %+v", ci)
			}
			key := got.Meta[vm.MetaSSHKey]
			if c.generated {
				if filepath.Dir(key) != filepath.Join(m.Config().Root, "keys") || !keyName.MatchString(filepath.Base(key)) {
					t.Fatalf("ssh_key meta = %q", key)
				}
				if !exists(key) || !exists(key+".pub") {
					t.Fatalf("key files missing at %s", key)
				}
				keys := spec.CloudInit.SSHAuthorizedKeys
				want := len(c.cloudInit.SSHAuthorizedKeys) + 1
				if len(keys) != want || !slices.Equal(keys[:want-1], c.cloudInit.SSHAuthorizedKeys) {
					t.Fatalf("authorized keys = %q", keys)
				}
				generated := keys[want-1]
				if !strings.HasPrefix(generated, "ssh-ed25519 ") || strings.HasSuffix(generated, "\n") {
					t.Fatalf("generated key = %q", generated)
				}
				pub, err := os.ReadFile(key + ".pub")
				if err != nil || strings.TrimSpace(string(pub)) != generated {
					t.Fatalf("public key file %q does not match %q", pub, generated)
				}
				if spec.CloudInit.Password != c.cloudInit.Password {
					t.Fatalf("password changed to %q", spec.CloudInit.Password)
				}
				if got.SSH == nil || got.SSH.User != c.sshUser || got.SSH.KeyPath != key {
					t.Fatalf("ssh access = %+v", got.SSH)
				}
			} else {
				if key != "" || got.SSH != nil {
					t.Fatalf("unexpected key: meta %q, ssh %+v", key, got.SSH)
				}
				if entries, _ := os.ReadDir(filepath.Join(m.Config().Root, "keys")); len(entries) > 0 {
					t.Fatalf("key files written: %v", entries)
				}
				if len(spec.CloudInit.SSHAuthorizedKeys) != len(c.cloudInit.SSHAuthorizedKeys) {
					t.Fatalf("authorized keys changed: %q", spec.CloudInit.SSHAuthorizedKeys)
				}
			}
			if got.Meta[vm.MetaSSHUser] != c.sshUser {
				t.Fatalf("ssh_user meta = %q, want %q", got.Meta[vm.MetaSSHUser], c.sshUser)
			}
			if c.sshUser != "" && spec.CloudInit.User != c.sshUser {
				t.Fatalf("cloud-init user = %q", spec.CloudInit.User)
			}
			pf, ok := forwardNamed(got, sshForwardName)
			if ok != c.sshForward {
				t.Fatalf("vmh-ssh forward present=%v, want %v (%+v)", ok, c.sshForward, got.PortForwards)
			}
			if ok && (pf.Protocol != "tcp" || pf.HostIP != "127.0.0.1" || pf.HostPort == 0 || pf.GuestPort != 22) {
				t.Fatalf("vmh-ssh = %+v", pf)
			}
			if ok && c.generated && (got.SSH.Host != "127.0.0.1" || got.SSH.Port != pf.HostPort) {
				t.Fatalf("ssh access = %+v, forward %+v", got.SSH, pf)
			}
		})
	}
}

func TestCreateRemovesGeneratedKeyOnFailure(t *testing.T) {
	rec := &recorder{Provider: memprovider.New(vm.VirtualBox), fail: errors.New("disk full")}
	m := newManager(t, Config{}, rec)
	_, err := m.Create(t.Context(), vm.Spec{Name: "ci", DiskImage: cloudImage(t), CloudInit: &vm.CloudInit{}})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
	if len(rec.last(t).CloudInit.SSHAuthorizedKeys) != 1 {
		t.Fatal("key was not generated before the provider call")
	}
	entries, _ := os.ReadDir(filepath.Join(m.Config().Root, "keys"))
	if len(entries) != 0 {
		t.Fatalf("key files left behind: %v", entries)
	}
}

func keyFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(root, "keys", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = string(data)
	}
	return files
}

func TestCreateNeverTouchesAnotherVMsKeys(t *testing.T) {
	rec := &recorder{Provider: memprovider.New(vm.VirtualBox)}
	m := newManager(t, Config{}, rec)
	root := m.Config().Root
	image := cloudImage(t)
	web, err := m.Create(t.Context(), vm.Spec{Name: "Web", DiskImage: image, CloudInit: &vm.CloudInit{}})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, web.Meta[vm.MetaSSHKey]+".known_hosts", "[127.0.0.1]:2222 ssh-ed25519 AAAA")
	before := keyFiles(t, root)

	for _, name := range []string{"web", "Web"} {
		_, err = m.Create(t.Context(), vm.Spec{Name: name, DiskImage: image, CloudInit: &vm.CloudInit{}})
		wantErr(t, err, vm.ErrExists)
	}
	rec.fail = errors.New("settings file already exists")
	_, err = m.Create(t.Context(), vm.Spec{Name: "web2", DiskImage: image, CloudInit: &vm.CloudInit{}})
	if err == nil {
		t.Fatal("create succeeded")
	}
	if after := keyFiles(t, root); !maps.Equal(before, after) {
		t.Fatalf("key files changed:\n%v\n%v", before, after)
	}

	rec.fail = nil
	for _, name := range []string{"Web.known_hosts", "Web.pub"} {
		if _, err := m.Create(t.Context(), vm.Spec{Name: name, DiskImage: image, CloudInit: &vm.CloudInit{}}); err != nil {
			t.Fatal(err)
		}
		if err := m.Delete(t.Context(), Ref{VM: name}, false); err != nil {
			t.Fatal(err)
		}
	}
	if after := keyFiles(t, root); !maps.Equal(before, after) {
		t.Fatalf("key files changed:\n%v\n%v", before, after)
	}
}

func TestCreateExistingNameKeepsItsKeys(t *testing.T) {
	e := newEnv(t)
	first, err := e.m.Create(t.Context(), vm.Spec{Name: "ci", DiskImage: cloudImage(t), CloudInit: &vm.CloudInit{}})
	if err != nil {
		t.Fatal(err)
	}
	key := first.Meta[vm.MetaSSHKey]
	before, _ := os.ReadFile(key)
	_, err = e.m.Create(t.Context(), vm.Spec{Name: "ci", DiskImage: cloudImage(t), CloudInit: &vm.CloudInit{}})
	wantErr(t, err, vm.ErrExists)
	after, _ := os.ReadFile(key)
	if string(before) != string(after) || len(after) == 0 {
		t.Fatal("existing vm's key was replaced")
	}
}

func TestCreateTagsTheNewVM(t *testing.T) {
	rec := &recorder{Provider: memprovider.New(vm.VirtualBox)}
	m := newManager(t, Config{}, rec)
	got, err := m.Create(t.Context(), vm.Spec{Name: "a", Labels: map[string]string{"team": "red"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.last(t).Meta[vm.MetaManaged]; ok {
		t.Fatalf("the provider was asked to store the marker itself: %v", rec.last(t).Meta)
	}
	if !got.Managed || got.Meta[vm.MetaManaged] != got.ID || got.Labels["team"] != "red" {
		t.Fatalf("created = %+v", got)
	}
	if calls := callsOf(rec.Provider, "setmeta"); !slices.Equal(calls, []string{"setmeta " + got.ID}) {
		t.Fatalf("setmeta calls = %v", calls)
	}
}

type untaggable struct {
	*memprovider.Provider
}

func (u untaggable) SetMeta(context.Context, string, map[string]string) error {
	return errors.New("extradata write failed")
}

func TestCreateRemovesTheVMWhenTaggingFails(t *testing.T) {
	p := untaggable{memprovider.New(vm.VirtualBox)}
	m := newManager(t, Config{}, p)
	_, err := m.Create(t.Context(), vm.Spec{Name: "a", DiskImage: cloudImage(t), CloudInit: &vm.CloudInit{}})
	if err == nil || !strings.Contains(err.Error(), "extradata write failed") {
		t.Fatalf("err = %v", err)
	}
	_, err = p.Get(t.Context(), "a")
	wantErr(t, err, vm.ErrNotFound)
	if entries, _ := os.ReadDir(filepath.Join(m.Config().Root, "keys")); len(entries) > 0 {
		t.Fatalf("key files left behind: %v", entries)
	}
}

type importer struct {
	*memprovider.Provider
	cpus, memoryMB int
}

func (i importer) Create(ctx context.Context, spec vm.Spec) (vm.Machine, error) {
	if _, err := i.Provider.Create(ctx, spec); err != nil {
		return vm.Machine{}, err
	}
	if err := i.Provider.Update(ctx, spec.Name, vm.Changes{CPUs: i.cpus, MemoryMB: i.memoryMB}); err != nil {
		return vm.Machine{}, err
	}
	return i.Provider.Get(ctx, spec.Name)
}

func TestCreateChecksWhatTheProviderBuilt(t *testing.T) {
	ova := writeFile(t, filepath.Join(t.TempDir(), "big.ova"), "ova")
	cases := []struct {
		name           string
		cpus, memoryMB int
		err            error
	}{
		{"too many cpus", 16, 1024, vm.ErrLimit},
		{"too much memory", 2, 65536, vm.ErrLimit},
		{"within the limits", 4, 8192, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := importer{memprovider.New(vm.VirtualBox), c.cpus, c.memoryMB}
			m := newManager(t, Config{Limits: Limits{MaxCPUs: 4, MaxMemoryMB: 8192}}, p)
			got, err := m.Create(t.Context(), vm.Spec{Name: "big", Appliance: ova})
			if c.err == nil {
				if err != nil || !got.Managed {
					t.Fatalf("create = %+v, %v", got, err)
				}
				return
			}
			wantErr(t, err, c.err)
			if _, err := p.Get(t.Context(), "big"); !errors.Is(err, vm.ErrNotFound) {
				t.Fatalf("over-limit vm kept: %v", err)
			}
		})
	}
}

func TestCreateRefusesNamesWindowsWouldAlias(t *testing.T) {
	for _, name := range []string{"web.", "web..", "nul", "NUL.txt", "con", "Com1", "lpt9.disk", "aux.x.y"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			_, err := e.m.Create(t.Context(), vm.Spec{Name: name, DiskImage: cloudImage(t), CloudInit: &vm.CloudInit{}})
			wantErr(t, err, vm.ErrInvalid)
			if exists(filepath.Join(e.root, "keys")) || len(callsOf(e.vbox, "create")) > 0 {
				t.Fatal("name was used")
			}
		})
	}
	for _, name := range []string{"a", "web-", "web_", "a.b", "console", "com10", "nul1", "lpt"} {
		if err := checkName("vm", name); err != nil {
			t.Errorf("checkName(%q) = %v", name, err)
		}
	}
}

func TestCreateSharedFolderRules(t *testing.T) {
	vmDir := filepath.Join(t.TempDir(), "legacy")
	writeFile(t, filepath.Join(vmDir, "legacy.vbox"), "<vbox/>")
	parent := filepath.Dir(vmDir)
	free := t.TempDir()
	cases := []struct {
		name   string
		folder func(root string) vm.SharedFolder
		err    error
	}{
		{"unrelated folder", func(string) vm.SharedFolder { return vm.SharedFolder{Name: "s", HostPath: free} }, nil},
		{"unmanaged vm folder", func(string) vm.SharedFolder { return vm.SharedFolder{Name: "s", HostPath: vmDir} }, vm.ErrForbidden},
		{"parent of an unmanaged vm folder", func(string) vm.SharedFolder { return vm.SharedFolder{Name: "s", HostPath: parent} }, vm.ErrForbidden},
		{"unmanaged vm folder read-only", func(string) vm.SharedFolder { return vm.SharedFolder{Name: "s", HostPath: vmDir, ReadOnly: true} }, nil},
		{"vmh root", func(root string) vm.SharedFolder { return vm.SharedFolder{Name: "s", HostPath: root} }, vm.ErrForbidden},
		{"inside the vmh root", func(root string) vm.SharedFolder {
			return vm.SharedFolder{Name: "s", HostPath: filepath.Join(root, "keys")}
		}, vm.ErrForbidden},
		{"parent of the vmh root", func(root string) vm.SharedFolder {
			return vm.SharedFolder{Name: "s", HostPath: filepath.Dir(root)}
		}, vm.ErrForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			if err := os.MkdirAll(filepath.Join(e.root, "keys"), 0o700); err != nil {
				t.Fatal(err)
			}
			legacy := foreignVM("legacy", vm.StateStopped)
			legacy.ConfigPath = filepath.Join(vmDir, "legacy.vbox")
			e.vmw.Put(legacy)
			_, err := e.m.Create(t.Context(), vm.Spec{Name: "a", SharedFolders: []vm.SharedFolder{c.folder(e.root)}})
			if c.err == nil {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			wantErr(t, err, c.err)
			if calls := callsOf(e.vbox, "create"); len(calls) > 0 {
				t.Fatalf("created: %v", calls)
			}
		})
	}
}

func TestCreateHostDirs(t *testing.T) {
	allowed := t.TempDir()
	inside := writeISO(t, filepath.Join(allowed, "boot.iso"))
	outside := writeFile(t, filepath.Join(t.TempDir(), "boot.iso"), "iso")
	e := newEnvWith(t, Config{HostDirs: []string{allowed}})
	cases := map[string]vm.Spec{
		"iso":           {Name: "a", ISO: outside},
		"disk image":    {Name: "a", DiskImage: outside},
		"appliance":     {Name: "a", Appliance: outside},
		"shared folder": {Name: "a", SharedFolders: []vm.SharedFolder{{Name: "s", HostPath: filepath.Dir(outside), ReadOnly: true}}},
		"missing file":  {Name: "a", ISO: filepath.Join(filepath.Dir(outside), "missing.iso")},
	}
	for name, spec := range cases {
		_, err := e.m.Create(t.Context(), spec)
		if !errors.Is(err, vm.ErrForbidden) {
			t.Errorf("%s: error = %v, want forbidden", name, err)
		}
	}
	if _, err := e.m.Create(t.Context(), vm.Spec{Name: "a", ISO: inside, SharedFolders: []vm.SharedFolder{{Name: "s", HostPath: allowed}}}); err != nil {
		t.Fatal(err)
	}
}
