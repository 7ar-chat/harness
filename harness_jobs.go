package harness

import "github.com/7ar-chat/harness/features/jobhunt"

// The job search (Harness.Jobs), as the HTTP product sees it.
type (
	// JobProfile is a person's job-search brief.
	JobProfile = jobhunt.Profile
	// JobCompany is a job board to watch.
	JobCompany = jobhunt.Company
	// JobItem is one posting known to a person's search.
	JobItem = jobhunt.Job
	// JobPatch changes a job.
	JobPatch = jobhunt.Patch
	// JobScanResult is what a scan found.
	JobScanResult = jobhunt.ScanResult
)

var (
	// ErrJobInvalid is a refused job-search setting or change.
	ErrJobInvalid = jobhunt.ErrInvalid
	// ErrJobNotFound means the user has no such job.
	ErrJobNotFound = jobhunt.ErrNotFound
)

// Statuses of a job.
const (
	JobQueued    = jobhunt.StatusQueued
	JobApproved  = jobhunt.StatusApproved
	JobSkipped   = jobhunt.StatusSkipped
	JobInterview = jobhunt.StatusInterview
	JobRejected  = jobhunt.StatusRejected
	JobOffer     = jobhunt.StatusOffer
	JobWithdrawn = jobhunt.StatusWithdrawn
)

// DetectJobBoard recognises the address of a Greenhouse, Lever or Ashby job board.
func DetectJobBoard(raw string) (ats, slug string, ok bool) { return jobhunt.DetectBoard(raw) }
