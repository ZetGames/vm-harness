package harness

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/fl4metf/vm-harness/vm"
)

const (
	fileNameNormalized = 0x0
	volumeNameDOS      = 0x0
)

func driveLetter(volume string) bool {
	if len(volume) != 2 || volume[1] != ':' {
		return false
	}
	c := volume[0] | 0x20
	return 'a' <= c && c <= 'z'
}

func checkPathSyntax(path string) error {
	volume := filepath.VolumeName(path)
	if volume != "" && !driveLetter(volume) {
		return fmt.Errorf("host path %s: only paths on a drive letter are accepted, not device or network paths: %w", path, vm.ErrInvalid)
	}
	rest := path[len(volume):]
	if strings.Contains(rest, ":") {
		return fmt.Errorf("host path %s: ':' may only follow the drive letter: %w", path, vm.ErrInvalid)
	}
	for _, part := range strings.FieldsFunc(rest, func(r rune) bool { return r == '\\' || r == '/' }) {
		switch {
		case part == "." || part == "..":
		case strings.HasSuffix(part, ".") || strings.HasSuffix(part, " "):
			return fmt.Errorf("host path %s: %q ends in a dot or a space, which windows drops: %w", path, part, vm.ErrInvalid)
		case reservedName(part):
			return fmt.Errorf("host path %s: %q is a windows device name: %w", path, part, vm.ErrInvalid)
		}
	}
	return nil
}

func stripDevicePrefix(path string) string {
	for _, prefix := range []string{`\\?\`, `\\.\`, `\??\`} {
		if rest, ok := strings.CutPrefix(path, prefix); ok {
			if driveLetter(filepath.VolumeName(rest)) {
				return rest
			}
			if unc, ok := strings.CutPrefix(rest, `UNC\`); ok && prefix == `\\?\` {
				return `\\` + unc
			}
		}
	}
	return path
}

func finalPath(path string) (string, error) {
	name, err := windows.UTF16PtrFromString(`\\?\` + path)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", &fs.PathError{Op: "open", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), fileNameNormalized|volumeNameDOS)
		if err != nil {
			return "", fmt.Errorf("final path of %s: %v", path, err)
		}
		if int(n) < len(buf) {
			return stripDevicePrefix(windows.UTF16ToString(buf[:n])), nil
		}
		buf = make([]uint16, n)
	}
}
