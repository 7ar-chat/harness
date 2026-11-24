// Package executors is the vocabulary of external coding-agent executors: a CLI agent (Claude Code, Codex,
// OpenCode, OMP, or a command a project defines) that 7ar runs non-interactively inside a run's sandbox, in place of
// its own loop, under the same safety rails. It holds the types, the builtin adapters (the command of each CLI
// and the parser of its output), the store port of the project-defined ones and the ports the product supplies.
// features/executors runs them; the product serves and seals their credentials.
package executors

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Kind says where an executor comes from.
type Kind string

// Kinds of an executor.
const (
	// KindBuiltin is code: the adapters of this package.
	KindBuiltin Kind = "builtin"
	// KindCustom is a command template a project's owner defined.
	KindCustom Kind = "custom"
)

// Level is how much the CLI may do, derived from the task's policy and the role.
type Level string

// Levels, from the safest.
const (
	// LevelReadOnly: the CLI investigates and changes nothing (a read-only role, a plan run).
	LevelReadOnly Level = "read-only"
	// LevelEdit: the CLI edits files and runs commands in the workspace (the supervised and sandboxed policies).
	LevelEdit Level = "edit"
	// LevelFull: the CLI asks for nothing (the autonomous policy; the sandbox is the boundary).
	LevelFull Level = "full"
)

// Output parsers (Executor.Parser).
const (
	ParserText         = "text"
	ParserClaudeJSON   = "claude-stream-json"
	ParserCodexJSONL   = "codex-jsonl"
	ParserOMPJSON      = "omp-json"
	ParserOpenCodeJSON = "opencode-json"
)

// Names of the builtin executors.
const (
	ClaudeCode = "claude-code"
	Codex      = "codex"
	OpenCode   = "opencode"
	OMP        = "omp"
)

// Limits.
const (
	MaxCommand     = 4000
	MaxTitle       = 80
	MaxDescription = 500
	MaxEnv         = 8
	// MaxTask is the longest task text handed to a CLI (it travels in an environment variable).
	MaxTask = 100_000
	// MaxListed bounds the custom executors of a project.
	MaxListed = 50
)

// Errors.
var (
	ErrNotFound = errors.New("executors: not found")
	ErrConflict = errors.New("executors: name already in use")
	ErrInvalid  = errors.New("executors: invalid executor")
	// ErrReadOnlyUnsupported: the CLI has no read-only mode, so it cannot serve a read-only role.
	ErrReadOnlyUnsupported = errors.New("executors: this CLI has no read-only mode")
)

var (
	// At most 24 characters: the sealed credentials of an executor are stored under "exec_<name>", a 32 character slug.
	nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)
	envRe  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	binRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+-]{0,127}$`)
)

// ValidName reports whether s can name an executor.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// forbiddenEnv are the variables a credential may never set: they would change what runs, not authenticate it.
var forbiddenEnv = []string{"PATH", "HOME", "SHELL", "IFS", "BASH_ENV", "ENV", "PWD", "USER", "LD_PRELOAD", "LD_LIBRARY_PATH",
	"DYLD_INSERT_LIBRARIES", "NODE_OPTIONS", "PYTHONPATH", "PYTHONSTARTUP", "GIT_SSH_COMMAND", "GIT_ASKPASS"}

// Executor is one external coding agent.
type Executor struct {
	// Name is the slug the executor is called by (the run_executor tool's executor argument, a role's executor).
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Kind        Kind   `json:"kind"`
	// ProjectID owns a custom executor.
	ProjectID string `json:"projectId,omitempty"`
	// Command is the shell command template. {task} stands for the task text (always passed as the environment
	// variable HARNESS_TASK, never spliced into the command line) and {mode} for the flags of the level.
	Command string `json:"command"`
	// ModeFlags are the flags {mode} expands to per level. A level with no entry gets no flags, except
	// read-only, which the CLI then cannot serve (ErrReadOnlyUnsupported).
	ModeFlags map[Level]string `json:"modeFlags,omitempty"`
	// Binary is what the installer check looks for (command -v); InstallCommand installs it, when the run is
	// allowed to ("" : the CLI must be in the sandbox image).
	Binary         string `json:"binary"`
	InstallCommand string `json:"installCommand,omitempty"`
	// CredentialEnv names the environment variables a user may set for this executor (their API key, base URL).
	// They are sealed per user and injected only into the process of a run.
	CredentialEnv []string `json:"credentialEnv,omitempty"`
	// Parser is how the output is read: ParserText (default), ParserClaudeJSON, ParserCodexJSONL or ParserOMPJSON.
	Parser    string    `json:"parser,omitempty"`
	Enabled   bool      `json:"enabled"`
	CreatedBy string    `json:"createdBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// SupportsReadOnly reports whether the CLI can run read-only.
func (e Executor) SupportsReadOnly() bool { return strings.TrimSpace(e.ModeFlags[LevelReadOnly]) != "" }

// Validate checks and normalizes a custom executor for storing.
func (e *Executor) Validate() error {
	e.Name = strings.TrimSpace(e.Name)
	e.Title = strings.TrimSpace(e.Title)
	e.Description = strings.TrimSpace(e.Description)
	e.Command = strings.TrimSpace(e.Command)
	e.Binary = strings.TrimSpace(e.Binary)
	e.InstallCommand = strings.TrimSpace(e.InstallCommand)
	e.Parser = strings.TrimSpace(e.Parser)
	e.Kind = KindCustom
	switch {
	case !ValidName(e.Name):
		return invalid("the name must be a lower case slug of at most 24 characters (letters, digits, -)")
	case IsBuiltin(e.Name):
		return invalid("that name is a builtin executor")
	case e.ProjectID == "":
		return invalid("a custom executor belongs to a project")
	case len([]rune(e.Title)) > MaxTitle:
		return invalid("the title is too long")
	case len([]rune(e.Description)) > MaxDescription:
		return invalid("the description is too long")
	case e.Command == "":
		return invalid("the command must not be empty")
	case len(e.Command) > MaxCommand:
		return invalid("the command is too long")
	case !strings.Contains(e.Command, "{task}"):
		return invalid("the command must contain {task}, where the task goes")
	case strings.ContainsAny(e.Command, "\x00\r"):
		return invalid("the command contains control characters")
	case !binRe.MatchString(e.Binary):
		return invalid("name the program the command runs (binary), for the installer check")
	case e.Parser != "" && e.Parser != ParserText && e.Parser != ParserClaudeJSON && e.Parser != ParserCodexJSONL && e.Parser != ParserOMPJSON && e.Parser != ParserOpenCodeJSON:
		return invalid("unknown output parser")
	case len(e.InstallCommand) > MaxCommand:
		return invalid("the install command is too long")
	case len(e.CredentialEnv) > MaxEnv:
		return invalid("too many credential variables")
	}
	var env []string
	for _, n := range e.CredentialEnv {
		n = strings.TrimSpace(n)
		switch {
		case n == "" || slices.Contains(env, n):
			continue
		case !envRe.MatchString(n):
			return invalid("a credential variable is an upper case name such as MY_API_KEY: " + n)
		case slices.Contains(forbiddenEnv, n) || strings.HasPrefix(n, "HARNESS_"):
			return invalid(n + " cannot be a credential variable")
		}
		env = append(env, n)
	}
	e.CredentialEnv = env
	for l, f := range e.ModeFlags {
		if l != LevelReadOnly && l != LevelEdit && l != LevelFull {
			return invalid("unknown level " + string(l))
		}
		if len(f) > 500 || strings.ContainsAny(f, "\x00\r\n") {
			return invalid("the flags of " + string(l) + " are not valid")
		}
	}
	return nil
}

type invalidErr struct{ msg string }

func (e invalidErr) Error() string        { return ErrInvalid.Error() + ": " + e.msg }
func (e invalidErr) Is(target error) bool { return target == ErrInvalid }

func invalid(msg string) error { return invalidErr{msg} }

// Render returns the shell command that runs the executor at level. The task is not in it: the runner passes it
// as HARNESS_TASK, which {task} refers to.
func (e Executor) Render(level Level) (string, error) {
	if level == LevelReadOnly && !e.SupportsReadOnly() {
		return "", ErrReadOnlyUnsupported
	}
	cmd := strings.ReplaceAll(e.Command, "{task}", `"$HARNESS_TASK"`)
	cmd = strings.ReplaceAll(cmd, "{mode}", strings.TrimSpace(e.ModeFlags[level]))
	return strings.TrimSpace(cmd), nil
}

// LevelFor is the level a run gets from its policy ("supervised", "sandboxed", "autonomous" or "") and whether the
// role or thread is read-only.
func LevelFor(policy string, readOnly bool) Level {
	switch {
	case readOnly:
		return LevelReadOnly
	case policy == "autonomous":
		return LevelFull
	}
	return LevelEdit
}

// IsBuiltin reports whether name is one of the builtin executors.
func IsBuiltin(name string) bool {
	_, ok := BuiltinNamed(name)
	return ok
}

// Builtins are the adapters shipped with the product.
func Builtins() []Executor {
	return []Executor{
		{
			Name: ClaudeCode, Title: "Claude Code", Kind: KindBuiltin, Enabled: true,
			Description: "Anthropic's coding agent CLI (claude -p), streaming its steps",
			Command:     `claude -p {task} --output-format stream-json --verbose {mode}`,
			ModeFlags: map[Level]string{
				LevelReadOnly: `--permission-mode plan`,
				LevelEdit:     `--permission-mode acceptEdits --allowedTools Bash,Edit,Write,Read,Glob,Grep`,
				LevelFull:     `--dangerously-skip-permissions`,
			},
			Binary: "claude", InstallCommand: "npm install -g @anthropic-ai/claude-code",
			CredentialEnv: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL"}, Parser: ParserClaudeJSON,
		},
		{
			Name: Codex, Title: "Codex", Kind: KindBuiltin, Enabled: true,
			// Verified against codex-cli 0.172: `codex exec` ignores OPENAI_API_KEY and OPENAI_BASE_URL. It reads
			// CODEX_API_KEY, and a base URL only from its config (-c openai_base_url=...). The command maps the
			// variables a user seals onto those, and closes stdin (codex appends a non-terminal stdin to the prompt).
			Description: "OpenAI's coding agent CLI (codex exec)",
			Command: `CODEX_API_KEY="${CODEX_API_KEY:-$OPENAI_API_KEY}" codex exec --json --skip-git-repo-check {mode} ` +
				`${OPENAI_BASE_URL:+-c "openai_base_url=\"$OPENAI_BASE_URL\""} -- {task} </dev/null`,
			ModeFlags: map[Level]string{
				LevelReadOnly: `--sandbox read-only`,
				LevelEdit:     `--sandbox workspace-write`,
				LevelFull:     `--dangerously-bypass-approvals-and-sandbox`,
			},
			Binary: "codex", InstallCommand: "npm install -g @openai/codex",
			CredentialEnv: []string{"CODEX_API_KEY", "OPENAI_API_KEY", "OPENAI_BASE_URL"}, Parser: ParserCodexJSONL,
		},
		{
			// Verified against opencode 1.19.6 (features/executors/clitest): `opencode run --format json` prints one JSON
			// event per line (step_start, text, tool_use, step_finish with the token counts, error) and exits 1 after an
			// error event. Three findings shaped the command:
			//  - with no key at all OpenCode does NOT fail: it falls back to its own hosted free model (opencode zen) and
			//    sends the task there. disabled_providers=["opencode"] (OPENCODE_CONFIG_CONTENT, which merges over the
			//    user's config) turns that off, so a run without a key fails with "API key is missing".
			//  - ANTHROPIC_BASE_URL and OPENAI_BASE_URL are ignored (the run went to api.anthropic.com), the base URL is
			//    only read from the provider options of the config, so the command writes them there (".../v1" is added:
			//    the AI SDK wants it, the sealed value has none, as for Claude Code).
			//  - read-only is the builtin "plan" agent: it denies every edit (and shell write), verified by a scripted
			//    write that failed with "a rule which prevents you from using this specific tool call". The plan agent may
			//    still write markdown under .opencode/plans/. Edit needs no flag (file edits and the shell are allowed in
			//    a headless run, anything that would ask is refused); full adds --auto (answers the asks yes).
			Name: OpenCode, Title: "OpenCode", Kind: KindBuiltin, Enabled: true,
			Description: "the open source coding agent CLI (opencode run --format json), streaming its steps",
			Command: `A="${ANTHROPIC_BASE_URL:+\"baseURL\":\"${ANTHROPIC_BASE_URL%/}/v1\"}"; O="${OPENAI_BASE_URL:+\"baseURL\":\"${OPENAI_BASE_URL%/}/v1\"}"; ` +
				`OPENCODE_CONFIG_CONTENT="{\"disabled_providers\":[\"opencode\"],\"agent\":{\"readonly\":{\"mode\":\"primary\",\"description\":\"reads the code, changes nothing\",\"permission\":{\"*\":\"deny\",\"read\":\"allow\",\"grep\":\"allow\",\"glob\":\"allow\",\"list\":\"allow\"}}},\"provider\":{\"anthropic\":{\"options\":{$A}},\"openai\":{\"options\":{$O}}}}" ` +
				`opencode run --format json {mode} {task} </dev/null`,
			ModeFlags: map[Level]string{
				LevelReadOnly: `--agent readonly`,
				LevelEdit:     ``,
				LevelFull:     `--auto`,
			},
			Binary: "opencode", InstallCommand: "npm install -g opencode-ai",
			CredentialEnv: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "ANTHROPIC_BASE_URL", "OPENAI_BASE_URL"}, Parser: ParserOpenCodeJSON,
		},
		{
			// From omp.sh/docs (cli, modes, approvals, env): -p prints the final assistant text and exits; a headless
			// session cannot answer an approval prompt (the call fails), so the edit and full levels use yolo and the
			// sandbox is the boundary; read-only is the read tools only (verified: a call to an absent tool fails
			// with "Tool bash not found"). stdin is closed because omp reads a non-terminal stdin to EOF before it
			// starts. --mode json prints one JSON event per line (verified on omp 19.1): the parser reads the final
			// text, the usage and the failure from it.
			Name: OMP, Title: "Oh My Pi (omp)", Kind: KindBuiltin, Enabled: true,
			Description: "the Oh My Pi coding agent CLI (omp --mode json), streaming its steps",
			Command:     `omp -p --mode json {mode} {task} </dev/null`,
			ModeFlags: map[Level]string{
				LevelReadOnly: `--tools read,grep,glob --approval-mode always-ask`,
				LevelEdit:     `--approval-mode yolo`,
				LevelFull:     `--approval-mode yolo`,
			},
			Binary: "omp", InstallCommand: "bun install -g @oh-my-pi/pi-coding-agent",
			CredentialEnv: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "GEMINI_API_KEY", "ANTHROPIC_BASE_URL", "OPENAI_BASE_URL"},
			Parser:        ParserOMPJSON,
		},
	}
}

// BuiltinNamed returns the builtin executor called name.
func BuiltinNamed(name string) (Executor, bool) {
	for _, b := range Builtins() {
		if b.Name == name {
			return b, true
		}
	}
	return Executor{}, false
}

// Store keeps the executors a project defined.
type Store interface {
	// Create adds e (Validate runs first). ErrConflict when the project has one of that name.
	Create(ctx context.Context, e Executor, actor string) (*Executor, error)
	// Update replaces the executor named e.Name; ErrNotFound when there is none.
	Update(ctx context.Context, e Executor, actor string) (*Executor, error)
	Get(ctx context.Context, projectID, name string) (*Executor, error)
	// List returns the project's executors ordered by name.
	List(ctx context.Context, projectID string) ([]Executor, error)
	Delete(ctx context.Context, projectID, name string) error
	// DeleteProject removes every executor of a project (a deleted project); none is not an error.
	DeleteProject(ctx context.Context, projectID string) error
}

// Query says on behalf of whom an executor name is looked up.
type Query struct {
	Login     string
	ProjectID string
	ThreadID  string
}

// Resolver finds the executor a name stands for: a project's custom executor, or a builtin one. The product
// supplies it (it knows the project of a thread). Resolve returns ErrNotFound for an unknown or disabled name.
type Resolver interface {
	Resolve(ctx context.Context, q Query, name string) (*Executor, error)
	Available(ctx context.Context, q Query) ([]Executor, error)
}

// CredentialSource gives the credentials a user sealed for an executor: environment variable name to value. The
// values reach only the process of a run; they are never written to disk, returned by an API or logged.
type CredentialSource interface {
	Credentials(ctx context.Context, login, executor string) (map[string]string, error)
}

// Request is one run of an executor.
type Request struct {
	Login    string
	ThreadID string
	// Executor is the name; Task the text handed to the CLI.
	Executor string
	Task     string
	// Workdir is a directory under /workspace ("": the repository when there is one, else /workspace).
	Workdir string
	Timeout time.Duration
	// ReadOnly runs the CLI read-only; Policy is the factory policy of the thread (it sets the level).
	ReadOnly bool
	Policy   string
	TaskID   string
	// Install allows installing a missing CLI with the executor's install command.
	Install bool
}

// Result is what a run of an executor produced.
type Result struct {
	Executor string `json:"executor"`
	Level    Level  `json:"level"`
	ExitCode int    `json:"exitCode"`
	// Summary is the CLI's final message.
	Summary string `json:"summary"`
	// DiffStat is `git diff --stat` of the workdir after the run; Changed the files git reports changed.
	DiffStat string   `json:"diffStat,omitempty"`
	Changed  []string `json:"changed,omitempty"`
	// Output is the redacted tail of what the CLI printed.
	Output   string        `json:"output,omitempty"`
	Duration time.Duration `json:"duration"`
	TimedOut bool          `json:"timedOut,omitempty"`
	// Tokens and USD are what the CLI reported it used (0 when it does not say).
	Tokens int64   `json:"tokens,omitempty"`
	USD    float64 `json:"usd,omitempty"`
	// Problem is a user-facing diagnosis of a failed run (the CLI is missing, or it could not authenticate) with the
	// fix; empty when the failure is something else.
	Problem string `json:"problem,omitempty"`
}

// Failed reports whether the run did not succeed.
func (r Result) Failed() bool { return r.ExitCode != 0 || r.TimedOut }

// Runner runs an executor: the delegate tool uses it for a role that has one. onOutput (nil: none) receives the
// CLI's output as it streams.
type Runner interface {
	Run(ctx context.Context, req Request, onOutput func(string)) (*Result, error)
}

// DriverInstructions is added to the instructions of a thread whose role has an executor: the internal loop is
// only the driver, the external CLI does the work.
func DriverInstructions(executor string) string {
	return fmt.Sprintf("This role does its work with the external coding agent %q. Do not edit files or write code yourself: call "+
		"run_executor with executor=%q and a self-contained task (the goal, the files and constraints, what to verify), then read its "+
		"report and the diff, check the result (run the project's tests), and if it falls short call run_executor again with what is "+
		"missing. Commit and report as the role says.", executor, executor)
}
