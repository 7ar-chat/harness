package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/7ar-chat/harness/app/factorycheck"
	"github.com/7ar-chat/harness/app/factoryenv"
)

// factoryCheck runs the read-only setup check of the factory (-factory-check owner/name) and returns the exit code:
// 0 when nothing failed (warnings do not fail), 1 on a failed check, 2 on a bad argument or an unusable App key.
func factoryCheck(ctx context.Context, repo string, getenv func(string) string, stdout, stderr io.Writer) int {
	if o, n, ok := strings.Cut(repo, "/"); !ok || o == "" || n == "" || strings.Contains(n, "/") {
		return fail(stderr, fmt.Errorf("-factory-check wants owner/name, got %q", repo))
	}
	// A key that does not parse is a finding of the check, not a reason to refuse to run it.
	gh, ghErr := factoryenv.GitHubFromEnv(getenv)
	rep := factorycheck.Run(ctx, factorycheck.Options{Getenv: getenv, App: gh, Repo: repo})
	if ghErr != nil {
		fmt.Fprintln(stderr, "harness-web: the GitHub App is not usable:", ghErr)
	}
	rep.Write(stdout)
	if rep.Failed() || ghErr != nil {
		return 1
	}
	return 0
}
