//go:build unix

package main

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// redirectStderr points file descriptor 2 at path so child processes and
// slog write there, and returns a function that restores the terminal.
func redirectStderr(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	saved, err := unix.Dup(2)
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := unix.Dup2(int(f.Fd()), 2); err != nil {
		unix.Close(saved)
		f.Close()
		return nil, err
	}
	f.Close()
	return func() {
		_ = unix.Dup2(saved, 2)
		_ = unix.Close(saved)
	}, nil
}
