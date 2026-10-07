//go:build !windows

package runner

import "os/exec"

func hideWindow(*exec.Cmd) {}
