package sandboxtools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
)

func TestEveryToolRetriesOnceOnANewSandboxWhenTheCachedOneIsGone(t *testing.T) {
	// A sandbox removed at the provider less than 30 s after its last use is replaced on the first failing call: the tool succeeds on a new sandbox and says /workspace was reset.
	cases := []struct{ tool, args, want string }{
		{"execute_command", `{"command":"ls"}`, `"exitCode":0`},
		{"read_file", `{"path":"notes.md"}`, "hello"},
		{"write_file", `{"path":"out.md","content":"x"}`, `"bytesWritten":1`},
		{"edit_file", `{"path":"notes.md","oldText":"hello","newText":"bye"}`, `"replacements":1`},
		{"list_files", `{}`, "/workspace/notes.md"},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			// Arrange
			f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/notes.md", "hello") })
			if _, err := f.call(context.Background(), "read_file", `{"path":"notes.md"}`); err != nil { // also lets edit_file edit it
				t.Fatal(err)
			}
			old := f.sb.ID()
			_ = f.backend.Kill(context.Background(), old)

			// Act
			out, err := f.call(context.Background(), c.tool, c.args)

			// Assert
			if err != nil || !strings.Contains(out, c.want) || !strings.Contains(out, "recreated") {
				t.Fatalf("out %q err %v", out, err)
			}
			if len(f.backend.Created()) != 2 || f.sb.ID() == old {
				t.Fatalf("sandboxes %d", len(f.backend.Created()))
			}
		})
	}
}
