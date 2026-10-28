// Package permissions is the safety layer of the local/coding profile: rules
// (allow / ask / deny) and modes decide, before a tool call runs, whether it is
// executed, needs the user's approval or is refused. It plugs into the agent as
// a BeforeToolHook (approvals reuse the AG-UI interrupt and resume flow) and
// knows how to read shell commands well enough to judge every simple command
// of a line.
package permissions

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/GhaziBenDahmane/harness/config"
)

// Action is a decision.
type Action int

const (
	Allow Action = iota
	Ask
	Deny
)

func (a Action) String() string { return [...]string{"allow", "ask", "deny"}[a] }

// Mode is how strictly calls are gated (AGENT_PERMISSIONS).
type Mode string

const (
	// ModeAsk applies the rules and defaults: what is not known safe is asked.
	ModeAsk Mode = "ask"
	// ModeAutoEdit is ask, but edits are free everywhere (still not .git internals or secrets).
	ModeAutoEdit Mode = "auto-edit"
	// ModeYolo never asks; deny rules still apply.
	ModeYolo Mode = "yolo"
	// ModeYoloUnsafe never asks and ignores deny rules too.
	ModeYoloUnsafe Mode = "yolo-unsafe"
	// ModeReadOnly refuses edits and every command that is not read-only.
	ModeReadOnly Mode = "readonly"
)

// ParseMode reads a mode name ("" is ask).
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return ModeAsk, nil
	case ModeAsk, ModeAutoEdit, ModeYolo, ModeYoloUnsafe, ModeReadOnly:
		return m, nil
	case "auto_edit", "autoedit":
		return ModeAutoEdit, nil
	case "read-only", "read_only":
		return ModeReadOnly, nil
	case "yolo_unsafe":
		return ModeYoloUnsafe, nil
	default:
		return "", fmt.Errorf("permissions: unknown mode %q (ask, auto-edit, yolo, yolo-unsafe, readonly)", s)
	}
}

// Rule kinds (Rule.Kind): what a rule's pattern is matched against.
const (
	kindCommand = "command" // execute_command: the text of each simple command
	kindEdit    = "edit"    // write_file / edit_file: the path
	kindTool    = "tool"    // any other tool: its name (and arguments)
)

// Rule is one allow, ask or deny rule.
type Rule struct {
	Action  Action
	Kind    string
	Tool    string // kindTool: glob on the tool name
	Pattern string
	Source  string // where it came from, for messages
}

func (r Rule) String() string {
	switch r.Kind {
	case kindCommand:
		return fmt.Sprintf("%s: execute_command(%s)", r.Action, r.Pattern)
	case kindEdit:
		return fmt.Sprintf("%s: write_file(%s)", r.Action, r.Pattern)
	}
	return fmt.Sprintf("%s: %s(%s)", r.Action, r.Tool, r.Pattern)
}

var (
	commandNames = map[string]bool{"execute_command": true, "executecommand": true, "bash": true, "command": true}
	editNames    = map[string]bool{"write_file": true, "edit_file": true, "writefile": true, "editfile": true, "write": true, "edit": true, "multiedit": true}
)

// ParseRule reads a rule entry of the config file:
//
//	execute_command(git push*)   Bash(git push*) and a bare "git push*" mean the same
//	write_file(**/.env)          Edit(...), Write(...), edit_file(...): paths of write_file and edit_file
//	write_file                   the whole tool (also execute_command / Bash)
//	mcp__github__create*(*)      any other tool: its name, and a glob on its JSON arguments
func ParseRule(a Action, entry, source string) (Rule, error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return Rule{}, errors.New("empty rule")
	}
	name, pattern, hasArgs := entry, "*", false
	if i := strings.Index(entry, "("); i > 0 && strings.HasSuffix(entry, ")") && isToolName(entry[:i]) {
		name, pattern, hasArgs = entry[:i], entry[i+1:len(entry)-1], true
	}
	key := strings.ToLower(name)
	switch {
	case commandNames[key] && (hasArgs || !strings.ContainsAny(entry, " *?")):
		return Rule{Action: a, Kind: kindCommand, Pattern: pattern, Source: source}, nil
	case editNames[key] && (hasArgs || !strings.ContainsAny(entry, " *?")):
		return Rule{Action: a, Kind: kindEdit, Pattern: pattern, Source: source}, nil
	case hasArgs:
		return Rule{Action: a, Kind: kindTool, Tool: name, Pattern: pattern, Source: source}, nil
	}
	return Rule{Action: a, Kind: kindCommand, Pattern: entry, Source: source}, nil // a bare command pattern
}

func isToolName(s string) bool {
	for _, r := range s {
		if !(r == '_' || r == '-' || r == '*' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return s != ""
}

// File is the YAML form of a permissions file (.har/permissions.yml).
type File struct {
	Mode  string   `yaml:"mode"`
	Allow []string `yaml:"allow"`
	Ask   []string `yaml:"ask"`
	Deny  []string `yaml:"deny"`
}

// ParseFile reads a permissions file.
func ParseFile(data []byte, source string) (mode string, rules []Rule, err error) {
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return "", nil, fmt.Errorf("%s: %w", source, err)
	}
	for _, g := range []struct {
		a       Action
		entries []string
	}{{Deny, f.Deny}, {Ask, f.Ask}, {Allow, f.Allow}} {
		for _, e := range g.entries {
			r, err := ParseRule(g.a, e, source)
			if err != nil {
				return "", nil, fmt.Errorf("%s: %s %q: %w", source, g.a, e, err)
			}
			rules = append(rules, r)
		}
	}
	if f.Mode != "" {
		if _, err := ParseMode(f.Mode); err != nil {
			return "", nil, fmt.Errorf("%s: %w", source, err)
		}
	}
	return f.Mode, rules, nil
}

// Options builds a Policy.
type Options struct {
	// Workspace is the host directory the agent works in (its /workspace).
	Workspace string
	// Mode is AGENT_PERMISSIONS ("" lets the files decide, then ask).
	Mode string
	// Allow, Ask and Deny are extra rules from the environment
	// (AGENT_PERMISSIONS_ALLOW|ASK|DENY: entries separated by newlines or ";;").
	Allow, Ask, Deny string
	// AskTools are globs on tool names that are asked about, once per run, unless a rule says otherwise
	// (the tools of MCP servers that start local programs: "name__*").
	AskTools []string
	// Files are read in order when they exist; earlier files have priority for
	// the mode. Default: <workspace>/.har/permissions.yml, then
	// ~/.har/permissions.yml.
	Files []string
	// Home is the user's home directory (default os.UserHomeDir).
	Home string
	// Trusted says the project is trusted (config.TrustFile): its permissions file may then add allow rules
	// and loosen the mode. Otherwise only the default workspace file is affected: its allow rules are ignored
	// and its mode only counts when it is as strict as the one already in force. Files given explicitly in
	// Files are the caller's own and are always honoured.
	Trusted bool
}

// Load builds the policy: the environment's mode and rules, then the workspace
// file, then the user's file. Rules of all sources are merged (a deny anywhere
// wins over an ask, an ask over an allow); the mode comes from the first
// source that sets one.
func Load(o Options) (*Policy, error) {
	home := o.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	files := o.Files
	projectFile := ""
	if files == nil {
		if o.Workspace != "" {
			projectFile = filepath.Join(o.Workspace, config.DirName, config.PermissionsFile)
			files = append(files, projectFile)
		}
		userDir := config.UserDir()
		if o.Home != "" {
			userDir = filepath.Join(o.Home, config.DirName)
		}
		if userDir != "" {
			files = append(files, filepath.Join(userDir, config.PermissionsFile))
		}
	}
	p := &Policy{Workspace: o.Workspace, Home: home, AskTools: o.AskTools}
	modeName := o.Mode
	for _, g := range []struct {
		a   Action
		src string
	}{{Deny, o.Deny}, {Ask, o.Ask}, {Allow, o.Allow}} {
		for _, e := range splitEnv(g.src) {
			r, err := ParseRule(g.a, e, "environment")
			if err != nil {
				return nil, fmt.Errorf("permissions: AGENT_PERMISSIONS_%s %q: %w", strings.ToUpper(g.a.String()), e, err)
			}
			p.Rules = append(p.Rules, r)
		}
	}
	type loaded struct {
		file  string
		mode  string
		rules []Rule
	}
	var all []loaded
	for _, f := range files {
		data, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("permissions: %w", err)
		}
		m, rules, err := ParseFile(data, f)
		if err != nil {
			return nil, fmt.Errorf("permissions: %w", err)
		}
		all = append(all, loaded{f, m, rules})
	}
	// The mode in force before the project file speaks: the environment's, else the first user file's, else ask.
	base := o.Mode
	if strings.TrimSpace(base) == "" {
		for _, l := range all {
			if l.file != projectFile && l.mode != "" {
				base = l.mode
				break
			}
		}
	}
	for _, l := range all {
		if l.file == projectFile && !o.Trusted {
			kept := l.rules[:0:0]
			dropped := 0
			for _, r := range l.rules {
				if r.Action == Allow {
					dropped++
					continue
				}
				kept = append(kept, r)
			}
			if dropped > 0 {
				p.Ignored = append(p.Ignored, fmt.Sprintf("%s: %d allow rule(s) ignored: the project is not trusted (run `harness trust` to honour them)", l.file, dropped))
			}
			l.rules = kept
			if l.mode != "" {
				pr, pok := config.PermissionStrictness(l.mode)
				br, _ := config.PermissionStrictness(base)
				if pok && pr < br {
					p.Ignored = append(p.Ignored, fmt.Sprintf("%s: mode %q ignored: it is looser than %q and the project is not trusted (run `harness trust` to honour it)", l.file, l.mode, cmpMode(base)))
					l.mode = ""
				}
			}
		}
		if strings.TrimSpace(modeName) == "" {
			modeName = l.mode
		}
		p.Rules = append(p.Rules, l.rules...)
		p.Files = append(p.Files, l.file)
	}
	if len(p.Ignored) > 0 {
		slog.Warn("permissions: project settings ignored (untrusted project)", "ignored", strings.Join(p.Ignored, "; "))
	}
	var err error
	if p.Mode, err = ParseMode(modeName); err != nil {
		return nil, err
	}
	return p, nil
}

func cmpMode(m string) string {
	if strings.TrimSpace(m) == "" {
		return "ask"
	}
	return m
}

func splitEnv(s string) []string {
	var out []string
	for _, part := range strings.Split(strings.ReplaceAll(s, ";;", "\n"), "\n") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Policy decides tool calls.
type Policy struct {
	Mode      Mode
	Rules     []Rule
	Workspace string // host directory of /workspace ("" if unknown)
	Home      string
	Files     []string // permission files that were read
	// Ignored says what an untrusted project's permissions file asked for and was not honoured.
	Ignored []string
	// AskTools: tools (name globs) that ask, once per run, when no rule matches them.
	AskTools []string
}

// Decision is the outcome for a tool call.
type Decision struct {
	Action Action
	// Reason says why, for the user and the model.
	Reason string
	// Rule is the user rule that decided, if any.
	Rule string
	// Hard decisions (built-in catastrophic denials) cannot be allowed by a rule.
	Hard bool
	// Once: approving the call approves the tool for the rest of the run.
	Once bool
	// ReadOnly: the call was judged read-only (what readonly mode allows).
	ReadOnly bool
}

func (d Decision) String() string { return d.Action.String() + ": " + d.Reason }
