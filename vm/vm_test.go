package vm

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
)

func TestCode(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{errors.New("boom"), "internal"},
		{fmt.Errorf("vm x: %w", ErrNotFound), "not_found"},
		{&CommandError{Path: "VBoxManage", Kind: ErrInvalidState}, "invalid_state"},
		{fmt.Errorf("wait: %w", context.DeadlineExceeded), "timeout"},
		{&CommandError{Path: "vmrun"}, "internal"},
	}
	for _, c := range cases {
		if got := Code(c.err); got != c.want {
			t.Errorf("Code(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestCommandErrorMessage(t *testing.T) {
	err := &CommandError{Path: "VBoxManage", Args: []string{"startvm", "x"}, ExitCode: 1, Stderr: "  no such vm\n", Kind: ErrNotFound}
	if got, want := err.Error(), "VBoxManage startvm: not found (exit 1): no such vm"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatal("errors.Is should see the kind")
	}
}

func TestApplyMeta(t *testing.T) {
	m := Machine{ID: "C:/VMs/Web/web.vmx", Meta: map[string]string{MetaManaged: "C:/VMs/Web/web.vmx", LabelKey("team"): "red", "ssh_user": "vmh", "label.": "x"}}
	ApplyMeta(&m)
	if !m.Managed {
		t.Fatal("expected managed")
	}
	if len(m.Labels) != 1 || m.Labels["team"] != "red" {
		t.Fatalf("labels = %v", m.Labels)
	}
	m.Meta = map[string]string{}
	ApplyMeta(&m)
	if m.Managed || m.Labels != nil {
		t.Fatalf("stale meta survived: %+v", m)
	}
	for _, marker := range []string{"1", "another-id", ""} {
		copied := Machine{ID: "4b1f", Meta: map[string]string{MetaManaged: marker}}
		ApplyMeta(&copied)
		if copied.Managed {
			t.Errorf("marker %q must not make %q managed", marker, copied.ID)
		}
	}
}

func TestNativeOSType(t *testing.T) {
	if got := NativeOSType(VirtualBox, "Ubuntu"); got != "Ubuntu_64" {
		t.Errorf("virtualbox ubuntu = %q", got)
	}
	if got := NativeOSType(VMware, "windows11"); got != "windows11-64" {
		t.Errorf("vmware windows11 = %q", got)
	}
	if got := NativeOSType(VMware, "centos7-64"); got != "centos7-64" {
		t.Errorf("passthrough = %q", got)
	}
	if got := nativeOSType(VirtualBox, "ubuntu", "arm64"); got != "Ubuntu_arm64" {
		t.Errorf("arm virtualbox ubuntu = %q", got)
	}
	if got := nativeOSType(VMware, "windows2022", "arm64"); got != "windows2019srvNext-64" {
		t.Errorf("arm fallback = %q", got)
	}
	if !(Machine{OSType: "Windows11_64"}).IsWindowsGuest() || (Machine{Meta: map[string]string{MetaOSType: "ubuntu"}, OSType: "Windows11_64"}).IsWindowsGuest() {
		t.Error("IsWindowsGuest should prefer the generic os type")
	}
	if !IsWindows("windows2022") || !IsWindows("Windows11_64") || !IsWindows("windows9-64") || IsWindows("ubuntu") {
		t.Error("IsWindows misclassified")
	}
}

func TestManagedMarkerCaseFollowsTheFilesystem(t *testing.T) {
	m := Machine{ID: "/vms/web/web.vmx", Meta: map[string]string{MetaManaged: "/VMS/web/web.vmx"}}
	ApplyMeta(&m)
	want := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	if m.Managed != want {
		t.Fatalf("managed = %v on %s, want %v", m.Managed, runtime.GOOS, want)
	}
}
