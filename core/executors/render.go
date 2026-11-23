package executors

import (
	"fmt"
	"strings"
	"time"
)

const maxChanged = 60

// Render is the report the model reads.
func Render(r *Result) string {
	var b strings.Builder
	head := fmt.Sprintf("executor %s finished: exit code %d, level %s, %s", r.Executor, r.ExitCode, r.Level, r.Duration.Round(time.Second))
	switch {
	case r.TimedOut:
		head = fmt.Sprintf("EXECUTOR FAILED: %s was killed after its timeout (%s). Its work so far is in the working tree; check it", r.Executor, r.Duration.Round(time.Second))
	case r.Failed():
		head = "EXECUTOR FAILED: " + head
	}
	b.WriteString(head + "\n")
	if r.Tokens > 0 || r.USD > 0 {
		fmt.Fprintf(&b, "usage: %d tokens, $%.4f (as the CLI reported)\n", r.Tokens, r.USD)
	}
	b.WriteString("--- summary ---\n")
	switch {
	case r.Summary != "":
		b.WriteString(r.Summary + "\n")
	case r.Output != "":
		b.WriteString("(no final message; the end of its output follows)\n" + r.Output + "\n")
	default:
		b.WriteString("(the CLI printed nothing)\n")
	}
	if len(r.Changed) > 0 {
		shown := r.Changed
		more := 0
		if len(shown) > maxChanged {
			more, shown = len(shown)-maxChanged, shown[:maxChanged]
		}
		b.WriteString("--- changed files ---\n" + strings.Join(shown, "\n") + "\n")
		if more > 0 {
			fmt.Fprintf(&b, "... and %d more\n", more)
		}
	} else {
		b.WriteString("--- changed files ---\nnone\n")
	}
	if r.DiffStat != "" {
		b.WriteString("--- diff stat ---\n" + r.DiffStat + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
