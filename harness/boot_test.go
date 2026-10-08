package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fl4metf/vm-harness/internal/memprovider"
	"github.com/fl4metf/vm-harness/vm"
)

const (
	stalledBoot = "[    0.000000] Linux version 6.8.0-generic\r\n" +
		"[    1.204301] raid6: using algorithm avx2x4 gen() 25000 MB/s\r\n" +
		"Begin: Loading essential drivers ... "
	ioapicPanic = "\r\n[    1.481516] Kernel panic - not syncing: IO-APIC + timer doesn't work!  Boot with apic=debug and send a report.  Then try booting with the 'noapic' kernel option\r\n" +
		"[    1.482002] ---[ end Kernel panic - not syncing: IO-APIC + timer doesn't work!  Boot with apic=debug and send a report.  Then try booting with the 'noapic' kernel option ]---\r\n"
	loginPrompt   = "\r\nUbuntu 24.04 LTS vmh-test ttyS0\r\n\r\nvmh-test login: "
	cloudInitDone = "[   42.513002] cloud-init[911]: Cloud-init v. 24.1.3 finished at Wed, 07 Oct 2026 10:00:00 +0000. Datasource DataSourceNoCloud.  Up 42.40 seconds\r\n"
	badMAC        = "[    3.265222] e1000 0000:00:03.0 0000:00:03.0 (uninitialized): Invalid MAC Address\r\n"
	panicRecovery = "kernel panic: IO-APIC + timer doesn't work! — reset"
	stallSuffix   = "s at: Begin: Loading essential drivers ... — reset"
)

func consoleFile(t *testing.T, content string) string {
	t.Helper()
	return writeFile(t, filepath.Join(t.TempDir(), "serial.log"), content)
}

func appendConsole(t *testing.T, path, text string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Error(err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Error(err)
	}
}

func silentFor(t *testing.T, path string, d time.Duration) {
	t.Helper()
	at := time.Now().Add(-d)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func bootingVM(t *testing.T, e *testEnv, mach vm.Machine, console string) string {
	t.Helper()
	mach.ConsoleLog = consoleFile(t, console)
	e.vbox.Put(mach)
	return mach.ConsoleLog
}

func failProbes(stub *sshStub) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.probe = nil
	for range 10000 {
		stub.probe = append(stub.probe, fmt.Errorf("ssh handshake failed: %w", vm.ErrNotReady))
	}
}

func guestWait(timeout time.Duration) WaitRequest {
	return WaitRequest{For: WaitGuest, Timeout: timeout, Access: Access{Credentials: vm.Credentials{User: "vmh", Password: "pw"}}}
}

func ipWait(timeout time.Duration) WaitRequest {
	return WaitRequest{For: WaitIP, Timeout: timeout}
}

func TestWaitResetsAfterAKernelPanic(t *testing.T) {
	e := newEnv(t)
	e.m.bootStall = time.Hour
	path := bootingVM(t, e, managedVM("a", vm.StateRunning), stalledBoot)
	booted := false
	e.vbox.ExecFunc = func(vm.Machine, vm.ExecRequest) (vm.ExecResult, error) {
		if booted {
			return vm.ExecResult{}, nil
		}
		appendConsole(t, path, ioapicPanic)
		return vm.ExecResult{}, fmt.Errorf("guest additions: %w", vm.ErrNotReady)
	}
	e.vbox.ResetFunc = func(vm.Machine) {
		appendConsole(t, path, stalledBoot+loginPrompt)
		booted = true
	}
	res, err := e.m.Wait(t.Context(), vboxRef("a"), guestWait(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Recoveries, []string{panicRecovery}) || res.Machine.State != vm.StateRunning {
		t.Fatalf("result = %+v", res)
	}
	if calls := callsOf(e.vbox, "reset"); len(calls) != 1 {
		t.Fatalf("reset calls = %v", calls)
	}
}

func TestWaitResetsAStalledBoot(t *testing.T) {
	e := newEnv(t)
	e.m.bootStall = 30 * time.Millisecond
	path := bootingVM(t, e, managedVM("a", vm.StateRunning), stalledBoot)
	e.vbox.ResetFunc = func(vm.Machine) {
		appendConsole(t, path, loginPrompt)
		e.vbox.SetIP("a", "10.0.2.15")
	}
	res, err := e.m.Wait(t.Context(), vboxRef("a"), quickWait(WaitIP))
	if err != nil {
		t.Fatal(err)
	}
	if res.IP != "10.0.2.15" || len(res.Recoveries) != 1 {
		t.Fatalf("result = %+v", res)
	}
	note := res.Recoveries[0]
	if !strings.HasPrefix(note, "boot stalled for ") || !strings.HasSuffix(note, stallSuffix) {
		t.Fatalf("recovery = %q", note)
	}
	if elapsed := time.Duration(res.ElapsedMS) * time.Millisecond; elapsed < 20*time.Millisecond {
		t.Fatalf("reset after %s, before the stall window passed", elapsed)
	}
}

func TestShortWaitsRecoverAStalledBoot(t *testing.T) {
	t.Run("log silent since before the wait", func(t *testing.T) {
		e := newEnv(t)
		e.m.bootStall = time.Minute
		path := bootingVM(t, e, managedVM("a", vm.StateRunning), stalledBoot)
		silentFor(t, path, time.Hour)
		e.vbox.ResetFunc = func(vm.Machine) { e.vbox.SetIP("a", "10.0.2.15") }
		res, err := e.m.Wait(t.Context(), vboxRef("a"), ipWait(150*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Recoveries) != 1 || !strings.HasSuffix(res.Recoveries[0], stallSuffix) {
			t.Fatalf("recoveries = %q", res.Recoveries)
		}
	})
	t.Run("many waits shorter than the window", func(t *testing.T) {
		e := newEnv(t)
		e.m.bootStall = 300 * time.Millisecond
		bootingVM(t, e, managedVM("a", vm.StateRunning), stalledBoot)
		e.vbox.ResetFunc = func(vm.Machine) { e.vbox.SetIP("a", "10.0.2.15") }
		start := time.Now()
		var notes []string
		for i := 0; ; i++ {
			res, err := e.m.Wait(t.Context(), vboxRef("a"), ipWait(100*time.Millisecond))
			notes = append(notes, res.Recoveries...)
			if err == nil {
				if len(notes) != 1 || i < 2 || time.Since(start) < 250*time.Millisecond {
					t.Fatalf("wait %d after %s: recoveries %q", i, time.Since(start), notes)
				}
				return
			}
			wantErr(t, err, context.DeadlineExceeded)
			if time.Since(start) > 5*time.Second {
				t.Fatal("the stalled boot was never reset")
			}
		}
	})
}

func TestWaitResetsAFailedBootAtOnce(t *testing.T) {
	cases := []struct {
		name    string
		console string
		note    string
	}{
		{name: "panic before the wait", console: stalledBoot + ioapicPanic, note: panicRecovery},
		{name: "panic in a boot without a banner", console: "Begin: Loading essential drivers ... " + ioapicPanic, note: panicRecovery},
		{name: "network card without a mac", console: stalledBoot + "\r\n" + badMAC + cloudInitDone + loginPrompt, note: "network card has an invalid MAC address, no network this boot — reset"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.m.bootStall = time.Hour
			bootingVM(t, e, managedVM("a", vm.StateRunning), c.console)
			e.vbox.ResetFunc = func(vm.Machine) { e.vbox.SetIP("a", "10.0.2.15") }
			res, err := e.m.Wait(t.Context(), vboxRef("a"), quickWait(WaitIP))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(res.Recoveries, []string{c.note}) {
				t.Fatalf("recoveries = %q", res.Recoveries)
			}
		})
	}
}

func TestWaitResetsWhenOnlyAnEarlierBootFinished(t *testing.T) {
	for name, console := range map[string]string{
		"new line":  stalledBoot + loginPrompt + cloudInitDone + stalledBoot,
		"same line": stalledBoot + cloudInitDone + loginPrompt + stalledBoot,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.m.bootStall = 20 * time.Millisecond
			bootingVM(t, e, managedVM("a", vm.StateRunning), console)
			e.vbox.ResetFunc = func(vm.Machine) { e.vbox.SetIP("a", "10.0.2.15") }
			res, err := e.m.Wait(t.Context(), vboxRef("a"), quickWait(WaitIP))
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Recoveries) != 1 || !strings.HasSuffix(res.Recoveries[0], stallSuffix) {
				t.Fatalf("recoveries = %q", res.Recoveries)
			}
		})
	}
}

func TestWaitResetsUntilSSHAnswers(t *testing.T) {
	e := newEnv(t)
	e.m.bootStall = 20 * time.Millisecond
	stub := stubSSH(e.m)
	failProbes(stub)
	path := bootingVM(t, e, sshVM("a", vm.PortForward{Name: sshForwardName, Protocol: "tcp", HostPort: 40022, GuestPort: 22}), stalledBoot)
	e.vbox.ResetFunc = func(vm.Machine) {
		appendConsole(t, path, stalledBoot+loginPrompt)
		stub.mu.Lock()
		stub.probe = nil
		stub.mu.Unlock()
	}
	res, err := e.m.Wait(t.Context(), vboxRef("a"), quickWait(WaitSSH))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recoveries) != 1 || !strings.HasPrefix(res.Recoveries[0], "boot stalled for ") || res.Machine.Name != "a" {
		t.Fatalf("result = %+v", res)
	}
}

func TestWaitGivesUpWhenTheBootFailsAgain(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			e := newEnv(t)
			e.m.bootStall = time.Hour
			e.m.bootResets = limit
			e.vbox.MaxReliableCPUs = 1
			mach := managedVM("a", vm.StateRunning)
			mach.CPUs = 2
			path := bootingVM(t, e, mach, stalledBoot+ioapicPanic)
			e.vbox.ResetFunc = func(vm.Machine) { appendConsole(t, path, stalledBoot+ioapicPanic) }
			start := time.Now()
			res, err := e.m.Wait(t.Context(), vboxRef("a"), ipWait(time.Minute))
			wantErr(t, err, vm.ErrNotReady)
			if calls := callsOf(e.vbox, "reset"); len(calls) != limit {
				t.Fatalf("reset %d times", len(calls))
			}
			want := slices.Repeat([]string{panicRecovery}, limit)
			if !slices.Equal(res.Recoveries, want) {
				t.Fatalf("recoveries = %q", res.Recoveries)
			}
			for _, part := range []string{
				`boot of vm "a" is stuck again` + resetNote(want) + ": kernel panic: IO-APIC + timer doesn't work!; this wait gives up, and another wait would reset the vm again",
				"fix the cause first (this host boots virtualbox VMs with more than 1 vCPU unreliably, stop the vm and set cpus to 1)",
			} {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("error %q lacks %q", err, part)
				}
			}
			if vm.Code(err) != vm.CodeNotReady || time.Since(start) > 5*time.Second {
				t.Fatalf("code %s after %s", vm.Code(err), time.Since(start))
			}
		})
	}
}

func TestWaitLeavesTheBootAlone(t *testing.T) {
	windows := managedVM("a", vm.StateRunning)
	windows.Meta[vm.MetaOSType] = "windows2022"
	cases := []struct {
		name    string
		mach    vm.Machine
		console string
		noLog   bool
		allow   bool
		req     WaitRequest
	}{
		{name: "vm not managed by vmh", mach: foreignVM("a", vm.StateRunning), console: stalledBoot + ioapicPanic, req: quickWait(WaitIP)},
		{name: "unmanaged vm with allow_unmanaged", mach: foreignVM("a", vm.StateRunning), console: stalledBoot, allow: true, req: guestWait(0)},
		{name: "windows guest", mach: windows, console: "2026-10-08 10:00:00.000 1234 INFO cloudbaseinit.init [-] Plugins execution done\r\n" + stalledBoot + ioapicPanic, req: guestWait(0)},
		{name: "no console log", mach: managedVM("a", vm.StateRunning), noLog: true, req: quickWait(WaitIP)},
		{name: "waiting for a power state", mach: managedVM("a", vm.StateRunning), console: stalledBoot + ioapicPanic, req: quickWait(WaitPaused)},
		{name: "login prompt", mach: sshVM("a", vm.PortForward{Name: sshForwardName, Protocol: "tcp", HostPort: 40022, GuestPort: 22}), console: stalledBoot + loginPrompt, req: quickWait(WaitSSH)},
		{name: "cloud-init finished", mach: managedVM("a", vm.StateRunning), console: stalledBoot + cloudInitDone, req: quickWait(WaitIP)},
		{name: "login prompt of the second boot", mach: managedVM("a", vm.StateRunning), console: stalledBoot + ioapicPanic + stalledBoot + loginPrompt, req: quickWait(WaitIP)},
		{name: "silent console", mach: managedVM("a", vm.StateRunning), console: "\r\n\x1b[2J", req: quickWait(WaitIP)},
		{name: "output without a kernel banner", mach: managedVM("a", vm.StateRunning), console: "SeaBIOS (version 1.16)\r\nBooting from Hard Disk...\r\n", req: quickWait(WaitIP)},
		{name: "half a panic line", mach: managedVM("a", vm.StateRunning), console: "Begin: Loading essential drivers ...\r\n[    1.48] Kernel panic - not syncing: IO-AP", req: quickWait(WaitIP)},
		{name: "paused vm", mach: managedVM("a", vm.StatePaused), console: stalledBoot + ioapicPanic, req: quickWait(WaitIP)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnvWith(t, Config{AllowUnmanaged: c.allow})
			e.m.bootStall = 10 * time.Millisecond
			failProbes(stubSSH(e.m))
			e.vbox.ExecFunc = func(vm.Machine, vm.ExecRequest) (vm.ExecResult, error) {
				return vm.ExecResult{}, fmt.Errorf("guest additions: %w", vm.ErrNotReady)
			}
			if c.noLog {
				e.vbox.Put(c.mach)
			} else {
				silentFor(t, bootingVM(t, e, c.mach, c.console), time.Hour)
			}
			req := c.req
			req.Timeout = 150 * time.Millisecond
			res, err := e.m.Wait(t.Context(), vboxRef("a"), req)
			wantErr(t, err, context.DeadlineExceeded)
			if calls := callsOf(e.vbox, "reset"); len(calls) > 0 || len(res.Recoveries) > 0 || strings.Contains(err.Error(), "automatic reset") {
				t.Fatalf("reset %d times: %v", len(calls), err)
			}
		})
	}
}

func TestWaitDoesNotCountPausedTime(t *testing.T) {
	e := newEnv(t)
	e.m.bootStall = 300 * time.Millisecond
	path := bootingVM(t, e, managedVM("a", vm.StatePaused), stalledBoot)
	silentFor(t, path, time.Hour)
	time.AfterFunc(100*time.Millisecond, func() { e.vbox.SetState("a", vm.StateRunning) })
	_, err := e.m.Wait(t.Context(), vboxRef("a"), ipWait(300*time.Millisecond))
	wantErr(t, err, context.DeadlineExceeded)
	if calls := callsOf(e.vbox, "reset"); len(calls) > 0 {
		t.Fatalf("reset right after the vm resumed: %v", err)
	}
}

func TestWaitDoesNotCountAShortPause(t *testing.T) {
	e := newEnv(t)
	e.m.bootStall = 300 * time.Millisecond
	path := bootingVM(t, e, managedVM("a", vm.StatePaused), stalledBoot)
	silentFor(t, path, 50*time.Millisecond)
	time.AfterFunc(200*time.Millisecond, func() { e.vbox.SetState("a", vm.StateRunning) })
	e.vbox.ResetFunc = func(vm.Machine) { e.vbox.SetIP("a", "10.0.2.15") }
	res, err := e.m.Wait(t.Context(), vboxRef("a"), ipWait(5*time.Second))
	if err != nil || len(res.Recoveries) != 1 {
		t.Fatalf("wait = %+v, %v", res, err)
	}
	if elapsed := time.Duration(res.ElapsedMS) * time.Millisecond; elapsed < 450*time.Millisecond {
		t.Fatalf("reset after %s, before a full window of running silence after the pause", elapsed)
	}
}

func TestWaitDoesNotResetWhileAnotherOperationHoldsTheVM(t *testing.T) {
	g := newGated()
	m := newManager(t, Config{}, g)
	m.bootStall = 200 * time.Millisecond
	mach := managedVM("a", vm.StateRunning)
	mach.ConsoleLog = consoleFile(t, stalledBoot)
	silentFor(t, mach.ConsoleLog, time.Hour)
	g.Put(mach)
	g.ResetFunc = func(vm.Machine) { g.SetIP("a", "10.0.2.15") }

	snap := snapshotAsync(t.Context(), m, "a", "s1")
	g.waitEntered(t)
	time.AfterFunc(150*time.Millisecond, func() { g.release <- struct{}{} })
	_, err := m.Wait(t.Context(), Ref{VM: "a"}, ipWait(300*time.Millisecond))
	wantErr(t, err, context.DeadlineExceeded)
	awaitDone(t, snap)
	if calls := callsOf(g.Provider, "reset"); len(calls) > 0 {
		t.Fatalf("reset as soon as the snapshot released the vm: %v", calls)
	}
	res, err := m.Wait(t.Context(), Ref{VM: "a"}, ipWait(2*time.Second))
	if err != nil || len(res.Recoveries) != 1 {
		t.Fatalf("later wait = %+v, %v", res, err)
	}
}

func TestConcurrentWaitsResetOnce(t *testing.T) {
	e := newEnv(t)
	e.m.bootStall = 50 * time.Millisecond
	path := bootingVM(t, e, managedVM("a", vm.StateRunning), stalledBoot)
	silentFor(t, path, time.Hour)
	e.vbox.ResetFunc = func(vm.Machine) {
		time.AfterFunc(200*time.Millisecond, func() { e.vbox.SetIP("a", "10.0.2.15") })
	}
	var wg sync.WaitGroup
	results := make([]WaitResult, 3)
	errs := make([]error, 3)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = e.m.Wait(t.Context(), vboxRef("a"), quickWait(WaitIP))
		}()
	}
	wg.Wait()
	notes := 0
	for i, res := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		notes += len(res.Recoveries)
	}
	if calls := callsOf(e.vbox, "reset"); len(calls) != 1 || notes != 1 {
		t.Fatalf("reset %d times, %d recoveries", len(calls), notes)
	}
}

func TestWaitAfterAResetElsewhere(t *testing.T) {
	for _, op := range []string{"reset", "start"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			e.m.bootStall = 10 * time.Millisecond
			state := vm.StateRunning
			if op == "start" {
				state = vm.StateStopped
			}
			path := bootingVM(t, e, managedVM("a", state), stalledBoot+ioapicPanic)
			silentFor(t, path, time.Hour)
			other := New(e.m.Config(), e.vbox)
			var err error
			if op == "reset" {
				_, err = other.Reset(t.Context(), vboxRef("a"))
			} else {
				_, err = other.Start(t.Context(), vboxRef("a"), false)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = e.m.Wait(t.Context(), vboxRef("a"), ipWait(150*time.Millisecond))
			wantErr(t, err, context.DeadlineExceeded)
			if calls := callsOf(e.vbox, "reset"); len(calls) != map[string]int{"reset": 1, "start": 0}[op] {
				t.Fatalf("resets = %v", calls)
			}
			appendConsole(t, path, stalledBoot)
			res, err := e.m.Wait(t.Context(), vboxRef("a"), ipWait(time.Second))
			wantErr(t, err, context.DeadlineExceeded)
			if len(res.Recoveries) != 1 || !strings.HasSuffix(res.Recoveries[0], stallSuffix) {
				t.Fatalf("the next boot was not watched: %q", res.Recoveries)
			}
		})
	}
}

type resetFails struct {
	*memprovider.Provider
	err error
}

func (r resetFails) Reset(ctx context.Context, ref string) error {
	if err := r.Provider.Reset(ctx, ref); err != nil {
		return err
	}
	return r.err
}

func TestWaitCountsAResetThatReportedAnError(t *testing.T) {
	vbox := memprovider.New(vm.VirtualBox)
	m := newManager(t, Config{}, resetFails{vbox, errors.New("VBoxManage: lost the session")})
	m.bootStall = time.Hour
	mach := managedVM("a", vm.StateRunning)
	mach.ConsoleLog = consoleFile(t, stalledBoot+ioapicPanic)
	vbox.Put(mach)
	vbox.ResetFunc = func(vm.Machine) { appendConsole(t, mach.ConsoleLog, stalledBoot+ioapicPanic) }
	res, err := m.Wait(t.Context(), vboxRef("a"), ipWait(time.Minute))
	wantErr(t, err, vm.ErrNotReady)
	note := "kernel panic: IO-APIC + timer doesn't work! — reset returned an error: VBoxManage: lost the session"
	if !slices.Equal(res.Recoveries, []string{note, note}) || len(callsOf(vbox, "reset")) != 2 {
		t.Fatalf("recoveries = %q, resets %v", res.Recoveries, callsOf(vbox, "reset"))
	}
}

func TestAFailedResetKeepsTheBootInView(t *testing.T) {
	cases := []struct {
		name    string
		console string
		stall   time.Duration
		note    string
	}{
		{name: "panic", console: stalledBoot + ioapicPanic, stall: time.Hour, note: "kernel panic: IO-APIC + timer doesn't work!"},
		{name: "stall", console: stalledBoot, stall: 50 * time.Millisecond, note: "boot stalled for 0s at: Begin: Loading essential drivers ..."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vbox := memprovider.New(vm.VirtualBox)
			m := newManager(t, Config{}, resetFails{vbox, errors.New("VBoxManage: the session is locked")})
			m.bootStall = c.stall
			mach := managedVM("a", vm.StateRunning)
			mach.ConsoleLog = consoleFile(t, c.console)
			silentFor(t, mach.ConsoleLog, time.Hour)
			vbox.Put(mach)
			if _, err := m.Reset(t.Context(), vboxRef("a")); err == nil {
				t.Fatal("reset did not fail")
			}
			note := c.note + " — reset returned an error: VBoxManage: the session is locked"
			for i := range 2 {
				start := time.Now()
				res, err := m.Wait(t.Context(), vboxRef("a"), ipWait(3*time.Second))
				wantErr(t, err, vm.ErrNotReady)
				if !slices.Equal(res.Recoveries, []string{note, note}) || time.Since(start) > 2*time.Second {
					t.Fatalf("wait %d after %s: recoveries %q", i, time.Since(start), res.Recoveries)
				}
			}
			if calls := callsOf(vbox, "reset"); len(calls) != 5 {
				t.Fatalf("reset %d times", len(calls))
			}
		})
	}
}

type unavailableAfterReset struct {
	*memprovider.Provider
	reset bool
}

func (u *unavailableAfterReset) Reset(ctx context.Context, ref string) error {
	u.reset = true
	return u.Provider.Reset(ctx, ref)
}

func (u *unavailableAfterReset) GuestIP(ctx context.Context, ref string) (string, error) {
	if u.reset {
		return "", fmt.Errorf("VBoxManage: VBoxSVC is not responding: %w", vm.ErrUnavailable)
	}
	return u.Provider.GuestIP(ctx, ref)
}

func TestWaitErrorsAfterAResetNameIt(t *testing.T) {
	p := &unavailableAfterReset{Provider: memprovider.New(vm.VirtualBox)}
	m := newManager(t, Config{}, p)
	mach := managedVM("a", vm.StateRunning)
	mach.ConsoleLog = consoleFile(t, stalledBoot+ioapicPanic)
	p.Put(mach)
	res, err := m.Wait(t.Context(), vboxRef("a"), quickWait(WaitIP))
	wantErr(t, err, vm.ErrUnavailable)
	want := `wait for vm "a" to report an ip address after 1 automatic reset (` + panicRecovery + "): VBoxManage: VBoxSVC is not responding"
	if !strings.HasPrefix(err.Error(), want) || vm.Code(err) != vm.CodeUnavailable || len(res.Recoveries) != 1 {
		t.Fatalf("error = %v, recoveries %q", err, res.Recoveries)
	}
}

func TestDeleteRemovesTheBootRecord(t *testing.T) {
	e := newEnv(t)
	bootingVM(t, e, managedVM("a", vm.StateRunning), stalledBoot)
	if _, err := e.m.Reset(t.Context(), vboxRef("a")); err != nil {
		t.Fatal(err)
	}
	record := e.m.bootMarkPath(e.vbox, mustGet(t, e.vbox, "a").ID)
	if !exists(record) {
		t.Fatal("reset left no boot record")
	}
	if err := e.m.Delete(t.Context(), vboxRef("a"), true); err != nil {
		t.Fatal(err)
	}
	if exists(record) {
		t.Fatal("boot record left behind")
	}
}

func TestScanBoot(t *testing.T) {
	cases := []struct {
		name    string
		console string
		want    bootScan
	}{
		{name: "empty", want: bootScan{}},
		{name: "no banner", console: "SeaBIOS\r\nBooting from Hard Disk...\r\n", want: bootScan{last: "Booting from Hard Disk..."}},
		{name: "stalled", console: stalledBoot, want: bootScan{booting: true, last: "Begin: Loading essential drivers ..."}},
		{name: "finished", console: stalledBoot + cloudInitDone + loginPrompt, want: bootScan{booting: true, finished: true, last: "vmh-test login:"}},
		{name: "panic", console: stalledBoot + ioapicPanic, want: bootScan{booting: true, failure: "kernel panic: IO-APIC + timer doesn't work!", last: "[    1.482002] ---[ end Kernel panic - not syncing: IO-APIC + timer doesn't work!  Boot with apic=de..."}},
		{name: "panic line still being written", console: stalledBoot + "\r\n[    1.48] Kernel panic - not syncing: IO-AP", want: bootScan{booting: true, last: "[    1.48] Kernel panic - not syncing: IO-AP"}},
		{name: "a new boot after a panic", console: stalledBoot + ioapicPanic + stalledBoot, want: bootScan{booting: true, last: "Begin: Loading essential drivers ..."}},
		{name: "a new boot after a login prompt", console: stalledBoot + loginPrompt + stalledBoot, want: bootScan{booting: true, last: "Begin: Loading essential drivers ..."}},
		{name: "bad mac despite login", console: stalledBoot + "\n" + badMAC + loginPrompt, want: bootScan{booting: true, finished: true, failure: "network card has an invalid MAC address, no network this boot", last: "vmh-test login:"}},
		{
			name:    "terminal noise",
			console: "[    0.000000] Linux version 6.8\r\n\x1b[0;32m  OK  \x1b[0m] Listening on \x1b[0;1;39msystemd-rfkill.socket\x1b[0m.\r\n\r[  *** ] A start job is running\r[ ***  ] Starting \xffjournal\tservice\r\n\r\n",
			want:    bootScan{booting: true, last: "[ ***  ] Starting journal service"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := scanBoot(c.console); got != c.want {
				t.Fatalf("scanBoot = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestBootWatchUsesTheLogAge(t *testing.T) {
	path := consoleFile(t, stalledBoot)
	w := &bootWatch{path: path, marks: filepath.Join(t.TempDir(), "a.boot"), window: 90 * time.Second, size: -1}
	now := time.Now()
	if got := w.look(now.Add(89 * time.Second)).reason; got != "" {
		t.Fatalf("stuck before the window: %q", got)
	}
	if got, want := w.look(now.Add(91*time.Second)).reason, "boot stalled for 90s at: Begin: Loading essential drivers ..."; got != want {
		t.Fatalf("stuck = %q, want %q", got, want)
	}
	silentFor(t, path, time.Hour)
	w = &bootWatch{path: path, marks: w.marks, window: 90 * time.Second, size: -1}
	if got := w.look(time.Now()).reason; got == "" {
		t.Fatal("an hour-old stalled boot is not stuck")
	}
	appendConsole(t, path, "\r\n[    9.000000] ata1: SATA link up\r\n")
	silentFor(t, path, time.Hour)
	if got := w.look(time.Now()).reason; got != "" {
		t.Fatalf("output seen by this wait still counts as stalled: %q", got)
	}
	w = &bootWatch{path: path, marks: w.marks, window: 90 * time.Second, size: -1, idle: time.Now()}
	if got := w.look(time.Now().Add(time.Minute)).reason; got != "" {
		t.Fatalf("stalled while the vm was not running: %q", got)
	}
	if got := w.look(time.Now().Add(2 * time.Minute)).reason; got == "" {
		t.Fatal("not stalled a full window after the vm ran again")
	}
}

func TestBootRecordSplitsTheLog(t *testing.T) {
	e := newEnv(t)
	path := bootingVM(t, e, managedVM("a", vm.StateRunning), stalledBoot+ioapicPanic)
	w := e.m.watchBoot(e.vbox, mustGet(t, e.vbox, "a"), WaitIP)
	if _, err := e.m.Reset(t.Context(), vboxRef("a")); err != nil {
		t.Fatal(err)
	}
	if got := w.look(time.Now()).reason; got != "" {
		t.Fatalf("the boot before the reset still counts: %q", got)
	}
	appendConsole(t, path, stalledBoot+ioapicPanic)
	if got := w.look(time.Now()).reason; got != "kernel panic: IO-APIC + timer doesn't work!" {
		t.Fatalf("the boot after the reset is not watched: %q", got)
	}
	replaced := stalledBoot + strings.Repeat("\r\n[    2.000000] ata1: SATA link up", 20) + ioapicPanic
	if err := os.WriteFile(path, []byte(replaced), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := w.look(time.Now()).reason; got != "kernel panic: IO-APIC + timer doesn't work!" {
		t.Fatalf("a replaced log is read from the old offset: %q", got)
	}
}

func TestResumeKeepsTheBootStart(t *testing.T) {
	e := newEnv(t)
	e.m.bootStall = 100 * time.Millisecond
	path := bootingVM(t, e, managedVM("a", vm.StateRunning), stalledBoot)
	if _, err := e.m.Reset(t.Context(), vboxRef("a")); err != nil {
		t.Fatal(err)
	}
	appendConsole(t, path, stalledBoot)
	if _, err := e.m.Pause(t.Context(), vboxRef("a")); err != nil {
		t.Fatal(err)
	}
	silentFor(t, path, time.Hour)
	if _, err := e.m.Resume(t.Context(), vboxRef("a")); err != nil {
		t.Fatal(err)
	}
	e.vbox.ResetFunc = func(vm.Machine) { e.vbox.SetIP("a", "10.0.2.15") }
	res, err := e.m.Wait(t.Context(), vboxRef("a"), quickWait(WaitIP))
	if err != nil || len(res.Recoveries) != 1 || res.ElapsedMS < 80 {
		t.Fatalf("wait after resume = %+v, %v", res, err)
	}
}

func TestBootWatchKeepsALargeLogBounded(t *testing.T) {
	path := consoleFile(t, stalledBoot+strings.Repeat("y", consoleTail*2))
	w := &bootWatch{path: path, marks: filepath.Join(t.TempDir(), "a.boot"), window: time.Minute, size: -1}
	if got := w.look(time.Now().Add(time.Hour)).reason; got != "" {
		t.Fatalf("a boot whose banner is out of reach is stuck: %q", got)
	}
	if got := scanBoot(stalledBoot + strings.Repeat("y", 500)).last; len([]rune(got)) != consoleLineMax+len("...") {
		t.Fatalf("last line of %d runes", len([]rune(got)))
	}
}

func TestPanicReason(t *testing.T) {
	cases := map[string]string{
		"[    1.48] Kernel panic - not syncing: IO-APIC + timer doesn't work!  Boot with apic=debug": "kernel panic: IO-APIC + timer doesn't work!",
		"Kernel panic - not syncing: Attempted to kill init! exitcode=0x00000009":                    "kernel panic: Attempted to kill init! exitcode=0x00000009",
		"[    3.10] Kernel panic: fatal exception":                                                   "kernel panic: fatal exception",
		"Kernel panic -": "kernel panic",
	}
	for line, want := range cases {
		if got := panicReason(line); got != want {
			t.Errorf("panicReason(%q) = %q, want %q", line, got, want)
		}
	}
}
