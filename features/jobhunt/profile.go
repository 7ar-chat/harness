// Package jobhunt lets an agent look for work for a person and apply on their behalf, without the person starting it.
//
// Three things make it generic. The person's search is data, not code: a Profile (the roles, places, salary floor,
// work authorisation, companies to watch, who never to apply to, how much freedom the agent has). The finding is plain
// HTTP, no model: the public job-board APIs of the common applicant-tracking systems are polled and filtered against
// the profile, so a scan costs nothing and can run every hour. The judging and the form filling are the agent's, guided
// by the job-search skill, and bounded by the code in this package: an application must be claimed first (Claim), and a
// claim is refused when the job is blacklisted, was never approved, would exceed the daily cap, or repeats a company
// already applied to.
//
// State is one directory per user (the login hashed): profile.json and jobs.json. Files stay readable and portable.
package jobhunt

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Modes of a Profile: how much the agent may do alone.
const (
	// ModeQueue prepares everything and waits: a job is applied to only after the person approved it.
	ModeQueue = "queue"
	// ModeAuto lets the agent apply by itself to jobs that score at least Profile.MinScore.
	ModeAuto = "auto"
)

// Limits.
const (
	MaxTitles    = 30
	MaxCompanies = 300
	MaxBlacklist = 200
	MaxLocations = 30
	MaxText      = 4000
	MaxDailyCap  = 50
	maxItemChars = 200
)

// ErrInvalid is a refused value; the message says what to change.
var ErrInvalid = errors.New("invalid job-search setting")

// Company is a job board to watch.
type Company struct {
	// Name is how the company is shown and matched against the blacklist.
	Name string `json:"name"`
	// ATS is the applicant-tracking system hosting the board: greenhouse, lever or ashby.
	ATS string `json:"ats"`
	// Slug is the company's id on that system (jobs.lever.co/<slug>).
	Slug string `json:"slug"`
}

// Profile is everything a person tells the agent about the search. Zero values mean "no preference".
type Profile struct {
	// Login owns the profile (set by the service, never trusted from input).
	Login string `json:"login"`
	// Enabled turns the autonomous search on: the host scans and starts the agent on its own.
	Enabled bool `json:"enabled"`
	// Mode is ModeQueue (default) or ModeAuto.
	Mode string `json:"mode"`
	// Titles are the roles wanted: a job matches when its title contains any of them (case-insensitive).
	Titles []string `json:"titles"`
	// Exclude are words that rule a title out ("intern", "junior", "manager").
	Exclude []string `json:"exclude,omitempty"`
	// Locations are accepted places ("Paris", "France", "remote"). Empty: anywhere.
	Locations []string `json:"locations,omitempty"`
	// RemoteOnly keeps only jobs that say they are remote.
	RemoteOnly bool `json:"remoteOnly,omitempty"`
	// MinSalary is the floor per year in Currency (0: none).
	MinSalary int    `json:"minSalary,omitempty"`
	Currency  string `json:"currency,omitempty"`
	// WorkAuth lists the countries the person may work in without sponsorship; NeedsSponsorship says whether they need
	// a visa anywhere else.
	WorkAuth         []string `json:"workAuth,omitempty"`
	NeedsSponsorship bool     `json:"needsSponsorship,omitempty"`
	// Companies are the boards the scanner polls.
	Companies []Company `json:"companies,omitempty"`
	// Blacklist is companies (by name) and domains the agent never applies to.
	Blacklist []string `json:"blacklist,omitempty"`
	// Dealbreakers and Notes are free text for the agent: what rules a job out, and anything else to know.
	Dealbreakers string `json:"dealbreakers,omitempty"`
	Notes        string `json:"notes,omitempty"`
	// MinScore (1-5) is the score from which the agent may apply alone in ModeAuto (default 4.5). QueueScore is the
	// score from which a job is worth the person's attention (default 4).
	MinScore   float64 `json:"minScore"`
	QueueScore float64 `json:"queueScore"`
	// DailyCap is how many applications the agent may send in 24 hours (default 10, at most MaxDailyCap).
	DailyCap int `json:"dailyCap"`
}

var slugRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Normalize returns the profile cleaned and with its defaults, or an error naming the first bad value.
func (p Profile) Normalize() (Profile, error) {
	var err error
	if p.Titles, err = cleanList("titles", p.Titles, MaxTitles); err != nil {
		return p, err
	}
	if p.Exclude, err = cleanList("exclude", p.Exclude, MaxTitles); err != nil {
		return p, err
	}
	if p.Locations, err = cleanList("locations", p.Locations, MaxLocations); err != nil {
		return p, err
	}
	if p.WorkAuth, err = cleanList("workAuth", p.WorkAuth, MaxLocations); err != nil {
		return p, err
	}
	if p.Blacklist, err = cleanList("blacklist", p.Blacklist, MaxBlacklist); err != nil {
		return p, err
	}
	switch strings.ToLower(strings.TrimSpace(p.Mode)) {
	case "", ModeQueue:
		p.Mode = ModeQueue
	case ModeAuto:
		p.Mode = ModeAuto
	default:
		return p, fmt.Errorf("%w: mode must be %q or %q", ErrInvalid, ModeQueue, ModeAuto)
	}
	if p.MinScore == 0 {
		p.MinScore = 4.5
	}
	if p.QueueScore == 0 {
		p.QueueScore = 4
	}
	if p.MinScore < 1 || p.MinScore > 5 || p.QueueScore < 1 || p.QueueScore > 5 {
		return p, fmt.Errorf("%w: scores are between 1 and 5", ErrInvalid)
	}
	if p.QueueScore > p.MinScore {
		return p, fmt.Errorf("%w: queueScore (%.1f) cannot be above minScore (%.1f)", ErrInvalid, p.QueueScore, p.MinScore)
	}
	if p.DailyCap == 0 {
		p.DailyCap = 10
	}
	if p.DailyCap < 1 || p.DailyCap > MaxDailyCap {
		return p, fmt.Errorf("%w: dailyCap is between 1 and %d", ErrInvalid, MaxDailyCap)
	}
	if p.MinSalary < 0 {
		return p, fmt.Errorf("%w: minSalary cannot be negative", ErrInvalid)
	}
	p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
	if len(p.Currency) > 5 {
		return p, fmt.Errorf("%w: currency is a code like EUR or USD", ErrInvalid)
	}
	if p.Dealbreakers, err = cleanText("dealbreakers", p.Dealbreakers); err != nil {
		return p, err
	}
	if p.Notes, err = cleanText("notes", p.Notes); err != nil {
		return p, err
	}
	if len(p.Companies) > MaxCompanies {
		return p, fmt.Errorf("%w: at most %d companies", ErrInvalid, MaxCompanies)
	}
	seen := map[string]bool{}
	cos := make([]Company, 0, len(p.Companies))
	for _, c := range p.Companies {
		c, err = c.normalize()
		if err != nil {
			return p, err
		}
		if k := c.ATS + "/" + strings.ToLower(c.Slug); !seen[k] {
			seen[k] = true
			cos = append(cos, c)
		}
	}
	p.Companies = cos
	return p, nil
}

func (c Company) normalize() (Company, error) {
	c.ATS = strings.ToLower(strings.TrimSpace(c.ATS))
	c.Slug = strings.TrimSpace(c.Slug)
	c.Name = strings.Join(strings.Fields(c.Name), " ")
	if !knownATS(c.ATS) {
		return c, fmt.Errorf("%w: ats %q is not supported (greenhouse, lever or ashby)", ErrInvalid, c.ATS)
	}
	if !slugRE.MatchString(c.Slug) {
		return c, fmt.Errorf("%w: %q is not a board id", ErrInvalid, c.Slug)
	}
	if c.Name == "" {
		c.Name = c.Slug
	}
	if len([]rune(c.Name)) > maxItemChars {
		return c, fmt.Errorf("%w: company name is too long", ErrInvalid)
	}
	return c, nil
}

func cleanList(what string, in []string, max int) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.Join(strings.Fields(s), " ")
		if s == "" || seen[strings.ToLower(s)] {
			continue
		}
		if len([]rune(s)) > maxItemChars {
			return nil, fmt.Errorf("%w: an item of %s is longer than %d characters", ErrInvalid, what, maxItemChars)
		}
		for _, r := range s {
			if unicode.IsControl(r) {
				return nil, fmt.Errorf("%w: %s has a control character", ErrInvalid, what)
			}
		}
		seen[strings.ToLower(s)] = true
		out = append(out, s)
	}
	if len(out) > max {
		return nil, fmt.Errorf("%w: at most %d items in %s", ErrInvalid, max, what)
	}
	return out, nil
}

func cleanText(what, s string) (string, error) {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > MaxText {
		return "", fmt.Errorf("%w: %s is longer than %d characters", ErrInvalid, what, MaxText)
	}
	return s, nil
}
