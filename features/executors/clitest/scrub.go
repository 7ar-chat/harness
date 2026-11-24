package clitest

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	reUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	rePort = regexp.MustCompile(`127\.0\.0\.1:\d+`)
	reTS   = regexp.MustCompile(`20\d\d-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z`)
	reRand = regexp.MustCompile(`(cf-ray: |request id: )[\w-]+`)
)

// Scrub makes the raw output of a CLI run stable for a fixture: the temporary directories, the ports, the ids and
// the timestamps are replaced by placeholders.
func (s *Sandbox) Scrub(out string) string {
	root := strings.TrimPrefix(s.Root, "/private")
	home := strings.TrimPrefix(s.Home(), "/private")
	// The home directory sits next to the root, so it is replaced first.
	for _, p := range []string{"/private" + home, home} {
		out = strings.ReplaceAll(out, p, "/home/user")
	}
	for _, p := range []string{"/private" + root, root} {
		out = strings.ReplaceAll(out, p, "/workspace")
	}
	ids := map[string]string{}
	out = reUUID.ReplaceAllStringFunc(out, func(u string) string {
		if _, ok := ids[u]; !ok {
			ids[u] = fmt.Sprintf("00000000-0000-0000-0000-%012d", len(ids)+1)
		}
		return ids[u]
	})
	out = rePort.ReplaceAllString(out, "127.0.0.1:PORT")
	out = reTS.ReplaceAllString(out, "2026-01-01T00:00:00.000Z")
	return reRand.ReplaceAllString(out, "${1}ID")
}

// CLIOutput returns the raw stdout, stderr and exit code of the last run of the CLI itself (the command the runner
// launched, not its probes).
func (s *Sandbox) CLIOutput(binary string) (stdout, stderr string, exit int) {
	caps := s.Captures()
	for i := len(caps) - 1; i >= 0; i-- {
		c := caps[i].Command
		if strings.Contains(c, binary+" ") && !strings.HasPrefix(c, "command -v") {
			return caps[i].Stdout, caps[i].Stderr, caps[i].ExitCode
		}
	}
	return "", "", -1
}
