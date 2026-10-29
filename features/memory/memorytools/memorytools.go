// Package memorytools exposes Work's memory MCP tools (memory_search,
// memory_get, memory_create, memory_update, memory_delete,
// memory_create_batch, memory_delete_batch, memory_capabilities) with the
// same names, input schemas and result shapes, backed by memory.Service.
package memorytools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/features/memory"
)

// Names are the tools, in Work's order (MEMORY_MCP_ENABLED_TOOLS).
var Names = []string{"memory_search", "memory_get", "memory_create", "memory_update", "memory_delete",
	"memory_create_batch", "memory_delete_batch", "memory_capabilities"}

type spec struct {
	name, desc, schema string
}

// specs are Work's _tool_schemas (descriptions and input schemas).
var specs = []spec{
	{"memory_search", "Search memory entries with scope, filters, and provenance.", `{"type":"object","properties":{
		"query":{"type":"string"},"scope":{"type":"string","enum":["personal","repository","workspace"]},"repository":{"type":"string"},
		"top_k":{"type":"integer","minimum":1,"maximum":50},"filters":{"type":"object"}},"required":["query"]}`},
	{"memory_get", "Get a specific memory entry by id or path.", `{"type":"object","properties":{
		"id":{"type":"string"},"path":{"type":"string"},"include_metadata":{"type":"boolean"}}}`},
	{"memory_create", "Create a durable memory entry.", `{"type":"object","properties":{
		"content":{"type":"string"},"metadata":{"type":"object"},"tags":{"type":"array","items":{"type":"string"}},
		"scope":{"type":"string","enum":["personal","repository","workspace"]},"repository":{"type":"string"},"path":{"type":"string"},
		"audience":{"type":"string","enum":["project","me"],"description":"In a project: project (default) is shared by the team, me is the current user's own memory inside this project."}},
		"required":["content"]}`},
	{"memory_update", "Update durable memory content and/or metadata.", `{"type":"object","properties":{
		"id":{"type":"string"},"path":{"type":"string"},"content":{"type":"string"},"metadata":{"type":"object"},
		"tags":{"type":"array","items":{"type":"string"}}}}`},
	{"memory_delete", "Delete a durable memory entry with policy checks.", `{"type":"object","properties":{
		"id":{"type":"string"},"path":{"type":"string"},"metadata":{"type":"object"}}}`},
	{"memory_create_batch", "Create multiple durable memory entries.", `{"type":"object","properties":{
		"entries":{"type":"array","items":{"type":"object","properties":{"content":{"type":"string"},"metadata":{"type":"object"},
		"tags":{"type":"array","items":{"type":"string"}},"scope":{"type":"string"},"repository":{"type":"string"},"path":{"type":"string"}},
		"required":["content"]}}},"required":["entries"]}`},
	{"memory_delete_batch", "Delete multiple durable memory entries, given by ids, paths or both.", `{"type":"object","properties":{
		"ids":{"type":"array","items":{"type":"string"}},"paths":{"type":"array","items":{"type":"string"}}}}`},
	{"memory_capabilities", "Return backend capability flags for this memory proxy.", `{"type":"object","properties":{}}`},
}

// InputSchema returns the JSON input schema of a tool (tests, docs).
func InputSchema(name string) (map[string]any, bool) {
	for _, s := range specs {
		if s.name == name {
			var out map[string]any
			_ = json.Unmarshal([]byte(s.schema), &out)
			return out, true
		}
	}
	return nil, false
}

// Provider offers the memory tools when the user's memory is enabled.
type Provider struct {
	Svc *memory.Service
}

// New returns the provider.
func New(svc *memory.Service) *Provider { return &Provider{Svc: svc} }

// Name implements tools.Provider.
func (p *Provider) Name() string { return "memory" }

// Tools implements tools.Provider.
func (p *Provider) Tools(ctx context.Context, rc tools.RunContext) ([]tool.BaseTool, func(), error) {
	layers := p.Svc.Layers(ctx, rc.Login, rc.ThreadID)
	enabled, err := p.Svc.Enabled(ctx, layers[0].Key)
	if err != nil {
		return nil, nil, err
	}
	if !enabled {
		return nil, func() {}, nil
	}
	out := make([]tool.BaseTool, 0, len(specs))
	for _, s := range specs {
		var js jsonschema.Schema
		if err := json.Unmarshal([]byte(s.schema), &js); err != nil {
			return nil, nil, fmt.Errorf("memory tool %s schema: %w", s.name, err)
		}
		out = append(out, &memTool{p: p, rc: rc, layers: layers, info: &schema.ToolInfo{Name: s.name, Desc: s.desc,
			ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&js)}})
	}
	return out, func() {}, nil
}

type memTool struct {
	p  *Provider
	rc tools.RunContext
	// layers are the memories of the run: the project's (or the user's own in a personal thread) first,
	// then the user's private layer inside a project.
	layers []memory.Layer
	info   *schema.ToolInfo
}

func (t *memTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }

// InvokableRun validates the arguments like Work, runs the operation and
// returns its JSON result; an error result is returned as a tool error
// ("Error: {"error": {code, message, details}}").
func (t *memTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	name := t.info.Name
	args, err := decodeArgs(argsJSON)
	if err != nil {
		return "", fail(errPayload("invalid_arguments", "Memory tool arguments must be an object", name))
	}
	if msg := validate(name, args); msg != "" {
		return "", fail(errPayload("invalid_arguments", msg, name))
	}
	key := t.layers[0].Key
	if aud, _ := args["audience"].(string); aud == "me" && len(t.layers) > 1 {
		key = t.layers[1].Key
	}
	w := memory.Writer{Login: key, ThreadID: t.rc.ThreadID, Source: store.MemorySourceProxy, Untrusted: tools.Untrusted(ctx)}
	svc := t.p.Svc
	// Operations on an existing entry find it in any layer.
	onLayers := func(op func(w memory.Writer) memory.Result) memory.Result {
		var r memory.Result
		for i, l := range t.layers {
			lw := w
			lw.Login = l.Key
			r = op(lw)
			if !r.IsError || i == len(t.layers)-1 || errCode(r.Data) != "memory_not_found" {
				return r
			}
		}
		return r
	}
	var r memory.Result
	switch name {
	case "memory_search":
		topK := memory.DefaultTopK
		if n, ok := intArg(args["top_k"]); ok && n != 0 {
			topK = n
		}
		r = svc.SearchAll(ctx, t.layers, memory.SearchInput{Query: str(args["query"]), Scope: str(args["scope"]),
			Repository: str(args["repository"]), TopK: topK, Filters: obj(args["filters"])})
	case "memory_get":
		include := true
		if b, ok := args["include_metadata"].(bool); ok {
			include = b
		}
		r = onLayers(func(w memory.Writer) memory.Result {
			return svc.Get(ctx, w.Login, str(args["id"]), str(args["path"]), include)
		})
	case "memory_create":
		r = svc.Create(ctx, w, createInput(args))
	case "memory_update":
		in := memory.UpdateInput{ID: str(args["id"]), Path: str(args["path"]), Metadata: obj(args["metadata"])}
		if c, ok := args["content"].(string); ok {
			in.Content = &c
		}
		if list, ok := args["tags"].([]any); ok {
			tags := strs(list)
			in.Tags = &tags
		}
		r = onLayers(func(w memory.Writer) memory.Result { return svc.Update(ctx, w, in) })
	case "memory_delete":
		r = onLayers(func(w memory.Writer) memory.Result {
			return svc.Delete(ctx, w, str(args["id"]), str(args["path"]), obj(args["metadata"]), "")
		})
	case "memory_create_batch":
		var entries []memory.CreateInput
		for _, item := range args["entries"].([]any) {
			if m, ok := item.(map[string]any); ok {
				entries = append(entries, createInput(m))
			}
		}
		r = svc.CreateBatch(ctx, w, entries)
	case "memory_delete_batch":
		ids, _ := args["ids"].([]any)
		paths, _ := args["paths"].([]any)
		r = svc.DeleteBatch(ctx, w, strs(ids), strs(paths))
	case "memory_capabilities":
		r = svc.Capabilities()
	default:
		return "", fail(errPayload("tool_not_found", "Unsupported memory tool: "+name, name))
	}
	for _, c := range r.Changes {
		tools.AfterResult(ctx, agui.CustomEvent(agui.CustomMemory, EventValue(c)))
	}
	b, _ := json.Marshal(r.Data)
	if r.IsError {
		return "", fail(r.Data)
	}
	return string(b), nil
}

// EventValue is the work.memory payload of a change: {action: created |
// updated | proposed | deleted, id, path, title, text, status, contentHash,
// proposal? (create | update: what waits for the user), mode?, proposed?
// (deleted after untrusted content: soft, undoable)}. For a proposed
// update, title, text and contentHash are the proposal's (the entry keeps its
// confirmed content until the user keeps it).
func EventValue(c memory.Change) map[string]any {
	e := c.Entry
	v := map[string]any{"action": c.Action, "id": e.ID, "path": e.Path, "title": e.Title, "text": memory.Snippet(e.Content),
		"status": e.Status, "contentHash": e.ContentHash}
	if c.Action == "proposed" {
		v["proposal"] = "create"
		if p := e.Proposal; p != nil {
			v["proposal"], v["title"], v["text"], v["contentHash"] = "update", p.Title, memory.Snippet(p.Content), p.ContentHash
		}
	}
	if c.Mode != "" {
		v["mode"] = c.Mode
	}
	if c.Proposed {
		v["proposed"] = true // a delete after untrusted content: soft, the user can undo it
	}
	return v
}

func createInput(m map[string]any) memory.CreateInput {
	tags, _ := m["tags"].([]any)
	return memory.CreateInput{Content: str(m["content"]), Metadata: obj(m["metadata"]), Tags: strs(tags), Scope: str(m["scope"]),
		Repository: str(m["repository"]), Path: str(m["path"])}
}

func fail(payload map[string]any) error {
	b, _ := json.Marshal(payload)
	return &tools.UserError{Msg: string(b)}
}

func errPayload(code, message, name string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": message, "details": map[string]any{"tool": name}}}
}

func decodeArgs(s string) (map[string]any, error) {
	if strings.TrimSpace(s) == "" {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if v == nil {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("not an object")
	}
	return m, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func strs(list []any) []string {
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// intArg accepts JSON integers only (Work rejects booleans and floats).
func intArg(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return int(i), true
}

// validate is Work's _validate_tool_arguments ("" when valid), plus Work
// Chat's batch bound.
func validate(name string, args map[string]any) string {
	optString := func(key string, required bool) string {
		v, ok := args[key]
		if !ok {
			if required {
				return key + " is required"
			}
			return ""
		}
		if _, isStr := v.(string); !isStr {
			return key + " must be a string"
		}
		return ""
	}
	optObject := func(key string) string {
		if v, ok := args[key]; ok {
			if _, isObj := v.(map[string]any); !isObj {
				return key + " must be an object"
			}
		}
		return ""
	}
	optStrings := func(key string, required bool) string {
		v, ok := args[key]
		if !ok {
			if required {
				return key + " is required"
			}
			return ""
		}
		list, isList := v.([]any)
		if !isList {
			return key + " must be an array of strings"
		}
		for _, item := range list {
			if _, isStr := item.(string); !isStr {
				return key + " must be an array of strings"
			}
		}
		return ""
	}
	first := func(msgs ...string) string {
		for _, m := range msgs {
			if m != "" {
				return m
			}
		}
		return ""
	}
	idOrPath := func() string {
		if str(args["id"]) == "" && str(args["path"]) == "" {
			return "id or path is required"
		}
		return ""
	}
	switch name {
	case "memory_search":
		if m := optString("query", true); m != "" {
			return m
		}
		if strings.TrimSpace(str(args["query"])) == "" {
			return "query is required"
		}
		if m := first(optString("scope", false), optString("repository", false)); m != "" {
			return m
		}
		if v, ok := args["top_k"]; ok {
			n, isInt := intArg(v)
			if !isInt {
				return "top_k must be an integer"
			}
			if n < 1 || n > memory.MaxTopK {
				return "top_k must be between 1 and 50"
			}
		}
		return optObject("filters")
	case "memory_get":
		if m := first(optString("id", false), optString("path", false), idOrPath()); m != "" {
			return m
		}
		if v, ok := args["include_metadata"]; ok {
			if _, isBool := v.(bool); !isBool {
				return "include_metadata must be a boolean"
			}
		}
	case "memory_create":
		return first(optString("content", true), optObject("metadata"), optStrings("tags", false),
			optString("scope", false), optString("repository", false), optString("path", false))
	case "memory_update", "memory_delete":
		if m := first(optString("id", false), optString("path", false), idOrPath(), optObject("metadata")); m != "" {
			return m
		}
		if name == "memory_update" {
			return first(optString("content", false), optStrings("tags", false))
		}
	case "memory_create_batch":
		entries, ok := args["entries"].([]any)
		if !ok {
			return "entries must be an array"
		}
		if len(entries) > memory.MaxBatch {
			return fmt.Sprintf("entries must contain at most %d items", memory.MaxBatch)
		}
		for _, item := range entries {
			e, isObj := item.(map[string]any)
			if !isObj {
				return "entries must contain objects"
			}
			if _, isStr := e["content"].(string); !isStr {
				return "each batch entry requires string content"
			}
			if v, has := e["metadata"]; has {
				if _, isObj := v.(map[string]any); !isObj {
					return "batch entry metadata must be an object"
				}
			}
			if v, has := e["tags"]; has {
				list, isList := v.([]any)
				if !isList {
					return "batch entry tags must be an array of strings"
				}
				for _, t := range list {
					if _, isStr := t.(string); !isStr {
						return "batch entry tags must be an array of strings"
					}
				}
			}
			for _, k := range []string{"scope", "repository", "path"} {
				if v, has := e[k]; has {
					if _, isStr := v.(string); !isStr {
						return "batch entry " + k + " must be a string"
					}
				}
			}
		}
	case "memory_delete_batch":
		if m := first(optStrings("ids", false), optStrings("paths", false)); m != "" {
			return m
		}
		ids, _ := args["ids"].([]any)
		paths, _ := args["paths"].([]any)
		if len(ids)+len(paths) == 0 {
			return "ids or paths is required"
		}
		if len(ids)+len(paths) > memory.MaxBatch {
			return fmt.Sprintf("ids and paths must contain at most %d items", memory.MaxBatch)
		}
	}
	return ""
}

// errCode is the code of a memory error payload ({"error": {"code": …}} or {"code": …}).
func errCode(data map[string]any) string {
	if e, ok := data["error"].(map[string]any); ok {
		data = e
	}
	c, _ := data["code"].(string)
	return c
}
