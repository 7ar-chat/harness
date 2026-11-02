//go:build !unix

package main

import (
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox"
)

func teamLocalBackend(*config.Config) (sandbox.Backend, error) { return nil, nil }
