package product

import (
	"errors"
	"net/http"
	"strings"

	"github.com/7ar-chat/harness"
)

// The job search (docs/jobs.md): the person's profile, the jobs the search found, and the decisions only they take.
//
//	GET    /api/me/jobs?status=queued     → {profile, counts, jobs}   (default status: everything but closed and skipped)
//	PUT    /api/me/jobs/profile {profile} → profile
//	POST   /api/me/jobs/boards {url|ats,slug,name} → {profile, openJobs}
//	POST   /api/me/jobs/scan              → {boards, fetched, matched, added, closed, errors}
//	POST   /api/me/jobs/thread            → {threadId}   (the job-search conversation, created on first use: the agent runs in job mode there)
//	PATCH  /api/me/jobs/{id} {status,note} → job    (approve, skip, or report an outcome: the person's word)
//
// Without HARNESS_JOBS_DIR the harness has no job search and these answer 404.

func (s *server) routeJobs() {
	s.Route("GET /me/jobs", s.listJobs)
	s.Route("PUT /me/jobs/profile", s.putJobProfile)
	s.Route("POST /me/jobs/boards", s.addJobBoard)
	s.Route("POST /me/jobs/scan", s.scanJobs)
	s.Route("POST /me/jobs/thread", s.jobThread)
	s.Route("POST /me/jobs/questions/{id}", s.answerJobQuestion)
	s.Route("DELETE /me/jobs/questions/{id}", s.forgetJobQuestion)
	s.Route("PATCH /me/jobs/{id}", s.patchJob)
}

func (s *server) hasJobs(w http.ResponseWriter) bool {
	if s.H == nil || s.H.Jobs == nil {
		writeError(w, http.StatusNotFound, "not_found", "This server has no job search.")
		return false
	}
	return true
}

func jobFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, harness.ErrJobInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, harness.ErrJobNotFound):
		writeError(w, http.StatusNotFound, "not_found", "No such job.")
	default:
		fail(w, err)
	}
}

func (s *server) listJobs(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.hasJobs(w) {
		return
	}
	svc := s.H.Jobs
	p, err := svc.Profile(c.Actor)
	if err != nil {
		jobFail(w, err)
		return
	}
	counts, err := svc.Counts(c.Actor)
	if err != nil {
		jobFail(w, err)
		return
	}
	status := r.URL.Query().Get("status")
	jobs, err := svc.List(c.Actor, orAll(status), 300)
	if err != nil {
		jobFail(w, err)
		return
	}
	if status == "" { // the working set: what the search found and decided, without the noise
		keep := jobs[:0]
		for _, j := range jobs {
			if j.Status != "skipped" && j.Status != "closed" {
				keep = append(keep, j)
			}
		}
		jobs = keep
	}
	if jobs == nil {
		jobs = []harness.JobItem{}
	}
	qs, err := svc.Questions(c.Actor)
	if err != nil {
		jobFail(w, err)
		return
	}
	if qs == nil {
		qs = []harness.JobQuestion{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"profile": p, "counts": counts, "jobs": jobs, "questions": qs})
}

func orAll(status string) string {
	if status == "" {
		return "all"
	}
	return status
}

func (s *server) putJobProfile(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.hasJobs(w) {
		return
	}
	var body harness.JobProfile
	if !decode(w, r, &body) {
		return
	}
	p, err := s.H.Jobs.SetProfile(c.Actor, body)
	if err != nil {
		jobFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) addJobBoard(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.hasJobs(w) {
		return
	}
	var body struct {
		URL  string `json:"url"`
		ATS  string `json:"ats"`
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	co := harness.JobCompany{ATS: body.ATS, Slug: body.Slug, Name: body.Name}
	if strings.TrimSpace(body.URL) != "" {
		ats, slug, ok := harness.DetectJobBoard(body.URL)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_request", "That is not the address of a Greenhouse, Lever or Ashby job board.")
			return
		}
		co.ATS, co.Slug = ats, slug
	}
	p, n, err := s.H.Jobs.AddCompany(r.Context(), c.Actor, co)
	if err != nil {
		jobFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"profile": p, "openJobs": n})
}

func (s *server) scanJobs(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.hasJobs(w) {
		return
	}
	res, err := s.H.Jobs.Scan(r.Context(), c.Actor)
	if err != nil {
		jobFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) patchJob(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.hasJobs(w) {
		return
	}
	var body struct {
		Status string  `json:"status"`
		Note   *string `json:"note"`
	}
	if !decode(w, r, &body) {
		return
	}
	// The person pressing a button is the approval the search needs.
	j, err := s.H.Jobs.Update(c.Actor, r.PathValue("id"), harness.JobPatch{Status: body.Status, Note: body.Note, UserApproved: true})
	if err != nil {
		jobFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *server) jobThread(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.hasJobs(w) {
		return
	}
	id, started, err := s.H.StartJobSearch(r.Context(), c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"threadId": id, "started": started})
}

func (s *server) answerJobQuestion(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.hasJobs(w) {
		return
	}
	var body struct {
		Answer string `json:"answer"`
	}
	if !decode(w, r, &body) {
		return
	}
	q, err := s.H.Jobs.Answer(c.Actor, r.PathValue("id"), body.Answer)
	if err != nil {
		jobFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

func (s *server) forgetJobQuestion(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.hasJobs(w) {
		return
	}
	if err := s.H.Jobs.Forget(c.Actor, r.PathValue("id")); err != nil {
		jobFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
