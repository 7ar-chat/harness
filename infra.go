package harness

import (
	"github.com/7ar-chat/harness/coding/sandboxtools"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/engine/eventlog"
	"github.com/7ar-chat/harness/engine/queue"
	"github.com/7ar-chat/harness/engine/runtime"
	"github.com/7ar-chat/harness/features/skills"
)

// Broker is the run queue and the per-run control bus (cancel, steers). The
// in-process one is queue.NewMemory; the Redis one (redisinfra) shares it between
// processes. Any other transport (NATS, SQS...) implements the same three
// interfaces.
type Broker interface {
	queue.Queue
	queue.Bus
	runtime.RunQueue
}

// Coordinator is the small shared key/value state the processes of one
// deployment use to cooperate: sandbox creation locks and health streaks
// (sandbox.Coordinator), the sweeper and job locks and the run liveness marks
// (runtime.Locks). redisx.NewCoordinator is the Redis one; sandbox.LocalCoordinator
// keeps it in the process.
type Coordinator interface {
	sandbox.Coordinator
	runtime.Locks
}

// Infra is everything the harness shares between processes. Every field is
// optional: a missing one falls back to its in-process implementation, so a
// zero Infra is a self-contained single-process harness. A host that runs
// several processes injects implementations built on the infrastructure it
// has (package redisinfra for Redis).
type Infra struct {
	// Log is the per-run event log (default eventlog.NewMemory over the Store).
	Log eventlog.Log
	// Broker is the run queue and control bus (default queue.NewMemory).
	Broker Broker
	// Coordinator shares sandbox and sweeper locks (default: none, in-process).
	Coordinator Coordinator
	// Jobs persists the running background jobs so that a restarted process
	// resumes their keep-alives (default: in memory only).
	Jobs sandboxtools.JobList
	// Installs shares which skill revisions each sandbox holds, and the install
	// locks (default: in-process).
	Installs skills.InstallState
}

// resolveInfra applies the options' infrastructure (Options.Infra, or nothing) and fills what is missing with the
// in-process implementations.
func resolveInfra(o Options, st Store) (Infra, error) {
	var in Infra
	if o.Infra != nil {
		in = *o.Infra
	}
	if in.Broker == nil {
		in.Broker = queue.NewMemory()
	}
	if in.Log == nil {
		in.Log = eventlog.NewMemory(st)
	}
	return in, nil
}
