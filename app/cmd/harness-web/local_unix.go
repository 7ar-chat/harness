//go:build unix

package main

import (
	"github.com/7ar-chat/harness/adapters/sandbox/local"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox"
)

// teamLocalBackend is the local sandbox with a directory of its own per thread
// (no shared project directory): every session of a team gets its own
// /workspace, where its project repository is cloned.
func teamLocalBackend(cfg *config.Config) (sandbox.Backend, error) {
	return local.New(local.Options{StateDir: cfg.LocalStateDir})
}
