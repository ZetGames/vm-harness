//go:build !windows

package vmware

func decodeCharset([]byte, string) (string, bool) { return "", false }
