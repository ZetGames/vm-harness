package virtualbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fl4metf/vm-harness/internal/fakerun"
	"github.com/fl4metf/vm-harness/runner"
	"github.com/fl4metf/vm-harness/vm"
)

const windowsExtradata = "Key: vmh/managed, Value: 1\r\nKey: vmh/os_type, Value: windows11\r\n"

func TestGuestIP(t *testing.T) {
	f := newFake(t).On("showvminfo", fixture(t, "showvminfo-running.txt")).On("guestproperty", "Value: 192.168.56.10\r\n")
	ip, err := newProvider(t, f).GuestIP(context.Background(), "demo-1")
	if err != nil || ip != "192.168.56.10" {
		t.Fatalf("ip = %q, err = %v", ip, err)
	}
	if !f.Called("guestproperty get " + demoID + " /VirtualBox/GuestInfo/Net/0/V4/IP") {
		t.Fatalf("calls = %q", commands(f))
	}

	f = newFake(t).On("showvminfo", fixture(t, "showvminfo-running.txt")).On("guestproperty", fixture(t, "guestproperty-novalue.txt"))
	if _, err := newProvider(t, f).GuestIP(context.Background(), "demo-1"); !errors.Is(err, vm.ErrNotReady) {
		t.Fatalf("no value: err = %v", err)
	}

	f = newFake(t).On("showvminfo", fixture(t, "showvminfo-stopped.txt"))
	if _, err := newProvider(t, f).GuestIP(context.Background(), "demo-1"); !errors.Is(err, vm.ErrInvalidState) {
		t.Fatalf("stopped: err = %v", err)
	}
}

func TestExecArgv(t *testing.T) {
	cmd := []string{`C:\Windows\System32\cmd.exe`, "/d", "/s", "/c"}
	cases := []struct {
		name      string
		extradata string
		req       vm.ExecRequest
		want      []string
		unquoted  bool
	}{
		{"linux absolute", "", vm.ExecRequest{Command: []string{"/bin/ls", "-la", "/tmp dir"}},
			[]string{"/bin/ls", "-la", "/tmp dir"}, false},
		{"linux relative", "", vm.ExecRequest{Command: []string{"uname", "-a"}},
			[]string{"/usr/bin/env", "uname", "-a"}, false},
		{"linux script", "", vm.ExecRequest{Script: "echo $HOME | wc -c", Command: []string{"ignored"}},
			[]string{"/bin/sh", "-c", "echo $HOME | wc -c"}, false},
		{"windows script", windowsExtradata, vm.ExecRequest{Script: `dir "C:\Program Files"`},
			append(cmd, `"dir "C:\Program Files""`), true},
		{"windows relative", windowsExtradata, vm.ExecRequest{Command: []string{"ipconfig", "/all", "a b"}},
			append(cmd, `"ipconfig /all ^"a b^""`), true},
		{"windows relative with cmd syntax", windowsExtradata, vm.ExecRequest{Command: []string{"type", `C:\data\R&D.txt`, "%PATH%"}},
			append(cmd, `"type C:\data\R^&D.txt ^%PATH^%"`), true},
		{"windows absolute", windowsExtradata, vm.ExecRequest{Command: []string{`C:\Windows\System32\whoami.exe`, "a&b"}},
			[]string{`C:\Windows\System32\whoami.exe`, "a&b"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var args []string
			f := newFake(t).
				On("showvminfo", fixture(t, "showvminfo-running.txt")).
				On("getextradata", c.extradata).
				OnFunc("guestcontrol", func(call fakerun.Call) (runner.Result, error) {
					args = call.Args
					return runner.Result{}, nil
				})
			req := c.req
			req.User = "vmh"
			if _, err := newProvider(t, f).Exec(context.Background(), "demo-1", req); err != nil {
				t.Fatal(err)
			}
			i := slices.Index(args, "--")
			if i < 0 || !reflect.DeepEqual(args[i+1:], c.want) {
				t.Fatalf("argv = %q, want %q", args, c.want)
			}
			if got := slices.Contains(args[:i], "--unquoted-args"); got != c.unquoted {
				t.Fatalf("--unquoted-args = %v in %q", got, args)
			}
		})
	}
}

func TestExecWindowsMultiLineScript(t *testing.T) {
	batchName := regexp.MustCompile(`^C:\\Windows\\Temp\\vmh-[a-z2-7]{26}\.cmd$`)
	for _, run := range []runner.Result{{ExitCode: 3, Stdout: []byte("one\r\ntwo\r\n")}, failure(t, "err-no-additions.txt")} {
		var local, content string
		f := newFake(t).
			On("showvminfo", fixture(t, "showvminfo-running.txt")).
			On("getextradata", windowsExtradata).
			OnFunc("guestcontrol "+demoID+" copyto", func(c fakerun.Call) (runner.Result, error) {
				local = c.Args[len(c.Args)-2]
				data, err := os.ReadFile(local)
				content = string(data)
				return runner.Result{}, err
			}).
			OnResult("guestcontrol "+demoID+" run", run).
			On("guestcontrol "+demoID+" rm", "")
		req := vm.ExecRequest{Script: "echo one\necho two\r\nexit /b 3", Credentials: vm.Credentials{User: "vmh"}}
		res, err := newProvider(t, f).Exec(context.Background(), "demo-1", req)
		if run.ExitCode == 3 && (err != nil || res.ExitCode != 3 || res.Stdout != "one\r\ntwo\r\n") {
			t.Fatalf("result = %+v, %v", res, err)
		}
		if run.ExitCode != 3 && !errors.Is(err, vm.ErrNotReady) {
			t.Fatalf("err = %v", err)
		}
		calls := f.Find("guestcontrol")
		if len(calls) != 3 {
			t.Fatalf("calls = %v", calls)
		}
		batch := calls[0].Args[len(calls[0].Args)-1]
		auth := " --username vmh --passwordfile " + argAfter(calls[0], "--passwordfile") + " "
		want := []string{
			"guestcontrol " + demoID + " copyto" + auth + local + " " + batch,
			"guestcontrol " + demoID + " run" + auth + `--wait-stdout --wait-stderr -- C:\Windows\System32\cmd.exe /d /c ` + batch,
			"guestcontrol " + demoID + " rm" + auth + batch,
		}
		for i, c := range calls {
			if c.String() != want[i] {
				t.Fatalf("call %d = %q\nwant %q", i, c, want[i])
			}
		}
		if !batchName.MatchString(batch) || content != "@echo off\r\nsetlocal DisableDelayedExpansion\r\necho one\r\necho two\r\nexit /b 3\r\nexit /b %errorlevel%\r\n" {
			t.Fatalf("batch %s holds %q", batch, content)
		}
		if _, err := os.Stat(local); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("local batch file not removed: %v", err)
		}
	}
}

func TestWindowsCommandRejectsLineBreaks(t *testing.T) {
	_, _, err := guestCommand(true, vm.ExecRequest{Command: []string{"echo", "a\nb"}})
	if !errors.Is(err, vm.ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestExecTimeout(t *testing.T) {
	cases := []struct {
		name  string
		delay time.Duration
		err   error
	}{
		{"killed at the timeout", 1100 * time.Millisecond, context.DeadlineExceeded},
		{"program exit code 19", 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t).
				On("showvminfo", fixture(t, "showvminfo-running.txt")).
				On("getextradata", "").
				OnFunc("guestcontrol", func(fakerun.Call) (runner.Result, error) {
					time.Sleep(c.delay)
					return runner.Result{ExitCode: 19, Stdout: []byte("partial")}, nil
				})
			req := vm.ExecRequest{Command: []string{"/bin/sleep", "5"}, TimeoutSec: 1, Credentials: vm.Credentials{User: "vmh"}}
			res, err := newProvider(t, f).Exec(context.Background(), "demo-1", req)
			if !errors.Is(err, c.err) {
				t.Fatalf("err = %v, want %v", err, c.err)
			}
			if c.err == nil && res.ExitCode != 19 {
				t.Fatalf("result = %+v", res)
			}
		})
	}
}

func TestExecRequest(t *testing.T) {
	var args []string
	var secret, password string
	f := newFake(t).
		On("showvminfo", fixture(t, "showvminfo-running.txt")).
		On("getextradata", "").
		OnFunc("guestcontrol", func(c fakerun.Call) (runner.Result, error) {
			args = c.Args
			secret = argAfter(c, "--passwordfile")
			data, err := os.ReadFile(secret)
			password = string(data)
			return runner.Result{ExitCode: 3, Stdout: []byte("out\n"), Stderr: []byte("warn\n")}, err
		})
	req := vm.ExecRequest{
		Command:     []string{"/usr/bin/false"},
		Env:         map[string]string{"B": "2", "A": "1 2"},
		WorkDir:     "/srv",
		TimeoutSec:  30,
		Credentials: vm.Credentials{User: "vmh", Password: "hunter2"},
	}
	res, err := newProvider(t, f).Exec(context.Background(), "demo-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.Stdout != "out\n" || res.Stderr != "warn\n" {
		t.Fatalf("result = %+v", res)
	}
	want := []string{"guestcontrol", demoID, "run", "--username", "vmh", "--passwordfile", secret, "--wait-stdout", "--wait-stderr",
		"--timeout", "30000", "--cwd", "/srv", "--putenv", "A=1 2", "--putenv", "B=2", "--", "/usr/bin/false"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %q", args)
	}
	if password != "hunter2" || slices.Contains(args, "hunter2") {
		t.Fatalf("password handling: file %q, args %q", password, args)
	}
	if _, err := os.Stat(secret); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("password file not removed: %v", err)
	}
}

func TestExecErrors(t *testing.T) {
	cases := []struct {
		name string
		info string
		req  vm.ExecRequest
		res  runner.Result
		err  error
	}{
		{"no additions", "showvminfo-running.txt", vm.ExecRequest{Command: []string{"/bin/true"}}, failure(t, "err-no-additions.txt"), vm.ErrNotReady},
		{"guest stderr is not a tool error", "showvminfo-running.txt", vm.ExecRequest{Command: []string{"/bin/true"}},
			runner.Result{ExitCode: 1, Stderr: []byte("grep: error: VBoxManage: error: \n")}, nil},
		{"stopped", "showvminfo-stopped.txt", vm.ExecRequest{Command: []string{"/bin/true"}}, runner.Result{}, vm.ErrInvalidState},
		{"empty", "showvminfo-running.txt", vm.ExecRequest{}, runner.Result{}, vm.ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t).On("showvminfo", fixture(t, c.info)).On("getextradata", "").OnResult("guestcontrol", c.res)
			req := c.req
			req.User = "vmh"
			_, err := newProvider(t, f).Exec(context.Background(), "demo-1", req)
			if !errors.Is(err, c.err) {
				t.Fatalf("err = %v, want %v", err, c.err)
			}
		})
	}
	f := newFake(t)
	if _, err := newProvider(t, f).Exec(context.Background(), "demo-1", vm.ExecRequest{Command: []string{"/bin/true"}}); !errors.Is(err, vm.ErrInvalid) {
		t.Fatalf("no user: err = %v", err)
	}
}

func TestCopy(t *testing.T) {
	host := filepath.Join(t.TempDir(), "file.txt")
	var secrets []string
	f := newFake(t).OnFunc("guestcontrol", func(c fakerun.Call) (runner.Result, error) {
		secrets = append(secrets, argAfter(c, "--passwordfile"))
		return runner.Result{}, nil
	})
	p := newProvider(t, f)
	ctx := context.Background()
	cred := vm.Credentials{User: "vmh", Password: "pw"}
	if err := p.CopyTo(ctx, demoID, vm.CopyRequest{HostPath: host, GuestPath: "/opt/app/file.txt", Credentials: cred}); err != nil {
		t.Fatal(err)
	}
	if err := p.CopyTo(ctx, demoID, vm.CopyRequest{HostPath: host, GuestPath: `C:\Temp\file.txt`, Credentials: cred}); err != nil {
		t.Fatal(err)
	}
	if err := p.CopyTo(ctx, demoID, vm.CopyRequest{HostPath: host, GuestPath: "/file.txt", Credentials: cred}); err != nil {
		t.Fatal(err)
	}
	if err := p.CopyFrom(ctx, demoID, vm.CopyRequest{HostPath: host, GuestPath: "/etc/hostname", Credentials: cred}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range f.Calls() {
		got = append(got, strings.Replace(c.String(), argAfter(c, "--passwordfile"), "PW", 1))
	}
	auth := " --username vmh --passwordfile PW "
	want := []string{
		"guestcontrol " + demoID + " mkdir" + auth + "--parents /opt/app",
		"guestcontrol " + demoID + " copyto" + auth + host + " /opt/app/file.txt",
		"guestcontrol " + demoID + " mkdir" + auth + `--parents C:\Temp`,
		"guestcontrol " + demoID + " copyto" + auth + host + ` C:\Temp\file.txt`,
		"guestcontrol " + demoID + " copyto" + auth + host + " /file.txt",
		"guestcontrol " + demoID + " copyfrom" + auth + "/etc/hostname " + host,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
	}
	for _, s := range secrets {
		if _, err := os.Stat(s); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("password file %s not removed", s)
		}
	}

	for _, req := range []vm.CopyRequest{
		{HostPath: host, GuestPath: "relative/file", Credentials: cred},
		{HostPath: "relative", GuestPath: "/tmp/x", Credentials: cred},
		{HostPath: host, GuestPath: "/tmp/x"},
	} {
		if err := p.CopyTo(ctx, demoID, req); !errors.Is(err, vm.ErrInvalid) {
			t.Errorf("%+v: err = %v", req, err)
		}
	}
}

func TestCopyNotReady(t *testing.T) {
	f := newFake(t).OnResult("guestcontrol", failure(t, "err-no-additions.txt"))
	err := newProvider(t, f).CopyFrom(context.Background(), demoID, vm.CopyRequest{
		HostPath: filepath.Join(t.TempDir(), "x"), GuestPath: "/x", Credentials: vm.Credentials{User: "vmh"},
	})
	if !errors.Is(err, vm.ErrNotReady) {
		t.Fatalf("err = %v", err)
	}
}

func TestScreenshot(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nimage")
	var file string
	f := newFake(t).OnFunc("controlvm", func(c fakerun.Call) (runner.Result, error) {
		file = c.Args[3]
		return runner.Result{}, os.WriteFile(file, png, 0o644)
	})
	got, err := newProvider(t, f).Screenshot(context.Background(), demoID, vm.Credentials{})
	if err != nil || !bytes.Equal(got, png) {
		t.Fatalf("screenshot = %q, err = %v", got, err)
	}
	if c := commands(f); len(c) != 1 || c[0] != "controlvm "+demoID+" screenshotpng "+file {
		t.Fatalf("calls = %q", c)
	}
	if _, err := os.Stat(filepath.Dir(file)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp dir not removed: %v", err)
	}

	f = newFake(t).OnResult("controlvm", failure(t, "err-not-running.txt"))
	if _, err := newProvider(t, f).Screenshot(context.Background(), demoID, vm.Credentials{}); !errors.Is(err, vm.ErrInvalidState) {
		t.Fatalf("stopped: err = %v", err)
	}
}
