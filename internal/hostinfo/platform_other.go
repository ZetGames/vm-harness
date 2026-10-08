//go:build !amd64

package hostinfo

func Detect() Platform { return BareMetal }
