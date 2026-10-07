package vmware

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVMXRoundTripKeepsVMwareFiles(t *testing.T) {
	for _, name := range []string{"vmware-written.vmx", "ovftool-imported.vmx"} {
		data, err := os.ReadFile(filepath.Join("testdata", "vmx", name))
		if err != nil {
			t.Fatal(err)
		}
		if got := parseVMX(data).encode(); string(got) != string(data) {
			t.Errorf("%s changed on rewrite:\n%s", name, got)
		}
	}
}

func TestVMXRoundTripKeepsOddLines(t *testing.T) {
	src := strings.Join([]string{
		"#!/usr/bin/vmware",
		`.encoding = "UTF-8"`,
		"",
		"# a comment = with equals",
		`displayName   =   "web"   # trailing comment`,
		`bare = TRUE`,
		`hash = "a # b"`,
		`not a key line`,
		`broken = "unterminated`,
		`junk = "x" y`,
		`empty =`,
		"",
	}, "\n")
	f := parseVMX([]byte(src))
	if got := string(f.encode()); got != src {
		t.Fatalf("rewrite changed the file:\n%s", got)
	}
	if got := f.get("displayname"); got != "web" {
		t.Errorf("displayName = %q", got)
	}
	if got := f.get("bare"); got != "TRUE" {
		t.Errorf("bare = %q", got)
	}
	if got := f.get("hash"); got != "a # b" {
		t.Errorf("hash = %q", got)
	}
	if value, ok := f.lookup("empty"); !ok || value != "" {
		t.Errorf("empty = %q, %v", value, ok)
	}
	for _, key := range []string{"broken", "junk", "not"} {
		if _, ok := f.lookup(key); ok {
			t.Errorf("%s should not parse as an entry", key)
		}
	}
	f.set("displayName", "api")
	want := strings.Replace(src, `displayName   =   "web"   # trailing comment`, `displayName = "api"`, 1)
	if got := string(f.encode()); got != want {
		t.Fatalf("set rewrote more than one line:\n%s", got)
	}
}

func TestVMXKeysAreCaseInsensitive(t *testing.T) {
	f := parseVMX([]byte("displayname = \"app\"\nGuestOS = \"other-64\"\nguestos = \"ubuntu-64\"\n"))
	if got := f.get("DisplayName"); got != "app" {
		t.Errorf("DisplayName = %q", got)
	}
	if got := f.get("guestOS"); got != "ubuntu-64" {
		t.Errorf("the last duplicate should win, got %q", got)
	}
	f.set("DISPLAYNAME", "web")
	f.set("memsize", "1024")
	f.remove("GUESTOS")
	want := "displayname = \"web\"\nmemsize = \"1024\"\n"
	if got := string(f.encode()); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestVMXEscaping(t *testing.T) {
	cases := []struct{ raw, escaped string }{
		{`a"b|c`, `a|22b|7Cc`},
		{`C:\vms\web\seed.iso`, `C:\vms\web\seed.iso`},
		{"line1\nline2\r", "line1|0Aline2|0D"},
		{"tab\there", "tab\there"},
		{"del\x7f", "del|7F"},
		{"привет #1", "привет #1"},
	}
	for _, c := range cases {
		if got := escapeValue(c.raw); got != c.escaped {
			t.Errorf("escape(%q) = %q, want %q", c.raw, got, c.escaped)
		}
		if got := unescapeValue(c.escaped); got != c.raw {
			t.Errorf("unescape(%q) = %q, want %q", c.escaped, got, c.raw)
		}
	}
	for in, want := range map[string]string{"a|7cb": "a|b", "|zz": "|zz", "end|2": "end|2", "|": "|"} {
		if got := unescapeValue(in); got != want {
			t.Errorf("unescape(%q) = %q, want %q", in, got, want)
		}
	}
	f := parseVMX(nil)
	f.set("vmh.label.team", `a"b|c`)
	if got := string(f.encode()); got != "vmh.label.team = \"a|22b|7Cc\"\n" {
		t.Fatalf("written as %q", got)
	}
	if got := parseVMX(f.encode()).get("vmh.label.team"); got != `a"b|c` {
		t.Fatalf("read back %q", got)
	}
}

func TestVMXWriteSwitchesLegacyCharsetToUTF8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.vmsd")
	legacy := ".encoding = \"windows-1251\"\r\nsnapshot0.displayName = \"s1\"\r\n"
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := readVMX(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.write(path); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != legacy {
		t.Fatalf("an ascii file must keep its charset, got:\n%s", got)
	}
	v.set("snapshot0.description", "перед обновлением")
	if err := v.write(path); err != nil {
		t.Fatal(err)
	}
	want := ".encoding = \"UTF-8\"\r\nsnapshot0.displayName = \"s1\"\r\nsnapshot0.description = \"перед обновлением\"\r\n"
	if got := readFile(t, path); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := parseVMX([]byte(want)).get("snapshot0.description"); got != "перед обновлением" {
		t.Fatalf("read back %q", got)
	}
}

func TestVMXRemovePrefixAndBOM(t *testing.T) {
	f := parseVMX([]byte("\xef\xbb\xbfethernet0.present = \"TRUE\"\r\nEthernet1.present = \"TRUE\"\r\nmemsize = \"64\"\r\n"))
	f.removePrefix("ethernet")
	if got := string(f.encode()); got != "memsize = \"64\"\r\n" {
		t.Fatalf("got %q", got)
	}
}

func TestWriteAtomicReplacesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.vmx")
	if err := os.WriteFile(path, []byte("memsize = \"64\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := readVMX(path)
	if err != nil {
		t.Fatal(err)
	}
	v.set("memsize", "128")
	if err := v.write(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "memsize = \"128\"\n" {
		t.Fatalf("file = %q", data)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}
