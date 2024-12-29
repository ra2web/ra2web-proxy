//go:build windows

package utils

import "os"

func LockFile(f *os.File) error {
	return nil
}

func UnlockFile(f *os.File) error {
	return nil
}
