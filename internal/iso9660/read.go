package iso9660

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/fl4metf/vm-harness/vm"
)

const (
	flagAssociated  = 0x04
	flagMultiExtent = 0x80
)

type volume struct {
	label  string
	joliet bool
	root   record
}

type record struct {
	extent uint32
	size   uint32
	flags  byte
	id     []byte
}

func Read(img []byte) (string, map[string][]byte, error) {
	primary, joliet, err := volumes(img)
	if err != nil {
		return "", nil, err
	}
	v := cmp.Or(joliet, primary)
	records, err := v.records(img)
	if err != nil {
		return "", nil, err
	}
	files := make(map[string][]byte)
	for _, r := range records {
		if r.flags&(flagDirectory|flagAssociated) != 0 {
			continue
		}
		name, err := v.name(r.id)
		if err != nil {
			return "", nil, err
		}
		if r.flags&flagMultiExtent != 0 {
			return "", nil, fmt.Errorf("file %q spans several extents: %w", name, vm.ErrUnsupported)
		}
		if _, dup := files[name]; dup {
			return "", nil, fmt.Errorf("file %q appears twice in the root directory: %w", name, vm.ErrInvalid)
		}
		data, err := bytesAt(img, r.extent, r.size)
		if err != nil {
			return "", nil, err
		}
		files[name] = data
	}
	return v.label, files, nil
}

func volumes(img []byte) (*volume, *volume, error) {
	var primary, joliet *volume
	for n := uint32(primaryDescriptor); ; n++ {
		d, err := bytesAt(img, n, sectorSize)
		if err != nil || string(d[1:6]) != "CD001" {
			return nil, nil, fmt.Errorf("sector %d is not an ISO 9660 volume descriptor: %w", n, vm.ErrInvalid)
		}
		switch {
		case d[0] == 255 && primary == nil:
			return nil, nil, fmt.Errorf("image has no primary volume descriptor: %w", vm.ErrInvalid)
		case d[0] == 255:
			return primary, joliet, nil
		case d[0] == 1 && primary == nil:
			primary, err = parseVolume(d, false)
		case d[0] == 2 && joliet == nil && string(d[88:90]) == "%/" && strings.IndexByte("@CE", d[90]) >= 0:
			joliet, err = parseVolume(d, true)
		}
		if err != nil {
			return nil, nil, err
		}
	}
}

func parseVolume(d []byte, joliet bool) (*volume, error) {
	if size := binary.LittleEndian.Uint16(d[128:130]); size != sectorSize {
		return nil, fmt.Errorf("logical block size %d: %w", size, vm.ErrUnsupported)
	}
	root, err := parseRecord(d[156:190])
	if err != nil {
		return nil, err
	}
	if root.flags&flagDirectory == 0 {
		return nil, fmt.Errorf("root directory record has flags %#x: %w", root.flags, vm.ErrInvalid)
	}
	v := &volume{joliet: joliet, root: root}
	v.label = strings.TrimRight(v.text(d[40:72]), " ")
	return v, nil
}

func (v *volume) records(img []byte) ([]record, error) {
	data, err := bytesAt(img, v.root.extent, v.root.size)
	if err != nil {
		return nil, err
	}
	var records []record
	for off := 0; off < len(data); {
		n := int(data[off])
		if n == 0 {
			off = (off/sectorSize + 1) * sectorSize
			continue
		}
		if off%sectorSize+n > sectorSize || off+n > len(data) {
			return nil, fmt.Errorf("directory record at byte %d of sector %d crosses a sector boundary: %w", off, v.root.extent, vm.ErrInvalid)
		}
		r, err := parseRecord(data[off : off+n])
		if err != nil {
			return nil, err
		}
		records = append(records, r)
		off += n
	}
	return records, nil
}

func parseRecord(b []byte) (record, error) {
	if len(b) < 34 || int(b[0]) != len(b) || 33+int(b[32]) > len(b) {
		return record{}, fmt.Errorf("directory record of %d bytes is malformed: %w", len(b), vm.ErrInvalid)
	}
	return record{
		extent: binary.LittleEndian.Uint32(b[2:6]) + uint32(b[1]),
		size:   binary.LittleEndian.Uint32(b[10:14]),
		flags:  b[25],
		id:     b[33 : 33+int(b[32])],
	}, nil
}

func (v *volume) name(id []byte) (string, error) {
	if v.joliet && len(id)%2 != 0 {
		return "", fmt.Errorf("joliet file identifier % x has an odd length: %w", id, vm.ErrInvalid)
	}
	name := strings.TrimSuffix(strings.TrimSuffix(v.text(id), ";1"), ".")
	if !v.joliet {
		name = strings.ToLower(name)
	}
	return name, nil
}

func (v *volume) text(b []byte) string {
	if !v.joliet {
		return string(b)
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = binary.BigEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(units))
}

func bytesAt(img []byte, start, size uint32) ([]byte, error) {
	if size == 0 {
		return nil, nil
	}
	off := int64(start) * sectorSize
	if off+int64(size) > int64(len(img)) {
		return nil, fmt.Errorf("extent at sector %d with %d bytes lies outside the image: %w", start, size, vm.ErrInvalid)
	}
	return img[off : off+int64(size)], nil
}
