package harness

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fl4metf/vm-harness/vm"
)

const (
	sniffSize     = 64 << 10
	maxDescriptor = 1 << 20
	maxOVF        = 16 << 20
	vdiSignature  = 0xbeda107f
)

var (
	descriptorText = regexp.MustCompile(`(?im)descriptorfile|createtype|parentfilenamehint|^\s*(?:rw|rdonly|noaccess)\s+\d+`)
	extentLine     = regexp.MustCompile(`(?i)^(?:rw|rdonly|noaccess)\s+\d+\s+\w+\s+"([^"]*)"`)
	opticalText    = regexp.MustCompile(`(?im)--iprt-iso-maker|^\s*file\s+\S`)
)

func refuseImage(kind, path, why string) error {
	return fmt.Errorf("%s %s %s, vmh does not use it from the host directories: %w", kind, path, why, vm.ErrForbidden)
}

func readAt(f *os.File, off int64, n int) ([]byte, error) {
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:got], nil
}

func checkISO(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("iso: %v: %w", err, vm.ErrInvalid)
	}
	defer f.Close()
	head, err := readAt(f, 0, 0x8006)
	if err != nil {
		return fmt.Errorf("iso: %v: %w", err, vm.ErrInvalid)
	}
	if len(head) < 0x8006 || string(head[0x8001:]) != "CD001" || opticalText.Match(head[:0x8000]) {
		return refuseImage("iso", path, "is not a plain ISO 9660 image")
	}
	return nil
}

func checkAppliance(path string) error {
	if !strings.EqualFold(filepath.Ext(path), ".ova") {
		return refuseImage("appliance", path, "is not an .ova archive (an .ovf can name any host file)")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("appliance: %v: %w", err, vm.ErrInvalid)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err != nil {
			return refuseImage("appliance", path, "is not a tar archive with an .ovf descriptor")
		}
		if strings.EqualFold(filepath.Ext(hdr.Name), ".ovf") {
			return checkOVF(path, io.LimitReader(tr, maxOVF))
		}
	}
}

func checkOVF(path string, r io.Reader) error {
	dec := xml.NewDecoder(r)
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return refuseImage("appliance", path, "has an unreadable .ovf descriptor")
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "File" {
			continue
		}
		for _, attr := range start.Attr {
			if attr.Name.Local == "href" && (!fs.ValidPath(attr.Value) || strings.ContainsAny(attr.Value, `:\`)) {
				return refuseImage("appliance", path, fmt.Sprintf("references %q outside the archive", attr.Value))
			}
		}
	}
}

func (m *Manager) checkDiskImage(path string) error {
	const kind = "disk image"
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%s: %v: %w", kind, err, vm.ErrInvalid)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("%s: %v: %w", kind, err, vm.ErrInvalid)
	}
	size := fi.Size()
	head, err := readAt(f, 0, sniffSize)
	if err != nil {
		return fmt.Errorf("%s: %v: %w", kind, err, vm.ErrInvalid)
	}
	tail, err := readAt(f, max(size-512, 0), 512)
	if err != nil {
		return fmt.Errorf("%s: %v: %w", kind, err, vm.ErrInvalid)
	}
	switch {
	case bytes.HasPrefix(head, []byte("COWD")):
		return refuseImage(kind, path, "is an ESX sparse disk, which can name a parent disk")
	case !bytes.HasPrefix(head, []byte("KDMV")) && (!bytes.Contains(head[:min(len(head), 512)], []byte{0}) || descriptorText.Match(head)):
		return refuseImage(kind, path, "is a text file such as a VMDK descriptor, which can make the hypervisor read other host files")
	case len(head) >= 0x50 && binary.LittleEndian.Uint32(head[0x40:]) == vdiSignature && !baseVDI(head):
		return refuseImage(kind, path, "is a differencing VDI image")
	case bytes.HasPrefix(head, []byte("QFI\xfb")) && len(head) >= 16 && binary.BigEndian.Uint64(head[8:]) != 0:
		return refuseImage(kind, path, "is a qcow image with a backing file")
	case bytes.HasPrefix(head, []byte("QED\x00")) && len(head) >= 24 && binary.LittleEndian.Uint64(head[16:])&1 != 0:
		return refuseImage(kind, path, "is a QED image with a backing file")
	case differencingVHD(head) || differencingVHD(tail):
		return refuseImage(kind, path, "is a differencing VHD image")
	}
	return m.checkSparseVMDK(f, path, size)
}

func baseVDI(head []byte) bool {
	typeOffset := 0x4c
	if binary.LittleEndian.Uint32(head[0x44:])>>16 == 0 {
		typeOffset = 0x48
	}
	kind := binary.LittleEndian.Uint32(head[typeOffset:])
	return kind == 1 || kind == 2
}

func differencingVHD(footer []byte) bool {
	return len(footer) >= 64 && bytes.HasPrefix(footer, []byte("conectix")) && binary.BigEndian.Uint32(footer[60:]) == 4
}

func (m *Manager) checkSparseVMDK(f *os.File, path string, size int64) error {
	for _, off := range []int64{0, size - 1024} {
		if off < 0 {
			continue
		}
		header, err := readAt(f, off, 512)
		if err != nil {
			return fmt.Errorf("disk image: %v: %w", err, vm.ErrInvalid)
		}
		if len(header) < 44 || !bytes.HasPrefix(header, []byte("KDMV")) {
			continue
		}
		start := binary.LittleEndian.Uint64(header[28:])
		length := binary.LittleEndian.Uint64(header[36:])
		if length == 0 {
			continue
		}
		if length > maxDescriptor/512 || start > uint64(size)/512 {
			return refuseImage("disk image", path, "has a malformed VMDK header")
		}
		desc, err := readAt(f, int64(start)*512, int(length)*512)
		if err != nil {
			return fmt.Errorf("disk image: %v: %w", err, vm.ErrInvalid)
		}
		if err := m.checkDescriptor(path, string(desc)); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) checkDescriptor(path, desc string) error {
	for _, line := range strings.FieldsFunc(desc, func(r rune) bool { return r == '\n' || r == '\r' || r == 0 }) {
		line = strings.TrimSpace(line)
		if key, value, ok := strings.Cut(line, "="); ok {
			value = strings.Trim(strings.TrimSpace(value), `"`)
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "parentfilenamehint":
				return refuseImage("disk image", path, "names a parent disk")
			case "parentcid":
				if !strings.EqualFold(value, "ffffffff") {
					return refuseImage("disk image", path, "names a parent disk")
				}
			case "ddb.uuid.parent":
				if strings.Trim(value, "0-") != "" {
					return refuseImage("disk image", path, "names a parent disk")
				}
			}
			continue
		}
		match := extentLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		if !filepath.IsLocal(match[1]) {
			return refuseImage("disk image", path, fmt.Sprintf("reads extent %q outside its folder", match[1]))
		}
		real, err := canonical(filepath.Join(filepath.Dir(path), match[1]))
		if err != nil || !m.inHostDirs(real) {
			return refuseImage("disk image", path, fmt.Sprintf("reads extent %q outside the host directories", match[1]))
		}
	}
	return nil
}
