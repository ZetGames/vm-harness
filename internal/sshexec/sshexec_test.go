package sshexec

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/fl4metf/vm-harness/internal/secfile/secfiletest"
	"github.com/fl4metf/vm-harness/vm"
)

func TestRunExitCodeAndOutput(t *testing.T) {
	srv := newTestServer(t).start()

	res, err := Run(context.Background(), srv.dialConfig(), vm.ExecRequest{Command: []string{"fail"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if res.Stdout != "partial output\n" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if res.Stderr != "something broke\n" {
		t.Errorf("stderr = %q", res.Stderr)
	}
	if res.DurationMS < 0 {
		t.Errorf("duration = %d", res.DurationMS)
	}

	res, err = Run(context.Background(), srv.dialConfig(), vm.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.Stdout != "" || res.Stderr != "" {
		t.Errorf("true: %+v", res)
	}
}

func TestRunCapsOutput(t *testing.T) {
	srv := newTestServer(t).start()

	res, err := Run(context.Background(), srv.dialConfig(), vm.ExecRequest{Command: []string{"flood"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0 after draining the output", res.ExitCode)
	}
	if !res.Truncated {
		t.Error("truncated = false")
	}
	if len(res.Stdout) != maxOutput-1 || !utf8.ValidString(res.Stdout) {
		t.Errorf("stdout has %d bytes (valid utf-8: %v), want %d", len(res.Stdout), utf8.ValidString(res.Stdout), maxOutput-1)
	}
	if len(res.Stderr) != maxOutput {
		t.Errorf("stderr has %d bytes, want %d", len(res.Stderr), maxOutput)
	}
}

func TestRunQuoting(t *testing.T) {
	srv := newTestServer(t).start()

	tests := []struct {
		name string
		req  vm.ExecRequest
		want string
	}{
		{
			name: "argv with env and workdir",
			req: vm.ExecRequest{
				Command: []string{"printf", `%s\n`, "it's"},
				Env:     map[string]string{"B": "two words", "A": "1"},
				WorkDir: "/tmp/work dir",
			},
			want: "export A=1; export B='two words'; cd '/tmp/work dir' || exit 1\n" + `printf '%s\n' 'it'\''s'`,
		},
		{
			name: "script",
			req:  vm.ExecRequest{Script: "ls -l | wc -l", WorkDir: "/srv"},
			want: "cd /srv || exit 1\nls -l | wc -l",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := Run(context.Background(), srv.dialConfig(), tt.req)
			if err != nil {
				t.Fatal(err)
			}
			if res.Stdout != tt.want {
				t.Errorf("server got %q, want %q", res.Stdout, tt.want)
			}
		})
	}
}

func TestRunMissingExitStatus(t *testing.T) {
	srv := newTestServer(t).start()

	_, err := Run(context.Background(), srv.dialConfig(), vm.ExecRequest{Command: []string{"vanish"}})
	var missing *ssh.ExitMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("err = %v, want ExitMissingError", err)
	}
	if errors.Is(err, vm.ErrNotReady) {
		t.Errorf("err = %v must not be ErrNotReady", err)
	}
}

func TestPasswordAuth(t *testing.T) {
	for _, keyboardInteractive := range []bool{false, true} {
		srv := newTestServer(t)
		srv.keyboardInteractive = keyboardInteractive
		srv.start()

		if err := Probe(context.Background(), srv.dialConfig()); err != nil {
			t.Errorf("keyboard-interactive=%v: %v", keyboardInteractive, err)
		}

		cfg := srv.dialConfig()
		cfg.Password = "wrong"
		err := Probe(context.Background(), cfg)
		if !errors.Is(err, vm.ErrNotReady) {
			t.Errorf("keyboard-interactive=%v: wrong password err = %v, want ErrNotReady", keyboardInteractive, err)
		}
	}
}

func TestKeyAuth(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	authorized, err := GenerateKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorized))
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t)
	srv.password = ""
	srv.authorizedKey = pub
	srv.start()

	cfg := srv.dialConfig()
	cfg.Password = ""
	cfg.KeyPath = keyPath
	res, err := Run(context.Background(), cfg, vm.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d", res.ExitCode)
	}

	otherKey := filepath.Join(dir, "other")
	if _, err := GenerateKey(otherKey); err != nil {
		t.Fatal(err)
	}
	cfg.KeyPath = otherKey
	if err := Probe(context.Background(), cfg); !errors.Is(err, vm.ErrNotReady) {
		t.Errorf("unauthorized key err = %v, want ErrNotReady", err)
	}
}

func TestInvalidConfig(t *testing.T) {
	srv := newTestServer(t).start()
	missingKey := srv.dialConfig()
	missingKey.KeyPath = filepath.Join(t.TempDir(), "absent")
	noUser := srv.dialConfig()
	noUser.User = ""
	noAuth := srv.dialConfig()
	noAuth.Password = ""
	noHost := srv.dialConfig()
	noHost.Host = ""

	for name, cfg := range map[string]Config{"missing key": missingKey, "no user": noUser, "no auth": noAuth, "no host": noHost} {
		if err := Probe(context.Background(), cfg); !errors.Is(err, vm.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := Run(context.Background(), srv.dialConfig(), vm.ExecRequest{}); !errors.Is(err, vm.ErrInvalid) {
		t.Errorf("empty request err = %v, want ErrInvalid", err)
	}
}

func TestConnectionRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().(*net.TCPAddr)
	l.Close()

	cfg := Config{Host: "127.0.0.1", Port: addr.Port, User: testUser, Password: testPassword, DialTimeout: 2 * time.Second}
	if err := Probe(context.Background(), cfg); !errors.Is(err, vm.ErrNotReady) {
		t.Errorf("err = %v, want ErrNotReady", err)
	}
}

func silentListener(t *testing.T) Config {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		var accepted []net.Conn
		defer func() {
			for _, c := range accepted {
				c.Close()
			}
		}()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted = append(accepted, c)
		}
	}()
	addr := l.Addr().(*net.TCPAddr)
	return Config{Host: "127.0.0.1", Port: addr.Port, User: testUser, Password: testPassword}
}

func TestHandshakeTimeout(t *testing.T) {
	cfg := silentListener(t)
	cfg.DialTimeout = 300 * time.Millisecond

	start := time.Now()
	err := Probe(context.Background(), cfg)
	if !errors.Is(err, vm.ErrNotReady) {
		t.Errorf("err = %v, want ErrNotReady", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("probe took %v", elapsed)
	}
}

func TestHandshakeCanceled(t *testing.T) {
	cfg := silentListener(t)
	cfg.DialTimeout = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := Probe(ctx, cfg)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, vm.ErrNotReady) {
		t.Errorf("err = %v, want only DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("probe took %v", elapsed)
	}
}

func TestRunTimeout(t *testing.T) {
	srv := newTestServer(t).start()

	start := time.Now()
	_, err := Run(context.Background(), srv.dialConfig(), vm.ExecRequest{Command: []string{"sleep"}, TimeoutSec: 1})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if vm.Code(err) != "timeout" {
		t.Errorf("code = %q", vm.Code(err))
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("run took %v", elapsed)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !srv.receivedSignal("KILL") {
		if time.Now().After(deadline) {
			t.Fatal("server never received KILL")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunCanceled(t *testing.T) {
	srv := newTestServer(t).start()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)

	_, err := Run(ctx, srv.dialConfig(), vm.ExecRequest{Command: []string{"sleep"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
}

func TestKnownHostsTrustOnFirstUse(t *testing.T) {
	srv := newTestServer(t).start()
	cfg := srv.dialConfig()
	cfg.KnownHostsPath = filepath.Join(t.TempDir(), "keys", "vm.known_hosts")

	if err := Probe(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := Probe(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfg.KnownHostsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := knownhosts.Line([]string{address(cfg)}, srv.publicHostKey()) + "\n"
	if string(data) != want {
		t.Errorf("known_hosts = %q, want %q", data, want)
	}
	if !strings.HasPrefix(string(data), "[127.0.0.1]:") {
		t.Errorf("known_hosts entry %q is not for [127.0.0.1]:port", data)
	}
	if err := secfiletest.CheckRestricted(cfg.KnownHostsPath); err != nil {
		t.Error(err)
	}

	srv.setHostKey(newHostKey(t))
	err = Probe(context.Background(), cfg)
	if err == nil {
		t.Fatal("probe accepted a changed host key")
	}
	if errors.Is(err, vm.ErrNotReady) {
		t.Errorf("mismatch err = %v must not be ErrNotReady", err)
	}
	if !errors.Is(err, errHostKey) || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("mismatch err = %v", err)
	}
	if vm.Code(err) != vm.CodeInvalidState || !strings.Contains(err.Error(), "delete that file") {
		t.Errorf("mismatch err = %v (code %s), want an actionable invalid_state error", err, vm.Code(err))
	}
	after, err := os.ReadFile(cfg.KnownHostsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, data) {
		t.Errorf("known_hosts changed after mismatch: %q", after)
	}
}

func TestKnownHostsCorrupt(t *testing.T) {
	srv := newTestServer(t).start()
	cfg := srv.dialConfig()
	cfg.KnownHostsPath = filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(cfg.KnownHostsPath, []byte("[127.0.0.1]:22 ssh-ed25519 not-base64\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Probe(context.Background(), cfg)
	if err == nil || errors.Is(err, vm.ErrNotReady) {
		t.Errorf("err = %v, want a hard failure", err)
	}
}

func TestUploadDownload(t *testing.T) {
	srv := newTestServer(t).start()
	cfg := srv.dialConfig()
	dir := t.TempDir()
	content := make([]byte, 3<<20)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}

	const guestPath = "/data/my dir/blob's.bin"
	if err := Upload(context.Background(), cfg, src, guestPath); err != nil {
		t.Fatal(err)
	}
	if got, want := srv.lastCommand(), `mkdir -p '/data/my dir' && cat > '/data/my dir/blob'\''s.bin'`; got != want {
		t.Errorf("upload command = %q, want %q", got, want)
	}
	stored, ok := srv.file(guestPath)
	if !ok || !bytes.Equal(stored, content) {
		t.Fatalf("server stored %d bytes, want %d", len(stored), len(content))
	}

	dst := filepath.Join(dir, "copy.bin")
	if err := Download(context.Background(), cfg, guestPath, dst); err != nil {
		t.Fatal(err)
	}
	if got, want := srv.lastCommand(), `cat '/data/my dir/blob'\''s.bin'`; got != want {
		t.Errorf("download command = %q, want %q", got, want)
	}
	downloaded, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(downloaded, content) {
		t.Errorf("downloaded %d bytes differ from the uploaded %d bytes", len(downloaded), len(content))
	}
	assertDirEntries(t, dir, "blob.bin", "copy.bin")
}

func TestDownloadMissingFile(t *testing.T) {
	srv := newTestServer(t).start()
	dir := t.TempDir()

	err := Download(context.Background(), srv.dialConfig(), "/no/such/file", filepath.Join(dir, "out"))
	if !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	assertDirEntries(t, dir)
}

func TestReadFile(t *testing.T) {
	srv := newTestServer(t).start()
	cfg := srv.dialConfig()
	srv.setFile("/etc/motd", []byte("hello"))

	data, err := ReadFile(context.Background(), cfg, "/etc/motd", 5)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Errorf("data = %q", data)
	}
	if got := srv.lastCommand(); got != "cat /etc/motd" {
		t.Errorf("command = %q", got)
	}

	if _, err := ReadFile(context.Background(), cfg, "/etc/motd", 4); !errors.Is(err, vm.ErrLimit) {
		t.Errorf("over the limit: err = %v, want ErrLimit", err)
	}
	if _, err := ReadFile(context.Background(), cfg, "/no/such/file", 5); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("missing file: err = %v, want ErrNotFound", err)
	}
}

func TestReadFileEndlessStream(t *testing.T) {
	srv := newTestServer(t).start()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	data, err := ReadFile(ctx, srv.dialConfig(), "/dev/zero", 1<<20)
	if !errors.Is(err, vm.ErrLimit) || data != nil {
		t.Fatalf("got %d bytes, err = %v, want ErrLimit", len(data), err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("read took %v, the session was not stopped at the limit", elapsed)
	}
}

func TestUploadDirectory(t *testing.T) {
	srv := newTestServer(t).start()
	err := Upload(context.Background(), srv.dialConfig(), t.TempDir(), "/tmp/x")
	if !errors.Is(err, vm.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func assertDirEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s contains %v, want %v", dir, got, want)
	}
}
