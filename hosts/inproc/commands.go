package inproc

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"

	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/features/commands"
	"github.com/7ar-chat/harness/hosts/client"
)

// WithProjectDir sets the directory Commands looks for the project's
// .har/commands from (the nearest .har up to the git root; default: the
// working directory of the process). The user's commands come from the user
// directory (HAR_HOME, nothing with HARNESS_NO_USER_DIR).
func WithProjectDir(dir string) Option { return func(c *impl) { c.projectDir = dir } }

// loadCommands reads the commands from disk on every call, so a file edited
// while a front end runs is picked up.
func (c *impl) loadCommands(ctx context.Context) *commands.Set {
	o := commands.Options{Warn: func(msg string) { slog.Warn("client: commands: " + msg) }}
	start := c.projectDir
	if start == "" {
		start = "."
	}
	if dir, _, found := config.FindProjectDir(start); found {
		o.ProjectDir = filepath.Join(dir, commands.Folder)
	}
	if u := config.UserDir(); u != "" {
		o.UserDir = filepath.Join(u, commands.Folder)
	}
	if c.h.Skills != nil {
		for _, s := range c.h.Skills.List(ctx) {
			o.Skills = append(o.Skills, commands.Skill{Name: s.Name, Description: s.Description})
		}
	}
	return commands.Load(o)
}

func (c *impl) Commands(ctx context.Context) ([]client.Command, error) {
	list := c.loadCommands(ctx).List()
	out := make([]client.Command, 0, len(list))
	for _, cmd := range list {
		out = append(out, client.Command{Name: cmd.Name, Description: cmd.Description, Source: cmd.Source, ArgHint: cmd.ArgHint})
	}
	return out, nil
}

func (c *impl) ExpandCommand(ctx context.Context, name, args string) (string, error) {
	text, err := c.loadCommands(ctx).Expand(name, args)
	if errors.Is(err, commands.ErrNotFound) {
		return "", client.Errorf(client.ErrNotFound, "no command named %q.", name)
	}
	return text, err
}
