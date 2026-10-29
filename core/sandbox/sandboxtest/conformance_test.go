package sandboxtest_test

import (
	"testing"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
)

// The in-memory fake is the double the rest of the repository tests against:
// it must keep honouring the contract it can express (no shell, so no Exec).
func TestFakeBackendConformance(t *testing.T) {
	sandboxtest.RunConformanceWith(t, func(*testing.T) sandbox.Backend { return sandboxtest.NewBackend() },
		sandboxtest.ConformanceOptions{Skip: map[string]string{
			"Exec": "the fake has no shell: Exec is scripted with SetExec",
			"Protocol/RealPathResolvesSymlinksAndMissingTails":     "needs a shell to create symlinks (the fake has Symlink for its own tests)",
			"Protocol/WriteFileNoFollowRefusesSymlinks":            "needs a shell to create symlinks",
			"Protocol/ReadRegularFilesSkipsSymlinksDirsAndMissing": "needs a shell to create files and symlinks",
		}})
}
