package clitest

import (
	"os/exec"
	"strconv"
	"strings"
)

// descendants lists the processes below pid (children, their children, ...).
func descendants(pid int) []int {
	var out []int
	queue := []int{pid}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		b, _ := exec.Command("pgrep", "-P", strconv.Itoa(p)).Output()
		for _, f := range strings.Fields(string(b)) {
			if c, err := strconv.Atoi(f); err == nil {
				out = append(out, c)
				queue = append(queue, c)
			}
		}
	}
	return out
}
