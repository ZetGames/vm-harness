package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZetGames/vm-harness/internal/memprovider"
	"github.com/ZetGames/vm-harness/vm"
)

func quickWait(cond string) WaitRequest {
	return WaitRequest{For: cond, Timeout: 2 * time.Second}
}

func TestWaitForState(t *testing.T) {
	cases := []struct {
		cond  string
		from  vm.State
		later vm.State
	}{
		{WaitRunning, vm.StateStopped, vm.StateRunning},
		{WaitStopped, vm.StateRunning, vm.StateStopped},
		{WaitPaused, vm.StateRunning, vm.StatePaused},
		{WaitSaved, vm.StateRunning, vm.StateSaved},
	}
	for _, c := range cases {
		t.Run(c.cond, func(t *testing.T) {
			e := newEnv(t)
			e.vbox.Put(foreignVM("a", c.from))
			time.AfterFunc(30*time.Millisecond, func() { e.vbox.SetState("a", c.later) })
			res, err := e.m.Wait(t.Context(), vboxRef("a"), quickWait(c.cond))
			if err != nil {
				t.Fatal(err)
			}
			if res.Machine.State != c.later || res.ElapsedMS < 20 {
				t.Fatalf("result = %s after %dms", res.Machine.State, res.ElapsedMS)
			}
		})
	}
}

func TestWaitForIP(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(foreignVM("a", vm.StateBusy))
	time.AfterFunc(20*time.Millisecond, func() { e.vbox.SetState("a", vm.StateRunning) })
	time.AfterFunc(40*time.Millisecond, func() { e.vbox.SetIP("a", "10.0.2.15") })
	res, err := e.m.Wait(t.Context(), vboxRef("a"), quickWait(WaitIP))
	if err != nil {
		t.Fatal(err)
	}
	if res.IP != "10.0.2.15" || res.Machine.Name != "a" {
		t.Fatalf("result = %+v", res)
	}
}

func TestWaitTimeout(t *testing.T) {
	cases := []struct {
		name  string
		cond  string
		state vm.State
		want  string
	}{
		{"state", WaitRunning, vm.StateStopped, `timed out after 50ms waiting for vm "a" to be running, last check: vm is stopped`},
		{"ip", WaitIP, vm.StateRunning, "last check: guest ip: not ready"},
		{"ip while starting", WaitIP, vm.StateBusy, "is busy: invalid state"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.vbox.Put(managedVM("a", c.state))
			start := time.Now()
			_, err := e.m.Wait(t.Context(), vboxRef("a"), WaitRequest{For: c.cond, Timeout: 50 * time.Millisecond})
			wantErr(t, err, context.DeadlineExceeded)
			if vm.Code(err) != "timeout" {
				t.Fatalf("code = %s", vm.Code(err))
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err, c.want)
			}
			if elapsed := time.Since(start); elapsed < 50*time.Millisecond || elapsed > 2*time.Second {
				t.Fatalf("returned after %s", elapsed)
			}
		})
	}
}

func TestWaitForSSH(t *testing.T) {
	e := newEnv(t)
	stub := stubSSH(e.m)
	stub.probe = []error{
		fmt.Errorf("dial: connection refused: %w", vm.ErrNotReady),
		fmt.Errorf("handshake: %w", vm.ErrNotReady),
	}
	e.vbox.Put(sshVM("a", vm.PortForward{Name: sshForwardName, Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 40022, GuestPort: 22}))
	res, err := e.m.Wait(t.Context(), vboxRef("a"), quickWait(WaitSSH))
	if err != nil {
		t.Fatal(err)
	}
	if res.Machine.Name != "a" || len(stub.configs) != 3 {
		t.Fatalf("result %+v after %d probes", res, len(stub.configs))
	}
	if cfg := stub.lastConfig(t); cfg.Host != "127.0.0.1" || cfg.Port != 40022 || cfg.User != "vmh" {
		t.Fatalf("probe config = %+v", cfg)
	}
}

func TestWaitForSSHTimeoutReportsProbeError(t *testing.T) {
	e := newEnv(t)
	stub := stubSSH(e.m)
	for range 1000 {
		stub.probe = append(stub.probe, fmt.Errorf("ssh handshake failed: %w", vm.ErrNotReady))
	}
	e.vbox.Put(sshVM("a", vm.PortForward{Name: sshForwardName, Protocol: "tcp", HostPort: 40022, GuestPort: 22}))
	_, err := e.m.Wait(t.Context(), vboxRef("a"), WaitRequest{For: WaitSSH, Timeout: 40 * time.Millisecond})
	wantErr(t, err, context.DeadlineExceeded)
	if !strings.Contains(err.Error(), "ssh handshake failed") || !strings.Contains(err.Error(), "accept ssh connections") {
		t.Fatalf("error = %v", err)
	}
	if errors.Is(err, vm.ErrNotReady) {
		t.Fatal("timeout error must classify as a timeout only")
	}
}

func TestWaitForSSHRoutesThroughGuestIP(t *testing.T) {
	e := newEnv(t)
	stub := stubSSH(e.m)
	e.vbox.Put(sshVM("a"))
	time.AfterFunc(20*time.Millisecond, func() { e.vbox.SetIP("a", "10.0.2.15") })
	if _, err := e.m.Wait(t.Context(), vboxRef("a"), quickWait(WaitSSH)); err != nil {
		t.Fatal(err)
	}
	if cfg := stub.lastConfig(t); cfg.Host != "10.0.2.15" || cfg.Port != 22 {
		t.Fatalf("probe config = %+v", cfg)
	}
}

func TestWaitForGuest(t *testing.T) {
	for _, osType := range []string{"ubuntu", "windows2022"} {
		t.Run(osType, func(t *testing.T) {
			e := newEnv(t)
			mach := managedVM("a", vm.StateRunning)
			mach.Meta[vm.MetaOSType] = osType
			e.vbox.Put(mach)
			var attempts atomic.Int32
			var seen vm.ExecRequest
			e.vbox.ExecFunc = func(_ vm.Machine, req vm.ExecRequest) (vm.ExecResult, error) {
				seen = req
				switch attempts.Add(1) {
				case 1:
					return vm.ExecResult{}, fmt.Errorf("guest additions: %w", vm.ErrNotReady)
				case 2:
					return vm.ExecResult{ExitCode: 1}, nil
				}
				return vm.ExecResult{}, nil
			}
			req := quickWait(WaitGuest)
			req.Access.Credentials = vm.Credentials{User: "admin", Password: "pw"}
			if _, err := e.m.Wait(t.Context(), vboxRef("a"), req); err != nil {
				t.Fatal(err)
			}
			if attempts.Load() != 3 {
				t.Fatalf("attempts = %d", attempts.Load())
			}
			want := []string{"/bin/true"}
			if osType == "windows2022" {
				want = []string{`C:\Windows\System32\cmd.exe`, "/c", "exit", "0"}
			}
			if !slices.Equal(seen.Command, want) || seen.User != "admin" || seen.Password != "pw" {
				t.Fatalf("probe request = %+v", seen)
			}
		})
	}
}

func TestWaitFailsFast(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *testEnv)
		req   WaitRequest
		err   error
	}{
		{
			name:  "unknown condition",
			setup: func(e *testEnv) { e.vbox.Put(managedVM("a", vm.StateRunning)) },
			req:   WaitRequest{For: "healthy"},
			err:   vm.ErrInvalid,
		},
		{
			name: "vm deleted while waiting",
			setup: func(e *testEnv) {
				e.vbox.Put(managedVM("a", vm.StateStopped))
				time.AfterFunc(20*time.Millisecond, func() { e.vbox.Delete(context.Background(), "a") })
			},
			req: WaitRequest{For: WaitRunning},
			err: vm.ErrNotFound,
		},
		{
			name:  "missing vm",
			setup: func(*testEnv) {},
			req:   WaitRequest{For: WaitRunning},
			err:   vm.ErrNotFound,
		},
		{
			name:  "guest probe on unmanaged vm",
			setup: func(e *testEnv) { e.vbox.Put(foreignVM("a", vm.StateRunning)) },
			req:   WaitRequest{For: WaitGuest},
			err:   vm.ErrForbidden,
		},
		{
			name:  "ssh without credentials",
			setup: func(e *testEnv) { e.vbox.Put(managedVM("a", vm.StateRunning)) },
			req:   WaitRequest{For: WaitSSH},
			err:   vm.ErrInvalid,
		},
		{
			name: "ssh to windows",
			setup: func(e *testEnv) {
				mach := sshVM("a")
				mach.Meta[vm.MetaOSType] = "windows10"
				e.vbox.Put(mach)
			},
			req: WaitRequest{For: WaitSSH},
			err: vm.ErrUnsupported,
		},
		{
			name: "guest exec unsupported",
			setup: func(e *testEnv) {
				e.vbox.Put(managedVM("a", vm.StateRunning))
				e.vbox.ExecFunc = func(vm.Machine, vm.ExecRequest) (vm.ExecResult, error) {
					return vm.ExecResult{}, fmt.Errorf("no guest control: %w", vm.ErrUnsupported)
				}
			},
			req: WaitRequest{For: WaitGuest, Access: Access{Credentials: vm.Credentials{User: "admin"}}},
			err: vm.ErrUnsupported,
		},
		{
			name:  "guest without a user",
			setup: func(e *testEnv) { e.vbox.Put(managedVM("a", vm.StateRunning)) },
			req:   WaitRequest{For: WaitGuest},
			err:   vm.ErrInvalid,
		},
		{
			name:  "ip of a stopped vm",
			setup: func(e *testEnv) { e.vbox.Put(managedVM("a", vm.StateStopped)) },
			req:   WaitRequest{For: WaitIP},
			err:   vm.ErrInvalidState,
		},
		{
			name: "ssh to a stopped vm",
			setup: func(e *testEnv) {
				e.vbox.Put(sshVM("a", vm.PortForward{Name: sshForwardName, Protocol: "tcp", HostPort: 40022, GuestPort: 22}))
				e.vbox.SetState("a", vm.StateStopped)
			},
			req: WaitRequest{For: WaitSSH},
			err: vm.ErrInvalidState,
		},
		{
			name:  "guest of a saved vm",
			setup: func(e *testEnv) { e.vbox.Put(managedVM("a", vm.StateSaved)) },
			req:   WaitRequest{For: WaitGuest, Access: Access{Credentials: vm.Credentials{User: "admin"}}},
			err:   vm.ErrInvalidState,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			stub := stubSSH(e.m)
			c.setup(e)
			start := time.Now()
			_, err := e.m.Wait(t.Context(), vboxRef("a"), c.req)
			wantErr(t, err, c.err)
			if time.Since(start) > 2*time.Second {
				t.Fatal("did not fail fast")
			}
			if stub.used() {
				t.Fatal("ssh probe ran")
			}
		})
	}
}

func TestWaitHonoursCancellation(t *testing.T) {
	e := newEnv(t)
	e.vbox.Put(managedVM("a", vm.StateStopped))
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(20*time.Millisecond, cancel)
	_, err := e.m.Wait(ctx, vboxRef("a"), WaitRequest{For: WaitRunning})
	wantErr(t, err, context.Canceled)
}

func TestWaitStopsOnRejectedSSHCredentials(t *testing.T) {
	e := newEnv(t)
	stub := stubSSH(e.m)
	for range 100 {
		stub.probe = append(stub.probe, fmt.Errorf("ssh key: open id: %w", vm.ErrInvalid))
	}
	e.vbox.Put(sshVM("a", vm.PortForward{Name: sshForwardName, Protocol: "tcp", HostPort: 40022, GuestPort: 22}))
	_, err := e.m.Wait(t.Context(), vboxRef("a"), quickWait(WaitSSH))
	wantErr(t, err, vm.ErrInvalid)
	if len(stub.configs) != 1 {
		t.Fatalf("probed %d times", len(stub.configs))
	}
}

func TestWaitGuestRetriesRejectedLogins(t *testing.T) {
	e := newEnv(t)
	e.vmw.Put(managedVM("a", vm.StateRunning))
	var attempts atomic.Int32
	e.vmw.ExecFunc = func(vm.Machine, vm.ExecRequest) (vm.ExecResult, error) {
		if attempts.Add(1) < 4 {
			return vm.ExecResult{}, fmt.Errorf("Invalid user name or password for the guest OS: %w", vm.ErrInvalid)
		}
		return vm.ExecResult{}, nil
	}
	req := quickWait(WaitGuest)
	req.Access.Credentials = vm.Credentials{User: "dev", Password: "pw"}
	if _, err := e.m.Wait(t.Context(), Ref{Provider: vm.VMware, VM: "a"}, req); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 4 {
		t.Fatalf("logged in %d times", attempts.Load())
	}

	e.vmw.ExecFunc = func(vm.Machine, vm.ExecRequest) (vm.ExecResult, error) {
		return vm.ExecResult{}, fmt.Errorf("invalid user name or password: %w", vm.ErrInvalid)
	}
	req.Timeout = 100 * time.Millisecond
	_, err := e.m.Wait(t.Context(), Ref{Provider: vm.VMware, VM: "a"}, req)
	wantErr(t, err, context.DeadlineExceeded)
	if !strings.Contains(err.Error(), "invalid user name or password") {
		t.Fatalf("error = %v", err)
	}

	e.vmw.ExecFunc = nil
	before := len(callsOf(e.vmw, "exec"))
	_, err = e.m.Wait(t.Context(), Ref{Provider: vm.VMware, VM: "a"}, quickWait(WaitGuest))
	wantErr(t, err, vm.ErrInvalid)
	if calls := callsOf(e.vmw, "exec"); len(calls) != before {
		t.Fatalf("probed without a user: %v", calls[before:])
	}
}

type stuckGet struct {
	*memprovider.Provider
}

func (s stuckGet) Get(ctx context.Context, ref string) (vm.Machine, error) {
	<-ctx.Done()
	return vm.Machine{}, ctx.Err()
}

func TestWaitTimeoutCoversTheLookup(t *testing.T) {
	p := stuckGet{memprovider.New(vm.VirtualBox)}
	p.Put(managedVM("a", vm.StateRunning))
	m := newManager(t, Config{}, p)
	for _, ref := range []Ref{{VM: "a"}, {Provider: vm.VirtualBox, VM: "a"}} {
		start := time.Now()
		_, err := m.Wait(t.Context(), ref, WaitRequest{For: WaitRunning, Timeout: 100 * time.Millisecond})
		wantErr(t, err, context.DeadlineExceeded)
		if vm.Code(err) != vm.CodeTimeout || !strings.Contains(err.Error(), "timed out after 100ms") {
			t.Fatalf("error = %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("returned after %s", elapsed)
		}
	}
}
