package cloudinit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fl4metf/vm-harness/cloudinit"
	"github.com/fl4metf/vm-harness/vm"
)

const parseScript = `set -e
for f in "$@"; do
	p=$(wslpath -u "$f")
	case "$p" in
	*/user-data) cloud-init schema --config-file "$p" >&2 ;;
	esac
	python3 -c 'import json, sys, yaml; print(json.dumps(yaml.safe_load(open(sys.argv[1], encoding="utf-8"))))' "$p"
done
`

func wsl(ctx context.Context, args ...string) *exec.Cmd {
	if distro := os.Getenv("VMH_WSL_DISTRO"); distro != "" {
		args = append([]string{"-d", distro}, args...)
	}
	return exec.CommandContext(ctx, "wsl.exe", args...)
}

func TestCloudInitAcceptsGeneratedFiles(t *testing.T) {
	if os.Getenv("VMH_INTEGRATION") != "1" {
		t.Skip("set VMH_INTEGRATION=1 to check the files with cloud-init in WSL (distro from VMH_WSL_DISTRO, default distro otherwise)")
	}
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		t.Skip("wsl.exe is not available")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := wsl(ctx, "--exec", "cloud-init", "--version").Run(); err != nil {
		t.Skipf("cloud-init in WSL is not available: %v", err)
	}

	dir := t.TempDir()
	var paths []string
	var want []any
	add := func(name string, data []byte, parsed any) {
		path := filepath.Join(dir, strconv.Itoa(len(paths)), name)
		if err := os.Mkdir(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
		want = append(want, parsed)
	}
	for _, ci := range []vm.CloudInit{
		{SSHAuthorizedKeys: []string{testKey}},
		{Password: "pw"},
		fullConfig,
		{Hostname: "odd", RunCmd: []string{"printf 'a\x7fb\u0085c\u0090d'", "echo \"quoted\" 'single' \\ back\tslash"}},
	} {
		data, err := cloudinit.UserData(ci)
		if err != nil {
			t.Fatal(err)
		}
		var parsed any
		if err := json.Unmarshal(body(t, data), &parsed); err != nil {
			t.Fatal(err)
		}
		add("user-data", data, parsed)
	}
	add("meta-data", cloudinit.MetaData("123", "true"), map[string]any{"instance-id": "123", "local-hostname": "true"})

	cmd := wsl(ctx, append([]string{"--exec", "sh", "-c", parseScript, "sh"}, paths...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("cloud-init rejected a generated file: %v\n%s", err, stderr.Bytes())
	}
	if strings.Contains(strings.ToLower(stderr.String()), "deprecat") {
		t.Errorf("cloud-init reports deprecated keys:\n%s", stderr.Bytes())
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(want) {
		t.Fatalf("got %d parsed documents, want %d:\n%s", len(lines), len(want), out)
	}
	for i, line := range lines {
		var got any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("%s: %v", paths[i], err)
		}
		if !reflect.DeepEqual(got, want[i]) {
			t.Errorf("PyYAML reads %s as\n%v\nwant\n%v", paths[i], got, want[i])
		}
	}
}
