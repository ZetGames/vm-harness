package vmware

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/fl4metf/vm-harness/vm"
)

type vmxFile struct {
	lines []vmxLine
	crlf  bool
}

type vmxLine struct {
	text  string
	key   string
	value string
}

func readVMX(path string) (*vmxFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("vm %s: %w", path, vm.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return parseVMX(data), nil
}

func parseVMX(data []byte) *vmxFile {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	text := decodeText(data)
	f := &vmxFile{crlf: strings.Contains(text, "\r\n")}
	text = strings.TrimSuffix(strings.TrimSuffix(text, "\n"), "\r")
	if text == "" {
		return f
	}
	for _, line := range strings.Split(text, "\n") {
		f.lines = append(f.lines, parseLine(strings.TrimSuffix(line, "\r")))
	}
	return f
}

func decodeText(data []byte) string {
	charset := declaredCharset(data)
	if isUTF8(charset) || isASCII(data) {
		return string(data)
	}
	if text, ok := decodeCharset(data, charset); ok {
		return text
	}
	return string(data)
}

func declaredCharset(data []byte) string {
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := parseLine(strings.TrimSuffix(string(raw), "\r"))
		if strings.EqualFold(line.key, ".encoding") {
			return line.value
		}
	}
	return ""
}

func isUTF8(charset string) bool {
	return charset == "" || strings.EqualFold(charset, "UTF-8") || strings.EqualFold(charset, "utf8")
}

func isASCII(data []byte) bool {
	for _, c := range data {
		if c >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func parseLine(line string) vmxLine {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return vmxLine{text: line}
	}
	key, rest, ok := strings.Cut(trimmed, "=")
	key = strings.TrimSpace(key)
	if !ok || key == "" || strings.ContainsAny(key, " \t#\"") {
		return vmxLine{text: line}
	}
	value, ok := parseValue(strings.TrimSpace(rest))
	if !ok {
		return vmxLine{text: line}
	}
	return vmxLine{text: line, key: key, value: value}
}

func parseValue(s string) (string, bool) {
	if quoted, ok := strings.CutPrefix(s, `"`); ok {
		end := strings.IndexByte(quoted, '"')
		if end < 0 {
			return "", false
		}
		tail := strings.TrimSpace(quoted[end+1:])
		if tail != "" && !strings.HasPrefix(tail, "#") {
			return "", false
		}
		return unescapeValue(quoted[:end]), true
	}
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if strings.Contains(s, `"`) {
		return "", false
	}
	return unescapeValue(s), true
}

func escapeValue(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '|' || c == 0x7f || (c < 0x20 && c != '\t') {
			fmt.Fprintf(&b, "|%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func unescapeValue(s string) string {
	if !strings.Contains(s, "|") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '|' && i+3 <= len(s) {
			if decoded, err := hex.DecodeString(s[i+1 : i+3]); err == nil {
				b.Write(decoded)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func (f *vmxFile) lookup(key string) (string, bool) {
	for i := len(f.lines) - 1; i >= 0; i-- {
		if f.lines[i].key != "" && strings.EqualFold(f.lines[i].key, key) {
			return f.lines[i].value, true
		}
	}
	return "", false
}

func (f *vmxFile) get(key string) string {
	value, _ := f.lookup(key)
	return value
}

func (f *vmxFile) set(key, value string) {
	for i := len(f.lines) - 1; i >= 0; i-- {
		line := &f.lines[i]
		if line.key != "" && strings.EqualFold(line.key, key) {
			if line.value != value {
				line.value = value
				line.text = ""
			}
			return
		}
	}
	f.lines = append(f.lines, vmxLine{key: key, value: value})
}

func (f *vmxFile) remove(key string) {
	f.removeFunc(func(k, _ string) bool { return strings.EqualFold(k, key) })
}

func (f *vmxFile) removePrefix(prefix string) {
	f.removeFunc(func(k, _ string) bool { return hasPrefixFold(k, prefix) })
}

func (f *vmxFile) removeFunc(match func(key, value string) bool) {
	kept := f.lines[:0]
	for _, line := range f.lines {
		if line.key == "" || !match(line.key, line.value) {
			kept = append(kept, line)
		}
	}
	f.lines = kept
}

func (f *vmxFile) encode() []byte {
	newline := "\n"
	if f.crlf {
		newline = "\r\n"
	}
	var b strings.Builder
	for _, line := range f.lines {
		switch {
		case line.key == "" || line.text != "":
			b.WriteString(line.text)
		default:
			b.WriteString(line.key + ` = "` + escapeValue(line.value) + `"`)
		}
		b.WriteString(newline)
	}
	return []byte(b.String())
}

func (f *vmxFile) write(path string) error {
	data := f.encode()
	if !isUTF8(f.get(".encoding")) && !isASCII(data) {
		f.set(".encoding", "UTF-8")
		data = f.encode()
	}
	return writeAtomic(path, data)
}

func writeAtomic(path string, data []byte) error {
	mode := fs.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vmh-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func isTrue(s string) bool {
	return strings.EqualFold(s, "TRUE")
}
