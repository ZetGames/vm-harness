//go:build unix

package cli

import "golang.org/x/sys/unix"

func terminalFd(fd uintptr) bool {
	_, err := unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ)
	return err == nil
}
