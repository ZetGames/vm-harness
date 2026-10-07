package sshexec

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/fl4metf/vm-harness/internal/secfile"
	"github.com/fl4metf/vm-harness/vm"
)

var errHostKey = errors.New("host key verification failed")

func hostKeyCallback(knownHostsPath string) (ssh.HostKeyCallback, error) {
	if knownHostsPath == "" {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	known, err := knownhosts.New(knownHostsPath)
	if errors.Is(err, fs.ErrNotExist) {
		known, err = knownhosts.New()
	}
	if err != nil {
		return nil, fmt.Errorf("known hosts: %w", err)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := known(hostname, remote, key)
		var keyErr *knownhosts.KeyError
		switch {
		case err == nil:
			return nil
		case errors.As(err, &keyErr) && len(keyErr.Want) == 0:
			if err := appendKnownHost(knownHostsPath, hostname, key); err != nil {
				return fmt.Errorf("%w: %w", errHostKey, err)
			}
			return nil
		case errors.As(err, &keyErr):
			return fmt.Errorf("%w: %s presented %s key %s which does not match the key recorded in %s; "+
				"if the guest's host keys were regenerated on purpose, delete that file to trust the new key: %w",
				errHostKey, knownhosts.Normalize(hostname), key.Type(), ssh.FingerprintSHA256(key), knownHostsPath, vm.ErrInvalidState)
		default:
			return fmt.Errorf("%w: %w", errHostKey, err)
		}
	}, nil
}

func appendKnownHost(knownHostsPath, hostname string, key ssh.PublicKey) error {
	if err := os.MkdirAll(filepath.Dir(knownHostsPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(knownHostsPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := secfile.Restrict(knownHostsPath); err != nil {
		f.Close()
		return err
	}
	if _, err := fmt.Fprintln(f, knownhosts.Line([]string{hostname}, key)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
