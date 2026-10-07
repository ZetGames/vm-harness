//go:build !windows && (!unix || aix)

package flock

import "os"

func tryLock(*os.File) (bool, error) { return true, nil }

func unlock(*os.File) error { return nil }
