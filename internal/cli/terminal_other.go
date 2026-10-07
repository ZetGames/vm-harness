//go:build !unix && !windows

package cli

func terminalFd(uintptr) bool { return false }
