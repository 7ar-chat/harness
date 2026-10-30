package harness

import (
	"errors"

	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/adapters/redisx"
	"github.com/GhaziBenDahmane/harness/coding/sandboxtools"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/engine/eventlog"
	"github.com/GhaziBenDahmane/harness/engine/queue"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
	"github.com/GhaziBenDahmane/harness/features/skills"
)

// Broker is the run queue and the per-run control bus (cancel, steers). The
// in-process one is queue.NewMemory; redisx.NewQueue shares it between
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
// has (RedisInfra for Redis).
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

// NewRedisClient builds the Redis client of the configuration (REDIS_*,
// USE_SENTINEL): a Sentinel failover client or a standalone one.
func NewRedisClient(cfg *config.Config) redis.UniversalClient {
	return redisx.New(redisx.Options{
		Sentinel: cfg.UseSentinel, SentinelHosts: cfg.RedisSentinelHosts, MasterName: cfg.RedisMasterName,
		Host: cfg.RedisHost, Port: cfg.RedisPort, Password: cfg.RedisPassword, DB: cfg.RedisDB, PoolSize: cfg.RedisPoolSize,
	})
}

// RedisInfra builds the Infra of a deployment that shares Redis: event log
// (streams over st), queue and bus, locks, job list and skill installs.
func RedisInfra(rdb redis.UniversalClient, st Store) Infra {
	q := redisx.NewQueue(rdb)
	return Infra{
		Log:         redisx.NewEventLog(rdb, st),
		Broker:      q,
		Coordinator: redisx.NewCoordinator(rdb),
		Jobs:        redisx.NewJobs(rdb),
		Installs:    redisx.NewSkillInstalls(rdb),
	}
}

// resolveInfra applies the options' infrastructure: Infra, else Redis, else
// nothing; then fills what is missing with the in-process implementations.
func resolveInfra(o Options, st Store) (Infra, error) {
	var in Infra
	switch {
	case o.Infra != nil && o.Redis != nil:
		return in, errors.New("harness: set Options.Infra or Options.Redis, not both")
	case o.Infra != nil:
		in = *o.Infra
	case o.Redis != nil:
		in = RedisInfra(o.Redis, st)
	}
	if in.Broker == nil {
		in.Broker = queue.NewMemory()
	}
	if in.Log == nil {
		in.Log = eventlog.NewMemory(st)
	}
	return in, nil
}
