// Package clitest drives the REAL external coding-agent CLIs (Claude Code, Codex, omp) through the executors runner
// against scripted fake model servers, so the adapters' command lines and parsers are checked against what the CLIs
// really print, without a real model or a paid key. Everything that needs a CLI is opt-in: see clis_test.go.
//
// This file holds what the three fake servers share: the scripted conversation (Step, Script), the request log, and
// the helpers that pick the tool a script calls from the tool schema the CLI really sent.
package clitest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// ToolUse is one tool call a scripted model makes.
type ToolUse struct {
	Name string
	Args map[string]any
	// Input, when Custom, is the raw text of a freeform (custom) tool call, e.g. the JavaScript of Codex's "exec" tool.
	Input  string
	Custom bool
}

// Step is what the scripted model answers one request: some text, tool calls, and the usage it reports.
type Step struct {
	Text  string
	Tools []ToolUse
	// In, Out, CacheRead are the usage numbers of the reply (defaults are filled in).
	In, Out, CacheRead int
	// Status, when not 0, makes the server answer that HTTP status with an error body instead of a reply.
	Status int
}

// Request is the part of a model request a script looks at.
type Request struct {
	Path  string
	Model string
	// Tools are the names of the tools the CLI offered, in order.
	Tools []string
	// ToolSchemas maps a tool name to its raw JSON schema (parameters).
	ToolSchemas map[string]json.RawMessage
	// ToolResults are the texts of every tool result in the conversation so far.
	ToolResults []string
	// UserTexts are the texts of the user messages.
	UserTexts []string
	Raw       []byte
}

// Has reports whether the CLI offered a tool with this name (case-insensitive).
func (r *Request) Has(name string) bool { return r.Tool(name) != "" }

// Tool returns the offered tool whose name equals one of names (case-insensitive, first name wins), "" if none.
func (r *Request) Tool(names ...string) string {
	for _, n := range names {
		for _, t := range r.Tools {
			if strings.EqualFold(t, n) {
				return t
			}
		}
	}
	return ""
}

// Script decides the reply to one request. It must be safe to call concurrently.
type Script func(r *Request) Step

// Log records the requests a fake server saw.
type Log struct {
	mu   sync.Mutex
	reqs []*Request
	all  []string // "METHOD path" of every HTTP request, including the ones no script saw
}

func (l *Log) add(r *Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reqs = append(l.reqs, r)
}

func (l *Log) seen(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = append(l.all, s)
}

// Requests returns the model requests recorded so far.
func (l *Log) Requests() []*Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*Request(nil), l.reqs...)
}

// Paths returns every HTTP request line the server saw.
func (l *Log) Paths() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.all...)
}

// MainRequests are the requests that carried tools: the agent loop itself, not the side calls (titles, probes).
func (l *Log) MainRequests() []*Request {
	var out []*Request
	for _, r := range l.Requests() {
		if len(r.Tools) > 0 {
			out = append(out, r)
		}
	}
	return out
}

// Server is a running fake model server.
type Server struct {
	*httptest.Server
	Log *Log
}

// URL of the server without a trailing slash.
func (s *Server) Base() string { return strings.TrimRight(s.Server.URL, "/") }

// WriteFileStep is the usual first turn: create path with content using whichever file tool the CLI offered (the
// argument name is read from the tool's own schema; a CLI with no file tool gets a shell command).
func WriteFileStep(r *Request, path, content string) ToolUse {
	for _, name := range []string{"Write", "write", "write_file"} {
		if t := r.Tool(name); t != "" {
			key := "path"
			switch sch := string(r.ToolSchemas[t]); {
			case strings.Contains(sch, `"file_path"`): // Claude Code
				key = "file_path"
			case strings.Contains(sch, `"filePath"`): // OpenCode
				key = "filePath"
			}
			return ToolUse{Name: t, Args: map[string]any{key: path, "content": content}}
		}
	}
	return ShellStep(r, "printf %s "+shq(content)+" > "+shq(path))
}

// ShellStep runs a shell command with whichever shell tool the CLI offered.
func ShellStep(r *Request, command string) ToolUse {
	switch {
	case r.Tool("Bash") != "":
		return ToolUse{Name: r.Tool("Bash"), Args: map[string]any{"command": command}}
	case r.Tool("bash") != "":
		return ToolUse{Name: r.Tool("bash"), Args: map[string]any{"command": command}}
	case r.Tool("shell") != "":
		return ToolUse{Name: r.Tool("shell"), Args: map[string]any{"command": []string{"bash", "-lc", command}}}
	case r.Tool("exec_command") != "":
		return ToolUse{Name: r.Tool("exec_command"), Args: map[string]any{"cmd": command}}
	case r.Tool("exec") != "":
		// Codex "code mode" (its default model): one freeform tool that runs JavaScript, and the shell is the
		// nested tool tools.exec_command(...) of that runtime.
		js, _ := json.Marshal(command)
		return ToolUse{Name: r.Tool("exec"), Custom: true,
			Input: "const r = await tools.exec_command({ cmd: " + string(js) + ", yield_time_ms: 30000 });\ntext(r.output ?? JSON.stringify(r));\n"}
	}
	return ToolUse{Name: "bash", Args: map[string]any{"command": command}}
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func textOfContent(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

func or(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}
