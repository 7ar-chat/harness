//go:build unix

package filestore

import (
	"fmt"
	"os"
	"syscall"
)

// lockDir takes an exclusive advisory lock so two processes never append to
// the same journal. The lock dies with the process (also on kill -9).
func lockDir(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("filestore: state directory is in use by another process: %w", err)
	}
	return f, nil
}
