package cloudinit_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ZetGames/vm-harness/cloudinit"
	"github.com/ZetGames/vm-harness/internal/iso9660"
	"github.com/ZetGames/vm-harness/internal/secfile/secfiletest"
	"github.com/ZetGames/vm-harness/vm"
)

const (
	testKey       = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGq2y8f8k0bM8m3k0Yx4c8i5lQ0v9qk2nq3m4o5p6r7s vmh@host"
	networkConfig = "version: 2\nethernets:\n  nics:\n    match:\n      name: \"en*\"\n    dhcp4: true\n"
)

var fullConfig = vm.CloudInit{
	User:              "dev",
	Password:          "s3cret",
	SSHAuthorizedKeys: []string{testKey, "ssh-rsa AAAAB3NzaC1yc2E dev@laptop"},
	Hostname:          "web-1",
	Packages:          []string{"curl", "jq"},
	RunCmd:            []string{"echo <ready> & date > /tmp/ready", "systemctl restart ssh"},
}

func TestUserDataGolden(t *testing.T) {
	cases := []struct {
		name string
		ci   vm.CloudInit
		want string
	}{
		{"key only", vm.CloudInit{SSHAuthorizedKeys: []string{testKey}}, `#cloud-config
{
  "users": [
    {
      "name": "vmh",
      "groups": [
        "sudo"
      ],
      "shell": "/bin/bash",
      "sudo": "ALL=(ALL) NOPASSWD:ALL",
      "ssh_authorized_keys": [
        "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGq2y8f8k0bM8m3k0Yx4c8i5lQ0v9qk2nq3m4o5p6r7s vmh@host"
      ]
    }
  ]
}
`},
		{"password only", vm.CloudInit{Password: "pw"}, `#cloud-config
{
  "users": [
    {
      "name": "vmh",
      "groups": [
        "sudo"
      ],
      "shell": "/bin/bash",
      "sudo": "ALL=(ALL) NOPASSWD:ALL",
      "lock_passwd": false
    }
  ],
  "chpasswd": {
    "expire": false,
    "users": [
      {
        "name": "vmh",
        "password": "pw",
        "type": "text"
      }
    ]
  },
  "ssh_pwauth": true
}
`},
		{"everything", fullConfig, `#cloud-config
{
  "users": [
    {
      "name": "dev",
      "groups": [
        "sudo"
      ],
      "shell": "/bin/bash",
      "sudo": "ALL=(ALL) NOPASSWD:ALL",
      "lock_passwd": false,
      "ssh_authorized_keys": [
        "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGq2y8f8k0bM8m3k0Yx4c8i5lQ0v9qk2nq3m4o5p6r7s vmh@host",
        "ssh-rsa AAAAB3NzaC1yc2E dev@laptop"
      ]
    }
  ],
  "chpasswd": {
    "expire": false,
    "users": [
      {
        "name": "dev",
        "password": "s3cret",
        "type": "text"
      }
    ]
  },
  "ssh_pwauth": true,
  "packages": [
    "curl",
    "jq"
  ],
  "runcmd": [
    "echo <ready> & date > /tmp/ready",
    "systemctl restart ssh"
  ]
}
`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := cloudinit.UserData(c.ci)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}

func TestUserDataEscapesCharactersYAMLCannotHold(t *testing.T) {
	cmd := "printf 'a\x7fb\u0085c\u0090d\ufffe'"
	got, err := cloudinit.UserData(vm.CloudInit{RunCmd: []string{cmd}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []rune{0x7f, 0x85, 0x90, 0xfffe} {
		if bytes.ContainsRune(got, r) {
			t.Errorf("user data contains raw %U", r)
		}
	}
	var cfg struct{ RunCmd []string }
	if err := json.Unmarshal(body(t, got), &cfg); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.RunCmd, []string{cmd}) {
		t.Fatalf("runcmd round trip = %q, want %q", cfg.RunCmd, cmd)
	}
}

func TestUserDataPassthrough(t *testing.T) {
	for _, raw := range []string{
		"#cloud-config\npackages: [htop]\n",
		"#cloud-config-archive\n- type: text/cloud-config\n",
		"#!/bin/sh\necho hello\n",
	} {
		got, err := cloudinit.UserData(vm.CloudInit{UserData: raw, User: "ignored user!", Password: "x"})
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if string(got) != raw {
			t.Fatalf("got %q, want %q", got, raw)
		}
	}
}

func TestUserDataRejects(t *testing.T) {
	cases := []struct {
		name string
		ci   vm.CloudInit
	}{
		{"user data without header", vm.CloudInit{UserData: "users: []\n"}},
		{"user data with BOM", vm.CloudInit{UserData: "\ufeff#cloud-config\n"}},
		{"user data with leading space", vm.CloudInit{UserData: " #cloud-config\n"}},
		{"json user data", vm.CloudInit{UserData: `{"users":[]}`}},
		{"user with space", vm.CloudInit{User: "dev user"}},
		{"upper case user", vm.CloudInit{User: "Dev"}},
		{"user starting with digit", vm.CloudInit{User: "1dev"}},
		{"user starting with dash", vm.CloudInit{User: "-dev"}},
		{"user too long", vm.CloudInit{User: strings.Repeat("u", 33)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := cloudinit.UserData(c.ci); !errors.Is(err, vm.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestMetaData(t *testing.T) {
	cases := []struct {
		id, host, want string
	}{
		{"vmh-web", "web", "instance-id: \"vmh-web\"\nlocal-hostname: \"web\"\n"},
		{"vmh-web", "", "instance-id: \"vmh-web\"\n"},
		{"123", "true", "instance-id: \"123\"\nlocal-hostname: \"true\"\n"},
		{"id: x", "a\"b\x7f", "instance-id: \"id: x\"\nlocal-hostname: \"a\\\"b\\u007f\"\n"},
	}
	for _, c := range cases {
		if got := string(cloudinit.MetaData(c.id, c.host)); got != c.want {
			t.Errorf("MetaData(%q, %q) = %q, want %q", c.id, c.host, got, c.want)
		}
	}
}

func TestWriteSeed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seed.iso")
	ci := vm.CloudInit{SSHAuthorizedKeys: []string{testKey}, NetworkConfig: networkConfig}
	if err := cloudinit.WriteSeed(path, ci, "vmh-web", "web"); err != nil {
		t.Fatal(err)
	}
	userData, err := cloudinit.UserData(ci)
	if err != nil {
		t.Fatal(err)
	}
	checkSeed(t, path, map[string]string{
		"meta-data":      string(cloudinit.MetaData("vmh-web", "web")),
		"network-config": networkConfig,
		"user-data":      string(userData),
	})
	checkDirectory(t, dir, "seed.iso")
}

func TestWriteSeedWithoutNetworkConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seed.iso")
	if err := cloudinit.WriteSeed(path, vm.CloudInit{UserData: "#!/bin/sh\ntrue\n"}, "vmh-db", "db"); err != nil {
		t.Fatal(err)
	}
	checkSeed(t, path, map[string]string{
		"meta-data": "instance-id: \"vmh-db\"\nlocal-hostname: \"db\"\n",
		"user-data": "#!/bin/sh\ntrue\n",
	})
}

func TestWriteSeedHostnameLivesInMetaData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed.iso")
	ci := vm.CloudInit{SSHAuthorizedKeys: []string{testKey}, Hostname: "web-1"}
	if err := cloudinit.WriteSeed(path, ci, "vmh-web", ""); err != nil {
		t.Fatal(err)
	}
	files := readSeed(t, path)
	if got, want := string(files["meta-data"]), "instance-id: \"vmh-web\"\nlocal-hostname: \"web-1\"\n"; got != want {
		t.Errorf("meta-data = %q, want %q", got, want)
	}
	checkNoHostname(t, files["user-data"])
}

func TestWriteSeedReplacesExistingSeed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seed.iso")
	for _, host := range []string{"old", "new"} {
		if err := cloudinit.WriteSeed(path, vm.CloudInit{}, "vmh-x", host); err != nil {
			t.Fatal(err)
		}
	}
	files := readSeed(t, path)
	if len(files) != 2 || string(files["meta-data"]) != "instance-id: \"vmh-x\"\nlocal-hostname: \"new\"\n" {
		t.Fatalf("seed was not replaced: %q", files)
	}
	checkDirectory(t, dir, "seed.iso")
}

func TestWriteSeedRejectsInvalidInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seed.iso")
	if err := cloudinit.WriteSeed(path, vm.CloudInit{UserData: "not a header"}, "vmh-x", "x"); !errors.Is(err, vm.ErrInvalid) {
		t.Errorf("bad user data: err = %v, want ErrInvalid", err)
	}
	if err := cloudinit.WriteSeed(path, vm.CloudInit{}, "", "x"); !errors.Is(err, vm.ErrInvalid) {
		t.Errorf("empty instance id: err = %v, want ErrInvalid", err)
	}
	checkDirectory(t, dir)
}

func TestWriteSeedMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "seed.iso")
	if err := cloudinit.WriteSeed(path, vm.CloudInit{}, "vmh-x", "x"); err == nil {
		t.Fatal("expected an error for a missing directory")
	}
}

func TestReseed(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "seed.iso"), filepath.Join(dir, "clone.iso")
	ci := vm.CloudInit{SSHAuthorizedKeys: []string{testKey}, Hostname: "web-1", NetworkConfig: networkConfig}
	if err := cloudinit.WriteSeed(src, ci, "vmh-web", "web-1"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := cloudinit.Reseed(src, dst, "vmh-clone", "clone"); err != nil {
		t.Fatal(err)
	}
	userData, err := cloudinit.UserData(ci)
	if err != nil {
		t.Fatal(err)
	}
	checkSeed(t, dst, map[string]string{
		"meta-data":      "instance-id: \"vmh-clone\"\nlocal-hostname: \"clone\"\n",
		"network-config": networkConfig,
		"user-data":      string(userData),
	})
	checkNoHostname(t, readSeed(t, dst)["user-data"])
	if after, err := os.ReadFile(src); err != nil || !bytes.Equal(after, before) {
		t.Errorf("source seed changed: %v", err)
	}
	checkDirectory(t, dir, "clone.iso", "seed.iso")
}

func TestReseedInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seed.iso")
	if err := cloudinit.WriteSeed(path, vm.CloudInit{UserData: "#!/bin/sh\ntrue\n"}, "vmh-db", "db"); err != nil {
		t.Fatal(err)
	}
	if err := cloudinit.Reseed(path, path, "vmh-db2", ""); err != nil {
		t.Fatal(err)
	}
	checkSeed(t, path, map[string]string{
		"meta-data": "instance-id: \"vmh-db2\"\n",
		"user-data": "#!/bin/sh\ntrue\n",
	})
	checkDirectory(t, dir, "seed.iso")
}

func TestReseedRejects(t *testing.T) {
	dir := t.TempDir()
	image := func(name, label string, files ...iso9660.File) string {
		var buf bytes.Buffer
		if err := iso9660.Write(&buf, label, files); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	seed := image("seed.iso", "cidata", iso9660.File{Name: "user-data", Data: []byte("#cloud-config\n")})
	noUserData := image("meta-only.iso", "cidata", iso9660.File{Name: "meta-data", Data: []byte("instance-id: x\n")})
	otherLabel := image("config-2.iso", "config-2", iso9660.File{Name: "user-data", Data: []byte("#cloud-config\n")})
	garbage := filepath.Join(dir, "garbage.iso")
	if err := os.WriteFile(garbage, bytes.Repeat([]byte("x"), 64<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "clone.iso")
	cases := []struct {
		name, src, id string
		want          error
	}{
		{"missing source", filepath.Join(dir, "absent.iso"), "vmh-clone", fs.ErrNotExist},
		{"not an image", garbage, "vmh-clone", vm.ErrInvalid},
		{"no user data", noUserData, "vmh-clone", vm.ErrInvalid},
		{"not a cidata volume", otherLabel, "vmh-clone", vm.ErrInvalid},
		{"empty instance id", seed, "", vm.ErrInvalid},
	}
	for _, c := range cases {
		if err := cloudinit.Reseed(c.src, dst, c.id, "clone"); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	checkDirectory(t, dir, "config-2.iso", "garbage.iso", "meta-only.iso", "seed.iso")
}

func TestSeedsArePrivate(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "seed.iso"), filepath.Join(dir, "clone.iso")
	if err := cloudinit.WriteSeed(src, vm.CloudInit{Password: "s3cret"}, "vmh-web", "web"); err != nil {
		t.Fatal(err)
	}
	if err := secfiletest.CheckRestricted(src); err != nil {
		t.Error(err)
	}
	if err := os.WriteFile(dst, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cloudinit.Reseed(src, dst, "vmh-clone", "clone"); err != nil {
		t.Fatal(err)
	}
	if err := secfiletest.CheckRestricted(dst); err != nil {
		t.Error(err)
	}
}

func body(t *testing.T, userData []byte) []byte {
	t.Helper()
	header, rest, ok := bytes.Cut(userData, []byte("\n"))
	if !ok || string(header) != "#cloud-config" {
		t.Fatalf("user data header = %q", header)
	}
	return rest
}

func readSeed(t *testing.T, path string) map[string][]byte {
	t.Helper()
	img, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	label, files, err := iso9660.Read(img)
	if err != nil {
		t.Fatal(err)
	}
	if label != "cidata" {
		t.Errorf("%s has label %q, want cidata", path, label)
	}
	return files
}

func checkSeed(t *testing.T, path string, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	for name, data := range readSeed(t, path) {
		got[name] = string(data)
	}
	if !maps.Equal(got, want) {
		t.Errorf("seed files:\n got %q\nwant %q", got, want)
	}
}

func checkNoHostname(t *testing.T, userData []byte) {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(body(t, userData), &cfg); err != nil {
		t.Fatal(err)
	}
	if host, ok := cfg["hostname"]; ok {
		t.Errorf("user data sets hostname %q, which overrides local-hostname in meta-data", host)
	}
}

func checkDirectory(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, want) {
		t.Errorf("directory holds %q, want %q", names, want)
	}
}
