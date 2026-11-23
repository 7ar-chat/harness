package harness

import (
	"errors"
	"time"

	"github.com/7ar-chat/harness/core/executors"
	execfeature "github.com/7ar-chat/harness/features/executors"
)

// External coding-agent executors (Claude Code, Codex, OpenCode, a project's own command) are a feature; the product
// reaches the vocabulary and the feature through the facade, which is the one layer allowed to import features.

// FeatureExecutors is the name of the Executors feature.
const FeatureExecutors = "executors"

// ExecutorTaskStore is the part of the factory store the executors use (budget, audit trail, thread to task).
type ExecutorTaskStore = execfeature.TaskStore

// ExecutorsOptions configures the Executors feature.
type ExecutorsOptions struct {
	// Resolver finds the executors in reach of a thread (nil: the builtin ones).
	Resolver executors.Resolver
	// Credentials gives the credentials a user sealed for an executor (nil: none).
	Credentials executors.CredentialSource
	// Store links a thread to its task, policy and budget and records the audit events (nil: none).
	Store ExecutorTaskStore
	// AllowInstall lets a run install a missing CLI with the executor's install command.
	AllowInstall bool
	// Timeout is the default timeout of a run (0: 30 minutes).
	Timeout time.Duration
}

// Executors offers the run_executor tool and lets roles that name an executor run on it (the Factory feature's
// delegate tool finds it: install Executors before Factory). It needs a sandbox.
func Executors(o ExecutorsOptions) Feature {
	return builtin{FeatureExecutors, func(b *Builder) error {
		if err := b.RequireSandbox(); err != nil {
			return errors.New("executors: " + err.Error())
		}
		p := execfeature.New(execfeature.Options{Sandboxes: b.Sandboxes, Resolver: o.Resolver, Credentials: o.Credentials,
			Store: o.Store, AllowInstall: o.AllowInstall, Timeout: o.Timeout})
		b.AddTools(p)
		return Provide[executors.Runner](b, p)
	}}
}
