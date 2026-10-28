package tools_test

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/memory"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/skills"
	"github.com/GhaziBenDahmane/harness/store/memstore"
	"github.com/GhaziBenDahmane/harness/tools"
	"github.com/GhaziBenDahmane/harness/tools/harnesstools"
	"github.com/GhaziBenDahmane/harness/tools/memorytools"
	"github.com/GhaziBenDahmane/harness/tools/sandboxtools"
	"github.com/GhaziBenDahmane/harness/tools/skilltools"
)

var update = flag.Bool("update", false, "rewrite testdata/schemas.golden.json")

// renderedTool is what the gateway receives for one tool.
type renderedTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	ReadOnly    bool           `json:"readOnly,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

// allTools builds every built-in tool of a typical run (sandbox, skills, harness, memory).
func allTools(t *testing.T) []renderedTool {
	t.Helper()
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	mgr := sandbox.NewManager(sandboxtest.NewBackend(), st, rdb, sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour})
	reg := skills.NewRegistry(os.DirFS(filepath.Join("..", "skills", "testdata", "builtin")), st)
	registry := tools.NewRegistry(
		sandboxtools.New(mgr, sandboxtools.Options{CommandTimeout: time.Minute}),
		&skilltools.Provider{Registry: reg, Installer: &skills.Installer{Registry: reg, Redis: rdb}, Sandbox: mgr},
		harnesstools.New(st),
		memorytools.New(memory.New(st, memory.Options{})),
	)
	list, _, err := registry.Tools(ctx, tools.RunContext{ThreadID: "t1", RunID: "r1", Login: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	var out []renderedTool
	for _, tl := range list {
		info, err := tl.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		js, err := info.ParamsOneOf.ToJSONSchema()
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(js)
		var params map[string]any
		_ = json.Unmarshal(raw, &params)
		ro, _ := tl.(tools.ReadOnly)
		out = append(out, renderedTool{Name: info.Name, Description: info.Desc, ReadOnly: ro != nil && ro.ReadOnly(), Parameters: params})
	}
	return out
}

// descriptions walks a JSON schema and returns "path: description" for every described node.
func descriptions(prefix string, node any, out map[string]string) {
	switch v := node.(type) {
	case map[string]any:
		if d, ok := v["description"].(string); ok {
			out[prefix] = d
		}
		for k, child := range v {
			descriptions(prefix+"/"+k, child, out)
		}
	case []any:
		for _, child := range v {
			descriptions(prefix+"[]", child, out)
		}
	}
}

func TestToolSchemasRenderFullDescriptions(t *testing.T) {
	// Every parameter description reaches the model whole: no description is cut (balanced brackets, no trailing comma clause lost), and every parameter is described.
	// Arrange
	rendered := allTools(t)

	// Act
	got := map[string]string{}
	for _, tl := range rendered {
		if !strings.HasPrefix(tl.Name, "memory_") { // Work's memory MCP schemas are mirrored verbatim
			descriptions(tl.Name, tl.Parameters, got)
			props, _ := tl.Parameters["properties"].(map[string]any)
			for name, p := range props {
				if _, ok := p.(map[string]any)["description"]; !ok {
					t.Errorf("%s.%s has no description", tl.Name, name)
				}
			}
		}
	}

	// Assert
	for where, d := range got {
		if strings.Count(d, "(") != strings.Count(d, ")") || strings.Count(d, "[") != strings.Count(d, "]") {
			t.Errorf("%s: truncated description %q", where, d)
		}
	}
}

func TestToolSchemasMatchGolden(t *testing.T) {
	// The rendered tool definitions (names, descriptions, read-only flags, parameter schemas) match the reviewed snapshot; run with -update after an intended change.
	// Arrange
	golden := filepath.Join("testdata", "schemas.golden.json")

	// Act
	got, err := json.MarshalIndent(allTools(t), "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("missing golden file (run go test ./internal/tools -run Golden -update): %v", err)
	}
	if strings.TrimSpace(string(want)) != strings.TrimSpace(string(got)) {
		t.Fatalf("tool schemas changed; review and run with -update.\n--- got ---\n%s", got)
	}
}
