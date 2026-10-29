package tui

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Limits of the "!command" escape.
const (
	shellTimeout   = 30 * time.Second
	maxShellOutput = 20 << 10
)

// shellDoneMsg is the result of a "!command".
type shellDoneMsg struct {
	cmd string
	out string
	err error
}

// preSubmit runs before the slash commands on Enter: it records the message in
// the input history and handles the "!command" shell escape. The escape runs
// the command on the user's machine, as the user, in the workspace, exactly as
// if they had typed it in their shell; it never reaches the model's tools. Its
// output is shown in the transcript and sent as context with the next message.
func (m *Model) preSubmit(text string) (tea.Cmd, bool) {
	if m.pending != nil {
		return nil, false
	}
	m.inp.hist.add(text, m.inp.stateDir)
	m.inp.sel = -1
	if !strings.HasPrefix(text, "!") {
		return nil, false
	}
	cmd := strings.TrimSpace(text[1:])
	m.in.Reset()
	switch {
	case m.inp.workspace == "":
		m.add(block{kind: blockNotice, text: "! runs commands in a local workspace; this session has none"})
		return nil, true
	case cmd == "":
		return nil, true
	}
	m.add(block{kind: blockNotice, text: "$ " + cmd})
	root, ctx := m.inp.workspace, m.ctx
	return func() tea.Msg {
		out, err := runShell(ctx, root, cmd)
		return shellDoneMsg{cmd, out, err}
	}, true
}

// runShell runs cmd with the platform shell in dir; stdout and stderr are
// combined and capped to maxShellOutput (head and tail kept).
func runShell(ctx context.Context, dir, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, shellTimeout)
	defer cancel()
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.CommandContext(ctx, "cmd", "/C", cmd)
	} else {
		c = exec.CommandContext(ctx, "sh", "-c", cmd)
	}
	c.Dir = dir
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	c.WaitDelay = time.Second
	err := c.Run()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timed out after %s", shellTimeout)
	}
	return capOutput(buf.String(), maxShellOutput), err
}

// capOutput keeps the head and the tail of s when it is longer than n bytes.
func capOutput(s string, n int) string {
	if len(s) <= n {
		return s
	}
	h := n / 2
	return strings.ToValidUTF8(s[:h], "") + fmt.Sprintf("\n… %d bytes omitted …\n", len(s)-n) + strings.ToValidUTF8(s[len(s)-h:], "")
}

func (m *Model) shellDone(r shellDoneMsg) {
	out := strings.TrimRight(r.out, "\n")
	show, ctx := out, "I ran `"+r.cmd+"` and got:\n```\n"+out+"\n```"
	if r.err != nil {
		show += "\n[" + r.err.Error() + "]"
		ctx = "I ran `" + r.cmd + "` and it failed (" + r.err.Error() + "); output:\n```\n" + out + "\n```"
	}
	if strings.TrimSpace(show) == "" {
		show = "(no output)"
	}
	ls := strings.Split(show, "\n")
	if len(ls) > 30 {
		show = strings.Join(ls[:30], "\n") + fmt.Sprintf("\n… +%d lines", len(ls)-30)
	}
	m.add(block{kind: blockNotice, text: show})
	m.inp.shellCtx = append(m.inp.shellCtx, ctx)
}
