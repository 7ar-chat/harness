package clitest_test

import osexec "os/exec"

// exec runs a host shell command and returns its output (used to look for leftover processes).
func exec(cmd string) (string, error) {
	b, err := osexec.Command("sh", "-c", cmd).Output()
	return string(b), err
}
