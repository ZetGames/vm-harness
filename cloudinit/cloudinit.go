package cloudinit

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ZetGames/vm-harness/internal/iso9660"
	"github.com/ZetGames/vm-harness/internal/secfile"
	"github.com/ZetGames/vm-harness/vm"
)

const (
	defaultUser = "vmh"
	volumeLabel = "cidata"
)

var validUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

type cloudConfig struct {
	Users     []user    `json:"users"`
	Chpasswd  *chpasswd `json:"chpasswd,omitempty"`
	SSHPwauth bool      `json:"ssh_pwauth,omitempty"`
	Packages  []string  `json:"packages,omitempty"`
	RunCmd    []string  `json:"runcmd,omitempty"`
}

type user struct {
	Name              string   `json:"name"`
	Groups            []string `json:"groups"`
	Shell             string   `json:"shell"`
	Sudo              string   `json:"sudo"`
	LockPasswd        *bool    `json:"lock_passwd,omitempty"`
	SSHAuthorizedKeys []string `json:"ssh_authorized_keys,omitempty"`
}

type chpasswd struct {
	Expire bool           `json:"expire"`
	Users  []userPassword `json:"users"`
}

type userPassword struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Type     string `json:"type"`
}

func UserData(ci vm.CloudInit) ([]byte, error) {
	if ci.UserData != "" {
		if !strings.HasPrefix(ci.UserData, "#cloud-config") && !strings.HasPrefix(ci.UserData, "#!") {
			return nil, fmt.Errorf("user data must start with #cloud-config or #!: %w", vm.ErrInvalid)
		}
		return []byte(ci.UserData), nil
	}

	name := cmp.Or(ci.User, defaultUser)
	if !validUser.MatchString(name) {
		return nil, fmt.Errorf("cloud-init user %q is not a valid login name: %w", name, vm.ErrInvalid)
	}
	u := user{
		Name:              name,
		Groups:            []string{"sudo"},
		Shell:             "/bin/bash",
		Sudo:              "ALL=(ALL) NOPASSWD:ALL",
		SSHAuthorizedKeys: ci.SSHAuthorizedKeys,
	}
	cfg := cloudConfig{Packages: ci.Packages, RunCmd: ci.RunCmd}
	if ci.Password != "" {
		unlocked := false
		u.LockPasswd = &unlocked
		cfg.Chpasswd = &chpasswd{Users: []userPassword{{Name: name, Password: ci.Password, Type: "text"}}}
		cfg.SSHPwauth = true
	}
	cfg.Users = []user{u}

	var buf bytes.Buffer
	buf.WriteString("#cloud-config\n")
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	return yamlSafe(buf.Bytes()), nil
}

func MetaData(instanceID, hostname string) []byte {
	b := []byte("instance-id: " + quote(instanceID) + "\n")
	if hostname != "" {
		b = append(b, "local-hostname: "+quote(hostname)+"\n"...)
	}
	return b
}

func WriteSeed(path string, ci vm.CloudInit, instanceID, hostname string) error {
	if instanceID == "" {
		return fmt.Errorf("cloud-init instance id is empty: %w", vm.ErrInvalid)
	}
	userData, err := UserData(ci)
	if err != nil {
		return err
	}
	return writeSeed(path, userData, []byte(ci.NetworkConfig), MetaData(instanceID, cmp.Or(hostname, ci.Hostname)))
}

func Reseed(src, dst, instanceID, hostname string) error {
	if instanceID == "" {
		return fmt.Errorf("cloud-init instance id is empty: %w", vm.ErrInvalid)
	}
	img, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read cloud-init seed: %w", err)
	}
	label, files, err := iso9660.Read(img)
	if err != nil {
		return fmt.Errorf("read cloud-init seed %s: %w", src, err)
	}
	userData, ok := files["user-data"]
	if !ok || !strings.EqualFold(label, volumeLabel) {
		return fmt.Errorf("%s is not a cloud-init seed with user-data: %w", src, vm.ErrInvalid)
	}
	return writeSeed(dst, userData, files["network-config"], MetaData(instanceID, hostname))
}

func writeSeed(path string, userData, networkConfig, metaData []byte) error {
	files := []iso9660.File{
		{Name: "user-data", Data: userData},
		{Name: "meta-data", Data: metaData},
	}
	if len(networkConfig) > 0 {
		files = append(files, iso9660.File{Name: "network-config", Data: networkConfig})
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("write cloud-init seed %s: %w", path, err)
	}
	err = secfile.Restrict(f.Name())
	if err == nil {
		err = iso9660.Write(f, volumeLabel, files)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("write cloud-init seed %s: %w", path, err)
	}
	return nil
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(yamlSafe(b))
}

func yamlSafe(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for _, r := range string(b) {
		if (r >= 0x7f && r <= 0x9f) || r == 0xfffe || r == 0xffff {
			out = fmt.Appendf(out, `\u%04x`, r)
			continue
		}
		out = utf8.AppendRune(out, r)
	}
	return out
}
