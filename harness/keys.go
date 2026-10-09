package harness

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ZetGames/vm-harness/internal/secfile"
	"github.com/ZetGames/vm-harness/internal/sshexec"
	"github.com/ZetGames/vm-harness/vm"
)

const knownHostsSuffix = ".known_hosts"

func idHash(id string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(id))))
	return hex.EncodeToString(sum[:8])
}

func (m *Manager) newKeyPath(provider, name string) string {
	suffix := make([]byte, 4)
	rand.Read(suffix)
	return filepath.Join(m.keysDir(), provider+"-"+name+"-"+hex.EncodeToString(suffix))
}

func ownsKey(provider, name, key string) bool {
	suffix, ok := strings.CutPrefix(pathKey(filepath.Base(key)), pathKey(provider+"-"+name+"-"))
	if !ok || len(suffix) != 8 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func (m *Manager) knownHostsPath(mach vm.Machine) string {
	if key := mach.Meta[vm.MetaSSHKey]; key != "" {
		return key + knownHostsSuffix
	}
	return filepath.Join(m.keysDir(), mach.Provider+"-"+idHash(mach.ID)+knownHostsSuffix)
}

func (m *Manager) generateSSHKey(spec *vm.Spec) (string, error) {
	ci := spec.CloudInit
	if ci == nil || ci.UserData != "" {
		return "", nil
	}
	if err := m.prepareKeysDir(); err != nil {
		return "", err
	}
	path := m.newKeyPath(spec.Provider, spec.Name)
	pub, err := sshexec.GenerateKey(path)
	if err != nil {
		return "", fmt.Errorf("generate ssh key: %w", err)
	}
	ci.SSHAuthorizedKeys = append(ci.SSHAuthorizedKeys, strings.TrimSpace(pub))
	spec.Meta[vm.MetaSSHKey] = path
	return path, nil
}

func (m *Manager) prepareKeysDir() error {
	dir := m.keysDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create key directory: %w", err)
	}
	if err := secfile.Restrict(dir); err != nil {
		return fmt.Errorf("restrict key directory: %w", err)
	}
	return nil
}

func (m *Manager) copyKey(src, provider, name string) (string, error) {
	if err := m.prepareKeysDir(); err != nil {
		return "", err
	}
	dst := m.newKeyPath(provider, name)
	for _, suffix := range []string{"", ".pub"} {
		data, err := os.ReadFile(src + suffix)
		if errors.Is(err, fs.ErrNotExist) && suffix != "" {
			continue
		}
		if err == nil {
			err = writeNew(dst+suffix, data, suffix == "")
		}
		if err != nil {
			m.removeKey(dst)
			return "", fmt.Errorf("copy ssh key: %w", err)
		}
	}
	return dst, nil
}

func writeNew(path string, data []byte, private bool) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if private {
		err = secfile.Restrict(path)
	}
	if err == nil {
		_, err = f.Write(data)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (m *Manager) removeKey(key string) {
	if key == "" {
		return
	}
	for _, suffix := range []string{"", ".pub", knownHostsSuffix} {
		m.removeKeyFile(key + suffix)
	}
}

func (m *Manager) forgetHostKeys(mach vm.Machine) {
	m.removeKeyFile(m.knownHostsPath(mach))
}

func (m *Manager) removeKeyFile(path string) {
	if pathKey(filepath.Dir(path)) != pathKey(m.keysDir()) {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.log.Warn("remove ssh key file", "path", path, "error", err)
	}
}
