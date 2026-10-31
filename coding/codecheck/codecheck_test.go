//go:build unix

package codecheck_test

import (
	"context"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/adapters/sandbox/local"
	"github.com/7ar-chat/harness/coding/codecheck"
	"github.com/7ar-chat/harness/core/sandbox"
)

func sb(t *testing.T) sandbox.Sandbox {
	t.Helper()
	b, err := local.New(local.Options{Workspace: t.TempDir(), StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	in, err := b.Create(context.Background(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func TestCheckReportsSyntaxErrorsAndStaysQuietOtherwise(t *testing.T) {
	// Arrange
	s := sb(t)
	ctx := context.Background()
	_ = s.WriteFile(ctx, "/workspace/bad.sh", []byte("if then\n"))
	_ = s.WriteFile(ctx, "/workspace/good.sh", []byte("echo hi\n"))
	_ = s.WriteFile(ctx, "/workspace/notes.txt", []byte("if then"))
	c := codecheck.New(nil)

	// Act
	bad, good, none := c.Check(ctx, s, "/workspace/bad.sh"), c.Check(ctx, s, "/workspace/good.sh"), c.Check(ctx, s, "/workspace/notes.txt")

	// Assert
	if !strings.Contains(bad, "Syntax check failed") || !strings.Contains(bad, "bad.sh") {
		t.Fatalf("bad: %q", bad)
	}
	if good != "" || none != "" {
		t.Fatalf("good %q none %q", good, none)
	}
}

func TestMissingToolIsSkippedAndCustomChecksOverrideDefaults(t *testing.T) {
	// Arrange
	s := sb(t)
	ctx := context.Background()
	_ = s.WriteFile(ctx, "/workspace/a.zzz", []byte("x"))
	_ = s.WriteFile(ctx, "/workspace/a.sh", []byte("if then\n"))
	missing := codecheck.New(map[string]string{".zzz": "no-such-tool-xyz {path}"})
	disabled := codecheck.New(map[string]string{".sh": ""})

	// Act
	m, d := missing.Check(ctx, s, "/workspace/a.zzz"), disabled.Check(ctx, s, "/workspace/a.sh")

	// Assert
	if m != "" || d != "" {
		t.Fatalf("missing %q disabled %q", m, d)
	}
}

func TestParse(t *testing.T) {
	// Act
	got, err := codecheck.Parse(".rs=cargo check -q; .TS = tsc --noEmit {path}")
	_, bad := codecheck.Parse("rs=cargo")

	// Assert
	if err != nil || got[".rs"] != "cargo check -q" || got[".ts"] != "tsc --noEmit {path}" || bad == nil {
		t.Fatalf("%v %v %v", got, err, bad)
	}
}

func TestParseIgnoresEmptyEntriesAndKeepsCommandEquals(t *testing.T) {
	// Act
	got, err := codecheck.Parse(" ; .py=python3 -c 'assert 1==1' {path};;")

	// Assert: blank entries are skipped, only the first "=" splits.
	if err != nil || len(got) != 1 || got[".py"] != "python3 -c 'assert 1==1' {path}" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestLongComplaintIsTruncatedForTheModel(t *testing.T) {
	// Arrange: a check that prints far more than the model should see.
	s := sb(t)
	ctx := context.Background()
	_ = s.WriteFile(ctx, "/workspace/a.big", []byte("x"))
	c := codecheck.New(map[string]string{".big": "sh -c 'yes ABCDEFGHIJ | head -c 20000; exit 3' {path}"})

	// Act
	got := c.Check(ctx, s, "/workspace/a.big")

	// Assert: header with the exit code, bounded body marked as cut.
	if !strings.Contains(got, "exit 3") || !strings.HasSuffix(got, "…") {
		t.Fatalf("complaint = %q", got[:min(len(got), 200)])
	}
	if len(got) > 2500 {
		t.Fatalf("complaint is %d bytes", len(got))
	}
}

func TestCheckQuotesPathsWithShellMetacharacters(t *testing.T) {
	// Arrange: a file name with a quote and a space must reach the checker as one argument.
	s := sb(t)
	ctx := context.Background()
	const name = "/workspace/it's bad.sh"
	_ = s.WriteFile(ctx, name, []byte("if then\n"))

	// Act
	got := codecheck.New(nil).Check(ctx, s, name)

	// Assert
	if !strings.Contains(got, "Syntax check failed") {
		t.Fatalf("check: %q", got)
	}
}
