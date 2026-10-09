package sshexec

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ZetGames/vm-harness/internal/secfile/secfiletest"
	"github.com/ZetGames/vm-harness/vm"
)

func TestGenerateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "virtualbox-web")

	authorized, err := GenerateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(authorized, "ssh-ed25519 AAAA") || !strings.HasSuffix(authorized, " vmh") {
		t.Errorf("authorized key = %q", authorized)
	}

	pubFile, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if string(pubFile) != authorized+"\n" {
		t.Errorf(".pub = %q, want %q", pubFile, authorized+"\n")
	}
	pub, comment, _, _, err := ssh.ParseAuthorizedKey(pubFile)
	if err != nil {
		t.Fatal(err)
	}
	if comment != "vmh" {
		t.Errorf("comment = %q", comment)
	}

	privPEM, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(privPEM, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n")) {
		t.Errorf("private key is not OpenSSH PEM: %q", privPEM[:min(len(privPEM), 40)])
	}
	signer, err := ssh.ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(signer.PublicKey().Marshal(), pub.Marshal()) {
		t.Error("private key does not match the public key")
	}

	if err := secfiletest.CheckRestricted(path); err != nil {
		t.Error(err)
	}
	if runtime.GOOS != "windows" {
		for file, want := range map[string]os.FileMode{path: 0o600, path + ".pub": 0o644} {
			info, err := os.Stat(file)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != want {
				t.Errorf("%s mode = %o, want %o", file, perm, want)
			}
		}
	}
}

func TestGenerateKeyRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "id")
	if _, err := GenerateKey(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateKey(path); !errors.Is(err, vm.ErrExists) {
		t.Errorf("err = %v, want ErrExists", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("existing private key was overwritten")
	}

	orphan := filepath.Join(dir, "orphan")
	if err := os.WriteFile(orphan+".pub", []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateKey(orphan); !errors.Is(err, vm.ErrExists) {
		t.Errorf("err = %v, want ErrExists", err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("private key left behind after failure: %v", err)
	}
	if data, _ := os.ReadFile(orphan + ".pub"); string(data) != "keep me\n" {
		t.Errorf("existing .pub was modified: %q", data)
	}
}
