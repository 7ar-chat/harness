package tui

import (
	"fmt"
	"strings"
)

// isShellTool says whether a tool runs a shell command (its header reads "$ command").
func isShellTool(name string) bool {
	switch name {
	case "run_command", "bash", "execute_command", "shell":
		return true
	}
	return false
}

// collapsedSummary is the one-line result of a finished tool call whose output
// is long and rarely worth reading in full (the file read, the matches of a
// search); ctrl+o shows the content. "" when the generic result applies.
func collapsedSummary(t ToolView) string {
	res := strings.TrimSpace(t.Result)
	n := 0
	if res != "" {
		n = strings.Count(res, "\n") + 1
	}
	switch t.Name {
	case "read_file":
		if n > collapsedLines {
			return fmt.Sprintf("%d lines (ctrl+o expands)", n)
		}
	case "search_files":
		if n > collapsedLines {
			return fmt.Sprintf("%d results (ctrl+o expands)", n)
		}
	case "list_files":
		if n > collapsedLines {
			return fmt.Sprintf("%d entries (ctrl+o expands)", n)
		}
	case "write_file":
		if c := argString(t.Args, "content"); c != "" {
			return fmt.Sprintf("wrote %d lines", strings.Count(c, "\n")+1)
		}
	}
	return ""
}
