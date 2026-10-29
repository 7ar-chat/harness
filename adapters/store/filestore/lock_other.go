//go:build !unix

package filestore

import "os"

// lockDir is a no-op where flock is unavailable.
func lockDir(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
}
