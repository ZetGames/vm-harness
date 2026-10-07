package vmware

import (
	"strconv"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

var codePages = map[string]uint32{
	"shift_jis":      932,
	"gbk":            936,
	"gb2312":         936,
	"gb18030":        54936,
	"big5":           950,
	"euc-kr":         949,
	"ks_c_5601-1987": 949,
	"iso-8859-1":     28591,
	"us-ascii":       20127,
}

func decodeCharset(data []byte, charset string) (string, bool) {
	cp, ok := codePage(charset)
	if !ok || len(data) == 0 {
		return "", false
	}
	n, err := windows.MultiByteToWideChar(cp, 0, &data[0], int32(len(data)), nil, 0)
	if err != nil || n == 0 {
		return "", false
	}
	buf := make([]uint16, n)
	if n, err = windows.MultiByteToWideChar(cp, 0, &data[0], int32(len(data)), &buf[0], n); err != nil {
		return "", false
	}
	return string(utf16.Decode(buf[:n])), true
}

func codePage(charset string) (uint32, bool) {
	name := strings.ToLower(charset)
	for _, prefix := range []string{"windows-", "cp", "ibm"} {
		if number, ok := strings.CutPrefix(name, prefix); ok {
			cp, err := strconv.ParseUint(number, 10, 16)
			return uint32(cp), err == nil
		}
	}
	cp, ok := codePages[name]
	return cp, ok
}
