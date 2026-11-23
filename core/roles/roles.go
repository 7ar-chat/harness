// Package roles is the vocabulary of roles: named agent configurations (a prompt, a model, tool limits, skills)
// that a project or a user defines, that the delegate tool and factory tasks start sub-agents with, and that a
// chat run can take on. It holds the types, the store port and the resolver port; adapters/store/rolestore
// persists them and the product serves them.
package roles

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Scope says who a role belongs to.
type Scope string

// Scopes of a role. A builtin role is code (the delegate catalogue) and is never stored.
const (
	ScopeBuiltin Scope = "builtin"
	ScopeProject Scope = "project"
	ScopeUser    Scope = "user"
)

// Tool profiles (Role.ToolProfile).
const (
	// ToolsInherit leaves the tool set as the run has it.
	ToolsInherit = ""
	// ToolsReadOnly keeps only the tools that change nothing (the run is a plan run).
	ToolsReadOnly = "read-only"
	// ToolsWrite is the run's normal tool set.
	ToolsWrite = "write"
)

// Permission modes (Role.PermissionMode).
const (
	// PermNormal runs as the caller asked.
	PermNormal = "normal"
	// PermPlan runs read-only, like plan mode: the role investigates and proposes.
	PermPlan = "plan"
	// PermAsk makes risky tool calls wait for a person's approval.
	PermAsk = "ask"
)

// Limits of a role.
const (
	MaxPrompt      = 20000
	MaxDescription = 500
	MaxTitle       = 80
	MaxListed      = 100
	// MaxHistory is how many past versions a role keeps.
	MaxHistory = 10
)

// Errors of a Store.
var (
	ErrNotFound = errors.New("roles: not found")
	// ErrConflict: the name is taken in its scope.
	ErrConflict = errors.New("roles: name already in use")
	// ErrStale: the role changed since the version the caller read.
	ErrStale = errors.New("roles: the role was changed meanwhile")
	// ErrInvalid is what errors.Is matches in what Validate reports.
	ErrInvalid = errors.New("roles: invalid role")
)

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidName reports whether s can name a role: a lower case slug of at most 32 characters.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// Role is one agent configuration.
type Role struct {
	// Scope and ScopeID say whose it is: the project id, or the user's login; empty for a builtin.
	Scope   Scope  `json:"scope"`
	ScopeID string `json:"scopeId,omitempty"`
	// Name is the slug the role is called by (@name, the delegate tool's role argument, a task's role).
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// Prompt is added to the system prompt of a run that takes the role on.
	Prompt string `json:"prompt"`
	// Model "" uses the default model; ReasoningEffort "" the model's default.
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	// ToolProfile is ToolsInherit, ToolsReadOnly or ToolsWrite. AllowTools, when set, is the only set of tools
	// the run keeps; DenyTools are removed from what is left.
	ToolProfile string   `json:"toolProfile,omitempty"`
	AllowTools  []string `json:"allowTools,omitempty"`
	DenyTools   []string `json:"denyTools,omitempty"`
	// Skills are loaded for the run; MCP names the marketplace servers the run may use (empty: all it has).
	Skills []string `json:"skills,omitempty"`
	MCP    []string `json:"mcp,omitempty"`
	// PermissionMode is PermNormal (also ""), PermPlan or PermAsk.
	PermissionMode string `json:"permissionMode,omitempty"`
	Enabled        bool   `json:"enabled"`
	// Version starts at 1 and grows with every change; older versions are kept (see Version).
	Version   int       `json:"version"`
	Builtin   bool      `json:"builtin,omitempty"`
	CreatedBy string    `json:"createdBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	UpdatedBy string    `json:"updatedBy,omitempty"`
}

// ReadOnly reports whether runs of the role change nothing.
func (r Role) ReadOnly() bool { return r.ToolProfile == ToolsReadOnly || r.PermissionMode == PermPlan }

// Ref is where a role lives; it is what a run records.
type Ref struct {
	Scope   Scope
	ScopeID string
	Name    string
}

// RefOf is the reference of r.
func RefOf(r Role) Ref { return Ref{Scope: r.Scope, ScopeID: r.ScopeID, Name: r.Name} }

// refPrefix marks the entry of a run's requested skills that records its role.
const refPrefix = "role:"

// String encodes the reference as the pseudo skill name a run records ("role:project:coder:<project id>"). The
// name comes before the scope id, which may contain anything.
func (r Ref) String() string {
	return refPrefix + string(r.Scope) + ":" + r.Name + ":" + r.ScopeID
}

// ParseRef decodes a run's requested skill name; ok is false for an ordinary skill.
func ParseRef(s string) (Ref, bool) {
	rest, ok := strings.CutPrefix(s, refPrefix)
	if !ok {
		return Ref{}, false
	}
	parts := strings.SplitN(rest, ":", 3)
	if len(parts) != 3 || parts[1] == "" {
		return Ref{}, false
	}
	sc := Scope(parts[0])
	if sc != ScopeBuiltin && sc != ScopeProject && sc != ScopeUser {
		return Ref{}, false
	}
	return Ref{Scope: sc, Name: parts[1], ScopeID: parts[2]}, true
}

// RefOfRun finds the role a run recorded among its requested skills.
func RefOfRun(requested []string) (Ref, bool) {
	for _, s := range requested {
		if r, ok := ParseRef(s); ok {
			return r, true
		}
	}
	return Ref{}, false
}

// Without returns the requested skill names that are not role references.
func Without(requested []string) []string {
	return slices.DeleteFunc(slices.Clone(requested), func(s string) bool { _, ok := ParseRef(s); return ok })
}

// invalidErr is what Validate returns: errors.Is(err, ErrInvalid) is true.
type invalidErr struct{ msg string }

func (e invalidErr) Error() string        { return ErrInvalid.Error() + ": " + e.msg }
func (e invalidErr) Is(target error) bool { return target == ErrInvalid }

func invalid(msg string) error { return invalidErr{msg} }

// Validate checks and normalizes r for storing: it trims text, drops empty and duplicate list entries and
// checks the enumerations.
func (r *Role) Validate() error {
	r.Name = strings.TrimSpace(r.Name)
	r.Title = strings.TrimSpace(r.Title)
	r.Description = strings.TrimSpace(r.Description)
	r.Prompt = strings.TrimSpace(r.Prompt)
	r.Model = strings.TrimSpace(r.Model)
	r.ReasoningEffort = strings.TrimSpace(r.ReasoningEffort)
	switch {
	case !ValidName(r.Name):
		return invalid("the name must be a lower case slug of at most 32 characters (letters, digits, -)")
	case r.Scope != ScopeProject && r.Scope != ScopeUser:
		return invalid("a stored role belongs to a project or a user")
	case r.ScopeID == "":
		return invalid("the role has no owner")
	case len([]rune(r.Title)) > MaxTitle:
		return invalid("the title is too long")
	case len([]rune(r.Description)) > MaxDescription:
		return invalid("the description is too long")
	case r.Prompt == "":
		return invalid("the prompt must not be empty")
	case len([]rune(r.Prompt)) > MaxPrompt:
		return invalid("the prompt is too long")
	case r.ToolProfile != ToolsInherit && r.ToolProfile != ToolsReadOnly && r.ToolProfile != ToolsWrite:
		return invalid("unknown tool profile")
	case r.PermissionMode != "" && r.PermissionMode != PermNormal && r.PermissionMode != PermPlan && r.PermissionMode != PermAsk:
		return invalid("unknown permission mode")
	case r.ToolProfile == ToolsWrite && r.PermissionMode == PermPlan:
		return invalid("a write role cannot run in plan mode")
	}
	var err error
	if r.AllowTools, err = cleanList(r.AllowTools, "allowed tools"); err != nil {
		return err
	}
	if r.DenyTools, err = cleanList(r.DenyTools, "denied tools"); err != nil {
		return err
	}
	if r.Skills, err = cleanList(r.Skills, "skills"); err != nil {
		return err
	}
	if r.MCP, err = cleanList(r.MCP, "MCP servers"); err != nil {
		return err
	}
	return nil
}

func cleanList(in []string, what string) ([]string, error) {
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || slices.Contains(out, s) {
			continue
		}
		if len(s) > 128 {
			return nil, invalid("an entry of " + what + " is too long")
		}
		out = append(out, s)
	}
	if len(out) > MaxListed {
		return nil, invalid("too many " + what)
	}
	return out, nil
}

// Version is a past state of a role, kept when it was replaced.
type Version struct {
	Version   int       `json:"version"`
	Role      Role      `json:"role"`
	ChangedBy string    `json:"changedBy,omitempty"`
	At        time.Time `json:"at"`
}

// Store keeps the roles of projects and users.
type Store interface {
	// Create adds r (its scope, scope id and a valid name are required; Validate runs first) at version 1.
	// ErrConflict when the name is taken in the scope.
	Create(ctx context.Context, r Role, actor string) (*Role, error)
	// Update replaces the content of the role named r.Name in r's scope. The previous state goes to the history
	// (the last MaxHistory are kept) and the version grows. When r.Version is not 0 it must be the current one
	// (ErrStale otherwise). ErrNotFound for an unknown role.
	Update(ctx context.Context, r Role, actor string) (*Role, error)
	// Get returns ErrNotFound for an unknown role.
	Get(ctx context.Context, scope Scope, scopeID, name string) (*Role, error)
	// List returns the scope's roles ordered by name.
	List(ctx context.Context, scope Scope, scopeID string) ([]Role, error)
	// History returns the past versions of a role, newest first.
	History(ctx context.Context, scope Scope, scopeID, name string) ([]Version, error)
	Delete(ctx context.Context, scope Scope, scopeID, name string) error
	// DeleteScope removes every role of a scope (a deleted project); none is not an error.
	DeleteScope(ctx context.Context, scope Scope, scopeID string) error
}

// Query says on behalf of whom a role name is looked up.
type Query struct {
	// Login is the person asking: their personal roles are in reach.
	Login string
	// ProjectID is the project of the work; when empty, ThreadID's project is used.
	ProjectID string
	ThreadID  string
}

// Resolver finds the role a name stands for. The product supplies it: a personal role overrides a project's,
// which overrides a builtin of the same name.
type Resolver interface {
	// Resolve returns the enabled role called name, ErrNotFound when there is none.
	Resolve(ctx context.Context, q Query, name string) (*Role, error)
	// Get returns the role a run recorded, even when it was disabled since: ErrNotFound when it is gone.
	Get(ctx context.Context, ref Ref) (*Role, error)
	// Available lists the enabled roles in reach, for rosters.
	Available(ctx context.Context, q Query) ([]Role, error)
}
