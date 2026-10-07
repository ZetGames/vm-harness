//go:build !windows

package harness

import "path/filepath"

func checkPathSyntax(string) error { return nil }

func stripDevicePrefix(path string) string { return path }

func finalPath(path string) (string, error) { return filepath.EvalSymlinks(path) }
