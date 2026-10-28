package sandboxtools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
)

type listResult struct {
	Path    string `json:"path"`
	Entries []struct {
		Path string `json:"path"`
		Type string `json:"type"`
		Size int64  `json:"size"`
	} `json:"entries"`
	Truncated bool `json:"truncated"`
}

func TestListFilesDefaultsToWorkspaceDepthTwo(t *testing.T) {
	// list_files lists /workspace two levels deep by default.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.Put("/workspace/a.txt", "1")
		sb.Put("/workspace/d/b.txt", "22")
		sb.Put("/workspace/d/e/c.txt", "333")
	})

	// Act
	out, err := f.call(context.Background(), "list_files", `{}`)

	// Assert
	var r listResult
	_ = json.Unmarshal([]byte(out), &r)
	paths := map[string]bool{}
	for _, e := range r.Entries {
		paths[e.Path] = true
	}
	if err != nil || r.Path != "/workspace" || !paths["/workspace/a.txt"] || !paths["/workspace/d/b.txt"] || paths["/workspace/d/e/c.txt"] {
		t.Fatalf("err %v out %s", err, out)
	}
}

func TestListFilesCapsEntries(t *testing.T) {
	// At most 500 entries are returned and truncation is flagged.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		for i := 0; i < 600; i++ {
			sb.Put(fmt.Sprintf("/workspace/f%03d", i), "x")
		}
	})

	// Act
	out, _ := f.call(context.Background(), "list_files", `{"path":"/workspace","depth":1}`)

	// Assert
	var r listResult
	_ = json.Unmarshal([]byte(out), &r)
	if len(r.Entries) != 500 || !r.Truncated {
		t.Fatalf("entries %d truncated %v", len(r.Entries), r.Truncated)
	}
}
