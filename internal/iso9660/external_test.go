package iso9660

import (
	"bytes"
	"context"
	"encoding/base64"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const mountScript = `set -e
iso=$(wslpath -u "$1")
blkid -p -o export "$iso" | sed -n 's/^LABEL=/label /p'
dir=$(mktemp -d)
trap 'sudo -n umount "$dir" 2>/dev/null || true; rmdir "$dir"' EXIT
for opts in ro ro,nojoliet; do
	sudo -n mount -t iso9660 -o "loop,$opts" "$iso" "$dir"
	for f in "$dir"/*; do
		echo "$opts $(basename "$f") $(base64 -w0 "$f")"
	done
	sudo -n umount "$dir"
done
`

func writeSeedFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seed.iso")
	if err := os.WriteFile(path, write(t, "cidata", seedFiles()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSevenZipReadsJolietTree(t *testing.T) {
	sevenZip, err := exec.LookPath("7z")
	if err != nil {
		t.Skip("7z is not installed")
	}
	path := writeSeedFile(t)
	out, err := exec.Command(sevenZip, "l", "-slt", path).CombinedOutput()
	if err != nil {
		t.Fatalf("7z l: %v\n%s", err, out)
	}
	_, listing, _ := strings.Cut(string(out), "----------")
	var names []string
	for line := range strings.Lines(listing) {
		if name, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "Path = "); ok {
			names = append(names, name)
		}
	}
	if want := []string{"meta-data", "network-config", "user-data"}; !slices.Equal(names, want) {
		t.Fatalf("7z lists %q, want %q", names, want)
	}
	for _, f := range seedFiles() {
		data, err := exec.Command(sevenZip, "e", "-so", path, f.Name).Output()
		if err != nil {
			t.Fatalf("7z e %s: %v", f.Name, err)
		}
		if !bytes.Equal(data, f.Data) {
			t.Errorf("7z extracts %s as %q, want %q", f.Name, data, f.Data)
		}
	}
}

func wsl(ctx context.Context, args ...string) *exec.Cmd {
	if distro := os.Getenv("VMH_WSL_DISTRO"); distro != "" {
		args = append([]string{"-d", distro}, args...)
	}
	return exec.CommandContext(ctx, "wsl.exe", args...)
}

func TestLinuxMountsSeedImage(t *testing.T) {
	if os.Getenv("VMH_INTEGRATION") != "1" {
		t.Skip("set VMH_INTEGRATION=1 to mount the image in WSL (distro from VMH_WSL_DISTRO, default distro otherwise)")
	}
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		t.Skip("wsl.exe is not available")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := wsl(ctx, "--exec", "sudo", "-n", "true").Run(); err != nil {
		t.Skipf("WSL with passwordless sudo is not available: %v", err)
	}
	cmd := wsl(ctx, "--exec", "sh", "-c", mountScript, "sh", writeSeedFile(t))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("mount script: %v\n%s%s", err, out, stderr.Bytes())
	}

	label := ""
	mounted := map[string]map[string]string{}
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 2 && fields[0] == "label":
			label = fields[1]
		case len(fields) == 3:
			data, err := base64.StdEncoding.DecodeString(fields[2])
			if err != nil {
				t.Fatalf("%q: %v", line, err)
			}
			if mounted[fields[0]] == nil {
				mounted[fields[0]] = map[string]string{}
			}
			mounted[fields[0]][fields[1]] = string(data)
		default:
			t.Fatalf("unexpected output line %q", line)
		}
	}
	if label != "cidata" {
		t.Errorf("blkid label = %q, want cidata", label)
	}
	want := map[string]string{"meta-data": metaData, "network-config": networkConfig, "user-data": userData}
	for _, opts := range []string{"ro", "ro,nojoliet"} {
		if !maps.Equal(mounted[opts], want) {
			t.Errorf("mount -o %s shows %q, want %q", opts, mounted[opts], want)
		}
	}
}
