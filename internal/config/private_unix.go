//go:build !windows

package config

import "os"

// PrivatePath restricts app state and credential files to the current user.
func PrivatePath(path string) error {
	info, e := os.Stat(path)
	if e != nil {
		return e
	}
	mode := os.FileMode(0600)
	if info.IsDir() {
		mode = 0700
	}
	return os.Chmod(path, mode)
}
func IsPrivate(path string) bool {
	info, e := os.Lstat(path)
	return e == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0077 == 0
}
