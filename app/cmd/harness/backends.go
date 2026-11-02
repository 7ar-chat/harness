package main

// The sandbox backends this command can run: SANDBOX_BACKEND=<name> needs the backend's package imported to register itself.
import (
	_ "github.com/7ar-chat/harness/adapters/sandbox/daytona"
	_ "github.com/7ar-chat/harness/adapters/sandbox/e2b"
	_ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox"
)
