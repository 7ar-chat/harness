package triggers

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/7ar-chat/harness/core/factory"
)

// payload is the union of the fields of the GitHub deliveries the factory
// reads; a delivery fills the ones its event has.
type payload struct {
	Action     string `json:"action"`
	Zen        string `json:"zen"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
	Label *label `json:"label"`

	Issue       *issue    `json:"issue"`
	PullRequest *pull     `json:"pull_request"`
	Comment     *comment  `json:"comment"`
	Review      *review   `json:"review"`
	CheckSuite  *checkRun `json:"check_suite"`
	CheckRun    *checkRun `json:"check_run"`
	WorkflowRun *checkRun `json:"workflow_run"`
}

type label struct {
	Name string `json:"name"`
}

type issue struct {
	Number  int     `json:"number"`
	Title   string  `json:"title"`
	Body    string  `json:"body"`
	HTMLURL string  `json:"html_url"`
	Labels  []label `json:"labels"`
	// PullRequest is set when the "issue" is a pull request (issue_comment on a PR).
	PullRequest *struct{} `json:"pull_request"`
}

type pull struct {
	Number  int     `json:"number"`
	Title   string  `json:"title"`
	Body    string  `json:"body"`
	HTMLURL string  `json:"html_url"`
	Merged  bool    `json:"merged"`
	Labels  []label `json:"labels"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type comment struct {
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
}

type review struct {
	State   string `json:"state"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
}

// checkRun covers check_suite, check_run and workflow_run, which share the
// fields the factory reads (a check_run keeps its branch in check_suite).
type checkRun struct {
	Name       string `json:"name"`
	HeadBranch string `json:"head_branch"`
	HeadSHA    string `json:"head_sha"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	CheckSuite *struct {
		HeadBranch string `json:"head_branch"`
	} `json:"check_suite"`
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
}

// Normalize turns a GitHub delivery (the X-GitHub-Event name and the JSON
// body) into an Event: Name is "<event>.<action>" (just "<event>" without an
// action), Fields the flat values triggers match and templates read.
func Normalize(event string, body []byte) (factory.Event, error) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return factory.Event{}, fmt.Errorf("decoding %s payload: %w", event, err)
	}
	name := event
	if p.Action != "" {
		name += "." + p.Action
	}
	repo := p.Repository.FullName
	ev := factory.Event{Name: name, Repo: repo, Fields: map[string]string{}}
	set := func(k, v string) {
		if v != "" {
			ev.Fields[k] = v
		}
	}
	set("repo", repo)
	set("sender", p.Sender.Login)
	set("action", p.Action)
	if p.Label != nil {
		set("label", p.Label.Name)
	}

	switch event {
	case "ping":
		set("zen", p.Zen)
	case "issues":
		i := p.Issue
		if i == nil {
			return factory.Event{}, fmt.Errorf("%s payload has no issue", name)
		}
		fromIssue(&ev, set, i)
		ev.Key, ev.URL = issueKey(repo, i.Number), i.HTMLURL
	case "issue_comment":
		i := p.Issue
		if i == nil {
			return factory.Event{}, fmt.Errorf("%s payload has no issue", name)
		}
		fromIssue(&ev, set, i)
		ev.Key, ev.URL = issueKey(repo, i.Number), i.HTMLURL
		if c := p.Comment; c != nil {
			set("comment", c.Body)
			if c.HTMLURL != "" {
				ev.URL = c.HTMLURL
			}
		}
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		pr := p.PullRequest
		if pr == nil {
			return factory.Event{}, fmt.Errorf("%s payload has no pull request", name)
		}
		fromPull(&ev, set, pr)
		ev.Key, ev.URL = prKey(repo, pr.Number), pr.HTMLURL
		if r := p.Review; r != nil {
			set("state", strings.ToLower(r.State))
			set("comment", r.Body)
			if r.HTMLURL != "" {
				ev.URL = r.HTMLURL
			}
		}
		if c := p.Comment; c != nil {
			set("comment", c.Body)
			if c.HTMLURL != "" {
				ev.URL = c.HTMLURL
			}
		}
	case "check_suite", "check_run", "workflow_run":
		c := p.CheckSuite
		switch event {
		case "check_run":
			c = p.CheckRun
		case "workflow_run":
			c = p.WorkflowRun
		}
		if c == nil {
			return factory.Event{}, fmt.Errorf("%s payload has no %s", name, event)
		}
		branch := c.HeadBranch
		if branch == "" && c.CheckSuite != nil {
			branch = c.CheckSuite.HeadBranch
		}
		set("branch", branch)
		set("sha", c.HeadSHA)
		set("conclusion", c.Conclusion)
		set("check", c.Name)
		ev.URL = c.HTMLURL
		switch {
		case len(c.PullRequests) > 0:
			set("number", strconv.Itoa(c.PullRequests[0].Number))
			ev.Key = prKey(repo, c.PullRequests[0].Number)
		case repo != "" && c.HeadSHA != "":
			ev.Key = "github:sha:" + repo + "@" + c.HeadSHA
		}
	}
	return ev, nil
}

func issueKey(repo string, n int) string { return fmt.Sprintf("github:issue:%s#%d", repo, n) }
func prKey(repo string, n int) string    { return fmt.Sprintf("github:pr:%s#%d", repo, n) }

func labelNames(ls []label) string {
	names := make([]string, 0, len(ls))
	for _, l := range ls {
		names = append(names, l.Name)
	}
	return strings.Join(names, ",")
}

func fromIssue(ev *factory.Event, set func(k, v string), i *issue) {
	set("number", strconv.Itoa(i.Number))
	set("title", i.Title)
	set("body", i.Body)
	set("labels", labelNames(i.Labels))
	if i.PullRequest != nil {
		set("is_pr", "true")
	}
}

func fromPull(ev *factory.Event, set func(k, v string), pr *pull) {
	set("number", strconv.Itoa(pr.Number))
	set("title", pr.Title)
	set("body", pr.Body)
	set("labels", labelNames(pr.Labels))
	set("branch", pr.Head.Ref)
	set("sha", pr.Head.SHA)
	set("base", pr.Base.Ref)
	if ev.Name == "pull_request.closed" {
		set("merged", strconv.FormatBool(pr.Merged))
	}
}

// matches reports whether the event's fields satisfy every key of match: equal
// value (case-insensitive for "repo"), "*" for any non-empty value, and a comma
// list field (labels) matches when it contains the value.
func matches(match map[string]string, fields map[string]string) bool {
	for k, want := range match {
		got := fields[k]
		switch {
		case want == "*":
			if got == "" {
				return false
			}
		case got == want:
		case k == "repo" && strings.EqualFold(got, want):
			// GitHub owner and repository names are case-insensitive: a project that spells its URL
			// "Acme/App" still gets the events of "acme/app".
		case strings.Contains(got, ","):
			found := false
			for _, item := range strings.Split(got, ",") {
				if strings.TrimSpace(item) == want {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// eventMatches reports whether a trigger's Event selects the event name: equal,
// or the trigger names just the GitHub event ("issues") and the event is any
// action of it ("issues.labeled").
func eventMatches(trigger, name string) bool {
	if trigger == name {
		return true
	}
	return !strings.Contains(trigger, ".") && strings.HasPrefix(name, trigger+".")
}
