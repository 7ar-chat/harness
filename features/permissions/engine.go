package permissions

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Tool classes the policy judges.
const (
	classCommand = "command"
	classEdit    = "edit"
	classOther   = "other"
)

func normalizeName(s string) string {
	return strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(strings.TrimSpace(s)))
}

// ToolClass is "command" for execute_command, "edit" for write_file/edit_file
// and "other" for every other tool.
func ToolClass(tool string) string {
	switch n := normalizeName(tool); {
	case n == "executecommand" || n == "bash":
		return classCommand
	case editNames[n]:
		return classEdit
	}
	return classOther
}

// Decide judges one tool call. judged is false for a tool the policy has no
// opinion on (no rule names it): the caller applies its own gate to it.
func (p *Policy) Decide(tool, arguments string) (d Decision, judged bool) {
	switch ToolClass(tool) {
	case classCommand:
		cmd, ok := stringArg(arguments, "command")
		if !ok {
			return p.adjust(Decision{Action: Ask, Reason: "the command could not be read from the arguments"}, classCommand), true
		}
		return p.adjust(p.decideCommand(cmd), classCommand), true
	case classEdit:
		path, ok := stringArg(arguments, "path", "file_path", "filePath")
		if !ok {
			return p.adjust(Decision{Action: Ask, Reason: "the file path could not be read from the arguments"}, classEdit), true
		}
		return p.adjust(p.pathDecision(path), classEdit), true
	}
	d, judged = p.decideOther(tool, arguments)
	if !judged {
		return Decision{}, false
	}
	return p.adjust(d, classOther), true
}

// adjust applies the mode to a decision of the rules.
func (p *Policy) adjust(d Decision, class string) Decision {
	switch p.Mode {
	case ModeYoloUnsafe:
		return Decision{Action: Allow, Reason: "yolo-unsafe mode: no permission checks"}
	case ModeYolo:
		if d.Action == Ask {
			return Decision{Action: Allow, Reason: "yolo mode: " + d.Reason, Rule: d.Rule}
		}
	case ModeAutoEdit:
		if d.Action == Ask && class == classEdit {
			return Decision{Action: Allow, Reason: "auto-edit mode: " + d.Reason, Rule: d.Rule}
		}
	case ModeReadOnly:
		if d.Action == Deny {
			return d
		}
		if class != classOther && !(d.Action == Allow && d.ReadOnly) {
			why := d.Reason
			if class == classEdit {
				why = "files cannot be written"
			}
			return Decision{Action: Deny, Reason: "readonly mode: " + why, Rule: d.Rule}
		}
	}
	return d
}

func stringArg(arguments string, keys ...string) (string, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(arguments)), &m); err != nil {
		return "", false
	}
	for _, k := range keys {
		if raw, ok := m[k]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return s, true
			}
		}
	}
	return "", false
}

// decideOther applies the user's rules about tools other than commands and edits.
func (p *Policy) decideOther(tool, arguments string) (Decision, bool) {
	var matched *Rule
	for _, want := range []Action{Deny, Ask, Allow} {
		for i := range p.Rules {
			r := &p.Rules[i]
			if r.Action == want && r.Kind == kindTool && globCommand(strings.ToLower(r.Tool), strings.ToLower(tool)) &&
				globCommand(r.Pattern, strings.TrimSpace(arguments)) {
				matched = r
				break
			}
		}
		if matched != nil {
			break
		}
	}
	if matched == nil {
		for _, g := range p.AskTools {
			if globCommand(strings.ToLower(g), strings.ToLower(tool)) {
				return Decision{Action: Ask, Once: true, Reason: "first use of " + tool + ", a tool of an MCP server that runs a local program (add an allow rule to stop asking)"}, true
			}
		}
		return Decision{}, false
	}
	return Decision{Action: matched.Action, Reason: "rule " + matched.String(), Rule: matched.String()}, true
}

// userRule returns the first user rule of the kind and action whose pattern is
// matched by one of the candidates.
func (p *Policy) userRule(kind string, a Action, match func(pattern string) bool) *Rule {
	for i := range p.Rules {
		r := &p.Rules[i]
		if r.Kind == kind && r.Action == a && match(r.Pattern) {
			return r
		}
	}
	return nil
}

func ruleDecision(r *Rule, why string) Decision {
	d := Decision{Action: r.Action, Rule: r.String(), Reason: "rule " + r.String()}
	if why != "" {
		d.Reason += " (" + why + ")"
	}
	return d
}

// pathDecision judges writing one path (write_file, edit_file, redirections, mutating commands).
func (p *Policy) pathDecision(raw string) Decision {
	info := p.classify(raw)
	cands := []string{raw}
	if info.clean != "" {
		cands = append(cands, info.clean, strings.TrimPrefix(info.clean, virtualWorkspace+"/"))
	}
	match := func(pattern string) bool {
		for _, c := range cands {
			if globPath(pattern, c) {
				return true
			}
		}
		if !strings.Contains(pattern, "/") && info.clean != "" {
			return globPath(pattern, info.clean[strings.LastIndex(info.clean, "/")+1:])
		}
		return false
	}
	if r := p.userRule(kindEdit, Deny, match); r != nil {
		return ruleDecision(r, raw)
	}
	allow := p.userRule(kindEdit, Allow, match)
	if info.protected != "" && allow == nil {
		return Decision{Action: Deny, Reason: "writing " + raw + " is blocked: " + info.protected}
	}
	if r := p.userRule(kindEdit, Ask, match); r != nil {
		return ruleDecision(r, raw)
	}
	if allow != nil {
		return ruleDecision(allow, raw)
	}
	switch info.class {
	case pathInside, pathTmp:
		return Decision{Action: Allow, Reason: "inside the workspace"}
	case pathUnknown:
		return Decision{Action: Ask, Reason: "the path " + raw + " depends on a variable or substitution"}
	}
	return Decision{Action: Ask, Reason: "writes " + raw + ", outside the workspace"}
}

// combiner accumulates the decisions of the parts of one call.
type combiner struct {
	worst    Decision
	any      bool
	readOnly bool
}

func (c *combiner) add(d Decision) {
	if !c.any {
		c.worst, c.any, c.readOnly = d, true, d.Action == Allow && d.ReadOnly
		return
	}
	c.readOnly = c.readOnly && d.Action == Allow && d.ReadOnly
	if d.Action > c.worst.Action || (d.Action == c.worst.Action && d.Action == Deny && d.Hard && !c.worst.Hard) {
		c.worst = d
	}
}

func (c *combiner) result() Decision {
	if !c.any {
		return Decision{Action: Allow, Reason: "nothing to run", ReadOnly: true}
	}
	d := c.worst
	d.ReadOnly = c.readOnly && d.Action == Allow
	return d
}

var forkFunc = regexp.MustCompile(`([A-Za-z_:][A-Za-z0-9_:]*)\(\)\{`)

// isForkBomb recognises a function that pipes itself into itself in the background.
func isForkBomb(line string) bool {
	compact := strings.Join(strings.Fields(line), "")
	for _, m := range forkFunc.FindAllStringSubmatch(compact, -1) {
		if strings.Contains(compact, m[1]+"|"+m[1]+"&") {
			return true
		}
	}
	return false
}

// decideCommand judges a shell line: the most severe decision among its simple
// commands, redirections and pipelines.
func (p *Policy) decideCommand(line string) Decision {
	var c combiner
	if strings.TrimSpace(line) == "" {
		return Decision{Action: Allow, Reason: "empty command", ReadOnly: true}
	}
	if isForkBomb(line) {
		return Decision{Action: Deny, Hard: true, Reason: "fork bomb"}
	}
	parsed := Parse(line)
	if parsed.Incomplete {
		c.add(Decision{Action: Ask, Reason: "the command could not be parsed with confidence (unbalanced quote or substitution)"})
	}
	// Pipelines a user allow rule covers: their commands skip the built-in checks.
	pipeAllowed := map[int]bool{}
	for id, text := range parsed.Pipelines {
		if r := p.userRule(kindCommand, Allow, func(pat string) bool { return strings.Contains(pat, "|") && globCommand(pat, text) }); r != nil {
			pipeAllowed[id] = true
		}
		if r := p.userRule(kindCommand, Deny, func(pat string) bool { return strings.Contains(pat, "|") && globCommand(pat, text) }); r != nil {
			c.add(ruleDecision(r, text))
		}
		if r := p.userRule(kindCommand, Ask, func(pat string) bool { return strings.Contains(pat, "|") && globCommand(pat, text) }); r != nil {
			c.add(ruleDecision(r, text))
		}
	}
	for _, s := range parsed.Commands {
		c.add(p.simple(s, pipeAllowed[s.Pipeline]))
		for _, rd := range s.Redirects {
			c.add(p.redirect(rd))
		}
	}
	if d, ok := p.pipelineToShell(parsed, pipeAllowed); ok {
		c.add(d)
	}
	return c.result()
}

// redirect judges the file a command's output goes to.
func (p *Policy) redirect(rd Redirect) Decision {
	t := rd.Target
	switch {
	case t == "/dev/null" || t == "/dev/stdout" || t == "/dev/stderr" || t == "/dev/tty" || t == "/dev/fd/1" || t == "/dev/fd/2":
		return Decision{Action: Allow, Reason: "discards output", ReadOnly: true}
	case blockDevice.MatchString(t):
		return Decision{Action: Deny, Hard: true, Reason: "redirects output onto the device " + t}
	}
	d := p.pathDecision(t)
	d.ReadOnly = false
	if d.Action != Allow {
		d.Reason = "redirection: " + d.Reason
	}
	return d
}

var blockDevice = regexp.MustCompile(`^/dev/(sd[a-z]|hd[a-z]|nvme\d|disk\d|vd[a-z]|xvd[a-z]|mmcblk\d|rdisk\d|mapper/|md\d)`)

// simple judges one simple command.
func (p *Policy) simple(s Simple, pipeAllowed bool) Decision {
	if len(s.Words) == 0 {
		return Decision{Action: Allow, Reason: "no command"}
	}
	if d, ok := p.hardDeny(s); ok {
		return d
	}
	cands := []string{s.Text()}
	if s.Raw != "" && s.Raw != cands[0] {
		cands = append(cands, s.Raw)
	}
	if s.Elevated {
		cands = append(cands, "sudo "+s.Text())
	}
	match := func(pattern string) bool {
		if strings.Contains(pattern, "|") {
			return false // a pipeline pattern: matched against whole pipelines
		}
		for _, c := range cands {
			if globCommand(pattern, c) {
				return true
			}
		}
		return false
	}
	if r := p.userRule(kindCommand, Deny, match); r != nil {
		return ruleDecision(r, s.Text())
	}
	if r := p.userRule(kindCommand, Ask, match); r != nil {
		return ruleDecision(r, s.Text())
	}
	if r := p.userRule(kindCommand, Allow, func(pattern string) bool {
		// A rule allowing "make*" does not allow "sudo make": elevated commands match their sudo form only.
		if s.Elevated {
			return !strings.Contains(pattern, "|") && globCommand(pattern, "sudo "+s.Text())
		}
		return match(pattern)
	}); r != nil {
		return ruleDecision(r, s.Text())
	}
	if pipeAllowed {
		return Decision{Action: Allow, Reason: "allowed by a pipeline rule"}
	}
	return p.builtin(s)
}
