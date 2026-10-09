package harness

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZetGames/vm-harness/vm"
)

func sparseVMDK(descriptor string, atEnd bool) string {
	header := make([]byte, 512)
	copy(header, "KDMV")
	binary.LittleEndian.PutUint32(header[4:], 1)
	binary.LittleEndian.PutUint64(header[28:], 1)
	binary.LittleEndian.PutUint64(header[36:], 20)
	desc := make([]byte, 20*512)
	copy(desc, descriptor)
	image := append(header, desc...)
	if !atEnd {
		return string(append(image, make([]byte, 2048)...))
	}
	plain := make([]byte, 512)
	copy(plain, "KDMV")
	stream := append(plain, desc...)
	stream = append(stream, make([]byte, 512)...)
	stream = append(stream, header...)
	return string(append(stream, make([]byte, 512)...))
}

func descriptorWith(extent string, extra ...string) string {
	lines := append([]string{"# Disk DescriptorFile", "version=1", "CID=5428e012", "parentCID=ffffffff",
		`createType="monolithicSparse"`, "", `RW 8192 SPARSE "` + extent + `"`, "", `ddb.uuid.parent="00000000-0000-0000-0000-000000000000"`}, extra...)
	return strings.Join(lines, "\n") + "\n"
}

func vdiImage(kind uint32) string {
	head := make([]byte, 1024)
	copy(head, "<<< Oracle VM VirtualBox Disk Image >>>\n")
	binary.LittleEndian.PutUint32(head[0x40:], 0xbeda107f)
	binary.LittleEndian.PutUint32(head[0x44:], 0x00010001)
	binary.LittleEndian.PutUint32(head[0x48:], 400)
	binary.LittleEndian.PutUint32(head[0x4c:], kind)
	return string(head)
}

func vhdFooter(kind uint32) []byte {
	footer := make([]byte, 512)
	copy(footer, "conectix")
	binary.BigEndian.PutUint32(footer[60:], kind)
	return footer
}

func rawDisk() []byte {
	disk := make([]byte, 4096)
	copy(disk, "\xfa\x31\xc0\x8e\xd8")
	disk[510], disk[511] = 0x55, 0xaa
	return disk
}

func qcowImage(backing uint64) string {
	head := make([]byte, 1024)
	copy(head, "QFI\xfb")
	binary.BigEndian.PutUint32(head[4:], 3)
	binary.BigEndian.PutUint64(head[8:], backing)
	return string(head)
}

func ova(t *testing.T, ovf string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct{ name, data string }{{"box.ovf", ovf}, {"disk1.vmdk", sparseVMDK(descriptorWith("disk1.vmdk"), false)}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.data)), Format: tar.FormatUSTAR}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func ovf(href string) string {
	return `<?xml version="1.0"?><Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1" xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1">` +
		`<References><File ovf:href="` + href + `" ovf:id="file1"/></References></Envelope>`
}

func TestConfinedCreateRefusesImagesThatReferenceOtherHostFiles(t *testing.T) {
	isoImage := func(t *testing.T) string {
		data, err := os.ReadFile(writeISO(t, filepath.Join(t.TempDir(), "x.iso")))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	viso := "--iprt-iso-maker-file-marker-bourne-sh 3a7b1d55-2b5e-4a27-8c1f-0f2d4e3c9a10\n--iso-level=3\n/loot.bin=../outside/secret.bin\n"
	cases := []struct {
		name    string
		field   string
		file    string
		content func(t *testing.T) string
		allowed bool
	}{
		{"iso9660 image", "iso", "boot.iso", isoImage, true},
		{"viso named like an iso", "iso", "notes.iso", func(*testing.T) string { return viso }, false},
		{"viso marker in front of an iso", "iso", "boot.iso", func(t *testing.T) string {
			img := []byte(isoImage(t))
			copy(img, viso)
			return string(img)
		}, false},
		{"cue sheet in front of an iso", "iso", "boot.cue", func(t *testing.T) string {
			img := []byte(isoImage(t))
			copy(img, "FILE \"../outside/secret.bin\" BINARY\n  TRACK 01 MODE1/2048\n    INDEX 01 00:00:00\n")
			return string(img)
		}, false},
		{"not an iso", "iso", "boot.iso", func(*testing.T) string { return string(rawDisk()) }, false},
		{"vmdk text descriptor", "disk_image", "notes.txt", func(*testing.T) string {
			return "# Disk DescriptorFile\nversion=1\ncreateType=\"monolithicFlat\"\nRW 131072 FLAT \"boot.raw\" 0\nRW 2 FLAT \"../outside/secret.bin\" 0\n"
		}, false},
		{"vmdk descriptor hidden behind a nul", "disk_image", "disk.img", func(*testing.T) string {
			return "# Disk DescriptorFile\x00\nRW 2 FLAT \"../outside/secret.bin\" 0\n" + string(make([]byte, 1024))
		}, false},
		{"sparse vmdk", "disk_image", "cloud.vmdk", func(*testing.T) string { return sparseVMDK(descriptorWith("cloud.vmdk"), false) }, true},
		{"sparse vmdk with an outside extent", "disk_image", "cloud.vmdk", func(*testing.T) string {
			return sparseVMDK(descriptorWith("../outside/secret.bin"), false)
		}, false},
		{"sparse vmdk with an absolute extent", "disk_image", "cloud.vmdk", func(*testing.T) string {
			return sparseVMDK(descriptorWith(filepath.Join(t.TempDir(), "secret.bin")), false)
		}, false},
		{"sparse vmdk with a parent file", "disk_image", "child.vmdk", func(*testing.T) string {
			return sparseVMDK(descriptorWith("child.vmdk", `parentFileNameHint="base.vmdk"`), false)
		}, false},
		{"sparse vmdk with a parent uuid", "disk_image", "child.vmdk", func(*testing.T) string {
			return sparseVMDK(strings.Replace(descriptorWith("child.vmdk"), "00000000-0000-0000-0000-000000000000", "4a06b83a-05b8-4031-8aee-2c9b15ed8dc4", 1), false)
		}, false},
		{"sparse vmdk with a parent cid", "disk_image", "child.vmdk", func(*testing.T) string {
			return sparseVMDK(strings.Replace(descriptorWith("child.vmdk"), "parentCID=ffffffff", "parentCID=5428e012", 1), false)
		}, false},
		{"stream vmdk with a bad footer descriptor", "disk_image", "stream.vmdk", func(*testing.T) string {
			return sparseVMDK(descriptorWith("../outside/secret.bin"), true)
		}, false},
		{"esx sparse disk", "disk_image", "disk.vmdk", func(*testing.T) string { return "COWD" + string(make([]byte, 2044)) }, false},
		{"dynamic vdi", "disk_image", "disk.vdi", func(*testing.T) string { return vdiImage(1) }, true},
		{"fixed vdi", "disk_image", "disk.vdi", func(*testing.T) string { return vdiImage(2) }, true},
		{"differencing vdi", "disk_image", "disk.vdi", func(*testing.T) string { return vdiImage(4) }, false},
		{"fixed vhd", "disk_image", "disk.vhd", func(*testing.T) string { return string(append(rawDisk(), vhdFooter(2)...)) }, true},
		{"differencing vhd", "disk_image", "disk.vhd", func(*testing.T) string {
			return string(append(append(vhdFooter(4), rawDisk()...), vhdFooter(4)...))
		}, false},
		{"differencing vhd footer behind raw data", "disk_image", "disk.img", func(*testing.T) string {
			return string(append(rawDisk(), vhdFooter(4)...))
		}, false},
		{"qcow2", "disk_image", "cloud.img", func(*testing.T) string { return qcowImage(0) }, true},
		{"qcow2 with a backing file", "disk_image", "cloud.img", func(*testing.T) string { return qcowImage(512) }, false},
		{"raw disk", "disk_image", "disk.raw", func(*testing.T) string { return string(rawDisk()) }, true},
		{"ova", "appliance", "box.ova", func(t *testing.T) string { return ova(t, ovf("disk1.vmdk")) }, true},
		{"ovf", "appliance", "box.ovf", func(*testing.T) string { return ovf("disk1.vmdk") }, false},
		{"ovf renamed to ova", "appliance", "box.ova", func(*testing.T) string { return ovf("disk1.vmdk") }, false},
		{"ova referencing a parent folder", "appliance", "box.ova", func(t *testing.T) string { return ova(t, ovf("../outside/secret.bin")) }, false},
		{"ova referencing an absolute path", "appliance", "box.ova", func(t *testing.T) string { return ova(t, ovf("C:/outside/secret.bin")) }, false},
		{"ova referencing a url", "appliance", "box.ova", func(t *testing.T) string { return ova(t, ovf("http://example.invalid/disk.vmdk")) }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			allowed := t.TempDir()
			writeFile(t, filepath.Join(allowed, "boot.raw"), string(rawDisk()))
			path := writeFile(t, filepath.Join(allowed, "files", c.file), c.content(t))
			spec := vm.Spec{Name: "a"}
			switch c.field {
			case "iso":
				spec.ISO = path
			case "disk_image":
				spec.DiskImage = path
			case "appliance":
				spec.Appliance = path
			}
			for _, confined := range []bool{false, true} {
				e := newEnv(t)
				if confined {
					e.m.cfg.HostDirs = []string{allowed}
				}
				_, err := e.m.Create(t.Context(), spec)
				if !confined || c.allowed {
					if err != nil {
						t.Fatalf("confined=%v: %v", confined, err)
					}
					continue
				}
				wantErr(t, err, vm.ErrForbidden)
				if calls := callsOf(e.vbox, "create"); len(calls) > 0 {
					t.Fatalf("created: %v", calls)
				}
			}
		})
	}
}
