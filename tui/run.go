package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

// Run starts the interactive terminal UI on the backend and returns when the
// user quits.
func Run(ctx context.Context, b Backend) error {
	m := New(b, Options{Context: ctx})
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx)).Run()
	return err
}
