package tui

import (
	"context"

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
	opts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithContext(ctx)}
	if o.Config.Mouse {
		opts = append(opts, tea.WithMouseCellMotion())
	}
	_, err := tea.NewProgram(New(b, o), opts...).Run()
	return err
}
