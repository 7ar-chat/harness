//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package main

import (
	"errors"
	"io"
	"os"
)

func readSecret(*os.File, string, io.Writer) (string, error) {
	return "", errors.New("hidden input is not supported on this platform: use -key-stdin")
}
