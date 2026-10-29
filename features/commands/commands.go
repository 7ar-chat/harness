// Package commands loads custom commands: markdown prompt templates a user
// invokes by name ("/review src/main.go").
//
// A command is a file <dir>/<name>.md with optional YAML frontmatter
//
//	---
//	description: Review a file
//	argument-hint: <file>
//	---
//	Review $1 for bugs. Extra instructions: $ARGUMENTS
//
// in <project>/.har/commands and <user dir>/commands (a file in a subfolder is
// named folder:name). The body is the prompt: $ARGUMENTS is the whole argument
// text and $1 to $9 the arguments split on spaces (double or single quotes
// group words). Installed skills and the builtin commands are listed too.
//
// A command is only text. Expanding it never runs a command, reads a file or
// follows a link; what the model then does with the prompt goes through the
// usual tools and approvals. A project command comes from the repository the
// agent works on, so it is labelled SourceProject, and it can replace a user,
// skill or builtin command of the same name only in the list the user sees
// (precedence: project, user, skill, builtin).
package commands

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Sources of a command, by precedence (project wins).
const (
	SourceProject = "project"
	SourceUser    = "user"
	SourceSkill   = "skill"
	SourceBuiltin = "builtin"
)

// FileExt is the extension of a command file; Folder is the folder name inside a configuration directory.
const (
	FileExt = ".md"
	Folder  = "commands"
)

// Limits of a command file.
const (
	// MaxFileBytes is the largest command file read; a larger one is skipped with a warning.
	MaxFileBytes = 64 << 10
	// MaxNameLen bounds a command name.
	MaxNameLen = 80
)

// ErrNotFound is returned by Expand for an unknown command.
var ErrNotFound = errors.New("unknown command")

// Command is a loaded command.
type Command struct {
	// Name is what follows the slash; a file in a subfolder is "folder:name".
	Name        string
	Description string
	// Source is SourceProject, SourceUser, SourceSkill or SourceBuiltin.
	Source string
	// ArgHint is shown after the name, e.g. "<file>".
	ArgHint string
	// Template is the prompt with its placeholders.
	Template string
}

// Options says where commands come from. An empty directory is not read.
type Options struct {
	// ProjectDir is <project>/.har/commands.
	ProjectDir string
	// UserDir is <user dir>/commands.
	UserDir string
	// Skills are the installed skills, listed as commands that ask the agent to use the skill.
	Skills []Skill
	// Warn receives a problem with a file (nil: slog.Warn); the file is ignored.
	Warn func(msg string)
}

// Skill is an installed skill offered as a command.
type Skill struct{ Name, Description string }

// Set is the loaded commands.
type Set struct {
	byName map[string]Command
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(:[A-Za-z0-9][A-Za-z0-9._-]*)*$`)

// InitPrompt is the prompt of the builtin /init command.
const InitPrompt = `Analyse this repository and create or update AGENTS.md at its root: the file future agent sessions read first.

Look at the build, test and lint setup, the layout of the code, the conventions the code follows (naming, error handling, tests) and the commands a contributor runs. Read the existing AGENTS.md, README and contributing notes first and keep what is still true.

Write it short and concrete: the commands to build, test and lint (copied from the repository, not guessed), the directories that matter and what lives in them, the conventions and pitfalls that are not obvious from reading one file. No generic advice. If AGENTS.md exists, edit it in place instead of rewriting it, and say what you changed.

$ARGUMENTS`

// Load reads the commands of every source. Lower precedence sources are added
// first and replaced by higher ones of the same name.
func Load(o Options) *Set {
	warn := o.Warn
	if warn == nil {
		warn = func(msg string) { slog.Warn("commands: " + msg) }
	}
	s := &Set{byName: map[string]Command{}}
	s.add(Command{Name: "init", Description: "Create or update AGENTS.md for this repository", Source: SourceBuiltin, ArgHint: "[focus]", Template: InitPrompt})
	for _, sk := range o.Skills {
		if !validName.MatchString(sk.Name) {
			continue
		}
		s.add(Command{Name: sk.Name, Description: sk.Description, Source: SourceSkill, ArgHint: "[task]",
			Template: fmt.Sprintf("Use the %q skill (load it with load_skill) for this task: $ARGUMENTS", sk.Name)})
	}
	s.addDir(o.UserDir, SourceUser, warn)
	s.addDir(o.ProjectDir, SourceProject, warn)
	return s
}

func (s *Set) add(c Command) { s.byName[c.Name] = c }

// addDir reads the *.md files below dir. Symbolic links, files that are not
// regular and files over MaxFileBytes are skipped: a repository must not be
// able to point a command at a file elsewhere on the machine.
func (s *Set) addDir(dir, source string, warn func(string)) {
	if dir == "" {
		return
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			warn(fmt.Sprintf("%s: %v", p, err))
			return nil
		}
		if d.IsDir() || filepath.Ext(p) != FileExt {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		name := strings.ReplaceAll(strings.TrimSuffix(filepath.ToSlash(rel), FileExt), "/", ":")
		if !validName.MatchString(name) || len(name) > MaxNameLen {
			warn(fmt.Sprintf("%s: %q is not a valid command name; ignored", p, name))
			return nil
		}
		if !d.Type().IsRegular() {
			warn(fmt.Sprintf("%s: not a regular file; ignored", p))
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > MaxFileBytes {
			warn(fmt.Sprintf("%s: larger than %d KiB; ignored", p, MaxFileBytes>>10))
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			warn(fmt.Sprintf("%s: %v", p, err))
			return nil
		}
		c, err := parse(name, source, string(data))
		if err != nil {
			warn(fmt.Sprintf("%s: %v; ignored", p, err))
			return nil
		}
		s.add(c)
		return nil
	})
	if err != nil {
		warn(fmt.Sprintf("%s: %v", dir, err))
	}
}

type frontmatter struct {
	Description string `yaml:"description"`
	ArgHint     string `yaml:"argument-hint"`
}

// parse splits the optional frontmatter off a command file.
func parse(name, source, text string) (Command, error) {
	c := Command{Name: name, Source: source}
	text = strings.TrimPrefix(text, "\xef\xbb\xbf")
	body := text
	if rest, ok := strings.CutPrefix(strings.ReplaceAll(text, "\r\n", "\n"), "---\n"); ok {
		head, after, found := strings.Cut(rest, "\n---")
		if !found {
			return c, errors.New("frontmatter is not closed with ---")
		}
		after, ok := strings.CutPrefix(after, "\n")
		if !ok && after != "" {
			return c, errors.New("frontmatter is not closed with ---")
		}
		var fm frontmatter
		if err := yaml.Unmarshal([]byte(head), &fm); err != nil {
			return c, fmt.Errorf("invalid frontmatter: %v", err)
		}
		c.Description, c.ArgHint = strings.TrimSpace(fm.Description), strings.TrimSpace(fm.ArgHint)
		body = after
	}
	c.Template = strings.TrimSpace(body)
	if c.Template == "" {
		return c, errors.New("empty prompt")
	}
	if c.Description == "" {
		c.Description = firstLine(c.Template)
	}
	return c, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(line); len(r) > 80 {
		return string(r[:79]) + "…"
	}
	return line
}

// List returns the commands sorted by name.
func (s *Set) List() []Command {
	out := make([]Command, 0, len(s.byName))
	for _, c := range s.byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Expand returns the prompt of "/name args": the template with $ARGUMENTS and
// $1 to $9 replaced (a placeholder without argument becomes ""). A template
// without any placeholder gets the arguments appended after a blank line.
func (s *Set) Expand(name, args string) (string, error) {
	c, ok := s.byName[strings.TrimPrefix(name, "/")]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return Expand(c.Template, args), nil
}

var placeholder = regexp.MustCompile(`\$(ARGUMENTS|[1-9])`)

// Expand substitutes the placeholders of a template in one pass, so a
// placeholder inside an argument is not expanded again.
func Expand(template, args string) string {
	args = strings.TrimSpace(args)
	fields := Fields(args)
	used := false
	out := placeholder.ReplaceAllStringFunc(template, func(m string) string {
		used = true
		if m == "$ARGUMENTS" {
			return args
		}
		if i := int(m[1] - '1'); i < len(fields) {
			return fields[i]
		}
		return ""
	})
	if !used && args != "" {
		out += "\n\n" + args
	}
	return strings.TrimSpace(out)
}

// Fields splits arguments on white space; single or double quotes group words
// (the quotes are dropped).
func Fields(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	in := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, in = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}
