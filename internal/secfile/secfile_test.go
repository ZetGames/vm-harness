package secfile_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/ZetGames/vm-harness/internal/secfile"
	"github.com/ZetGames/vm-harness/internal/secfile/secfiletest"
)

func TestRestrictFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if secfiletest.CheckRestricted(path) == nil {
		t.Fatal("a new file is already private before Restrict")
	}

	if err := secfile.Restrict(path); err != nil {
		t.Fatal(err)
	}
	if err := secfiletest.CheckRestricted(path); err != nil {
		t.Error(err)
	}
	if err := os.WriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatalf("owner lost write access: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "two" {
		t.Errorf("owner read %q, %v", data, err)
	}
}

func TestRestrictDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := secfile.Restrict(dir); err != nil {
		t.Fatal(err)
	}
	if err := secfiletest.CheckRestricted(dir); err != nil {
		t.Error(err)
	}
	child := filepath.Join(dir, "key")
	if err := os.WriteFile(child, []byte("x"), 0o600); err != nil {
		t.Fatalf("owner cannot create files in the restricted dir: %v", err)
	}
	if err := secfiletest.CheckRestricted(child); err != nil {
		t.Errorf("file created in a restricted dir: %v", err)
	}
}

func TestRestrictMissing(t *testing.T) {
	err := secfile.Restrict(filepath.Join(t.TempDir(), "absent"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want ErrNotExist", err)
	}
}
