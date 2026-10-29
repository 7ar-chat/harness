package tui

import (
	"context"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

// Run starts the interactive terminal UI on the backend and returns when the
// user quits. o.Context is replaced by ctx.
func Run(ctx context.Context, b Backend, o Options) error {
	o.Context = ctx
	if o.Config == nil {
		cfg := ParseTUIConfig(o.ConfigData)
		o.Config = &cfg
	}
	// Keep the user's terminal title: push it now, pop it on exit (xterm CSI 22/23).
	fmt.Fprint(os.Stdout, "\x1b[22;0t")
	defer fmt.Fprint(os.Stdout, "\x1b[23;0t")
	opts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithReportFocus(), tea.WithContext(ctx)}
	if o.Config.Mouse {
		opts = append(opts, tea.WithMouseCellMotion())
	}
	_, err := tea.NewProgram(New(b, o), opts...).Run()
	return err
}
