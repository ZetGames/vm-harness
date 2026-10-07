package sshexec

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/fl4metf/vm-harness/internal/secfile"
	"github.com/fl4metf/vm-harness/vm"
)

const keyComment = "vmh"

func GenerateKey(path string) (string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, keyComment)
	if err != nil {
		return "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	authorizedKey := strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(sshPub)), "\n") + " " + keyComment

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := writeNewFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", err
	}
	if err := writeNewFile(path+".pub", []byte(authorizedKey+"\n"), 0o644); err != nil {
		os.Remove(path)
		return "", err
	}
	return authorizedKey, nil
}

func writeNewFile(path string, data []byte, perm fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s: %w", path, vm.ErrExists)
	}
	if err != nil {
		return err
	}
	if perm&0o077 == 0 {
		err = secfile.Restrict(path)
	}
	if err == nil {
		_, err = f.Write(data)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(path)
		return err
	}
	return nil
}
