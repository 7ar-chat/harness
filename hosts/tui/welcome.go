package tui

import (
	"os"
	"strings"
)

// tips rotate in the welcome screen, one per launch.
var tips = []string{
	"ctrl+o expands the tool calls of the transcript",
	"/model switches the model; /sessions resumes an earlier conversation",
	"alt+up and alt+down select a message, alt+y copies it",
	"/stash parks the half-written message, /stash pop brings it back",
	"/editor composes a long message in $EDITOR",
	"/thinking shows the model's reasoning, /timestamps the time of each message",
	"ctrl+b opens the side panel with the plan and the files the agent changed",
	"paste or drop a png/jpg path to attach an image; /image <path> does too",
	"/export writes the conversation as Markdown",
	"/notify rings the terminal when a run needs you and the window is in the background",
}

// welcome is the screen of an empty conversation: the name, the model, the
// directory, three hints and a tip.
func (m *Model) welcome() string {
	var b strings.Builder
	b.WriteString(styleUser.Render("harness") + styleDim.Render("  a coding agent in your terminal") + "\n\n")
	b.WriteString(styleDim.Render("  model  ") + m.be.Model() + "\n")
	if dir := m.inp.workspace; dir != "" {
		b.WriteString(styleDim.Render("  cwd    ") + tildePath(dir) + "\n")
	}
	b.WriteString("\n")
	b.WriteString(styleDim.Render("  / ") + "for commands   " + styleDim.Render("@ ") + "for files   " + styleDim.Render("! ") + "for the shell\n")
	b.WriteString("\n" + styleDim.Render("  tip: "+tips[m.inp.tipIdx%len(tips)]))
	return b.String()
}

// tildePath shortens the home directory prefix of p.
func tildePath(p string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" && strings.HasPrefix(p, h) {
		return "~" + strings.TrimPrefix(p, h)
	}
	return p
}
