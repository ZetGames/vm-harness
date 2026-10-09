package iso9660

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ZetGames/vm-harness/vm"
)

const (
	userData      = "#cloud-config\nusers:\n  - name: vmh\n    sudo: \"ALL=(ALL) NOPASSWD:ALL\"\n    shell: /bin/bash\n    ssh_authorized_keys:\n      - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGq2y8f8k0bM8m3k0Yx4c8i5lQ0v9qk2nq3m4o5p6r7s vmh@host\n"
	metaData      = "instance-id: iid-vmh-0001\nlocal-hostname: vmh-0001\n"
	networkConfig = "version: 2\nethernets:\n  nics:\n    match:\n      name: \"en*\"\n    dhcp4: true\n"

	seedImageSHA256 = "2e1ad80213ea8b438b8eb69f0d01332b77016cf59baf845345b611c5d0121da2"
)

func seedFiles() []File {
	return []File{
		{Name: "user-data", Data: []byte(userData)},
		{Name: "meta-data", Data: []byte(metaData)},
		{Name: "network-config", Data: []byte(networkConfig)},
	}
}

func seedContents() map[string][]byte {
	files := map[string][]byte{}
	for _, f := range seedFiles() {
		files[f.Name] = f.Data
	}
	return files
}

func write(t *testing.T, label string, files []File) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, label, files); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type dir struct {
	label   string
	root    uint32
	entries []string
}

func entry(name string, extent uint32, data string) string {
	return fmt.Sprintf("%s@%d=%q", name, extent, data)
}

func readDirs(t *testing.T, img []byte) (dir, dir) {
	t.Helper()
	if len(img)%sectorSize != 0 {
		t.Fatalf("image of %d bytes is not sector aligned", len(img))
	}
	size := make([]byte, 8)
	putBoth32(size, uint32(len(img)/sectorSize))
	for _, n := range []uint32{primaryDescriptor, jolietDescriptor} {
		if got := sector(img, n)[80:88]; !bytes.Equal(got, size) {
			t.Errorf("descriptor %d volume space size = % x, want % x", n, got, size)
		}
	}
	primary, joliet, err := volumes(img)
	if err != nil {
		t.Fatal(err)
	}
	if joliet == nil {
		t.Fatal("image has no Joliet volume")
	}
	return readDir(t, img, primary), readDir(t, img, joliet)
}

func readDir(t *testing.T, img []byte, v *volume) dir {
	t.Helper()
	records, err := v.records(img)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) < 2 {
		t.Fatalf("root directory has %d records", len(records))
	}
	for i, r := range records[:2] {
		if r.extent != v.root.extent || r.size != v.root.size || r.flags != flagDirectory || !bytes.Equal(r.id, []byte{byte(i)}) {
			t.Errorf("root record %d is not a self or parent entry: %+v", i, r)
		}
	}
	d := dir{label: v.label, root: v.root.extent}
	for _, r := range records[2:] {
		if r.flags != 0 {
			t.Errorf("entry %q has flags %#x", r.id, r.flags)
		}
		data, err := bytesAt(img, r.extent, r.size)
		if err != nil {
			t.Fatal(err)
		}
		d.entries = append(d.entries, entry(v.text(r.id), r.extent, string(data)))
	}
	return d
}

func checkDir(t *testing.T, tree string, got, want dir) {
	t.Helper()
	if got.label != want.label || got.root != want.root || !slices.Equal(got.entries, want.entries) {
		t.Errorf("%s directory:\n got %q at %d: %q\nwant %q at %d: %q", tree, got.label, got.root, got.entries, want.label, want.root, want.entries)
	}
}

func TestSeedImageGolden(t *testing.T) {
	img := write(t, "cidata", seedFiles())
	if len(img) != 28*2048 {
		t.Fatalf("image is %d bytes, want 28 sectors", len(img))
	}
	sum := sha256.Sum256(img)
	if got := hex.EncodeToString(sum[:]); got != seedImageSHA256 {
		t.Fatalf("sha256 = %s, want %s", got, seedImageSHA256)
	}
}

func TestSeedImageLayout(t *testing.T) {
	primary, joliet := readDirs(t, write(t, "cidata", seedFiles()))
	checkDir(t, "primary", primary, dir{"CIDATA", 23, []string{
		entry("META-DATA.;1", 25, metaData),
		entry("NETWORK-CONFIG.;1", 26, networkConfig),
		entry("USER-DATA.;1", 27, userData),
	}})
	checkDir(t, "joliet", joliet, dir{"cidata", 24, []string{
		entry("meta-data", 25, metaData),
		entry("network-config", 26, networkConfig),
		entry("user-data", 27, userData),
	}})
}

func TestWriteIgnoresInputOrder(t *testing.T) {
	files := seedFiles()
	first := write(t, "cidata", files)
	slices.Reverse(files)
	if !bytes.Equal(write(t, "cidata", files), first) {
		t.Fatal("image depends on the order of the input files")
	}
}

func TestDirectoryOrder(t *testing.T) {
	var files []File
	for _, name := range []string{"c.txt", "a1", "B", "a-b", "a.b", "a"} {
		files = append(files, File{Name: name, Data: []byte(name)})
	}
	primary, joliet := readDirs(t, write(t, "test", files))
	checkDir(t, "primary", primary, dir{"TEST", 23, []string{
		entry("A.;1", 25, "a"),
		entry("A.B;1", 26, "a.b"),
		entry("A-B.;1", 27, "a-b"),
		entry("A1.;1", 28, "a1"),
		entry("B.;1", 29, "B"),
		entry("C.TXT;1", 30, "c.txt"),
	}})
	checkDir(t, "joliet", joliet, dir{"test", 24, []string{
		entry("B", 29, "B"),
		entry("a", 25, "a"),
		entry("a.b", 26, "a.b"),
		entry("a-b", 27, "a-b"),
		entry("a1", 28, "a1"),
		entry("c.txt", 30, "c.txt"),
	}})
}

func TestFileExtents(t *testing.T) {
	large := bytes.Repeat([]byte("x"), 5000)
	exact := bytes.Repeat([]byte("y"), 2048)
	img := write(t, "cidata", []File{
		{Name: "a", Data: large},
		{Name: "b", Data: exact},
		{Name: "c", Data: []byte("z")},
		{Name: "empty", Data: nil},
	})
	if len(img) != 30*sectorSize {
		t.Errorf("image has %d bytes, want 30 sectors", len(img))
	}
	_, joliet := readDirs(t, img)
	checkDir(t, "joliet", joliet, dir{"cidata", 24, []string{
		entry("a", 25, string(large)),
		entry("b", 28, string(exact)),
		entry("c", 29, "z"),
		entry("empty", 0, ""),
	}})
}

func TestWriteWithoutFiles(t *testing.T) {
	img := write(t, "cidata", nil)
	primary, joliet := readDirs(t, img)
	if len(img) != 25*sectorSize || len(primary.entries) != 0 || len(joliet.entries) != 0 {
		t.Fatalf("empty image has %d bytes and entries %q, %q", len(img), primary.entries, joliet.entries)
	}
}

func TestLongestNames(t *testing.T) {
	plain := strings.Repeat("a", 30)
	withExt := strings.Repeat("b", 26) + ".json"
	primary, joliet := readDirs(t, write(t, "cidata", []File{{Name: plain}, {Name: withExt}}))
	checkDir(t, "primary", primary, dir{"CIDATA", 23, []string{
		entry(strings.ToUpper(plain)+".;1", 0, ""),
		entry(strings.ToUpper(withExt)+";1", 0, ""),
	}})
	checkDir(t, "joliet", joliet, dir{"cidata", 24, []string{entry(plain, 0, ""), entry(withExt, 0, "")}})
}

func TestRootDirectoryCapacity(t *testing.T) {
	files := make([]File, 22)
	for i := range files {
		files[i] = File{Name: fmt.Sprintf("%030d", i), Data: []byte{byte(i)}}
	}
	primary, joliet := readDirs(t, write(t, "cidata", files[:21]))
	if len(joliet.entries) != 21 || len(primary.entries) != 21 {
		t.Fatalf("got %d joliet and %d primary entries", len(joliet.entries), len(primary.entries))
	}
	var buf bytes.Buffer
	if err := Write(&buf, "cidata", files); !errors.Is(err, vm.ErrInvalid) {
		t.Fatalf("22 files: err = %v, want ErrInvalid", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("%d bytes written before the error", buf.Len())
	}
}

func TestWriteRejects(t *testing.T) {
	file := func(names ...string) []File {
		var files []File
		for _, n := range names {
			files = append(files, File{Name: n, Data: []byte("x")})
		}
		return files
	}
	cases := []struct {
		name  string
		label string
		files []File
	}{
		{"empty label", "", file("a")},
		{"label too long", "abcdefghijklmnopq", file("a")},
		{"label with space", "ci data", file("a")},
		{"label with dot", "ci.data", file("a")},
		{"empty name", "cidata", file("")},
		{"space", "cidata", file("user data")},
		{"slash", "cidata", file("a/b")},
		{"semicolon", "cidata", file("a;1")},
		{"two dots", "cidata", file("a.b.c")},
		{"leading dot", "cidata", file(".hidden")},
		{"trailing dot", "cidata", file("data.")},
		{"non-ascii", "cidata", file("данные")},
		{"too long", "cidata", file(strings.Repeat("a", 31))},
		{"too long with extension", "cidata", file(strings.Repeat("a", 27) + ".json")},
		{"duplicate", "cidata", file("user-data", "user-data")},
		{"duplicate ignoring case", "cidata", file("user-data", "USER-DATA")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := Write(&buf, c.label, c.files)
			if !errors.Is(err, vm.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if buf.Len() != 0 {
				t.Fatalf("%d bytes written before the error", buf.Len())
			}
		})
	}
}

type failingWriter struct{ after int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.after <= 0 {
		return 0, errors.New("disk full")
	}
	w.after--
	return len(p), nil
}

func TestWriteReturnsWriterErrors(t *testing.T) {
	for after := range 3 {
		if err := Write(&failingWriter{after: after}, "cidata", seedFiles()); err == nil || err.Error() != "disk full" {
			t.Errorf("failing after %d writes: err = %v", after, err)
		}
	}
}

func checkRead(t *testing.T, img []byte, wantLabel string, want map[string][]byte) {
	t.Helper()
	label, files, err := Read(img)
	if err != nil {
		t.Fatal(err)
	}
	if label != wantLabel {
		t.Errorf("label = %q, want %q", label, wantLabel)
	}
	if !maps.EqualFunc(files, want, bytes.Equal) {
		t.Errorf("files = %q, want %q", files, want)
	}
}

func TestReadJolietTree(t *testing.T) {
	checkRead(t, write(t, "cidata", seedFiles()), "cidata", seedContents())
}

func TestReadPrimaryTreeWithoutJoliet(t *testing.T) {
	img := write(t, "cidata", seedFiles())
	clear(sector(img, jolietDescriptor)[88:91])
	checkRead(t, img, "CIDATA", seedContents())
}

func TestReadImageFromAnotherWriter(t *testing.T) {
	img, err := os.ReadFile(filepath.Join("testdata", "imapi-cidata.iso"))
	if err != nil {
		t.Fatal(err)
	}
	checkRead(t, img, "cidata", map[string][]byte{
		"user-data":      []byte("#cloud-config\n{\"users\":[{\"name\":\"vmh\"}]}\n"),
		"meta-data":      []byte("instance-id: \"vmh-src\"\nlocal-hostname: \"src\"\n"),
		"network-config": []byte("version: 2\nethernets:\n  nics:\n    match:\n      name: \"e*\"\n    dhcp4: true\n"),
	})
}

func TestReadRejectsDamagedImages(t *testing.T) {
	good := write(t, "cidata", seedFiles())
	at := func(n uint32, off int) int { return int(n)*sectorSize + off }
	firstFile := at(jolietRoot, 68)
	cases := []struct {
		name   string
		damage func([]byte) []byte
		want   error
	}{
		{"empty", func([]byte) []byte { return nil }, vm.ErrInvalid},
		{"truncated descriptors", func(b []byte) []byte { return b[:at(jolietDescriptor, 0)] }, vm.ErrInvalid},
		{"truncated file", func(b []byte) []byte { return b[:at(27, 0)] }, vm.ErrInvalid},
		{"standard identifier", func(b []byte) []byte { b[at(primaryDescriptor, 1)] = 'X'; return b }, vm.ErrInvalid},
		{"no terminator", func(b []byte) []byte { b[at(terminatorDescriptor, 0)] = 0; return b }, vm.ErrInvalid},
		{"no primary descriptor", func(b []byte) []byte { b[at(primaryDescriptor, 0)] = 3; return b }, vm.ErrInvalid},
		{"block size", func(b []byte) []byte { b[at(primaryDescriptor, 129)] = 0x10; return b }, vm.ErrUnsupported},
		{"root is a file", func(b []byte) []byte { b[at(jolietDescriptor, 156+25)] = 0; return b }, vm.ErrInvalid},
		{"record length", func(b []byte) []byte { b[firstFile] = 40; return b }, vm.ErrInvalid},
		{"file extent", func(b []byte) []byte { b[firstFile+2] = 200; return b }, vm.ErrInvalid},
		{"odd joliet name", func(b []byte) []byte { b[firstFile+32] = 17; return b }, vm.ErrInvalid},
		{"multi-extent file", func(b []byte) []byte { b[firstFile+25] = flagMultiExtent; return b }, vm.ErrUnsupported},
		{"duplicate name", func(b []byte) []byte { copy(b[firstFile+33:], ucs2("user-data")); return b }, vm.ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := Read(c.damage(bytes.Clone(good))); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}
