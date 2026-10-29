//go:build !unix

package main

import (
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/sandbox"
)

func teamLocalBackend(*config.Config) (sandbox.Backend, error) { return nil, nil }
