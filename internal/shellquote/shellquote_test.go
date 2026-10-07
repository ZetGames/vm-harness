package shellquote

import "testing"

func TestPOSIX(t *testing.T) {
	cases := map[string]string{
		"":            "''",
		"plain":       "plain",
		"a/b-c_d.e:f": "a/b-c_d.e:f",
		"two words":   "'two words'",
		"it's":        `'it'\''s'`,
		"$HOME":       "'$HOME'",
		"a;rm -rf /":  "'a;rm -rf /'",
	}
	for in, want := range cases {
		if got := POSIX(in); got != want {
			t.Errorf("POSIX(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPOSIXScript(t *testing.T) {
	got := POSIXScript([]string{"echo", "hello world"}, "", map[string]string{"B": "2", "A": "x y"}, "/tmp/a b")
	want := "export A='x y'; export B=2; cd '/tmp/a b' || exit 1\necho 'hello world'"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if got := POSIXScript([]string{"ignored"}, "ls | wc -l", nil, ""); got != "ls | wc -l" {
		t.Fatalf("script mode: %q", got)
	}
	if got := POSIXScript(nil, "pwd; touch marker", nil, "/srv"); got != "cd /srv || exit 1\npwd; touch marker" {
		t.Fatalf("every statement of a script must run inside workdir: %q", got)
	}
}

func TestWindows(t *testing.T) {
	cases := map[string]string{
		"":                   `""`,
		"plain":              "plain",
		`C:\Program Files\x`: `"C:\Program Files\x"`,
		`say "hi"`:           `"say \"hi\""`,
		`trailing\ `:         `"trailing\ "`,
		`ends with\`:         `"ends with\\"`,
		`a\"b c`:             `"a\\\"b c"`,
	}
	for in, want := range cases {
		if got := Windows(in); got != want {
			t.Errorf("Windows(%q) = %q, want %q", in, got, want)
		}
	}
	if got := WindowsJoin([]string{"cmd.exe", "/c", "echo a b"}); got != `cmd.exe /c "echo a b"` {
		t.Errorf("WindowsJoin = %q", got)
	}
}
