package iso9660

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/ZetGames/vm-harness/vm"
)

type File struct {
	Name string
	Data []byte
}

const (
	sectorSize    = 2048
	maxNameLen    = 30
	pathTableSize = 10
	flagDirectory = 0x02
)

const (
	primaryDescriptor = 16 + iota
	jolietDescriptor
	terminatorDescriptor
	primaryPathTableL
	primaryPathTableM
	jolietPathTableL
	jolietPathTableM
	primaryRoot
	jolietRoot
	firstFileSector
)

var (
	validLabel = regexp.MustCompile(`^[A-Za-z0-9_-]{1,16}$`)
	validName  = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)?$`)

	recordingTime = time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)

	unspecifiedVolumeTime = []byte("0000000000000000\x00")
)

type tree struct {
	joliet     bool
	descriptor uint32
	pathTableL uint32
	pathTableM uint32
	root       uint32
}

var (
	primaryTree = tree{descriptor: primaryDescriptor, pathTableL: primaryPathTableL, pathTableM: primaryPathTableM, root: primaryRoot}
	jolietTree  = tree{joliet: true, descriptor: jolietDescriptor, pathTableL: jolietPathTableL, pathTableM: jolietPathTableM, root: jolietRoot}
)

type extent struct {
	file  File
	start uint32
}

func Write(w io.Writer, volumeLabel string, files []File) error {
	if !validLabel.MatchString(volumeLabel) {
		return fmt.Errorf("volume label %q must be 1 to 16 letters, digits, '-' or '_': %w", volumeLabel, vm.ErrInvalid)
	}
	if err := checkFiles(files); err != nil {
		return err
	}
	extents, volumeSize := allocate(files)

	head := make([]byte, firstFileSector*sectorSize)
	for _, t := range []tree{primaryTree, jolietTree} {
		t.writeDescriptor(sector(head, t.descriptor), volumeLabel, volumeSize)
		copy(sector(head, t.pathTableL), pathTable(t.root, binary.LittleEndian))
		copy(sector(head, t.pathTableM), pathTable(t.root, binary.BigEndian))
		if err := t.writeRoot(sector(head, t.root), extents); err != nil {
			return err
		}
	}
	writeTerminator(sector(head, terminatorDescriptor))

	if _, err := w.Write(head); err != nil {
		return err
	}
	for _, e := range extents {
		if err := writePadded(w, e.file.Data); err != nil {
			return err
		}
	}
	return nil
}

func writePadded(w io.Writer, data []byte) error {
	if _, err := w.Write(data); err != nil {
		return err
	}
	var zeros [sectorSize]byte
	_, err := w.Write(zeros[:(sectorSize-len(data)%sectorSize)%sectorSize])
	return err
}

func checkFiles(files []File) error {
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		base, ext, _ := strings.Cut(f.Name, ".")
		key := strings.ToUpper(f.Name)
		switch {
		case f.Name == "":
			return fmt.Errorf("empty file name: %w", vm.ErrInvalid)
		case !validName.MatchString(f.Name):
			return fmt.Errorf("file name %q may only contain letters, digits, '-', '_' and one inner '.': %w", f.Name, vm.ErrInvalid)
		case len(base)+len(ext) > maxNameLen:
			return fmt.Errorf("file name %q is longer than %d characters: %w", f.Name, maxNameLen, vm.ErrInvalid)
		case seen[key]:
			return fmt.Errorf("duplicate file name %q: %w", f.Name, vm.ErrInvalid)
		case uint64(len(f.Data)) > math.MaxUint32:
			return fmt.Errorf("file %q is larger than 4 GiB: %w", f.Name, vm.ErrInvalid)
		}
		seen[key] = true
	}
	return nil
}

func allocate(files []File) ([]extent, uint32) {
	extents := make([]extent, len(files))
	for i, f := range files {
		extents[i].file = f
	}
	slices.SortFunc(extents, func(a, b extent) int { return primaryTree.compare(a.file.Name, b.file.Name) })
	next := uint32(firstFileSector)
	for i := range extents {
		if n := sectors(len(extents[i].file.Data)); n > 0 {
			extents[i].start = next
			next += n
		}
	}
	return extents, next
}

func (t tree) writeDescriptor(b []byte, label string, volumeSize uint32) {
	if t.joliet {
		b[0] = 2
		copy(b[88:91], "%/E")
	} else {
		b[0] = 1
		label = strings.ToUpper(label)
	}
	copy(b[1:6], "CD001")
	b[6] = 1
	t.text(b[8:40], "")
	t.text(b[40:72], label)
	putBoth32(b[80:88], volumeSize)
	putBoth16(b[120:124], 1)
	putBoth16(b[124:128], 1)
	putBoth16(b[128:132], sectorSize)
	putBoth32(b[132:140], pathTableSize)
	binary.LittleEndian.PutUint32(b[140:144], t.pathTableL)
	binary.BigEndian.PutUint32(b[148:152], t.pathTableM)
	copy(b[156:190], directoryRecord([]byte{0}, t.root, sectorSize, flagDirectory))
	for _, field := range [][]byte{b[190:318], b[318:446], b[446:574], b[574:702], b[702:739], b[739:776], b[776:813]} {
		t.text(field, "")
	}
	created := volumeTime(recordingTime)
	copy(b[813:830], created)
	copy(b[830:847], created)
	copy(b[847:864], unspecifiedVolumeTime)
	copy(b[864:881], unspecifiedVolumeTime)
	b[881] = 1
}

func (t tree) writeRoot(b []byte, extents []extent) error {
	sorted := slices.SortedFunc(slices.Values(extents), func(a, b extent) int { return t.compare(a.file.Name, b.file.Name) })
	off := copy(b, directoryRecord([]byte{0}, t.root, sectorSize, flagDirectory))
	off += copy(b[off:], directoryRecord([]byte{1}, t.root, sectorSize, flagDirectory))
	for _, e := range sorted {
		r := directoryRecord(t.identifier(e.file.Name), e.start, uint32(len(e.file.Data)), 0)
		if off+len(r) > len(b) {
			return fmt.Errorf("%d files do not fit in one directory sector: %w", len(extents), vm.ErrInvalid)
		}
		off += copy(b[off:], r)
	}
	return nil
}

func (t tree) identifier(name string) []byte {
	if t.joliet {
		return ucs2(name)
	}
	id := strings.ToUpper(name)
	if !strings.Contains(id, ".") {
		id += "."
	}
	return []byte(id + ";1")
}

func (t tree) compare(a, b string) int {
	pad := "\x00"
	if !t.joliet {
		a, b, pad = strings.ToUpper(a), strings.ToUpper(b), " "
	}
	aBase, aExt, _ := strings.Cut(a, ".")
	bBase, bExt, _ := strings.Cut(b, ".")
	return cmp.Or(comparePadded(aBase, bBase, pad), comparePadded(aExt, bExt, pad))
}

func (t tree) text(field []byte, s string) {
	if t.joliet {
		copy(field, ucs2(padRight(s, len(field)/2, " ")))
		return
	}
	copy(field, padRight(s, len(field), " "))
}

func directoryRecord(id []byte, start, size uint32, flags byte) []byte {
	n := 33 + len(id)
	if n%2 == 1 {
		n++
	}
	r := make([]byte, n)
	r[0] = byte(n)
	putBoth32(r[2:10], start)
	putBoth32(r[10:18], size)
	copy(r[18:25], recordTime(recordingTime))
	r[25] = flags
	putBoth16(r[28:32], 1)
	r[32] = byte(len(id))
	copy(r[33:], id)
	return r
}

func pathTable(root uint32, order binary.ByteOrder) []byte {
	b := make([]byte, pathTableSize)
	b[0] = 1
	order.PutUint32(b[2:6], root)
	order.PutUint16(b[6:8], 1)
	return b
}

func writeTerminator(b []byte) {
	b[0] = 255
	copy(b[1:6], "CD001")
	b[6] = 1
}

func volumeTime(t time.Time) []byte {
	return []byte(t.Format("20060102150405") + "00\x00")
}

func recordTime(t time.Time) []byte {
	return []byte{byte(t.Year() - 1900), byte(t.Month()), byte(t.Day()), byte(t.Hour()), byte(t.Minute()), byte(t.Second()), 0}
}

func comparePadded(a, b, pad string) int {
	n := max(len(a), len(b))
	return strings.Compare(padRight(a, n, pad), padRight(b, n, pad))
}

func padRight(s string, n int, pad string) string {
	return s + strings.Repeat(pad, n-len(s))
}

func ucs2(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.BigEndian.AppendUint16(b, u)
	}
	return b
}

func putBoth16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b[0:2], v)
	binary.BigEndian.PutUint16(b[2:4], v)
}

func putBoth32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b[0:4], v)
	binary.BigEndian.PutUint32(b[4:8], v)
}

func sectors(size int) uint32 {
	return uint32((size + sectorSize - 1) / sectorSize)
}

func sector(img []byte, n uint32) []byte {
	return img[n*sectorSize : (n+1)*sectorSize]
}
