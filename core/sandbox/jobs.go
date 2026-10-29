package sandbox

import (
	"context"
	"time"
)

// ListedJob is a running background job of the JobList.
type ListedJob struct {
	ThreadID, JobID string
	// Started is when the job started (its keep-alive budget counts from it).
	Started time.Time
}

// JobList persists the running background jobs so that a restarted runtime
// process resumes their keep-alives (ResumeJobs), and leases each job to one
// process at a time.
type JobList interface {
	// Add records a running job and returns its start: the first one recorded.
	Add(ctx context.Context, threadID, jobID string) (time.Time, error)
	Remove(ctx context.Context, threadID, jobID string) error
	List(ctx context.Context) ([]ListedJob, error)
	// Claim takes the job's lease for owner, or renews it when owner holds it
	// (false: another process holds it).
	Claim(ctx context.Context, threadID, jobID, owner string, ttl time.Duration) (bool, error)
	// Release drops owner's lease.
	Release(ctx context.Context, threadID, jobID, owner string) error
}
