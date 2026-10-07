//go:build !windows

package secfiletest

import (
	"fmt"
	"os"
)

func CheckRestricted(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s is not private to the current user: mode %o", path, perm)
	}
	return nil
}
