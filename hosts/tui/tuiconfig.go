package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Defaults of the settings tui.json can change.
const (
	DefaultLeader        = "ctrl+x"
	DefaultLeaderTimeout = 2000 // milliseconds
	DefaultScrollSpeed   = 3    // lines per mouse wheel tick
)

// TUIConfig is the parsed tui.json (~/.har/tui.json; a project file is never
// read, so a repository cannot rebind the user's keys). The zero value is the
// defaults.
type TUIConfig struct {
	Theme         string
	Keybinds      map[string][]string // action -> keys; see Keymap
	Leader        string
	LeaderTimeout int // milliseconds
	ScrollSpeed   int
	DiffStyle     string // auto | stacked
	CursorStyle   string // block | underline
	CursorBlink   bool
	Mouse         bool
	// Notices are the problems found while parsing: unknown fields, invalid values (which fall back to the default).
	Notices []string
}

// DefaultTUIConfig is the configuration without a tui.json.
func DefaultTUIConfig() TUIConfig {
	return TUIConfig{Leader: DefaultLeader, LeaderTimeout: DefaultLeaderTimeout, ScrollSpeed: DefaultScrollSpeed, DiffStyle: "auto", CursorStyle: "block", CursorBlink: true, Mouse: true}
}

// ParseTUIConfig parses the text of a tui.json (comments allowed). It never
// fails: a file that is not JSON gives the defaults and a notice.
func ParseTUIConfig(data []byte) TUIConfig {
	c := DefaultTUIConfig()
	if len(bytes.TrimSpace(data)) == 0 {
		return c
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(stripComments(data), &raw); err != nil {
		c.Notices = append(c.Notices, "tui.json: "+err.Error()+" — using the defaults")
		return c
	}
	bad := func(field, why string) {
		c.Notices = append(c.Notices, fmt.Sprintf("tui.json: %s %s — using the default", field, why))
	}
	for _, k := range sortedKeys(raw) {
		v := raw[k]
		switch k {
		case "theme":
			var s string
			if json.Unmarshal(v, &s) != nil {
				bad("theme", "must be a string")
			} else if _, ok := themeByName(s); !ok {
				bad("theme", fmt.Sprintf("%q is not a theme (%s)", s, themeNames()))
			} else {
				c.Theme = s
			}
		case "leader":
			var s string
			if json.Unmarshal(v, &s) != nil || strings.TrimSpace(s) == "" {
				bad("leader", "must be a key such as \"ctrl+x\"")
			} else {
				c.Leader = normKey(s)
			}
		case "leader_timeout":
			var n int
			if json.Unmarshal(v, &n) != nil || n <= 0 {
				bad("leader_timeout", "must be a positive number of milliseconds")
			} else {
				c.LeaderTimeout = n
			}
		case "scroll_speed":
			var n int
			if json.Unmarshal(v, &n) != nil || n < 1 || n > 100 {
				bad("scroll_speed", "must be a number of lines from 1 to 100")
			} else {
				c.ScrollSpeed = n
			}
		case "diff_style":
			var s string
			if json.Unmarshal(v, &s) != nil || (s != "auto" && s != "stacked") {
				bad("diff_style", "must be \"auto\" or \"stacked\"")
			} else {
				c.DiffStyle = s
			}
		case "mouse":
			var b bool
			if json.Unmarshal(v, &b) != nil {
				bad("mouse", "must be true or false")
			} else {
				c.Mouse = b
			}
		case "cursor":
			parseCursor(&c, v, bad)
		case "keybinds":
			parseKeybinds(&c, v, bad)
		default:
			c.Notices = append(c.Notices, fmt.Sprintf("tui.json: unknown field %q ignored", k))
		}
	}
	return c
}

func parseCursor(c *TUIConfig, v json.RawMessage, bad func(field, why string)) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(v, &raw) != nil {
		bad("cursor", "must be an object {style, blinking}")
		return
	}
	for _, k := range sortedKeys(raw) {
		switch k {
		case "style":
			var s string
			if json.Unmarshal(raw[k], &s) != nil || (s != "block" && s != "underline") {
				bad("cursor.style", "must be \"block\" or \"underline\"")
			} else {
				c.CursorStyle = s
			}
		case "blinking":
			var b bool
			if json.Unmarshal(raw[k], &b) != nil {
				bad("cursor.blinking", "must be true or false")
			} else {
				c.CursorBlink = b
			}
		default:
			c.Notices = append(c.Notices, fmt.Sprintf("tui.json: unknown field \"cursor.%s\" ignored", k))
		}
	}
}

func parseKeybinds(c *TUIConfig, v json.RawMessage, bad func(field, why string)) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(v, &raw) != nil {
		bad("keybinds", "must be an object {action: key}")
		return
	}
	for _, name := range sortedKeys(raw) {
		if !knownAction(name) {
			c.Notices = append(c.Notices, fmt.Sprintf("tui.json: unknown keybind %q ignored", name))
			continue
		}
		var keys []string
		var one string
		if json.Unmarshal(raw[name], &one) == nil {
			keys = strings.Split(one, ",")
		} else if json.Unmarshal(raw[name], &keys) != nil {
			bad("keybinds."+name, "must be a key or a list of keys")
			continue
		}
		var clean []string
		for _, k := range keys {
			if k = normKey(k); k != "" {
				clean = append(clean, k)
			}
		}
		if len(clean) == 0 {
			bad("keybinds."+name, "has no key (use \"none\" to unbind)")
			continue
		}
		if c.Keybinds == nil {
			c.Keybinds = map[string][]string{}
		}
		c.Keybinds[name] = clean
	}
}

func sortedKeys(m map[string]json.RawMessage) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func themeNames() string {
	names := make([]string, len(themes))
	for i, t := range themes {
		names[i] = t.Name
	}
	return strings.Join(names, ", ")
}

// stripComments removes // and /* */ comments outside strings, and trailing commas.
func stripComments(src []byte) []byte {
	var out bytes.Buffer
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '"':
			j := i + 1
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			end := min(j+1, len(src))
			out.Write(src[i:end])
			i = end - 1
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			out.WriteByte('\n')
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i++
			out.WriteByte(' ')
		default:
			out.WriteByte(c)
		}
	}
	return dropTrailingCommas(out.Bytes())
}

// dropTrailingCommas removes a comma that is followed (after spaces) by } or ].
func dropTrailingCommas(src []byte) []byte {
	var out bytes.Buffer
	inStr := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inStr {
			out.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				i++
				out.WriteByte(src[i])
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
		}
		if c == ',' {
			j := i + 1
			for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r') {
				j++
			}
			if j < len(src) && (src[j] == '}' || src[j] == ']') {
				continue
			}
		}
		out.WriteByte(c)
	}
	return out.Bytes()
}
