// Package tapd reads the open stories (requirements) of a TAPD project, Tencent's project tool, through its open
// API (https://open.tapd.cn/document/api-doc/). It is the provider behind the requirements import for TAPD.
//
// What the official documentation confirms, and what this package therefore does:
//
//   - GET {base}/stories?workspace_id=&limit=&page= lists stories; limit is at most 200, page starts at 1; the
//     response is {"status":1,"data":[{"Story":{...}}],"info":"success"} and status != 1 is an error.
//   - GET {base}/workflows/last_steps?system=story&workspace_id= answers {"status":1,"data":{"resolved":"..."}}: the
//     English names of the end statuses of the project's story workflows.
//   - Authentication is HTTP Basic (api_user:api_password) or an OAuth access token as "Authorization: Bearer".
//
// What the documentation does not say (docs/requirements.md lists these as assumptions): the separator of an
// enumerated status filter, and the address of a story's page in the web app. So "open" is decided here, on the
// client, from each story's status field, never by a server-side filter, and the story URL is built from the
// pattern https://www.tapd.cn/{workspace}/prong/stories/view/{id}.
package tapd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DefaultBaseURL is the API of the public TAPD service.
const DefaultBaseURL = "https://api.tapd.cn"

const (
	// PageSize is the largest page the stories API serves.
	PageSize = 200
	// MaxImport caps the stories one call returns.
	MaxImport = 500
	// MaxScan caps the stories read per call, open or not (a project's closed stories are most of its stories).
	MaxScan = 2000
	// maxBody caps a response.
	maxBody = 8 << 20
)

// Errors a caller tells apart.
var (
	// ErrAuth: TAPD refused the credentials.
	ErrAuth = errors.New("tapd: the credentials were refused")
	// ErrNoStatusInfo: neither the workflow end statuses nor a user filter say which stories are open.
	ErrNoStatusInfo = errors.New("tapd: cannot tell which stories are open")
)

// Config says where the project is and how to sign in. Either User and Password (an API account) or Token (an
// OAuth access token) is set.
type Config struct {
	BaseURL     string `json:"baseUrl,omitempty"`
	WorkspaceID string `json:"workspaceId"`
	User        string `json:"user,omitempty"`
	Password    string `json:"password,omitempty"`
	Token       string `json:"token,omitempty"`
	// OpenStatuses overrides the computed open set: the English status names (planning, developing...) that count
	// as open, comma separated.
	OpenStatuses string `json:"openStatuses,omitempty"`
}

// Story is one TAPD story as the import needs it.
type Story struct {
	ID          string
	Name        string
	Description string // plain text
	Status      string
	Priority    string // the label, or the legacy value
	Owner       string
	Creator     string
	URL         string
}

// Result is what one listing found.
type Result struct {
	Stories []Story
	// Source says how "open" was decided: "workflow" (the end statuses of the story workflows) or "filter" (the
	// user's list of open statuses).
	Source string
	// Scanned is how many stories were read; Truncated that the scan or the cap stopped before the end.
	Scanned   int
	Truncated bool
}

// Client talks to one TAPD deployment.
type Client struct {
	HTTP *http.Client
	Cfg  Config
}

// ValidWorkspaceID reports whether s looks like a project id (digits).
func ValidWorkspaceID(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ParseStatuses splits a user list of statuses ("planning, developing" or with | or ;).
func ParseStatuses(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '|' || r == ';' || r == '\n' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// base returns the API base URL without a trailing slash.
func (c *Client) base() string {
	b := strings.TrimSpace(c.Cfg.BaseURL)
	if b == "" {
		b = DefaultBaseURL
	}
	return strings.TrimRight(b, "/")
}

// StoryURL is the address of a story's page in the web app.
func StoryURL(baseURL, workspace, id string) string {
	web := "https://www.tapd.cn"
	if b := strings.TrimSpace(baseURL); b != "" {
		if u, err := url.Parse(b); err == nil && u.Host != "" && u.Hostname() != "api.tapd.cn" {
			// A private deployment serves the web app on the host of its API.
			web = u.Scheme + "://" + u.Host
		}
	}
	return web + "/" + url.PathEscape(workspace) + "/prong/stories/view/" + url.PathEscape(id)
}

type envelope struct {
	Status json.Number     `json:"status"`
	Data   json.RawMessage `json:"data"`
	Info   json.RawMessage `json:"info"`
}

func (c *Client) get(ctx context.Context, path string, q url.Values) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "7ar-requirements-import")
	if c.Cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Cfg.Token)
	} else {
		req.SetBasicAuth(c.Cfg.User, c.Cfg.Password)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		// The error of net/http names the URL, which may be a private host: it is not passed on.
		return nil, errors.New("tapd: the server could not be reached")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, errors.New("tapd: reading the response failed")
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrAuth
	}
	var env envelope
	if jerr := json.Unmarshal(body, &env); jerr != nil {
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("tapd: the server answered %d", resp.StatusCode)
		}
		return nil, errors.New("tapd: the answer is not the expected JSON")
	}
	if env.Status.String() != "1" {
		var info string
		_ = json.Unmarshal(env.Info, &info)
		if len(info) > 200 {
			info = info[:200]
		}
		// TAPD may answer 200 with status 401 or 403 in the envelope for bad credentials.
		if s := env.Status.String(); s == "401" || s == "403" {
			return nil, ErrAuth
		}
		return nil, fmt.Errorf("tapd: status %s: %s", env.Status.String(), strings.TrimSpace(info))
	}
	return env.Data, nil
}

// EndStatuses returns the English names of the end statuses of the project's story workflows.
func (c *Client) EndStatuses(ctx context.Context) ([]string, error) {
	data, err := c.get(ctx, "/workflows/last_steps", url.Values{"system": {"story"}, "workspace_id": {c.Cfg.WorkspaceID}})
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		// An empty set is sometimes sent as [].
		var arr []json.RawMessage
		if json.Unmarshal(data, &arr) == nil && len(arr) == 0 {
			return nil, nil
		}
		return nil, errors.New("tapd: the end statuses are not in the expected form")
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// Check asks for one story: it proves the credentials and the workspace id.
func (c *Client) Check(ctx context.Context) error {
	if !ValidWorkspaceID(c.Cfg.WorkspaceID) {
		return errors.New("tapd: the workspace id must be a number")
	}
	_, err := c.get(ctx, "/stories", url.Values{"workspace_id": {c.Cfg.WorkspaceID}, "limit": {"1"}, "page": {"1"}, "fields": {"id"}})
	return err
}

// OpenStories lists up to max open stories (at most MaxImport), newest first. With OpenStatuses set, those
// statuses are open; otherwise every status that is not an end status of the story workflows. When the workflow
// endpoint fails and no list is set the error wraps ErrNoStatusInfo.
func (c *Client) OpenStories(ctx context.Context, max int) (Result, error) {
	if max <= 0 || max > MaxImport {
		max = MaxImport
	}
	if !ValidWorkspaceID(c.Cfg.WorkspaceID) {
		return Result{}, errors.New("tapd: the workspace id must be a number")
	}
	res := Result{}
	user := ParseStatuses(c.Cfg.OpenStatuses)
	var isOpen func(status string) bool
	if len(user) > 0 {
		res.Source = "filter"
		set := map[string]bool{}
		for _, s := range user {
			set[strings.ToLower(s)] = true
		}
		isOpen = func(s string) bool { return set[strings.ToLower(s)] }
	} else {
		ends, err := c.EndStatuses(ctx)
		if err != nil {
			if errors.Is(err, ErrAuth) {
				return res, err
			}
			return res, fmt.Errorf("%w: %v", ErrNoStatusInfo, err)
		}
		res.Source = "workflow"
		set := map[string]bool{}
		for _, s := range ends {
			set[strings.ToLower(s)] = true
		}
		isOpen = func(s string) bool { return !set[strings.ToLower(s)] }
	}
	for page := 1; res.Scanned < MaxScan && len(res.Stories) < max; page++ {
		q := url.Values{"workspace_id": {c.Cfg.WorkspaceID}, "limit": {strconv.Itoa(PageSize)}, "page": {strconv.Itoa(page)}, "order": {"created desc"}}
		data, err := c.get(ctx, "/stories", q)
		if err != nil {
			return res, err
		}
		var items []struct {
			Story rawStory `json:"Story"`
		}
		if err := json.Unmarshal(data, &items); err != nil {
			var arr []json.RawMessage // an empty page may come as [] or {}
			if json.Unmarshal(data, &arr) != nil && string(data) != "{}" {
				return res, errors.New("tapd: the stories are not in the expected form")
			}
			items = nil
		}
		for _, it := range items {
			res.Scanned++
			s := it.Story
			if s.ID.String() == "" || s.Name == "" || !isOpen(s.Status) {
				continue
			}
			if len(res.Stories) >= max {
				res.Truncated = true
				break
			}
			pr := s.PriorityLabel
			if pr == "" {
				pr = s.Priority
			}
			res.Stories = append(res.Stories, Story{ID: s.ID.String(), Name: s.Name, Description: HTMLToText(s.Description), Status: s.Status,
				Priority: pr, Owner: s.Owner, Creator: s.Creator, URL: StoryURL(c.Cfg.BaseURL, c.Cfg.WorkspaceID, s.ID.String())})
		}
		if len(items) < PageSize {
			return res, nil
		}
	}
	res.Truncated = true
	return res, nil
}

// rawStory takes ids and strings in whatever JSON type TAPD sends them.
type rawStory struct {
	ID            flex   `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Status        string `json:"status"`
	Priority      string `json:"priority"`
	PriorityLabel string `json:"priority_label"`
	Owner         string `json:"owner"`
	Creator       string `json:"creator"`
}

// flex is a scalar that may be a JSON string or number; String is its text.
type flex struct{ s string }

func (f *flex) UnmarshalJSON(b []byte) error {
	var v any
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return err
	}
	switch x := v.(type) {
	case string:
		f.s = strings.TrimSpace(x)
	case json.Number:
		f.s = x.String()
	}
	return nil
}

func (f flex) String() string { return f.s }

// PriorityRank maps a TAPD priority label to the requirement priority (higher first): urgent 4, high 3, medium 2,
// low 1, anything else 0.
func PriorityRank(label string) int {
	l := strings.ToLower(strings.TrimSpace(label))
	switch {
	case l == "":
		return 0
	case strings.Contains(l, "urgent") || strings.Contains(l, "critical") || strings.Contains(l, "紧急"):
		return 4
	case strings.Contains(l, "nice") || strings.Contains(l, "low") || strings.Contains(l, "低"):
		return 1
	case strings.Contains(l, "high") || strings.Contains(l, "高"):
		return 3
	case strings.Contains(l, "mid") || strings.Contains(l, "medium") || strings.Contains(l, "normal") || strings.Contains(l, "中"):
		return 2
	}
	return 0
}

// HTMLToText turns the HTML of a story description into plain text: tags are dropped (never interpreted), script
// and style content with them, block ends and <br> become line breaks, list items become "- ", and entities are
// decoded. What remains is text only; a decoded "<script>" is just characters.
func HTMLToText(src string) string {
	if src == "" {
		return ""
	}
	var b strings.Builder
	i := 0
	for i < len(src) {
		if src[i] != '<' {
			j := strings.IndexByte(src[i:], '<')
			if j < 0 {
				j = len(src) - i
			}
			b.WriteString(src[i : i+j])
			i += j
			continue
		}
		if strings.HasPrefix(src[i:], "<!--") {
			end := strings.Index(src[i+4:], "-->")
			if end < 0 {
				break
			}
			i += 4 + end + 3
			continue
		}
		end := tagEnd(src, i)
		if end < 0 { // a lone "<" that opens no tag
			b.WriteByte('<')
			i++
			continue
		}
		name, closing := tagName(src[i+1 : end])
		i = end + 1
		switch name {
		case "script", "style":
			if !closing {
				if k := indexFold(src[i:], "</"+name); k >= 0 {
					i += k
					if e := strings.IndexByte(src[i:], '>'); e >= 0 {
						i += e + 1
					} else {
						i = len(src)
					}
				} else {
					i = len(src)
				}
			}
		case "br", "hr":
			b.WriteByte('\n')
		case "p", "div", "tr", "h1", "h2", "h3", "h4", "h5", "h6", "ul", "ol", "table", "blockquote", "pre":
			lineBreak(&b)
		case "li":
			lineBreak(&b)
			if !closing {
				b.WriteString("- ")
			}
		case "td", "th":
			b.WriteByte(' ')
		}
	}
	text := html.UnescapeString(b.String())
	text = strings.Map(func(r rune) rune {
		switch {
		case r == ' ':
			return ' '
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f || r == utf8.RuneError:
			return -1
		}
		return r
	}, text)
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	var out []string
	blank := 0
	for _, l := range lines {
		l = strings.Join(strings.Fields(l), " ")
		if l == "" {
			if blank++; blank > 1 || len(out) == 0 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// tagEnd returns the index of the ">" closing the tag opened at src[i], honouring quoted attribute values; -1 when
// the "<" is not followed by a tag name or "/" and a name.
func tagEnd(src string, i int) int {
	j := i + 1
	if j < len(src) && src[j] == '/' {
		j++
	}
	if j >= len(src) || !(src[j] >= 'a' && src[j] <= 'z' || src[j] >= 'A' && src[j] <= 'Z') {
		return -1
	}
	var quote byte
	for ; j < len(src); j++ {
		c := src[j]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return j
		}
	}
	return -1
}

func tagName(inner string) (name string, closing bool) {
	if strings.HasPrefix(inner, "/") {
		closing, inner = true, inner[1:]
	}
	end := strings.IndexAny(inner, " \t\r\n/")
	if end < 0 {
		end = len(inner)
	}
	return strings.ToLower(inner[:end]), closing
}

func indexFold(s, sub string) int {
	return strings.Index(strings.ToLower(s), strings.ToLower(sub))
}

// lineBreak ends the current line unless it is already ended.
func lineBreak(b *strings.Builder) {
	if s := b.String(); s != "" && !strings.HasSuffix(s, "\n") {
		b.WriteByte('\n')
	}
}
