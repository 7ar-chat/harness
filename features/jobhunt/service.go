package jobhunt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Job statuses.
const (
	StatusNew       = "new"       // found by a scan, not judged yet
	StatusQueued    = "queued"    // worth the person's attention: waits for their decision
	StatusApproved  = "approved"  // may be applied to (the person said so, or auto mode and a high score)
	StatusApplying  = "applying"  // claimed: an agent is filling the form now
	StatusApplied   = "applied"   // submitted
	StatusSkipped   = "skipped"   // not for this person (the reason is on the job)
	StatusFailed    = "failed"    // the agent could not complete the form (the note says why)
	StatusClosed    = "closed"    // gone from the board
	StatusInterview = "interview" // outcomes the person reports
	StatusRejected  = "rejected"
	StatusOffer     = "offer"
	StatusWithdrawn = "withdrawn"
)

// Approvals: who allowed the application.
const (
	ApprovedByUser = "user"
	ApprovedByAuto = "auto"
)

const (
	maxJobs       = 5000
	claimTTL      = 30 * time.Minute
	maxNewPerScan = 100
	// SameCompanyDays is how long after applying to a company the agent leaves its other jobs alone.
	SameCompanyDays = 30
)

// Job is one posting known to the search.
type Job struct {
	ID       string    `json:"id"`
	Company  string    `json:"company"`
	Title    string    `json:"title"`
	Location string    `json:"location,omitempty"`
	Remote   bool      `json:"remote,omitempty"`
	URL      string    `json:"url"`
	ATS      string    `json:"ats"`
	Board    string    `json:"board"`
	Posted   time.Time `json:"posted,omitzero"`
	Seen     time.Time `json:"seen"`
	Status   string    `json:"status"`
	// Score is the agent's 1-5 judgement of the fit; Reason says why (a line).
	Score  float64 `json:"score,omitempty"`
	Reason string  `json:"reason,omitempty"`
	Note   string  `json:"note,omitempty"`
	// Approval is who allowed the application (ApprovedBy*).
	Approval  string    `json:"approval,omitempty"`
	ClaimedAt time.Time `json:"claimedAt,omitzero"`
	AppliedAt time.Time `json:"appliedAt,omitzero"`
	Updated   time.Time `json:"updated"`
}

// Service keeps the profiles and the jobs of the users under Dir and scans the job boards.
type Service struct {
	Dir string
	// HTTP fetches the boards (nil: a client with a 20 s timeout). Bases overrides the boards' addresses (tests).
	HTTP  *http.Client
	Bases map[string]string
	// Now is the clock (nil: time.Now).
	Now func() time.Time

	mu      sync.Mutex
	threads map[string]string // user tag → job-search thread (cache of state.json)
}

// New returns a service storing under dir.
func New(dir string) *Service { return &Service{Dir: dir} }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Tag is the name of a user's directory: a hash, never the login.
func Tag(login string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(login))))
	return hex.EncodeToString(sum[:8])
}

func (s *Service) userDir(login string) (string, error) {
	if strings.TrimSpace(login) == "" {
		return "", fmt.Errorf("%w: no user", ErrInvalid)
	}
	return filepath.Join(s.Dir, Tag(login)), nil
}

type jobsFile struct {
	Jobs []Job `json:"jobs"`
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Profile returns the user's profile; the zero Profile (defaults applied, disabled) when they have none.
func (s *Service) Profile(login string) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.profile(login)
}

func (s *Service) profile(login string) (Profile, error) {
	dir, err := s.userDir(login)
	if err != nil {
		return Profile{}, err
	}
	var p Profile
	if err := readJSON(filepath.Join(dir, "profile.json"), &p); err != nil {
		return Profile{}, err
	}
	p.Login = login
	return p.Normalize()
}

// SetProfile validates and stores the profile (the owner is always login).
func (s *Service) SetProfile(login string, p Profile) (Profile, error) {
	p.Login = login
	p, err := p.Normalize()
	if err != nil {
		return p, err
	}
	dir, err := s.userDir(login)
	if err != nil {
		return p, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return p, writeJSON(filepath.Join(dir, "profile.json"), p)
}

// Users lists the logins that turned the search on.
func (s *Service) Users() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var p Profile
		if readJSON(filepath.Join(s.Dir, e.Name(), "profile.json"), &p) == nil && p.Enabled && p.Login != "" {
			out = append(out, p.Login)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *Service) load(login string) (string, []Job, error) {
	dir, err := s.userDir(login)
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, "jobs.json")
	var f jobsFile
	if err := readJSON(path, &f); err != nil {
		return "", nil, err
	}
	// A claim nobody finished frees the job again.
	now := s.now()
	for i := range f.Jobs {
		if j := &f.Jobs[i]; j.Status == StatusApplying && now.Sub(j.ClaimedAt) > claimTTL {
			j.Status, j.Note, j.Updated = StatusApproved, "the earlier attempt did not finish", now
		}
	}
	return path, f.Jobs, nil
}

func (s *Service) save(path string, jobs []Job) error {
	if len(jobs) > maxJobs { // drop the oldest finished-with ones first
		sort.SliceStable(jobs, func(a, b int) bool {
			return rank(jobs[a]) > rank(jobs[b]) || rank(jobs[a]) == rank(jobs[b]) && jobs[a].Seen.After(jobs[b].Seen)
		})
		jobs = jobs[:maxJobs]
	}
	return writeJSON(path, jobsFile{Jobs: jobs})
}

// rank orders jobs by how much they are worth keeping (higher: keep).
func rank(j Job) int {
	switch j.Status {
	case StatusSkipped, StatusClosed:
		return 0
	case StatusNew:
		return 1
	}
	return 2
}

// JobID is the stable id of a posting.
func JobID(ats, board, postingID string) string {
	sum := sha256.Sum256([]byte(ats + "\x00" + strings.ToLower(board) + "\x00" + postingID))
	return hex.EncodeToString(sum[:6])
}

// List returns the user's jobs with the given status ("" or "all": every one), best score first among equals newest
// first, at most limit (0: 50).
func (s *Service) List(login, status string, limit int) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, jobs, err := s.load(login)
	if err != nil {
		return nil, err
	}
	var out []Job
	for _, j := range jobs {
		if status == "" || status == "all" || j.Status == status {
			out = append(out, j)
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		return out[a].Seen.After(out[b].Seen)
	})
	if limit <= 0 {
		limit = 50
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Counts returns how many jobs are in each status.
func (s *Service) Counts(login string) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, jobs, err := s.load(login)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, j := range jobs {
		out[j.Status]++
	}
	return out, nil
}

// ErrNotFound means the user has no such job.
var ErrNotFound = errors.New("job not found")

// Get returns one job.
func (s *Service) Get(login, id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, jobs, err := s.load(login)
	if err != nil {
		return Job{}, err
	}
	for _, j := range jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return Job{}, ErrNotFound
}

// ScanResult is what one scan found.
type ScanResult struct {
	Boards  int      `json:"boards"`
	Fetched int      `json:"fetched"`
	Matched int      `json:"matched"`
	Added   int      `json:"added"`
	Closed  int      `json:"closed"`
	New     []Job    `json:"new,omitempty"`
	Errors  []string `json:"errors,omitempty"`
}

// Scan polls the user's boards, keeps the postings that fit the profile and adds the ones not seen before as new jobs.
// A job that disappeared from a board that answered is marked closed. At most maxNewPerScan jobs are added at once; the
// rest come at the next scan.
func (s *Service) Scan(ctx context.Context, login string) (ScanResult, error) {
	p, err := s.Profile(login)
	if err != nil {
		return ScanResult{}, err
	}
	if len(p.Titles) == 0 {
		return ScanResult{}, fmt.Errorf("%w: set the roles you want (titles) before scanning", ErrInvalid)
	}
	if len(p.Companies) == 0 {
		return ScanResult{}, fmt.Errorf("%w: no company boards to scan yet (add some with companies)", ErrInvalid)
	}
	type fetched struct {
		co    Company
		posts []Posting
		err   error
	}
	results := make([]fetched, len(p.Companies))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, co := range p.Companies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			posts, err := s.Fetch(ctx, co.ATS, co.Slug)
			results[i] = fetched{co, posts, err}
		}()
	}
	wg.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()
	path, jobs, err := s.load(login)
	if err != nil {
		return ScanResult{}, err
	}
	known := map[string]int{}
	for i, j := range jobs {
		known[j.ID] = i
	}
	now := s.now()
	var res ScanResult
	for _, r := range results {
		if r.err != nil {
			res.Errors = append(res.Errors, r.err.Error())
			continue
		}
		res.Boards++
		open := map[string]bool{}
		for _, post := range r.posts {
			res.Fetched++
			id := JobID(r.co.ATS, r.co.Slug, post.ID)
			open[id] = true
			if _, ok := known[id]; ok || !p.matches(r.co, post) {
				continue
			}
			res.Matched++
			if res.Added >= maxNewPerScan {
				continue
			}
			j := Job{ID: id, Company: r.co.Name, Title: post.Title, Location: post.Location, Remote: post.Remote, URL: post.URL,
				ATS: r.co.ATS, Board: r.co.Slug, Posted: post.Posted, Seen: now, Status: StatusNew, Updated: now}
			jobs = append(jobs, j)
			known[id] = len(jobs) - 1
			res.Added++
			if len(res.New) < 50 {
				res.New = append(res.New, j)
			}
		}
		for i := range jobs {
			j := &jobs[i]
			if j.ATS == r.co.ATS && strings.EqualFold(j.Board, r.co.Slug) && !open[j.ID] &&
				(j.Status == StatusNew || j.Status == StatusQueued || j.Status == StatusApproved) {
				j.Status, j.Updated = StatusClosed, now
				res.Closed++
			}
		}
	}
	if res.Added > 0 || res.Closed > 0 {
		if err := s.save(path, jobs); err != nil {
			return res, err
		}
	}
	return res, nil
}

// matches reports whether a posting fits the profile's roles, places and blacklist.
func (p Profile) matches(co Company, post Posting) bool {
	if p.Blacklisted(co.Name, post.URL) {
		return false
	}
	title := strings.ToLower(post.Title)
	hit := false
	for _, t := range p.Titles {
		if containsWord(title, strings.ToLower(t), false) {
			hit = true
			break
		}
	}
	if !hit {
		return false
	}
	for _, x := range p.Exclude {
		if containsWord(title, strings.ToLower(x), true) {
			return false
		}
	}
	return p.locationOK(post)
}

func (p Profile) locationOK(post Posting) bool {
	if p.RemoteOnly {
		return post.Remote
	}
	if len(p.Locations) == 0 || strings.TrimSpace(post.Location) == "" {
		return true
	}
	loc := strings.ToLower(post.Location)
	for _, l := range p.Locations {
		l = strings.ToLower(l)
		if l == "remote" {
			if post.Remote {
				return true
			}
			continue
		}
		if strings.Contains(loc, l) {
			return true
		}
	}
	return false
}

// containsWord reports whether word appears in s starting at a word boundary (so "go" does not match "algorithms"),
// and, when whole is set, ending at one (so "intern" does not match "international").
func containsWord(s, word string, whole bool) bool {
	if word == "" {
		return false
	}
	for from := 0; ; {
		i := strings.Index(s[from:], word)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(word)
		before := i == 0 || !isLetter(s[i-1])
		after := !whole || end == len(s) || !isLetter(s[end])
		if before && after {
			return true
		}
		from = i + 1
	}
}

func isLetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b >= 0x80 }

// Blacklisted reports whether the company (by name, ignoring case and punctuation) or the address's host (or a parent
// domain of it) is on the profile's blacklist.
func (p Profile) Blacklisted(company, rawURL string) bool {
	host := ""
	if u, err := url.Parse(rawURL); err == nil {
		host = strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	}
	name := squash(company)
	for _, b := range p.Blacklist {
		if strings.Contains(b, ".") && !strings.Contains(b, " ") {
			d := strings.TrimSuffix(strings.ToLower(b), ".")
			if host == d || strings.HasSuffix(host, "."+d) {
				return true
			}
			continue
		}
		if name != "" && squash(b) == name {
			return true
		}
	}
	return false
}

func squash(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127 {
			b.WriteRune(r)
		}
	}
	return b.String()
}
