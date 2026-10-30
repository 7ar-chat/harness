package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// MCPServer is one MCP server: an entry of MCP_SERVERS, of a .mcp.json file or
// of the mcp.servers section of config.yml. A remote server has a URL (and
// maybe headers), a local one a Command that is started as a child process
// (stdio; see Config.MCPAllowStdio).
type MCPServer struct {
	Name    string            `json:"name"`
	URL     string            `json:"url,omitempty"`
	Tools   []string          `json:"tools,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Type is "http" (streamable HTTP, the default for a URL), "sse" or "stdio".
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// Origin says where the definition was read from (for messages and -config-dump).
	Origin string `json:"-"`
	// ProjectRoot is set on a server a project file defines (.mcp.json, .har/mcp.json, the project config.yml):
	// a stdio one needs the user's approval for this exact command before it is started (MCPApproved).
	ProjectRoot string `json:"-"`
}

// IsStdio reports a server started as a local child process.
func (s MCPServer) IsStdio() bool { return s.Command != "" }

// Describe is a one-line summary without header or environment values.
func (s MCPServer) Describe() string {
	if s.IsStdio() {
		return "stdio: " + strings.TrimSpace(s.Command+" "+strings.Join(s.Args, " "))
	}
	t := s.Type
	if t == "" {
		t = "http"
	}
	return t + ": " + s.URL
}

// mcpDoc is the mcp.servers section of a config.yml.
type mcpDoc struct {
	file    string
	source  Source
	servers map[string]any
}

// mcpEntry is the definition of a server in .mcp.json / config.yml.
type mcpEntry struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Tools   []string          `json:"tools"`
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// expandEnv replaces ${VAR} and ${VAR:-default}; an unset variable without a
// default is an error.
func expandEnv(s string, lookup func(string) (string, bool)) (string, error) {
	var missing string
	out := envRef.ReplaceAllStringFunc(s, func(m string) string {
		sub := envRef.FindStringSubmatch(m)
		if v, ok := lookup(sub[1]); ok && v != "" {
			return v
		}
		if strings.Contains(m, ":-") {
			return sub[2]
		}
		if missing == "" {
			missing = sub[1]
		}
		return m
	})
	if missing != "" {
		return "", fmt.Errorf("environment variable %s is not set", missing)
	}
	return out, nil
}

// parseMCPServers turns {"name": definition} into servers, expanding ${VAR} references.
func parseMCPServers(origin string, raw map[string]any, lookup func(string) (string, bool)) ([]MCPServer, error) {
	names := make([]string, 0, len(raw))
	for n := range raw {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []MCPServer
	for _, name := range names {
		b, err := json.Marshal(raw[name])
		if err != nil {
			return nil, fmt.Errorf("%s: mcp server %q: %w", origin, name, err)
		}
		var e mcpEntry
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("%s: mcp server %q: %w", origin, name, err)
		}
		s := MCPServer{Name: name, URL: e.URL, Tools: e.Tools, Type: strings.ToLower(e.Type), Command: e.Command, Args: e.Args, Env: e.Env, Headers: e.Headers, Origin: origin}
		wrap := func(what string, err error) error {
			return fmt.Errorf("%s: mcp server %q: %s: %w", origin, name, what, err)
		}
		switch s.Type {
		case "":
			if s.Command != "" {
				s.Type = "stdio"
			} else {
				s.Type = "http"
			}
		case "http", "sse", "stdio":
		case "streamable-http", "streamable_http":
			s.Type = "http"
		default:
			return nil, fmt.Errorf("%s: mcp server %q: type must be http, sse or stdio, got %q", origin, name, e.Type)
		}
		if (s.Type == "stdio") != (s.Command != "") {
			return nil, fmt.Errorf("%s: mcp server %q: a stdio server needs a command and a remote one a url (type %s)", origin, name, s.Type)
		}
		if s.Type != "stdio" && s.URL == "" {
			return nil, fmt.Errorf("%s: mcp server %q: url is required", origin, name)
		}
		var err2 error
		if s.URL, err2 = expandEnv(s.URL, lookup); err2 != nil {
			return nil, wrap("url", err2)
		}
		for k, v := range s.Headers {
			if s.Headers[k], err2 = expandEnv(v, lookup); err2 != nil {
				return nil, wrap("header "+k, err2)
			}
		}
		for k, v := range s.Env {
			if s.Env[k], err2 = expandEnv(v, lookup); err2 != nil {
				return nil, wrap("env "+k, err2)
			}
		}
		if s.Command, err2 = expandEnv(s.Command, lookup); err2 != nil {
			return nil, wrap("command", err2)
		}
		for i, a := range s.Args {
			if s.Args[i], err2 = expandEnv(a, lookup); err2 != nil {
				return nil, wrap("args", err2)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// readMCPJSON reads a .mcp.json style file ({"mcpServers": {name: definition}});
// a missing file yields nothing.
func readMCPJSON(file string, lookup func(string) (string, bool)) ([]MCPServer, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var doc struct {
		Servers map[string]any `json:"mcpServers"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return parseMCPServers(file, doc.Servers, lookup)
}

// loadMCPFiles merges the MCP servers of the files into c.MCPServers. From the
// lowest priority: user config.yml, ~/.har/mcp.json, <root>/.mcp.json,
// <root>/.har/mcp.json, project config.yml, then MCP_SERVERS. A server of a
// later source replaces one of the same name. The three project sources only
// count when the project is trusted (see TrustFile); otherwise they are listed in
// Layers.MCPSkipped. Stdio servers are dropped unless the local backend and
// MCPAllowStdio are on, and a stdio server a project defines also needs the user's approval of its exact
// command (MCPApproved, `harness trust mcp`); until then it is listed in Layers.MCPPending.
func (c *Config) loadMCPFiles(yamlDocs []mcpDoc) error {
	ly := c.Layers
	lookup := func(name string) (string, bool) { return os.LookupEnv(name) }
	if c.envLookup != nil {
		lookup = c.envLookup
	}
	var sources [][]MCPServer
	// add records a source. project says the file comes from the repository: unless the project is trusted
	// its servers are not started, only listed (the definitions are read with ${VAR} left as they are).
	add := func(list []MCPServer, err error, file string, project bool) error {
		if err != nil {
			return err
		}
		if project && !ly.Trusted {
			for _, s := range list {
				ly.MCPSkipped = append(ly.MCPSkipped, fmt.Sprintf("%s (%s): %s: the project is not trusted, run `harness trust` to enable", s.Name, file, s.Describe()))
			}
			return nil
		}
		if project {
			for i := range list {
				list[i].ProjectRoot = ly.ProjectRoot
			}
		}
		if len(list) > 0 {
			sources = append(sources, list)
			ly.MCPSources = append(ly.MCPSources, file)
		}
		return nil
	}
	yamlOf := func(src Source) (mcpDoc, bool) {
		for _, d := range yamlDocs {
			if d.source == src {
				return d, true
			}
		}
		return mcpDoc{}, false
	}
	if d, ok := yamlOf(SourceUser); ok {
		l, err := parseMCPServers(d.file, d.servers, lookup)
		if err := add(l, err, d.file, false); err != nil {
			return err
		}
	}
	if ly.UserDir != "" {
		f := filepath.Join(ly.UserDir, MCPFile)
		l, err := readMCPJSON(f, lookup)
		if err := add(l, err, f, false); err != nil {
			return err
		}
	}
	projectLookup := lookup
	if !ly.Trusted {
		projectLookup = func(n string) (string, bool) { return "${" + n + "}", true }
	}
	if ly.ProjectRoot != "" {
		f := filepath.Join(ly.ProjectRoot, ".mcp.json")
		l, err := readMCPJSON(f, projectLookup)
		if err := add(l, err, f, true); err != nil {
			return err
		}
	}
	if ly.ProjectDir != "" {
		f := filepath.Join(ly.ProjectDir, MCPFile)
		l, err := readMCPJSON(f, projectLookup)
		if err := add(l, err, f, true); err != nil {
			return err
		}
	}
	if d, ok := yamlOf(SourceProject); ok {
		l, err := parseMCPServers(d.file, d.servers, projectLookup)
		if err := add(l, err, d.file, true); err != nil {
			return err
		}
	}
	if len(c.MCPServers) > 0 {
		for i := range c.MCPServers {
			c.MCPServers[i].Origin = "MCP_SERVERS"
		}
		sources = append(sources, c.MCPServers)
	}
	var order []string
	byName := map[string]MCPServer{}
	for _, list := range sources {
		for _, s := range list {
			if _, ok := byName[s.Name]; !ok {
				order = append(order, s.Name)
			}
			byName[s.Name] = s
		}
	}
	stdioOK := c.LocalSandbox() && c.MCPAllowStdio
	var merged []MCPServer
	for _, n := range order {
		s := byName[n]
		if s.IsStdio() && !stdioOK {
			why := "set mcp.allow_stdio: true in the user file or HARNESS_MCP_STDIO=1, and use the local sandbox backend"
			slog.Warn("mcp: stdio server skipped", "server", s.Name, "origin", s.Origin, "how_to_enable", why)
			ly.MCPSkipped = append(ly.MCPSkipped, fmt.Sprintf("%s (%s): stdio servers need the local backend and an opt-in", s.Name, s.Origin))
			continue
		}
		if s.IsStdio() && s.ProjectRoot != "" {
			ok, err := MCPApproved(ly.UserDir, s.ProjectRoot, s)
			if err != nil {
				return err
			}
			if !ok {
				slog.Warn("mcp: project stdio server not approved", "server", s.Name, "origin", s.Origin, "how_to_enable", "harness trust mcp "+s.Name)
				ly.MCPSkipped = append(ly.MCPSkipped, fmt.Sprintf("%s (%s): %s: a project's stdio server needs your approval of this exact command, run `harness trust mcp %s`", s.Name, s.Origin, s.Describe(), s.Name))
				ly.MCPPending = append(ly.MCPPending, s)
				continue
			}
		}
		merged = append(merged, s)
	}
	c.MCPServers = merged
	return nil
}

func validURL(s string) error {
	_, err := url.Parse(s)
	return err
}
