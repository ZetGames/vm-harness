package harness

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/fl4metf/vm-harness/vm"
)

func guestWithFile(t *testing.T, e *testEnv, data string) Ref {
	t.Helper()
	stubSSH(e.m)
	e.vbox.Put(managedVM("a", vm.StateRunning))
	if err := e.m.WriteFile(t.Context(), vboxRef("a"), "/tmp/x", []byte(data), Access{}); err != nil {
		t.Fatal(err)
	}
	return vboxRef("a")
}

func TestOnlyTheFilesFolderOfTheRootIsWritable(t *testing.T) {
	attack := `{"allow_unmanaged": true}`
	cases := []struct {
		name       string
		hostDirs   func(root string) []string
		besideRoot bool
	}{
		{"no host dirs", func(string) []string { return nil }, true},
		{"host dir above the root", func(root string) []string { return []string{filepath.Dir(root)} }, true},
		{"host dir is the root", func(root string) []string { return []string{root} }, false},
		{"host dir is the files folder", func(root string) []string { return []string{filepath.Join(root, "files")} }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.m.cfg.HostDirs = c.hostDirs(e.root)
			ref := guestWithFile(t, e, attack)
			config := writeFile(t, filepath.Join(e.root, "config.json"), "{}")
			for _, rel := range []string{"config.json", "serve.token", filepath.Join("keys", "virtualbox-a-00112233"),
				filepath.Join("locks", "create.lock"), filepath.Join("virtualbox", "a", "notes.txt"), filepath.Join("vmware", "b", "b.txt")} {
				wantErr(t, copyFrom(e.m, t.Context(), ref, filepath.Join(e.root, rel)), vm.ErrForbidden)
			}
			if got, _ := os.ReadFile(config); string(got) != "{}" {
				t.Fatalf("config.json became %s", got)
			}
			if exists(filepath.Join(e.root, "serve.token")) {
				t.Fatal("serve.token was written")
			}

			out := filepath.Join(e.root, "files", "out.txt")
			if err := copyFrom(e.m, t.Context(), ref, out); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(out); string(got) != attack {
				t.Fatalf("copied %q", got)
			}
			if fi, err := os.Stat(filepath.Dir(out)); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
				t.Fatalf("files folder: %v, %v", fi.Mode(), err)
			}
			beside := filepath.Join(filepath.Dir(e.root), "beside.txt")
			if err := copyFrom(e.m, t.Context(), ref, beside); c.besideRoot && err != nil {
				t.Fatalf("copy next to the root: %v", err)
			}

			for _, rel := range []string{"keys", "virtualbox", "locks"} {
				dir := filepath.Join(e.root, rel)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				_, err := e.m.Create(t.Context(), vm.Spec{Name: "s", SharedFolders: []vm.SharedFolder{{Name: "s", HostPath: dir}}})
				wantErr(t, err, vm.ErrForbidden)
			}
			if _, err := e.m.Create(t.Context(), vm.Spec{Name: "s", SharedFolders: []vm.SharedFolder{{Name: "s", HostPath: filepath.Join(e.root, "files")}}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFilesFolderIsCreatedOnDemand(t *testing.T) {
	e := newEnv(t)
	files := filepath.Join(e.root, "files")
	if exists(files) {
		t.Fatal("files folder exists before use")
	}
	if _, err := e.m.Create(t.Context(), vm.Spec{Name: "s", SharedFolders: []vm.SharedFolder{{Name: "s", HostPath: files}}}); err != nil {
		t.Fatal(err)
	}
	if !exists(files) {
		t.Fatal("files folder was not created")
	}
}

func TestLinkInsideHostDirsDoesNotEscape(t *testing.T) {
	allowed := t.TempDir()
	secret := t.TempDir()
	writeFile(t, filepath.Join(secret, "secret.txt"), "secret")
	link := filepath.Join(allowed, "ext")
	dirLink(t, secret, link)
	e := newEnvWith(t, Config{HostDirs: []string{allowed}})
	ref := guestWithFile(t, e, "planted")

	wantErr(t, copyFrom(e.m, t.Context(), ref, filepath.Join(link, "new.txt")), vm.ErrForbidden)
	if exists(filepath.Join(secret, "new.txt")) {
		t.Fatal("file written through the link")
	}
	err := e.m.CopyTo(t.Context(), ref, CopyRequest{CopyRequest: vm.CopyRequest{HostPath: filepath.Join(link, "secret.txt"), GuestPath: "/tmp/s"}})
	wantErr(t, err, vm.ErrForbidden)
	_, err = e.m.Create(t.Context(), vm.Spec{Name: "s", SharedFolders: []vm.SharedFolder{{Name: "s", HostPath: link}}})
	wantErr(t, err, vm.ErrForbidden)
	if calls := callsOf(e.vbox, "create"); len(calls) > 0 {
		t.Fatalf("created: %v", calls)
	}
	if err := copyFrom(e.m, t.Context(), ref, filepath.Join(allowed, "ok.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestUnmanagedFolderBehindALink(t *testing.T) {
	base := t.TempDir()
	target := t.TempDir()
	writeFile(t, filepath.Join(target, "vm1", "vm1.vbox"), "<vbox/>")
	notes := writeFile(t, filepath.Join(target, "vm1", "notes.txt"), "keep")
	dirLink(t, target, filepath.Join(base, "vms"))
	e := newEnv(t)
	ref := guestWithFile(t, e, "planted")
	legacy := foreignVM("legacy", vm.StateStopped)
	legacy.ConfigPath = filepath.Join(base, "vms", "vm1", "vm1.vbox")
	e.vmw.Put(legacy)

	free := t.TempDir()
	if err := copyFrom(e.m, t.Context(), ref, filepath.Join(free, "out.txt")); err != nil {
		t.Fatalf("an unmanaged vm behind a link broke copy-from: %v", err)
	}
	for _, host := range []string{filepath.Join(base, "vms", "vm1", "notes.txt"), notes, filepath.Join(target, "vm1", "new.txt")} {
		wantErr(t, copyFrom(e.m, t.Context(), ref, host), vm.ErrForbidden)
	}
	if got, _ := os.ReadFile(notes); string(got) != "keep" {
		t.Fatalf("notes became %q", got)
	}
	for dir, want := range map[string]error{free: nil, filepath.Join(target, "vm1"): vm.ErrForbidden, filepath.Join(base, "vms"): vm.ErrForbidden} {
		_, err := e.m.Create(t.Context(), vm.Spec{Name: "s", SharedFolders: []vm.SharedFolder{{Name: "s", HostPath: dir}}})
		if want == nil {
			if err != nil {
				t.Fatalf("share %s: %v", dir, err)
			}
			if err := e.m.Delete(t.Context(), vboxRef("s"), false); err != nil {
				t.Fatal(err)
			}
			continue
		}
		wantErr(t, err, want)
	}
}

func TestUnresolvableUnmanagedFolderDoesNotBlockOtherPaths(t *testing.T) {
	base := t.TempDir()
	gone := t.TempDir()
	dirLink(t, gone, filepath.Join(base, "vms"))
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	ref := guestWithFile(t, e, "data")
	legacy := foreignVM("legacy", vm.StateStopped)
	legacy.ConfigPath = filepath.Join(base, "vms", "vm1", "vm1.vbox")
	e.vmw.Put(legacy)

	out := filepath.Join(t.TempDir(), "out.txt")
	if err := copyFrom(e.m, t.Context(), ref, out); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, []byte("data")) {
		t.Fatalf("copied %q", got)
	}
	if _, err := e.m.Create(t.Context(), vm.Spec{Name: "s", SharedFolders: []vm.SharedFolder{{Name: "s", HostPath: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	wantErr(t, copyFrom(e.m, t.Context(), ref, filepath.Join(base, "vms", "vm1", "x.txt")), vm.ErrInvalid)
}
