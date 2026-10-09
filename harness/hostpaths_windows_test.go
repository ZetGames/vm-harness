package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/ZetGames/vm-harness/vm"
)

type windowsPaths struct {
	e      *testEnv
	ref    Ref
	vmDir  string
	notes  string
	config string
}

func newWindowsPaths(t *testing.T) windowsPaths {
	t.Helper()
	e := newEnv(t)
	ref := guestWithFile(t, e, `{"managed":"planted","allow_unmanaged":true}`)
	vmDir := t.TempDir()
	legacy := foreignVM("legacy", vm.StateStopped)
	legacy.ConfigPath = filepath.Join(vmDir, "legacy.vmx")
	e.vmw.Put(legacy)
	return windowsPaths{
		e:      e,
		ref:    ref,
		vmDir:  vmDir,
		notes:  writeFile(t, filepath.Join(vmDir, "notes.txt"), "keep"),
		config: filepath.Join(e.root, "config.json"),
	}
}

func adminShare(path string) string {
	volume := filepath.VolumeName(path)
	return `\\localhost\` + volume[:1] + `$` + path[len(volume):]
}

func TestWindowsDeviceAndNetworkPathsAreRefused(t *testing.T) {
	w := newWindowsPaths(t)
	writeFile(t, w.config, "{}")
	for _, host := range []string{
		`\\?\` + w.config,
		`\\.\` + w.config,
		`\??\` + w.config,
		`//?/` + filepath.ToSlash(w.config),
		adminShare(w.config),
		`\\127.0.0.1\` + w.config[:1] + `$` + w.config[2:],
		`\\?\UNC\localhost\` + w.config[:1] + `$` + w.config[2:],
		`\\?\` + w.notes,
		adminShare(w.notes),
	} {
		t.Run(host, func(t *testing.T) {
			wantErr(t, copyFrom(w.e.m, t.Context(), w.ref, host), vm.ErrInvalid)
		})
	}
	if got, _ := os.ReadFile(w.config); string(got) != "{}" {
		t.Fatalf("config.json became %s", got)
	}
	if got, _ := os.ReadFile(w.notes); string(got) != "keep" {
		t.Fatalf("notes became %s", got)
	}
	for _, dir := range []string{`\\?\` + w.vmDir, `\\?\` + w.e.root, `\\?\` + filepath.Dir(w.e.root), adminShare(w.vmDir)} {
		_, err := w.e.m.Create(t.Context(), vm.Spec{Name: "s", SharedFolders: []vm.SharedFolder{{Name: "s", HostPath: dir}}})
		wantErr(t, err, vm.ErrInvalid)
	}
	if calls := callsOf(w.e.vbox, "create"); len(calls) > 0 {
		t.Fatalf("created: %v", calls)
	}
}

func TestWindowsStreamsTrailingDotsAndDevicesAreRefused(t *testing.T) {
	w := newWindowsPaths(t)
	sidecar := filepath.Join(w.vmDir, "legacy.vmh.json")
	planted := filepath.Join(w.e.root, "vmware", "mine", "evil.vmx")
	free := t.TempDir()
	for _, host := range []string{
		sidecar + "::$DATA",
		w.config + "::$DATA",
		w.config + ":s",
		planted + "::$DATA",
		w.notes + ".",
		w.notes + " ",
		w.config + ". ",
		filepath.Join(w.vmDir+".", "notes.txt"),
		filepath.Join(free, "COM1"),
		filepath.Join(free, "nul.txt"),
		filepath.Join(free, "x:y"),
	} {
		t.Run(host, func(t *testing.T) {
			wantErr(t, copyFrom(w.e.m, t.Context(), w.ref, host), vm.ErrInvalid)
		})
	}
	for _, path := range []string{sidecar, w.config, planted} {
		if exists(path) {
			t.Fatalf("%s was created", path)
		}
	}
	if got, _ := os.ReadFile(w.notes); string(got) != "keep" {
		t.Fatalf("notes became %s", got)
	}
	for _, spec := range []vm.Spec{
		{Name: "s", ISO: writeISO(t, filepath.Join(free, "boot.iso")) + ":alt"},
		{Name: "s", SharedFolders: []vm.SharedFolder{{Name: "s", HostPath: free + "."}}},
	} {
		_, err := w.e.m.Create(t.Context(), spec)
		wantErr(t, err, vm.ErrInvalid)
	}
	_, err := w.e.m.Exec(t.Context(), w.ref, execCmd(TransportSSH, SSHOptions{User: "u", KeyPath: `\\?\` + filepath.Join(free, "id")}, "id"))
	wantErr(t, err, vm.ErrInvalid)
}

func TestWindowsShortNamesAreResolved(t *testing.T) {
	w := newWindowsPaths(t)
	long := filepath.Join(t.TempDir(), "unmanaged machine folder")
	notes := writeFile(t, filepath.Join(long, "notes.txt"), "keep")
	legacy := foreignVM("legacy2", vm.StateStopped)
	legacy.ConfigPath = filepath.Join(long, "legacy2.vmx")
	w.e.vmw.Put(legacy)
	name, err := windows.UTF16PtrFromString(long)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_PATH)
	n, err := windows.GetShortPathName(name, &buf[0], uint32(len(buf)))
	short := windows.UTF16ToString(buf[:n])
	if err != nil || strings.EqualFold(short, long) {
		t.Skipf("no 8.3 name for %s: %v", long, err)
	}
	wantErr(t, copyFrom(w.e.m, t.Context(), w.ref, filepath.Join(short, "notes.txt")), vm.ErrForbidden)
	wantErr(t, copyFrom(w.e.m, t.Context(), w.ref, filepath.Join(strings.ToUpper(long), "new.txt")), vm.ErrForbidden)
	if got, _ := os.ReadFile(notes); string(got) != "keep" {
		t.Fatalf("notes became %s", got)
	}
}
