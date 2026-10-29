package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

// RunOptions configure Run.
type RunOptions struct {
	// ConfigData is the text of the user's tui.json (nil for none); problems in it become notices in the transcript.
	ConfigData []byte
	// StatePath is the file the theme choice is kept in (~/.har/tui-state.json); "" keeps nothing.
	StatePath string
}

// Run starts the interactive terminal UI on the backend and returns when the
// user quits.
func Run(ctx context.Context, b Backend, ro RunOptions) error {
	cfg := ParseTUIConfig(ro.ConfigData)
	m := New(b, Options{Context: ctx, Config: &cfg, StatePath: ro.StatePath})
	opts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithContext(ctx)}
	if cfg.Mouse {
		opts = append(opts, tea.WithMouseCellMotion())
	}
	_, err := tea.NewProgram(m, opts...).Run()
	return err
}
