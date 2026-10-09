package virtualbox

import (
	"context"
	"crypto/rand"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ZetGames/vm-harness/internal/shellquote"
	"github.com/ZetGames/vm-harness/vm"
)

const (
	ipProperty   = "/VirtualBox/GuestInfo/Net/0/V4/IP"
	exitTimedOut = 19
)

var (
	toolError  = regexp.MustCompile(`(?m)^VBoxManage(?:\.exe)?: error: `)
	cmdSpecial = regexp.MustCompile(`[()%!^"<>&|]`)
)

func (p *Provider) GuestIP(ctx context.Context, ref string) (string, error) {
	info, err := p.inspect(ctx, ref)
	if err != nil {
		return "", err
	}
	if s := info.state(); s != vm.StateRunning && s != vm.StatePaused {
		return "", fmt.Errorf("vm %s is %s: %w", info.name(), s, vm.ErrInvalidState)
	}
	out, err := p.run(ctx, "guestproperty", "get", info.id(), ipProperty)
	if err != nil {
		return "", err
	}
	if ip, ok := lineValue(out, "Value:"); ok && ip != "" {
		return ip, nil
	}
	return "", fmt.Errorf("vm %s has not reported an ip address yet: %w", info.name(), vm.ErrNotReady)
}

func (p *Provider) Exec(ctx context.Context, ref string, req vm.ExecRequest) (vm.ExecResult, error) {
	if req.User == "" {
		return vm.ExecResult{}, fmt.Errorf("guest exec needs a user: %w", vm.ErrInvalid)
	}
	m, err := p.Get(ctx, ref)
	if err != nil {
		return vm.ExecResult{}, err
	}
	if m.State != vm.StateRunning {
		return vm.ExecResult{}, fmt.Errorf("vm %s is %s: %w", m.Name, m.State, vm.ErrInvalidState)
	}
	windows := m.IsWindowsGuest()
	secret, err := writeSecret(req.Password)
	if err != nil {
		return vm.ExecResult{}, err
	}
	defer os.Remove(secret)
	if windows && strings.ContainsAny(req.Script, "\r\n") {
		batch, err := p.copyBatch(ctx, m.ID, req.User, secret, req.Script)
		if err != nil {
			return vm.ExecResult{}, err
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			p.run(ctx, append(guestArgs(m.ID, "rm", req.User, secret), batch)...)
		}()
		req.Script, req.Command = "", []string{vm.WindowsShell, "/d", "/c", batch}
	}
	argv, unquoted, err := guestCommand(windows, req)
	if err != nil {
		return vm.ExecResult{}, err
	}
	args := append(guestArgs(m.ID, "run", req.User, secret), "--wait-stdout", "--wait-stderr")
	if unquoted {
		args = append(args, "--unquoted-args")
	}
	if req.TimeoutSec > 0 {
		args = append(args, "--timeout", strconv.Itoa(req.TimeoutSec*1000))
	}
	if req.WorkDir != "" {
		args = append(args, "--cwd", req.WorkDir)
	}
	for _, k := range slices.Sorted(maps.Keys(req.Env)) {
		args = append(args, "--putenv", k+"="+req.Env[k])
	}
	args = append(append(args, "--"), argv...)
	start := time.Now()
	res, err := p.call(ctx, args)
	if err != nil {
		return vm.ExecResult{}, err
	}
	elapsed := time.Since(start)
	if toolError.Match(res.Stderr) {
		return vm.ExecResult{}, p.commandError(args, res)
	}
	if timeout := time.Duration(req.TimeoutSec) * time.Second; timeout > 0 && res.ExitCode == exitTimedOut && elapsed >= timeout {
		return vm.ExecResult{}, fmt.Errorf("guest command did not finish within %ds: %w", req.TimeoutSec, context.DeadlineExceeded)
	}
	return vm.ExecResult{
		ExitCode:   res.ExitCode,
		Stdout:     string(res.Stdout),
		Stderr:     string(res.Stderr),
		DurationMS: elapsed.Milliseconds(),
	}, nil
}

func guestCommand(windows bool, req vm.ExecRequest) (argv []string, unquoted bool, err error) {
	switch {
	case req.Script != "" && windows:
		return cmdArgs(req.Script), true, nil
	case req.Script != "":
		return []string{"/bin/sh", "-c", req.Script}, false, nil
	case len(req.Command) == 0 || req.Command[0] == "":
		return nil, false, fmt.Errorf("guest exec needs a command or a script: %w", vm.ErrInvalid)
	case windows && !windowsAbs(req.Command[0]):
		line, err := cmdLine(req.Command)
		if err != nil {
			return nil, false, err
		}
		return cmdArgs(line), true, nil
	case !windows && !strings.HasPrefix(req.Command[0], "/"):
		return append([]string{"/usr/bin/env"}, req.Command...), false, nil
	}
	return req.Command, false, nil
}

func (p *Provider) copyBatch(ctx context.Context, id, user, secret, script string) (string, error) {
	local, err := writeSecret(batchFile(script))
	if err != nil {
		return "", err
	}
	defer os.Remove(local)
	guest := `C:\Windows\Temp\vmh-` + strings.ToLower(rand.Text()) + ".cmd"
	_, err = p.run(ctx, append(guestArgs(id, "copyto", user, secret), local, guest)...)
	return guest, err
}

func batchFile(script string) string {
	script = strings.ReplaceAll(strings.ReplaceAll(script, "\r\n", "\n"), "\n", "\r\n")
	return "@echo off\r\nsetlocal DisableDelayedExpansion\r\n" + script + "\r\nexit /b %errorlevel%\r\n"
}

func cmdArgs(line string) []string {
	return []string{vm.WindowsShell, "/d", "/s", "/c", `"` + line + `"`}
}

func cmdLine(argv []string) (string, error) {
	if strings.Contains(argv[0], `"`) || slices.ContainsFunc(argv, func(arg string) bool { return strings.ContainsAny(arg, "\r\n") }) {
		return "", fmt.Errorf("windows command %q: a program name without a path must not contain quotes and arguments must not contain line breaks: %w", argv[0], vm.ErrInvalid)
	}
	line := cmdSpecial.ReplaceAllString(argv[0], "^$0")
	if strings.ContainsAny(argv[0], " \t") {
		line = `"` + argv[0] + `"`
	}
	for _, arg := range argv[1:] {
		line += " " + cmdSpecial.ReplaceAllString(shellquote.Windows(arg), "^$0")
	}
	return line, nil
}

func windowsAbs(path string) bool {
	if strings.HasPrefix(path, `\\`) {
		return true
	}
	return len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/')
}

func guestArgs(id, sub, user, secret string) []string {
	return []string{"guestcontrol", id, sub, "--username", user, "--passwordfile", secret}
}

func (p *Provider) CopyTo(ctx context.Context, ref string, req vm.CopyRequest) error {
	return p.transfer(ctx, ref, req, true)
}

func (p *Provider) CopyFrom(ctx context.Context, ref string, req vm.CopyRequest) error {
	return p.transfer(ctx, ref, req, false)
}

func (p *Provider) transfer(ctx context.Context, ref string, req vm.CopyRequest, toGuest bool) error {
	if err := checkRef(ref); err != nil {
		return err
	}
	switch {
	case req.User == "":
		return fmt.Errorf("guest copy needs a user: %w", vm.ErrInvalid)
	case !strings.HasPrefix(req.GuestPath, "/") && !windowsAbs(req.GuestPath):
		return fmt.Errorf("guest path %q must be absolute: %w", req.GuestPath, vm.ErrInvalid)
	case !filepath.IsAbs(req.HostPath):
		return fmt.Errorf("host path %q must be absolute: %w", req.HostPath, vm.ErrInvalid)
	}
	secret, err := writeSecret(req.Password)
	if err != nil {
		return err
	}
	defer os.Remove(secret)
	if !toGuest {
		_, err := p.run(ctx, append(guestArgs(ref, "copyfrom", req.User, secret), req.GuestPath, req.HostPath)...)
		return err
	}
	if dir := guestDir(req.GuestPath); dir != "" {
		if _, err := p.run(ctx, append(guestArgs(ref, "mkdir", req.User, secret), "--parents", dir)...); err != nil {
			return err
		}
	}
	_, err = p.run(ctx, append(guestArgs(ref, "copyto", req.User, secret), req.HostPath, req.GuestPath)...)
	return err
}

func guestDir(path string) string {
	i := strings.LastIndexAny(path, `/\`)
	if i <= 0 || strings.HasSuffix(path[:i], ":") {
		return ""
	}
	return path[:i]
}

func (p *Provider) Screenshot(ctx context.Context, ref string, _ vm.Credentials) ([]byte, error) {
	if err := checkRef(ref); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "vmh-screenshot-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "screen.png")
	if _, err := p.run(ctx, "controlvm", ref, "screenshotpng", file); err != nil {
		return nil, err
	}
	return os.ReadFile(file)
}

func writeSecret(secret string) (string, error) {
	f, err := os.CreateTemp("", "vmh-secret-")
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(secret)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
