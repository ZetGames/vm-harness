package vmware

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"net"
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
	linuxShell     = "/bin/sh"
	windowsKill    = `C:\Windows\System32\taskkill.exe`
	cleanupTimeout = 30 * time.Second
	killAfterSec   = 5
	guestSlack     = 15 * time.Second
)

var (
	errNoCredentials = fmt.Errorf("guest user and password are required: %w", vm.ErrInvalid)
	exitCodePattern  = regexp.MustCompile(`exit code: (-?\d+)`)
	processPattern   = regexp.MustCompile(`(?m)^pid=(\d+), owner=.*?, cmd=(.*?)\r?$`)
	envNamePattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func (p *Provider) GuestIP(ctx context.Context, ref string) (string, error) {
	path, err := p.resolve(ctx, ref)
	if err != nil {
		return "", err
	}
	ip, err := p.toolsIP(ctx, path)
	if errors.Is(err, vm.ErrNotReady) {
		if leased := p.leasedIP(path); leased != "" {
			return leased, nil
		}
	}
	return ip, err
}

func (p *Provider) toolsIP(ctx context.Context, path string) (string, error) {
	out, err := p.vmrunCmd(ctx, "getGuestIPAddress", path)
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(out)
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("guest reported ip %q: %w", ip, vm.ErrNotReady)
	}
	return ip, nil
}

func (p *Provider) Exec(ctx context.Context, ref string, req vm.ExecRequest) (vm.ExecResult, error) {
	if req.User == "" {
		return vm.ExecResult{}, errNoCredentials
	}
	if len(req.Command) == 0 && req.Script == "" {
		return vm.ExecResult{}, fmt.Errorf("a command or a script is required: %w", vm.ErrInvalid)
	}
	for key := range req.Env {
		if !envNamePattern.MatchString(key) {
			return vm.ExecResult{}, fmt.Errorf("environment variable name %q: %w", key, vm.ErrInvalid)
		}
	}
	path, err := p.resolve(ctx, ref)
	if err != nil {
		return vm.ExecResult{}, err
	}
	windows, err := windowsGuest(path)
	if err != nil {
		return vm.ExecResult{}, err
	}
	job := newGuestJob(windows)
	script, err := job.script(req)
	if err != nil {
		return vm.ExecResult{}, err
	}
	if err := p.toolsReady(ctx, path); err != nil {
		return vm.ExecResult{}, err
	}
	started := time.Now()
	res, err := p.runJob(ctx, path, req, job, script)
	res.DurationMS = time.Since(started).Milliseconds()
	return res, err
}

func (p *Provider) runJob(ctx context.Context, path string, req vm.ExecRequest, job guestJob, script string) (vm.ExecResult, error) {
	dir, err := os.MkdirTemp("", "vmh-exec-")
	if err != nil {
		return vm.ExecResult{}, err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "script")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		return vm.ExecResult{}, err
	}
	cred := req.Credentials
	if _, err := p.guestCmd(ctx, cred, "CopyFileFromHostToGuest", path, file, job.scriptPath()); err != nil {
		return vm.ExecResult{}, err
	}
	defer p.removeGuestFiles(ctx, path, cred, job.scriptPath(), job.stdoutPath(), job.stderrPath())
	exitCode, err := p.runProgram(ctx, path, cred, req.TimeoutSec, job)
	if err != nil {
		return vm.ExecResult{}, err
	}
	stdout, err := p.readGuestFile(ctx, path, cred, job.stdoutPath(), filepath.Join(dir, "stdout"))
	if err != nil {
		return vm.ExecResult{}, err
	}
	stderr, err := p.readGuestFile(ctx, path, cred, job.stderrPath(), filepath.Join(dir, "stderr"))
	if err != nil {
		return vm.ExecResult{}, err
	}
	return vm.ExecResult{ExitCode: exitCode, Stdout: stdout, Stderr: stderr}, nil
}

func (p *Provider) runProgram(ctx context.Context, path string, cred vm.Credentials, timeoutSec int, job guestJob) (int, error) {
	runCtx := ctx
	if timeoutSec > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, job.hostTimeout(timeoutSec))
		defer cancel()
	}
	started := time.Now()
	_, err := p.guestCmd(runCtx, cred, append([]string{"runProgramInGuest", path}, job.command()...)...)
	if runCtx.Err() != nil {
		p.killJob(ctx, path, cred, job)
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, timeoutError(timeoutSec)
	}
	exitCode, err := exitStatus(err)
	if err == nil && timeoutSec > 0 && job.timedOut(exitCode) && time.Since(started) >= time.Duration(timeoutSec)*time.Second {
		return 0, timeoutError(timeoutSec)
	}
	return exitCode, err
}

func exitStatus(err error) (int, error) {
	var cmdErr *vm.CommandError
	if errors.As(err, &cmdErr) {
		if m := exitCodePattern.FindStringSubmatch(cmdErr.Stdout + cmdErr.Stderr); m != nil {
			return strconv.Atoi(m[1])
		}
	}
	return 0, err
}

func timeoutError(sec int) error {
	return fmt.Errorf("guest command did not finish within %ds: %w", sec, context.DeadlineExceeded)
}

func (p *Provider) killJob(ctx context.Context, path string, cred vm.Credentials, job guestJob) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	out, err := p.guestCmd(ctx, cred, "listProcessesInGuest", path)
	if err != nil {
		return
	}
	for _, m := range processPattern.FindAllStringSubmatch(out, -1) {
		if !strings.Contains(strings.ToLower(m[2]), strings.ToLower(job.scriptPath())) {
			continue
		}
		if job.windows {
			p.guestCmd(ctx, cred, "runProgramInGuest", path, windowsKill, "/F", "/T", "/PID", m[1])
		} else {
			p.guestCmd(ctx, cred, "killProcessInGuest", path, m[1])
		}
	}
}

func (p *Provider) readGuestFile(ctx context.Context, path string, cred vm.Credentials, guestPath, hostPath string) (string, error) {
	if _, err := p.guestCmd(ctx, cred, "CopyFileFromGuestToHost", path, guestPath, hostPath); err != nil {
		return "", err
	}
	data, err := os.ReadFile(hostPath)
	return string(data), err
}

func (p *Provider) removeGuestFiles(ctx context.Context, path string, cred vm.Credentials, files ...string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	for _, file := range files {
		p.guestCmd(ctx, cred, "deleteFileInGuest", path, file)
	}
}

func (p *Provider) CopyTo(ctx context.Context, ref string, req vm.CopyRequest) error {
	return p.transfer(ctx, ref, req.Credentials, "CopyFileFromHostToGuest", req.HostPath, req.GuestPath)
}

func (p *Provider) CopyFrom(ctx context.Context, ref string, req vm.CopyRequest) error {
	return p.transfer(ctx, ref, req.Credentials, "CopyFileFromGuestToHost", req.GuestPath, req.HostPath)
}

func (p *Provider) transfer(ctx context.Context, ref string, cred vm.Credentials, command, from, to string) error {
	if cred.User == "" {
		return errNoCredentials
	}
	path, err := p.resolve(ctx, ref)
	if err != nil {
		return err
	}
	if err := p.toolsReady(ctx, path); err != nil {
		return err
	}
	_, err = p.guestCmd(ctx, cred, command, path, from, to)
	return err
}

func (p *Provider) Screenshot(ctx context.Context, ref string, cred vm.Credentials) ([]byte, error) {
	if cred.User == "" {
		return nil, fmt.Errorf("vmware captures the screen through vmware tools and needs guest credentials: %w", vm.ErrInvalid)
	}
	path, err := p.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := p.toolsReady(ctx, path); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "vmh-screen-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "screen.png")
	if _, err := p.guestCmd(ctx, cred, "captureScreen", path, file); err != nil {
		return nil, err
	}
	return os.ReadFile(file)
}

func (p *Provider) toolsReady(ctx context.Context, path string) error {
	out, err := p.vmrunCmd(ctx, "checkToolsState", path)
	state := strings.TrimSpace(out)
	if state == "running" {
		return nil
	}
	if err != nil && state != "installed" && state != "unknown" {
		return err
	}
	running, err := p.isRunning(ctx, path)
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("vm %s is not running: %w", path, vm.ErrInvalidState)
	}
	return fmt.Errorf("vmware tools are not running in %s (state %s): %w", path, state, vm.ErrNotReady)
}

func windowsGuest(path string) (bool, error) {
	v, err := readVMX(path)
	if err != nil {
		return false, err
	}
	meta, err := readMeta(path)
	if err != nil {
		return false, err
	}
	return vm.Machine{OSType: v.get("guestOS"), Meta: meta}.IsWindowsGuest(), nil
}

type guestJob struct {
	windows bool
	base    string
}

func newGuestJob(windows bool) guestJob {
	id := strings.ToLower(rand.Text())
	if windows {
		return guestJob{windows: true, base: `C:\Windows\Temp\vmh-` + id}
	}
	return guestJob{base: "/tmp/vmh-" + id}
}

func (j guestJob) scriptPath() string {
	if j.windows {
		return j.base + ".cmd"
	}
	return j.base + ".sh"
}

func (j guestJob) stdoutPath() string { return j.base + ".out" }

func (j guestJob) stderrPath() string { return j.base + ".err" }

func (j guestJob) command() []string {
	if j.windows {
		return []string{vm.WindowsShell, "/c", j.scriptPath(), ">" + j.stdoutPath(), "2>" + j.stderrPath()}
	}
	return []string{linuxShell, j.scriptPath()}
}

func (j guestJob) hostTimeout(sec int) time.Duration {
	if j.windows {
		return time.Duration(sec) * time.Second
	}
	return time.Duration(sec)*time.Second + guestSlack
}

func (j guestJob) timedOut(exitCode int) bool {
	return !j.windows && (exitCode == 124 || exitCode == 128+9)
}

func (j guestJob) script(req vm.ExecRequest) (string, error) {
	if j.windows {
		return batchScript(req)
	}
	redirect := "exec >" + j.stdoutPath() + " 2>" + j.stderrPath() + "\n"
	body := shellquote.POSIXScript(req.Command, req.Script, req.Env, req.WorkDir) + "\n"
	if req.TimeoutSec <= 0 {
		return redirect + body, nil
	}
	wrap := fmt.Sprintf("if timeout -k %d 1 true 2>/dev/null; then export VMH_TIMEOUT=1; exec timeout -k %d %d %s \"$0\"; fi\n", killAfterSec, killAfterSec, req.TimeoutSec, linuxShell)
	return "if [ -z \"$VMH_TIMEOUT\" ]; then\n" + redirect + wrap + "fi\nunset VMH_TIMEOUT\n" + body, nil
}

func batchScript(req vm.ExecRequest) (string, error) {
	for _, value := range slices.Concat(req.Command, slices.Collect(maps.Values(req.Env)), []string{req.WorkDir}) {
		if strings.ContainsAny(value, "\r\n\x00") {
			return "", fmt.Errorf("command arguments, environment values and the working directory must not contain line breaks on windows guests: %w", vm.ErrInvalid)
		}
	}
	if strings.Contains(req.WorkDir, `"`) {
		return "", fmt.Errorf("working directory %q: %w", req.WorkDir, vm.ErrInvalid)
	}
	var b strings.Builder
	b.WriteString("setlocal DisableDelayedExpansion\r\n")
	for _, key := range slices.Sorted(maps.Keys(req.Env)) {
		fmt.Fprintf(&b, "set %s=%s\r\n", key, batchEscape(req.Env[key]))
	}
	if req.WorkDir != "" {
		fmt.Fprintf(&b, "cd /d \"%s\" || exit /b 1\r\n", strings.ReplaceAll(req.WorkDir, "%", "%%"))
	}
	if req.Script != "" {
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(req.Script, "\r\n", "\n"), "\n", "\r\n"))
	} else {
		args := make([]string, len(req.Command))
		for i, arg := range req.Command {
			fmt.Fprintf(&b, "set vmh_arg%d=%s\r\n", i, batchEscape(shellquote.Windows(arg)))
			args[i] = fmt.Sprintf("!vmh_arg%d!", i)
		}
		b.WriteString("setlocal EnableDelayedExpansion\r\n")
		b.WriteString(strings.Join(args, " "))
	}
	b.WriteString("\r\nexit /b %errorlevel%\r\n")
	header := "@echo off\r\n"
	if !isASCII([]byte(b.String())) {
		header += "chcp 65001 >nul\r\n"
	}
	return header + b.String(), nil
}

func batchEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '%':
			b.WriteString("%%")
		case '^', '&', '|', '<', '>', '(', ')', '"':
			b.WriteByte('^')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
