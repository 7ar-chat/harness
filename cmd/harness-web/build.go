package main

import (
	"fmt"
	"io"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/config"
)

// buildHarness is the only place this command constructs the harness; the rest of
// the command talks to the returned *harness.Harness. durable keeps threads, runs
// and event logs on disk (harness.OpenLocalStore); otherwise they live in memory.
// The returned closer releases the store.
func buildHarness(cfg *config.Config, durable bool) (*harness.Harness, io.Closer, error) {
	o := harness.Options{Config: cfg, Version: "web"}
	var closer io.Closer = nopCloser{}
	if durable {
		st, err := harness.OpenLocalStore(cfg.LocalStateDir)
		if err != nil {
			return nil, nil, fmt.Errorf("open the session store: %w", err)
		}
		o.Store, closer = st, st
	}
	h, err := harness.New(o)
	if err != nil {
		_ = closer.Close()
		return nil, nil, err
	}
	return h, closer, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
