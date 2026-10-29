package tui

import (
	"context"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

// Run starts the interactive terminal UI on the backend and returns when the
// user quits.
func Run(ctx context.Context, b Backend) error { return RunWith(ctx, b, Options{}) }

// RunWith is Run with options (workspace, state directory, notifications).
func RunWith(ctx context.Context, b Backend, o Options) error {
	o.Context = ctx
	m := New(b, o)
	// Keep the user's terminal title: push it now, pop it on exit (xterm CSI 22/23).
	fmt.Fprint(os.Stdout, "\x1b[22;0t")
	defer fmt.Fprint(os.Stdout, "\x1b[23;0t")
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithReportFocus(), tea.WithContext(ctx)).Run()
	return err
}
