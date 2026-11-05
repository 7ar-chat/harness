package jobhunt

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Patch changes what the agent or the person knows about a job.
type Patch struct {
	// Status is one of skipped, queued, approved, interview, rejected, offer, withdrawn. Applying and applied are set
	// only by Claim and Applied.
	Status string
	Score  *float64
	Reason *string
	Note   *string
	// UserApproved says the person themself allowed this application (they said so, or pressed approve).
	UserApproved bool
}

func (p Profile) autoApprovable(j Job) bool {
	return p.Mode == ModeAuto && j.Score >= p.MinScore && j.Score > 0
}

// Update changes a job. Approving is the guarded step: a job becomes approved only when the person allowed it, or in
// auto mode when its score reaches the profile's MinScore.
func (s *Service) Update(login, id string, pa Patch) (Job, error) {
	prof, err := s.Profile(login)
	if err != nil {
		return Job{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, jobs, err := s.load(login)
	if err != nil {
		return Job{}, err
	}
	i := indexOf(jobs, id)
	if i < 0 {
		return Job{}, ErrNotFound
	}
	j := jobs[i]
	if j.Status == StatusApplying || j.Status == StatusApplied && pa.Status == StatusApproved {
		return j, fmt.Errorf("%w: the job is %s", ErrInvalid, j.Status)
	}
	if pa.Score != nil {
		if *pa.Score < 0 || *pa.Score > 5 {
			return j, fmt.Errorf("%w: a score is between 1 and 5", ErrInvalid)
		}
		j.Score = *pa.Score
	}
	if pa.Reason != nil {
		j.Reason = clip(*pa.Reason, 500)
	}
	if pa.Note != nil {
		j.Note = clip(*pa.Note, 1000)
	}
	switch st := strings.ToLower(strings.TrimSpace(pa.Status)); st {
	case "":
	case StatusSkipped, StatusQueued, StatusInterview, StatusRejected, StatusOffer, StatusWithdrawn, StatusNew:
		j.Status = st
	case StatusApproved:
		switch {
		case pa.UserApproved:
			j.Approval = ApprovedByUser
		case prof.autoApprovable(j):
			j.Approval = ApprovedByAuto
		case prof.Mode != ModeAuto:
			return j, fmt.Errorf("%w: the search is in %q mode, so only the person can approve %q; ask them, then set userApproved", ErrInvalid, ModeQueue, j.Title)
		default:
			return j, fmt.Errorf("%w: score %.1f is below the auto-apply score %.1f; queue it for the person instead", ErrInvalid, j.Score, prof.MinScore)
		}
		if prof.Blacklisted(j.Company, j.URL) {
			return j, fmt.Errorf("%w: %s is on the blacklist", ErrInvalid, j.Company)
		}
		j.Status = StatusApproved
	case StatusApplying, StatusApplied:
		return j, fmt.Errorf("%w: use claim before applying and applied after", ErrInvalid)
	default:
		return j, fmt.Errorf("%w: unknown status %q", ErrInvalid, st)
	}
	j.Updated = s.now()
	jobs[i] = j
	return j, s.save(path, jobs)
}

// Decision is whether an application may start, with the reasons when it may not.
type Decision struct {
	Allowed bool     `json:"allowed"`
	Reasons []string `json:"reasons,omitempty"`
}

// check applies the rules to a job; the caller holds the lock.
func (s *Service) check(prof Profile, jobs []Job, j Job) Decision {
	var why []string
	if j.Status != StatusApproved {
		why = append(why, fmt.Sprintf("the job is %q, not approved", j.Status))
	}
	if prof.Blacklisted(j.Company, j.URL) {
		why = append(why, j.Company+" is on the blacklist")
	}
	if j.Status == StatusApproved && j.Approval != ApprovedByUser && !(prof.Mode == ModeAuto && j.Approval == ApprovedByAuto && j.Score >= prof.MinScore) {
		why = append(why, "the person has not approved it and the search is not allowed to apply alone to this score")
	}
	now := s.now()
	day, sameCo := 0, ""
	for _, o := range jobs {
		if o.ID == j.ID {
			continue
		}
		recent := o.Status == StatusApplying || o.Status == StatusApplied || o.Status == StatusInterview || o.Status == StatusRejected || o.Status == StatusOffer
		at := o.AppliedAt
		if o.Status == StatusApplying {
			at = o.ClaimedAt
		}
		if recent && now.Sub(at) < 24*time.Hour {
			day++
		}
		if recent && squash(o.Company) == squash(j.Company) && now.Sub(at) < SameCompanyDays*24*time.Hour {
			sameCo = o.Title
		}
	}
	if day >= prof.DailyCap {
		why = append(why, fmt.Sprintf("the daily cap of %d applications is used up", prof.DailyCap))
	}
	if sameCo != "" && j.Approval != ApprovedByUser {
		why = append(why, fmt.Sprintf("already applied to %s (%q) in the last %d days", j.Company, sameCo, SameCompanyDays))
	}
	return Decision{Allowed: len(why) == 0, Reasons: why}
}

// Check says whether the job may be applied to now, without reserving it.
func (s *Service) Check(login, id string) (Decision, error) {
	prof, err := s.Profile(login)
	if err != nil {
		return Decision{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, jobs, err := s.load(login)
	if err != nil {
		return Decision{}, err
	}
	i := indexOf(jobs, id)
	if i < 0 {
		return Decision{}, ErrNotFound
	}
	return s.check(prof, jobs, jobs[i]), nil
}

// Claim reserves the job for an application: it is refused unless Check allows it, and the reservation counts toward the
// daily cap at once. A claim nobody finishes is released after half an hour.
func (s *Service) Claim(login, id string) (Job, Decision, error) {
	prof, err := s.Profile(login)
	if err != nil {
		return Job{}, Decision{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, jobs, err := s.load(login)
	if err != nil {
		return Job{}, Decision{}, err
	}
	i := indexOf(jobs, id)
	if i < 0 {
		return Job{}, Decision{}, ErrNotFound
	}
	d := s.check(prof, jobs, jobs[i])
	if !d.Allowed {
		return jobs[i], d, nil
	}
	jobs[i].Status, jobs[i].ClaimedAt, jobs[i].Updated = StatusApplying, s.now(), s.now()
	return jobs[i], d, s.save(path, jobs)
}

// Applied records that the application was submitted (the page showed a confirmation). Only a claimed job can be.
func (s *Service) Applied(login, id, note string) (Job, error) {
	return s.finish(login, id, StatusApplied, note)
}

// Release gives a claimed job up: back to queued for the person (needs them), or failed.
func (s *Service) Release(login, id, status, note string) (Job, error) {
	if status != StatusQueued && status != StatusFailed && status != StatusSkipped {
		return Job{}, fmt.Errorf("%w: a released job becomes queued, failed or skipped", ErrInvalid)
	}
	return s.finish(login, id, status, note)
}

func (s *Service) finish(login, id, status, note string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, jobs, err := s.load(login)
	if err != nil {
		return Job{}, err
	}
	i := indexOf(jobs, id)
	if i < 0 {
		return Job{}, ErrNotFound
	}
	if jobs[i].Status != StatusApplying {
		return jobs[i], fmt.Errorf("%w: the job is %q: claim it first", ErrInvalid, jobs[i].Status)
	}
	jobs[i].Status, jobs[i].Updated = status, s.now()
	if note != "" {
		jobs[i].Note = clip(note, 1000)
	}
	if status == StatusApplied {
		jobs[i].AppliedAt = s.now()
	}
	return jobs[i], s.save(path, jobs)
}

// AddCompany puts a board in the profile's list after checking that it answers.
func (s *Service) AddCompany(ctx context.Context, login string, c Company) (Profile, int, error) {
	c, err := c.normalize()
	if err != nil {
		return Profile{}, 0, err
	}
	posts, err := s.Fetch(ctx, c.ATS, c.Slug)
	if err != nil {
		return Profile{}, 0, err
	}
	p, err := s.Profile(login)
	if err != nil {
		return p, 0, err
	}
	p.Companies = append(p.Companies, c)
	p, err = s.SetProfile(login, p)
	return p, len(posts), err
}

func indexOf(jobs []Job, id string) int {
	for i := range jobs {
		if jobs[i].ID == id {
			return i
		}
	}
	return -1
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
