package sshexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/ZetGames/vm-harness/internal/shellquote"
	"github.com/ZetGames/vm-harness/vm"
)

const (
	defaultPort        = 22
	defaultDialTimeout = 10 * time.Second
	maxOutput          = 1 << 20
)

type Config struct {
	Host           string
	Port           int
	User           string
	Password       string
	KeyPath        string
	KnownHostsPath string
	DialTimeout    time.Duration
}

func Run(ctx context.Context, cfg Config, req vm.ExecRequest) (vm.ExecResult, error) {
	if len(req.Command) == 0 && req.Script == "" {
		return vm.ExecResult{}, fmt.Errorf("command or script is required: %w", vm.ErrInvalid)
	}
	if req.TimeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutSec)*time.Second)
		defer cancel()
	}
	stdout := &capBuffer{limit: maxOutput}
	stderr := &capBuffer{limit: maxOutput}
	start := time.Now()
	err := execute(ctx, cfg, shellquote.POSIXScript(req.Command, req.Script, req.Env, req.WorkDir), nil, stdout, stderr)
	res := vm.ExecResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		DurationMS: time.Since(start).Milliseconds(),
		Truncated:  stdout.full || stderr.full,
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitStatus()
		return res, nil
	}
	return res, err
}

func Upload(ctx context.Context, cfg Config, hostPath, guestPath string) error {
	f, err := os.Open(hostPath)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory: %w", hostPath, vm.ErrInvalid)
	}
	cmd := "mkdir -p " + shellquote.POSIX(path.Dir(guestPath)) + " && cat > " + shellquote.POSIX(guestPath)
	stderr := &capBuffer{limit: maxOutput}
	if err := execute(ctx, cfg, cmd, f, nil, stderr); err != nil {
		return remoteFailure("upload "+guestPath, err, stderr.String())
	}
	return nil
}

func Download(ctx context.Context, cfg Config, guestPath, hostPath string) error {
	tmp, err := os.CreateTemp(filepath.Dir(hostPath), "."+filepath.Base(hostPath)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	stderr := &capBuffer{limit: maxOutput}
	err = execute(ctx, cfg, "cat "+shellquote.POSIX(guestPath), nil, tmp, stderr)
	closeErr := tmp.Close()
	if err != nil {
		return remoteFailure("download "+guestPath, err, stderr.String())
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp.Name(), hostPath)
}

func ReadFile(ctx context.Context, cfg Config, guestPath string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stdout := &capBuffer{limit: limit, onFull: cancel}
	stderr := &capBuffer{limit: maxOutput}
	err := execute(ctx, cfg, "cat "+shellquote.POSIX(guestPath), nil, stdout, stderr)
	if stdout.full {
		return nil, fmt.Errorf("guest file %s is larger than %d bytes: %w", guestPath, limit, vm.ErrLimit)
	}
	if err != nil {
		return nil, remoteFailure("read "+guestPath, err, stderr.String())
	}
	return stdout.buf, nil
}

func Probe(ctx context.Context, cfg Config) error {
	stderr := &capBuffer{limit: maxOutput}
	if err := execute(ctx, cfg, "true", nil, nil, stderr); err != nil {
		return remoteFailure("probe", err, stderr.String())
	}
	return nil
}

func remoteFailure(op string, err error, stderr string) error {
	var exitErr *ssh.ExitError
	if !errors.As(err, &exitErr) {
		return fmt.Errorf("%s: %w", op, err)
	}
	msg := strings.TrimSpace(stderr)
	if strings.Contains(msg, "No such file or directory") {
		return fmt.Errorf("%s: %s: %w", op, msg, vm.ErrNotFound)
	}
	return fmt.Errorf("%s: exit %d: %s", op, exitErr.ExitStatus(), msg)
}

func execute(ctx context.Context, cfg Config, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	addr := address(cfg)
	client, err := connect(ctx, cfg, addr)
	if err != nil {
		return err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return connectFailure(ctx, addr, err)
	}
	defer session.Close()
	session.Stdin = stdin
	session.Stdout = stdout
	session.Stderr = stderr

	stop := context.AfterFunc(ctx, func() {
		session.Signal(ssh.SIGKILL)
		client.Close()
	})
	defer stop()

	err = session.Run(cmd)
	if err != nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("ssh %s: %w", addr, err)
	}
	return nil
}

func address(cfg Config) string {
	port := cfg.Port
	if port == 0 {
		port = defaultPort
	}
	return net.JoinHostPort(cfg.Host, strconv.Itoa(port))
}

func connect(ctx context.Context, cfg Config, addr string) (*ssh.Client, error) {
	config, err := clientConfig(cfg)
	if err != nil {
		return nil, err
	}
	timeout := cfg.DialTimeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}

	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, connectFailure(ctx, addr, err)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	conn.SetDeadline(time.Now().Add(timeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		conn.Close()
		return nil, connectFailure(ctx, addr, err)
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

func connectFailure(ctx context.Context, addr string, err error) error {
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("ssh %s: %w", addr, ctx.Err())
	case errors.Is(err, errHostKey):
		return fmt.Errorf("ssh %s: %w", addr, err)
	default:
		return fmt.Errorf("ssh %s: %w: %w", addr, err, vm.ErrNotReady)
	}
}

func clientConfig(cfg Config) (*ssh.ClientConfig, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("ssh host is required: %w", vm.ErrInvalid)
	}
	if cfg.User == "" {
		return nil, fmt.Errorf("ssh user is required: %w", vm.ErrInvalid)
	}
	auth, err := authMethods(cfg)
	if err != nil {
		return nil, err
	}
	hostKey, err := hostKeyCallback(cfg.KnownHostsPath)
	if err != nil {
		return nil, err
	}
	return &ssh.ClientConfig{User: cfg.User, Auth: auth, HostKeyCallback: hostKey}, nil
}

func authMethods(cfg Config) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if cfg.KeyPath != "" {
		pemBytes, err := os.ReadFile(cfg.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("ssh key: %w: %w", err, vm.ErrInvalid)
		}
		signer, err := ssh.ParsePrivateKey(pemBytes)
		if err != nil {
			return nil, fmt.Errorf("ssh key %s: %w: %w", cfg.KeyPath, err, vm.ErrInvalid)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if cfg.Password != "" {
		methods = append(methods, ssh.Password(cfg.Password), ssh.KeyboardInteractive(answerWith(cfg.Password)))
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("ssh key or password is required: %w", vm.ErrInvalid)
	}
	return methods, nil
}

func answerWith(password string) ssh.KeyboardInteractiveChallenge {
	return func(name, instruction string, questions []string, echos []bool) ([]string, error) {
		answers := make([]string, len(questions))
		for i := range answers {
			answers[i] = password
		}
		return answers, nil
	}
}

type capBuffer struct {
	buf    []byte
	limit  int64
	full   bool
	onFull func()
}

func (b *capBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.limit - int64(len(b.buf)); int64(n) > room {
		p = p[:room]
		b.full = true
		if b.onFull != nil {
			b.onFull()
		}
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

func (b *capBuffer) String() string {
	s := b.buf
	if b.full && len(s) > 0 {
		i := len(s) - 1
		for i > 0 && i > len(s)-utf8.UTFMax && !utf8.RuneStart(s[i]) {
			i--
		}
		if !utf8.FullRune(s[i:]) {
			s = s[:i]
		}
	}
	return string(s)
}
