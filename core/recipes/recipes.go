// Package recipes is the vocabulary of recipes: ready-made, parameterized definitions of a recurring schedule, a
// one-off task or a trigger that a person starts in two clicks and that a team shares. A recipe is data: its text has
// {{param}} placeholders that a tiny strict templater fills (no code runs, unknown parameters are refused, values are
// length-limited). The builtin gallery is code (Builtins); the recipes people write are kept by a Store
// (adapters/store/recipestore) and served by the product.
package recipes

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Kind says what applying a recipe creates.
type Kind string

// Kinds of a recipe.
const (
	KindSchedule Kind = "schedule"
	KindTask     Kind = "task"
	KindTrigger  Kind = "trigger"
)

// Category groups the gallery.
type Category string

// Categories of a recipe.
const (
	CatMaintenance Category = "maintenance"
	CatQuality     Category = "quality"
	CatDocs        Category = "docs"
	CatReporting   Category = "reporting"
	CatSecurity    Category = "security"
	CatOnboarding  Category = "onboarding"
)

// Categories lists them in display order.
func Categories() []Category {
	return []Category{CatMaintenance, CatQuality, CatDocs, CatReporting, CatSecurity, CatOnboarding}
}

// Scope says who can see and use a recipe.
type Scope string

// Scopes of a recipe. A builtin recipe is code and never stored; instance recipes are shared with everyone on the
// deployment (an administrator publishes them); project recipes with the members of a project; personal ones are the
// author's own.
const (
	ScopeBuiltin  Scope = "builtin"
	ScopeInstance Scope = "instance"
	ScopeProject  Scope = "project"
	ScopePersonal Scope = "personal"
)

// Parameter types (Param.Type).
const (
	TypeText    = "text"
	TypeNumber  = "number"
	TypeChoice  = "choice"
	TypeBoolean = "boolean"
	TypeCron    = "cron"
)

// Limits.
const (
	MaxName       = 80
	MaxSummary    = 300
	MaxBodyText   = 20000
	MaxParams     = 12
	MaxChoices    = 20
	MaxValue      = 2000
	MaxHistory    = 10
	MaxImportSize = 128 << 10
	// MaxPerScope bounds the recipes of one project, one person or the instance.
	MaxPerScope = 200
)

// Errors of the package and of a Store.
var (
	ErrNotFound = errors.New("recipes: not found")
	// ErrStale: the recipe changed since the version the caller read.
	ErrStale = errors.New("recipes: the recipe was changed meanwhile")
	// ErrInvalid is what errors.Is matches in what Validate and Render report.
	ErrInvalid = errors.New("recipes: invalid")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Param is one value the person fills in when starting a recipe.
type Param struct {
	// Key is the name used in the text as {{key}}: lower case letters, digits and underscores.
	Key   string `json:"key"`
	Label string `json:"label"`
	// Help is one plain sentence under the field.
	Help string `json:"help,omitempty"`
	// Type is text, number, choice, boolean or cron.
	Type string `json:"type"`
	// Choices are the allowed values of a choice.
	Choices  []string `json:"choices,omitempty"`
	Default  string   `json:"default,omitempty"`
	Required bool     `json:"required,omitempty"`
}

// Body is the definition a recipe creates, with {{param}} placeholders in every text. Which fields are used depends
// on the kind:
//
//	schedule  Name, Prompt (what the agent is told), Cron, Timezone
//	task      Title, Prompt (the task's spec)
//	trigger   Name, Title, Prompt (the spec of each task), and Cron (a schedule) or Event (a GitHub event such as
//	          issues.opened, with optional Match); the title and spec are Go templates over the event
//	          ({{.title}}, {{.URL}}), so only {{key}} placeholders are filled by the recipe.
type Body struct {
	Name     string            `json:"name,omitempty"`
	Title    string            `json:"title,omitempty"`
	Prompt   string            `json:"prompt,omitempty"`
	Cron     string            `json:"cron,omitempty"`
	Timezone string            `json:"timezone,omitempty"`
	Event    string            `json:"event,omitempty"`
	Match    map[string]string `json:"match,omitempty"`
}

// Suggest holds the settings a recipe proposes; the person can change them under "Advanced".
type Suggest struct {
	// Role is a role name of the project ("" for the default).
	Role string `json:"role,omitempty"`
	// Policy is readonly or default for a schedule; supervised, sandboxed or autonomous for a task or trigger.
	Policy string `json:"policy,omitempty"`
	// MaxUSD and MaxTokens cap what one run (or task) may spend.
	MaxUSD    float64 `json:"maxUsd,omitempty"`
	MaxTokens int64   `json:"maxTokens,omitempty"`
	// TimeoutMinutes is the time limit of a scheduled run.
	TimeoutMinutes int `json:"timeoutMinutes,omitempty"`
	// BaseBranch is where a task's pull request goes ("" for the project's default).
	BaseBranch string `json:"baseBranch,omitempty"`
}

// Recipe is one template.
type Recipe struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Summary  string   `json:"summary"`
	Category Category `json:"category"`
	Kind     Kind     `json:"kind"`
	Body     Body     `json:"body"`
	Params   []Param  `json:"params"`
	Suggest  Suggest  `json:"suggest"`
	// Version starts at 1 and grows with every change; older versions are kept (Version).
	Version int   `json:"version"`
	Scope   Scope `json:"scope"`
	// ScopeID is the project id of a project recipe and the author's login of a personal one.
	ScopeID   string    `json:"scopeId,omitempty"`
	Author    string    `json:"author,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Uses counts how many times it was applied. The store keeps the count apart; List and Get leave it zero.
	Uses int `json:"uses"`
}

// Version is a past version of a recipe.
type Version struct {
	Version   int       `json:"version"`
	Recipe    Recipe    `json:"recipe"`
	ChangedBy string    `json:"changedBy,omitempty"`
	At        time.Time `json:"at"`
}

// Store persists the recipes of the instance, projects and people. Builtins are not stored.
type Store interface {
	// Create stores r with a fresh id, version 1 and author actor.
	Create(ctx context.Context, r Recipe, actor string) (*Recipe, error)
	// Update replaces the editable fields of r.ID and bumps its version, keeping the old one in the history.
	// ErrStale when r.Version is not 0 and differs from the stored one.
	Update(ctx context.Context, r Recipe, actor string) (*Recipe, error)
	Get(ctx context.Context, id string) (*Recipe, error)
	// List returns the stored recipes of a scope and scope id; an empty scope lists all of them.
	List(ctx context.Context, scope Scope, scopeID string) ([]Recipe, error)
	Delete(ctx context.Context, id string) error
	// History returns the past versions, newest first (at most MaxHistory).
	History(ctx context.Context, id string) ([]Version, error)
	// DeleteScope removes every recipe of the scope (a deleted project).
	DeleteScope(ctx context.Context, scope Scope, scopeID string) error
	// AddUse counts one application of the recipe (stored or builtin id); Uses returns the counts by id.
	AddUse(ctx context.Context, id string) error
	Uses(ctx context.Context) (map[string]int, error)
}

var (
	keyRe   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	tokenRe = regexp.MustCompile(`\{\{\s*([a-z][a-z0-9_]{0,31})\s*\}\}`)
	// anyTokenRe finds look-alikes (upper case, dashes) so a typo is reported instead of being left in the text.
	anyTokenRe = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_-]*)\s*\}\}`)
)

// ValidCategory reports whether c is one of Categories.
func ValidCategory(c Category) bool {
	for _, k := range Categories() {
		if k == c {
			return true
		}
	}
	return false
}

// texts returns the body fields that carry placeholders.
func (b Body) texts() []string {
	return []string{b.Name, b.Title, b.Prompt, b.Cron, b.Timezone, b.Event}
}

// Validate checks the recipe as an author wrote it: the fields, the parameters and that every placeholder of the text
// names a declared parameter.
func (r *Recipe) Validate() error {
	r.Name, r.Summary = strings.TrimSpace(r.Name), strings.TrimSpace(r.Summary)
	switch {
	case r.Name == "":
		return invalid("name is required")
	case len([]rune(r.Name)) > MaxName:
		return invalid("name is at most %d characters", MaxName)
	case len([]rune(r.Summary)) > MaxSummary:
		return invalid("summary is at most %d characters", MaxSummary)
	case !ValidCategory(r.Category):
		return invalid("category must be one of maintenance, quality, docs, reporting, security, onboarding")
	}
	b := r.Body
	for _, t := range b.texts() {
		if len(t) > MaxBodyText {
			return invalid("a text is at most %d characters", MaxBodyText)
		}
	}
	need := func(field, v string) error {
		if strings.TrimSpace(v) == "" {
			return invalid("%s is required for a %s", field, r.Kind)
		}
		return nil
	}
	var errs []error
	switch r.Kind {
	case KindSchedule:
		errs = append(errs, need("name", b.Name), need("prompt", b.Prompt), need("cron", b.Cron))
		if b.Title != "" || b.Event != "" || len(b.Match) > 0 {
			errs = append(errs, invalid("title and event do not belong to a schedule"))
		}
	case KindTask:
		errs = append(errs, need("title", b.Title), need("prompt", b.Prompt))
		if b.Name != "" || b.Cron != "" || b.Event != "" || len(b.Match) > 0 {
			errs = append(errs, invalid("name, cron and event do not belong to a task"))
		}
	case KindTrigger:
		errs = append(errs, need("name", b.Name), need("title", b.Title), need("prompt", b.Prompt))
		if (b.Cron == "") == (b.Event == "") {
			errs = append(errs, invalid("a trigger needs either a cron schedule or an event, not both"))
		}
	default:
		return invalid("kind must be schedule, task or trigger")
	}
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	if len(r.Params) > MaxParams {
		return invalid("at most %d parameters", MaxParams)
	}
	declared := map[string]Param{}
	for i := range r.Params {
		p := &r.Params[i]
		p.Label = strings.TrimSpace(p.Label)
		if !keyRe.MatchString(p.Key) {
			return invalid("parameter key %q must be lower case letters, digits and underscores, starting with a letter", p.Key)
		}
		if _, dup := declared[p.Key]; dup {
			return invalid("parameter %q is declared twice", p.Key)
		}
		if p.Label == "" || len(p.Label) > 80 || len(p.Help) > 300 {
			return invalid("parameter %q needs a label of at most 80 characters and help of at most 300", p.Key)
		}
		switch p.Type {
		case TypeText, TypeNumber, TypeBoolean, TypeCron:
			if len(p.Choices) > 0 {
				return invalid("parameter %q has choices but is not a choice", p.Key)
			}
		case TypeChoice:
			if len(p.Choices) < 2 || len(p.Choices) > MaxChoices {
				return invalid("parameter %q needs between 2 and %d choices", p.Key, MaxChoices)
			}
		default:
			return invalid("parameter %q: type must be text, number, choice, boolean or cron", p.Key)
		}
		if p.Default != "" {
			if _, err := p.check(p.Default); err != nil {
				return invalid("default of %q: %v", p.Key, err)
			}
		}
		declared[p.Key] = *p
	}
	for _, t := range b.texts() {
		for _, m := range anyTokenRe.FindAllStringSubmatch(t, -1) {
			if _, ok := declared[m[1]]; !ok {
				return invalid("the text uses {{%s}} but no parameter has that key", m[1])
			}
		}
	}
	for k, v := range b.Match {
		if len(k) > 40 || len(v) > 200 {
			return invalid("match entries are short")
		}
	}
	if r.Suggest.MaxUSD < 0 || r.Suggest.MaxTokens < 0 || r.Suggest.TimeoutMinutes < 0 || r.Suggest.TimeoutMinutes > 240 {
		return invalid("suggested limits are out of range")
	}
	return nil
}

// check validates one value against the parameter's type and returns it normalized.
func (p Param) check(v string) (string, error) {
	if len(v) > MaxValue {
		return "", fmt.Errorf("at most %d characters", MaxValue)
	}
	for _, c := range v {
		if c < 0x20 && c != '\n' && c != '\t' || c == 0x7f {
			return "", errors.New("control characters are not allowed")
		}
	}
	v = strings.TrimSpace(v)
	switch p.Type {
	case TypeNumber:
		if !numRe.MatchString(v) {
			return "", errors.New("must be a number")
		}
	case TypeBoolean:
		if v != "true" && v != "false" {
			return "", errors.New("must be true or false")
		}
	case TypeChoice:
		for _, c := range p.Choices {
			if c == v {
				return v, nil
			}
		}
		return "", errors.New("is not one of the choices")
	case TypeCron:
		if err := checkCronShape(v); err != nil {
			return "", err
		}
	}
	return v, nil
}

var numRe = regexp.MustCompile(`^-?[0-9]{1,12}(\.[0-9]{1,6})?$`)

// checkCronShape accepts a five-field expression, a macro (@daily) or an @every interval; the scheduler validates the
// expression itself when the schedule is created.
func checkCronShape(v string) error {
	if strings.ContainsAny(v, "\n\r") || v == "" {
		return errors.New("must be a schedule such as 0 2 * * *")
	}
	if strings.HasPrefix(v, "@") {
		return nil
	}
	if len(strings.Fields(v)) != 5 {
		return errors.New("must have five fields, for example 0 2 * * *")
	}
	return nil
}

// Values resolves what the person filled in: unknown keys are refused, defaults fill the gaps, a required value must
// be there and every value must fit its type and length.
func (r Recipe) Values(given map[string]string) (map[string]string, error) {
	known := map[string]bool{}
	for _, p := range r.Params {
		known[p.Key] = true
	}
	for k := range given {
		if !known[k] {
			return nil, invalid("unknown parameter %q", k)
		}
	}
	out := make(map[string]string, len(r.Params))
	for _, p := range r.Params {
		v, ok := given[p.Key]
		if !ok || strings.TrimSpace(v) == "" {
			v = p.Default
		}
		if strings.TrimSpace(v) == "" {
			if p.Required {
				return nil, invalid("%s is required", p.Label)
			}
			out[p.Key] = ""
			continue
		}
		c, err := p.check(v)
		if err != nil {
			return nil, invalid("%s: %v", p.Label, err)
		}
		out[p.Key] = c
	}
	return out, nil
}

// Render fills the {{key}} placeholders of text from values in one pass: a value is never searched for placeholders
// again, so it cannot pull in another parameter or anything else. A key without a value is refused. For a trigger the
// text is also a Go template, so the delimiter of a value is escaped there.
func Render(text string, values map[string]string, goTemplate bool) (string, error) {
	var missing string
	out := tokenRe.ReplaceAllStringFunc(text, func(tok string) string {
		key := tokenRe.FindStringSubmatch(tok)[1]
		v, ok := values[key]
		if !ok {
			missing = key
			return ""
		}
		if goTemplate {
			v = strings.ReplaceAll(v, "{{", `{{"{{"}}`)
		}
		return v
	})
	if missing != "" {
		return "", invalid("no value for {{%s}}", missing)
	}
	return out, nil
}

// Rendered is a recipe with its parameters filled in.
type Rendered struct {
	Kind    Kind    `json:"kind"`
	Body    Body    `json:"body"`
	Suggest Suggest `json:"suggest"`
}

// Apply renders the body of r with the values the person gave.
func (r Recipe) Apply(given map[string]string) (*Rendered, error) {
	vals, err := r.Values(given)
	if err != nil {
		return nil, err
	}
	gt := r.Kind == KindTrigger
	out := Rendered{Kind: r.Kind, Suggest: r.Suggest}
	b := r.Body
	for _, f := range []struct {
		src string
		dst *string
	}{{b.Name, &out.Body.Name}, {b.Title, &out.Body.Title}, {b.Prompt, &out.Body.Prompt}, {b.Cron, &out.Body.Cron},
		{b.Timezone, &out.Body.Timezone}, {b.Event, &out.Body.Event}} {
		if *f.dst, err = Render(f.src, vals, gt); err != nil {
			return nil, err
		}
	}
	for k, v := range b.Match {
		if out.Body.Match == nil {
			out.Body.Match = map[string]string{}
		}
		if out.Body.Match[k], err = Render(v, vals, false); err != nil {
			return nil, err
		}
	}
	for _, s := range []string{out.Body.Name, out.Body.Title, out.Body.Cron, out.Body.Event} {
		if strings.ContainsAny(s, "\n\r") {
			return nil, invalid("a value put a line break into a one-line field")
		}
	}
	out.Body.Name, out.Body.Title = strings.TrimSpace(out.Body.Name), strings.TrimSpace(out.Body.Title)
	return &out, nil
}
