//go:build linux || darwin || freebsd || netbsd || openbsd

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// readSecret prompts on w and reads one line from the terminal f without echo.
func readSecret(f *os.File, prompt string, w io.Writer) (string, error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGet)
	if err != nil {
		return "", err
	}
	noEcho := *old
	noEcho.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, ioctlSet, &noEcho); err != nil {
		return "", err
	}
	defer func() { _ = unix.IoctlSetTermios(fd, ioctlSet, old) }()
	fmt.Fprint(w, prompt)
	line, err := bufio.NewReader(f).ReadString('\n')
	fmt.Fprintln(w)
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}
