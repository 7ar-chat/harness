package jobhunt

import (
	"context"
	"errors"
	"strings"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/engine/tools"
)

// Provider offers the job-search tools (a tools.Provider).
type Provider struct{ Svc *Service }

// Name implements tools.Provider.
func (p Provider) Name() string { return "jobhunt" }

type noIn struct{}

// ProfileIn changes the profile: only the fields that are present change, and a list that is present replaces the old one.
type ProfileIn struct {
	Titles           *[]string  `json:"titles,omitempty" jsonschema_description:"Roles wanted; a job matches when its title contains one of them"`
	Exclude          *[]string  `json:"exclude,omitempty" jsonschema_description:"Words that rule a title out, such as intern or manager"`
	Locations        *[]string  `json:"locations,omitempty" jsonschema_description:"Accepted places, such as Paris, France, remote. Empty: anywhere"`
	RemoteOnly       *bool      `json:"remoteOnly,omitempty"`
	MinSalary        *int       `json:"minSalary,omitempty" jsonschema_description:"Yearly floor in the currency"`
	Currency         *string    `json:"currency,omitempty"`
	WorkAuth         *[]string  `json:"workAuth,omitempty" jsonschema_description:"Countries the person may work in without sponsorship"`
	NeedsSponsorship *bool      `json:"needsSponsorship,omitempty"`
	Blacklist        *[]string  `json:"blacklist,omitempty" jsonschema_description:"Companies (names) and domains never to apply to"`
	Dealbreakers     *string    `json:"dealbreakers,omitempty" jsonschema_description:"Free text: what rules a job out"`
	Notes            *string    `json:"notes,omitempty" jsonschema_description:"Free text: anything else to know about the search"`
	Enabled          *bool      `json:"enabled,omitempty" jsonschema_description:"Run the search by itself, around the clock"`
	Mode             *string    `json:"mode,omitempty" jsonschema_description:"queue (prepare, the person approves) or auto (apply alone above minScore)"`
	MinScore         *float64   `json:"minScore,omitempty"`
	QueueScore       *float64   `json:"queueScore,omitempty"`
	DailyCap         *int       `json:"dailyCap,omitempty"`
	UserConfirmed    bool       `json:"userConfirmed,omitempty" jsonschema_description:"Required to change enabled, mode, minScore or dailyCap: true only when the person asked for that change in this conversation"`
	Companies        *[]Company `json:"companies,omitempty" jsonschema_description:"Replaces the watched boards; to add one use jobs_track_company"`
}

func (in ProfileIn) apply(p Profile) (Profile, error) {
	guarded := in.Enabled != nil || in.Mode != nil || in.MinScore != nil || in.DailyCap != nil
	if guarded && !in.UserConfirmed {
		return p, errors.New("enabled, mode, minScore and dailyCap decide how freely you act: change them only when the person asked for it, then pass userConfirmed")
	}
	set := func(dst *[]string, v *[]string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&p.Titles, in.Titles)
	set(&p.Exclude, in.Exclude)
	set(&p.Locations, in.Locations)
	set(&p.WorkAuth, in.WorkAuth)
	set(&p.Blacklist, in.Blacklist)
	if in.RemoteOnly != nil {
		p.RemoteOnly = *in.RemoteOnly
	}
	if in.MinSalary != nil {
		p.MinSalary = *in.MinSalary
	}
	if in.Currency != nil {
		p.Currency = *in.Currency
	}
	if in.NeedsSponsorship != nil {
		p.NeedsSponsorship = *in.NeedsSponsorship
	}
	if in.Dealbreakers != nil {
		p.Dealbreakers = *in.Dealbreakers
	}
	if in.Notes != nil {
		p.Notes = *in.Notes
	}
	if in.Enabled != nil {
		p.Enabled = *in.Enabled
	}
	if in.Mode != nil {
		p.Mode = *in.Mode
	}
	if in.MinScore != nil {
		p.MinScore = *in.MinScore
	}
	if in.QueueScore != nil {
		p.QueueScore = *in.QueueScore
	}
	if in.DailyCap != nil {
		p.DailyCap = *in.DailyCap
	}
	if in.Companies != nil {
		p.Companies = *in.Companies
	}
	return p, nil
}

type profileOut struct {
	Profile Profile        `json:"profile"`
	Jobs    map[string]int `json:"jobs"`
}

type trackIn struct {
	URL  string `json:"url,omitempty" jsonschema_description:"A job-board address, such as https://jobs.lever.co/acme or https://job-boards.greenhouse.io/acme"`
	ATS  string `json:"ats,omitempty" jsonschema_description:"greenhouse, lever or ashby (instead of url)"`
	Slug string `json:"slug,omitempty" jsonschema_description:"The company's id on that system (instead of url)"`
	Name string `json:"name,omitempty" jsonschema_description:"How to show the company"`
}

type trackOut struct {
	Tracked  Company `json:"tracked"`
	OpenJobs int     `json:"open_jobs"`
}

type listIn struct {
	Status string `json:"status,omitempty" jsonschema_description:"new, queued, approved, applying, applied, skipped, failed, closed, interview, rejected, offer, or all. Default: new"`
	Limit  int    `json:"limit,omitempty" jsonschema_description:"At most this many (default 25, best score first)"`
}

type idIn struct {
	ID string `json:"id" jsonschema_description:"The job's id from jobs_list"`
}

type updateIn struct {
	ID           string   `json:"id"`
	Status       string   `json:"status,omitempty" jsonschema_description:"skipped, queued (waits for the person), approved, interview, rejected, offer or withdrawn"`
	Score        *float64 `json:"score,omitempty" jsonschema_description:"Your 1-5 judgement of the fit"`
	Reason       *string  `json:"reason,omitempty" jsonschema_description:"One line: why this score"`
	Note         *string  `json:"note,omitempty"`
	UserApproved bool     `json:"userApproved,omitempty" jsonschema_description:"true only when the person told you in this conversation to apply to this job"`
}

type claimOut struct {
	Job      Job      `json:"job"`
	Allowed  bool     `json:"allowed"`
	Reasons  []string `json:"reasons,omitempty"`
	Guidance string   `json:"guidance"`
}

type finishIn struct {
	ID     string `json:"id"`
	Note   string `json:"note,omitempty" jsonschema_description:"What the confirmation page said, or why it did not work out"`
	Status string `json:"status,omitempty" jsonschema_description:"For jobs_release: queued (needs the person), failed or skipped"`
}

// Tools implements tools.Provider.
func (p Provider) Tools(_ context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	if p.Svc == nil || rc.Login == "" {
		return nil, func() {}, nil
	}
	svc, login := p.Svc, rc.Login
	fail := func(err error) error { return tools.Errorf("%v", err) }

	var out []ai.BaseTool
	add := func(t ai.BaseTool, err error) error {
		if err != nil {
			return err
		}
		out = append(out, t)
		return nil
	}
	steps := []func() error{
		func() error {
			return add(ai.InferTool("jobs_profile", "Read the person's job-search profile (roles, places, salary floor, blacklist, mode, caps, watched companies) and how many jobs are in each status. Read it before searching or applying.",
				func(_ context.Context, _ noIn) (profileOut, error) {
					pr, err := svc.Profile(login)
					if err != nil {
						return profileOut{}, fail(err)
					}
					c, err := svc.Counts(login)
					if err != nil {
						return profileOut{}, fail(err)
					}
					return profileOut{pr, c}, nil
				}))
		},
		func() error {
			return add(ai.InferTool("jobs_profile_set", "Change the job-search profile. Only the fields you pass change. Save what the person tells you (roles, places, salary, visa, blacklist...) so the search keeps it. Changing enabled, mode, minScore or dailyCap needs userConfirmed.",
				func(_ context.Context, in ProfileIn) (profileOut, error) {
					cur, err := svc.Profile(login)
					if err != nil {
						return profileOut{}, fail(err)
					}
					next, err := in.apply(cur)
					if err != nil {
						return profileOut{}, fail(err)
					}
					next, err = svc.SetProfile(login, next)
					if err != nil {
						return profileOut{}, fail(err)
					}
					c, _ := svc.Counts(login)
					return profileOut{next, c}, nil
				}))
		},
		func() error {
			return add(ai.InferTool("jobs_track_company", "Watch a company's job board: pass the board address (jobs.lever.co/acme, job-boards.greenhouse.io/acme, jobs.ashbyhq.com/acme) or ats and slug. The board is checked and added to the profile; the scan then polls it. Only Greenhouse, Lever and Ashby boards are supported.",
				func(ctx context.Context, in trackIn) (trackOut, error) {
					c := Company{ATS: in.ATS, Slug: in.Slug, Name: in.Name}
					if in.URL != "" {
						ats, slug, ok := DetectBoard(in.URL)
						if !ok {
							return trackOut{}, tools.Errorf("%q is not a Greenhouse, Lever or Ashby board address; open the company's careers page and look for the link to its board", in.URL)
						}
						c.ATS, c.Slug = ats, slug
					}
					if strings.TrimSpace(c.Name) == "" {
						c.Name = c.Slug
					}
					_, n, err := svc.AddCompany(ctx, login, c)
					if err != nil {
						return trackOut{}, fail(err)
					}
					return trackOut{c, n}, nil
				}))
		},
		func() error {
			return add(ai.InferTool("jobs_scan", "Poll the watched boards now and add the postings that fit the profile as new jobs (the search also does this by itself every hour). Costs nothing.",
				func(ctx context.Context, _ noIn) (ScanResult, error) {
					res, err := svc.Scan(ctx, login)
					if err != nil {
						return res, fail(err)
					}
					return res, nil
				}))
		},
		func() error {
			return add(ai.InferTool("jobs_list", "List the person's jobs by status, best score first. 'new' are the ones you have not judged yet.",
				func(_ context.Context, in listIn) ([]Job, error) {
					st, lim := in.Status, in.Limit
					if st == "" {
						st = StatusNew
					}
					if lim <= 0 {
						lim = 25
					}
					jobs, err := svc.List(login, st, lim)
					if err != nil {
						return nil, fail(err)
					}
					return jobs, nil
				}))
		},
		func() error {
			return add(ai.InferTool("jobs_update", "Record your judgement of a job or its outcome: a score (1-5) with a reason, and a status. queued means it waits for the person; approved is allowed only when the person said so (userApproved), or in auto mode when the score reaches the profile's minScore.",
				func(_ context.Context, in updateIn) (Job, error) {
					j, err := svc.Update(login, in.ID, Patch{Status: in.Status, Score: in.Score, Reason: in.Reason, Note: in.Note, UserApproved: in.UserApproved})
					if err != nil {
						return j, fail(err)
					}
					return j, nil
				}))
		},
		func() error {
			return add(ai.InferTool("jobs_claim", "Reserve an approved job before you open its application form. The claim is refused (allowed=false) when the job is blacklisted, not approved, over the daily cap, or at a company already applied to. Never submit an application without a successful claim.",
				func(_ context.Context, in idIn) (claimOut, error) {
					j, d, err := svc.Claim(login, in.ID)
					if err != nil {
						return claimOut{}, fail(err)
					}
					g := "Do not apply. " + strings.Join(d.Reasons, "; ")
					if d.Allowed {
						g = "Reserved for 30 minutes. Fill in the form from the person's documents and profile only. Submit only if every required answer is known. Then call jobs_applied, or jobs_release if you could not finish."
					}
					return claimOut{j, d.Allowed, d.Reasons, g}, nil
				}))
		},
		func() error {
			return add(ai.InferTool("jobs_applied", "Record that a claimed application was submitted. Call it only after the page showed a confirmation.",
				func(_ context.Context, in finishIn) (Job, error) {
					j, err := svc.Applied(login, in.ID, in.Note)
					if err != nil {
						return j, fail(err)
					}
					return j, nil
				}))
		},
		func() error {
			return add(ai.InferTool("jobs_release", "Give up a claimed application: status queued when the person must answer something (say what in note), failed when the form could not be completed, skipped when the job turned out not to fit.",
				func(_ context.Context, in finishIn) (Job, error) {
					j, err := svc.Release(login, in.ID, in.Status, in.Note)
					if err != nil {
						return j, fail(err)
					}
					return j, nil
				}))
		},
	}
	for _, f := range steps {
		if err := f(); err != nil {
			return nil, nil, err
		}
	}
	return out, func() {}, nil
}
