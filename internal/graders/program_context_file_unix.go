//go:build !windows

package graders

import "os"

// mkdirPrivateTemp creates a new directory with mode 0700.
func mkdirPrivateTemp(pattern string) (string, error) {
	return os.MkdirTemp("", pattern)
}

// createPrivateFile exclusively creates a file with mode 0600.
func createPrivateFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}
