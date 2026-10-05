package store

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether a process with this pid exists. A process
// owned by another user still counts (EPERM), since its sessions are not ours
// to interrupt.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
