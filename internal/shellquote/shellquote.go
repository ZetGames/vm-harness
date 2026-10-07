package shellquote

import (
	"sort"
	"strings"
)

func POSIX(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, unsafePOSIX) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func unsafePOSIX(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	return !strings.ContainsRune("@%+=:,./_-", r)
}

func POSIXJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = POSIX(a)
	}
	return strings.Join(q, " ")
}

func POSIXScript(argv []string, script string, env map[string]string, workdir string) string {
	var b strings.Builder
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("export " + k + "=" + POSIX(env[k]) + "; ")
	}
	if workdir != "" {
		b.WriteString("cd " + POSIX(workdir) + " || exit 1\n")
	}
	if script != "" {
		b.WriteString(script)
	} else {
		b.WriteString(POSIXJoin(argv))
	}
	return b.String()
}

func Windows(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\v\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range s {
		switch r {
		case '\\':
			slashes++
			continue
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes*2+1))
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
		}
		slashes = 0
		b.WriteRune(r)
	}
	b.WriteString(strings.Repeat(`\`, slashes*2))
	b.WriteByte('"')
	return b.String()
}

func WindowsJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = Windows(a)
	}
	return strings.Join(q, " ")
}
