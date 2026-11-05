package jobhunt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The applicant-tracking systems whose public job-board APIs are polled. Each needs no account or key.
const (
	Greenhouse = "greenhouse"
	Lever      = "lever"
	Ashby      = "ashby"
)

func knownATS(k string) bool { return k == Greenhouse || k == Lever || k == Ashby }

// Posting is one open job as a board lists it.
type Posting struct {
	ID       string
	Title    string
	Location string
	Remote   bool
	URL      string // the job's page
	Posted   time.Time
}

// defaultBases are the APIs' addresses; Service.Bases overrides them (tests).
var defaultBases = map[string]string{
	Greenhouse: "https://boards-api.greenhouse.io",
	Lever:      "https://api.lever.co",
	Ashby:      "https://api.ashbyhq.com",
}

const maxBoardBytes = 16 << 20

// Fetch lists the open postings of a board.
func (s *Service) Fetch(ctx context.Context, ats, slug string) ([]Posting, error) {
	if !knownATS(ats) || !slugRE.MatchString(slug) {
		return nil, fmt.Errorf("%w: %s/%s", ErrInvalid, ats, slug)
	}
	base := defaultBases[ats]
	if b := s.Bases[ats]; b != "" {
		base = b
	}
	var path string
	switch ats {
	case Greenhouse:
		path = "/v1/boards/" + url.PathEscape(slug) + "/jobs"
	case Lever:
		path = "/v0/postings/" + url.PathEscape(slug) + "?mode=json"
	case Ashby:
		path = "/posting-api/job-board/" + url.PathEscape(slug)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "7ar-jobhunt/1.0")
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s/%s: %w", ats, slug, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s/%s: no such board (404)", ats, slug)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s/%s: status %d", ats, slug, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBoardBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s/%s: %w", ats, slug, err)
	}
	if len(body) > maxBoardBytes {
		return nil, fmt.Errorf("%s/%s: the board is larger than %d MiB", ats, slug, maxBoardBytes>>20)
	}
	posts, err := parseBoard(ats, body)
	if err != nil {
		return nil, fmt.Errorf("%s/%s: %w", ats, slug, err)
	}
	return posts, nil
}

func parseBoard(ats string, body []byte) ([]Posting, error) {
	switch ats {
	case Greenhouse:
		var r struct {
			Jobs []struct {
				ID       json.Number `json:"id"`
				Title    string      `json:"title"`
				Updated  string      `json:"updated_at"`
				URL      string      `json:"absolute_url"`
				Location struct {
					Name string `json:"name"`
				} `json:"location"`
			} `json:"jobs"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("not a Greenhouse board: %w", err)
		}
		out := make([]Posting, 0, len(r.Jobs))
		for _, j := range r.Jobs {
			out = append(out, Posting{ID: j.ID.String(), Title: j.Title, Location: j.Location.Name,
				Remote: looksRemote(j.Location.Name), URL: j.URL, Posted: parseTime(j.Updated)})
		}
		return out, nil
	case Lever:
		var r []struct {
			ID         string `json:"id"`
			Text       string `json:"text"`
			HostedURL  string `json:"hostedUrl"`
			Created    int64  `json:"createdAt"`
			Workplace  string `json:"workplaceType"`
			Categories struct {
				Location string   `json:"location"`
				All      []string `json:"allLocations"`
			} `json:"categories"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("not a Lever board: %w", err)
		}
		out := make([]Posting, 0, len(r))
		for _, j := range r {
			loc := j.Categories.Location
			if loc == "" && len(j.Categories.All) > 0 {
				loc = strings.Join(j.Categories.All, ", ")
			}
			var posted time.Time
			if j.Created > 0 {
				posted = time.UnixMilli(j.Created).UTC()
			}
			out = append(out, Posting{ID: j.ID, Title: j.Text, Location: loc,
				Remote: strings.EqualFold(j.Workplace, "remote") || looksRemote(loc), URL: j.HostedURL, Posted: posted})
		}
		return out, nil
	case Ashby:
		var r struct {
			Jobs []struct {
				ID        string `json:"id"`
				Title     string `json:"title"`
				Location  string `json:"location"`
				Remote    bool   `json:"isRemote"`
				JobURL    string `json:"jobUrl"`
				Published string `json:"publishedAt"`
				Listed    *bool  `json:"isListed"`
			} `json:"jobs"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("not an Ashby board: %w", err)
		}
		out := make([]Posting, 0, len(r.Jobs))
		for _, j := range r.Jobs {
			if j.Listed != nil && !*j.Listed {
				continue
			}
			out = append(out, Posting{ID: j.ID, Title: j.Title, Location: j.Location,
				Remote: j.Remote || looksRemote(j.Location), URL: j.JobURL, Posted: parseTime(j.Published)})
		}
		return out, nil
	}
	return nil, fmt.Errorf("%w: ats %q", ErrInvalid, ats)
}

func looksRemote(loc string) bool { return strings.Contains(strings.ToLower(loc), "remote") }

func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05.000-07:00", "2006-01-02T15:04:05.000Z0700", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func (s *Service) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// DetectBoard recognises a job-board address (https://jobs.lever.co/acme, https://job-boards.greenhouse.io/acme/jobs/1,
// https://jobs.ashbyhq.com/acme/<id>) and returns the system and the company's id on it.
func DetectBoard(raw string) (ats, slug string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", "", false
	}
	host := strings.ToLower(u.Hostname())
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	first := ""
	if len(parts) > 0 {
		first = parts[0]
	}
	switch host {
	case "boards.greenhouse.io", "job-boards.greenhouse.io", "job-boards.eu.greenhouse.io", "boards.eu.greenhouse.io":
		ats = Greenhouse
		if first == "embed" { // /embed/job_board?for=acme
			first = u.Query().Get("for")
		}
	case "jobs.lever.co", "jobs.eu.lever.co":
		ats = Lever
	case "jobs.ashbyhq.com":
		ats = Ashby
	default:
		return "", "", false
	}
	if !slugRE.MatchString(first) {
		return "", "", false
	}
	return ats, first, true
}
