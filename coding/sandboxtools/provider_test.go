package sandboxtools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/coding/sandboxtools"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/engine/tools"
)

func TestProviderIsAbsentWithoutAnEnabledManager(t *testing.T) {
	// Act
	none := sandboxtools.New(nil, sandboxtools.Options{})
	disabled := sandboxtools.New(stubManager{disabled: true}, sandboxtools.Options{})

	// Assert
	if none != nil || disabled != nil {
		t.Fatalf("providers %v %v, want nil", none, disabled)
	}
}

func TestARunWithoutSandboxGetsNoSandboxTools(t *testing.T) {
	// Arrange
	p := sandboxtools.New(stubManager{}, sandboxtools.Options{})

	// Act
	off, cleanup, err := p.Tools(context.Background(), tools.RunContext{ThreadID: "t", Sandbox: tools.SandboxOff})

	// Assert
	if err != nil || len(off) != 0 || cleanup == nil || p.Name() != "sandbox" {
		t.Fatalf("tools %d cleanup %v err %v name %q", len(off), cleanup != nil, err, p.Name())
	}
	cleanup()
}

func TestOnlyTheToolsWithoutSideEffectsAreReadOnly(t *testing.T) {
	// Plan mode offers only the read-only tools: a write tool marked read-only would run in a plan.
	// Arrange
	f := newFixture(t, nil)
	want := map[string]bool{"job_output": true, "read_file": true, "view_file": true, "search_files": true, "list_files": true,
		"execute_command": false, "job_kill": false, "write_file": false, "edit_file": false}

	// Act / Assert
	if len(f.tools) != len(want) {
		t.Fatalf("%d tools", len(f.tools))
	}
	for name, tl := range f.tools {
		ro, ok := tl.(tools.ReadOnly)
		if got := ok && ro.ReadOnly(); got != want[name] {
			t.Errorf("%s read-only = %v, want %v", name, got, want[name])
		}
	}
}

func TestTheCodingProfileDescribesReadFileWithoutDocumentOptions(t *testing.T) {
	// Arrange
	work := newFixtureWith(t, nil, nil)
	coding := newFixtureWith(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.go", "l1\nl2\nl3\n") },
		func(o *sandboxtools.Options, _ *sandbox.ManagerOptions) { o.Coding = true })

	// Act
	wi, _ := work.tools["read_file"].Info(context.Background())
	ci, _ := coding.tools["read_file"].Info(context.Background())
	out, err := coding.call(context.Background(), "read_file", `{"path":"a.go","offset":2,"limit":1}`)

	// Assert
	if _, ok := wi.Parameters["properties"].(map[string]any)["pages"]; !ok {
		t.Fatalf("work profile lost pages: %v", wi.Parameters)
	}
	props := ci.Parameters["properties"].(map[string]any)
	if _, ok := props["pages"]; ok || props["sheet"] != nil || props["offset"] == nil {
		t.Fatalf("coding properties %v", props)
	}
	if err != nil || !strings.HasPrefix(out, "     2\tl2\n") || ci.Desc == wi.Desc {
		t.Fatalf("err %v out %q", err, out)
	}
}

func TestSandboxAcquisitionFailuresAreExplainedToTheModel(t *testing.T) {
	cases := map[string]struct {
		err      error
		want     string
		userFace bool
	}{
		"quota":       {sandbox.ErrQuota, "already has 3 active sandboxes", true},
		"unavailable": {sandbox.ErrUnavailable, "temporarily unavailable", true},
		"other":       {errors.New("backend exploded"), "sandbox unavailable: backend exploded", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			p := sandboxtools.New(stubManager{acquire: func(context.Context, string, string) (sandbox.Sandbox, error) { return nil, c.err }},
				sandboxtools.Options{MaxSandboxesPerUser: 3})
			ts := toolsOf(t, p, tools.RunContext{ThreadID: "t", Login: "alice"})

			// Act
			_, err := ts["list_files"].InvokableRun(context.Background(), `{}`)

			// Assert
			var ue *tools.UserError
			if err == nil || !strings.Contains(err.Error(), c.want) || errors.As(err, &ue) != c.userFace {
				t.Fatalf("err %v (user error %v)", err, errors.As(err, &ue))
			}
		})
	}
}

func TestOnAcquireRunsForEveryToolCallWithTheRunContext(t *testing.T) {
	// Arrange
	var got []string
	f := newFixtureWith(t, nil, func(o *sandboxtools.Options, _ *sandbox.ManagerOptions) {
		o.OnAcquire = func(_ context.Context, rc tools.RunContext, sb sandbox.Sandbox) {
			got = append(got, rc.Login+"@"+sb.ID())
		}
	})

	// Act
	_, err1 := f.call(context.Background(), "list_files", `{}`)
	_, err2 := f.call(context.Background(), "execute_command", `{"command":"true"}`)

	// Assert
	if err1 != nil || err2 != nil || len(got) != 2 || got[0] != "alice@"+f.sb.ID() {
		t.Fatalf("errs %v %v hook calls %v", err1, err2, got)
	}
}

func TestAWorkspaceRestoreNoticeIsToldToTheModelOnce(t *testing.T) {
	// Arrange
	f := newFixtureWith(t, nil, func(_ *sandboxtools.Options, m *sandbox.ManagerOptions) {
		m.OnCreate = func(context.Context, string, sandbox.Sandbox, bool) string {
			return "restored 3 files from the last session"
		}
	})

	// Act
	first, err1 := f.call(context.Background(), "list_files", `{}`)
	second, err2 := f.call(context.Background(), "list_files", `{}`)

	// Assert
	if err1 != nil || err2 != nil || !strings.Contains(first, "restored 3 files") || strings.Contains(second, "restored") {
		t.Fatalf("first %s second %s errs %v %v", first, second, err1, err2)
	}
}

func TestARecreatedSandboxNoticePrefixesAnUnexpectedToolError(t *testing.T) {
	// A write that fails for a reason that is not the model's (disk full) still tells the model its workspace was reset.
	// Arrange
	fresh := sandboxtest.NewSandbox("sbx-new")
	fresh.SetWriteErr(func(string) error { return errors.New("disk full") })
	p := sandboxtools.New(recreatedManager{sb: fresh}, sandboxtools.Options{})
	ts := toolsOf(t, p, tools.RunContext{ThreadID: "t", Login: "alice"})

	// Act
	_, err := ts["write_file"].InvokableRun(context.Background(), `{"path":"a.txt","content":"x"}`)

	// Assert
	if err == nil || !strings.Contains(err.Error(), sandboxtools.RecreatedNotice+" write /workspace/a.txt: disk full") {
		t.Fatalf("err %v", err)
	}
}

func TestAFailingLeftoverSweepDoesNotFailTheToolCall(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.Put("/workspace/a.txt", "x")
		sb.FailScript("# work:sweep", errors.New("sweep exploded"))
	})

	// Act
	out, err := f.call(context.Background(), "list_files", `{}`)

	// Assert
	if err != nil || !strings.Contains(out, "/workspace/a.txt") {
		t.Fatalf("err %v out %s", err, out)
	}
}

func TestWritesThroughASymbolicLinkAreRefusedAndNothingIsWritten(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.Put("/workspace/real/x.txt", "original")
		sb.Symlink("/workspace/real", "/workspace/link")
	})
	_, _ = f.call(context.Background(), "read_file", `{"path":"link/x.txt"}`)

	// Act
	_, wErr := f.call(context.Background(), "write_file", `{"path":"link/y.txt","content":"new"}`)
	_, eErr := f.call(context.Background(), "edit_file", `{"path":"link/x.txt","oldText":"original","newText":"changed"}`)

	// Assert
	for _, err := range []error{wErr, eErr} {
		var ue *tools.UserError
		if err == nil || !errors.As(err, &ue) || !strings.Contains(err.Error(), "symbolic link") || !strings.Contains(err.Error(), "/workspace/real") {
			t.Fatalf("err %v", err)
		}
	}
	if _, ok := f.sb.File("/workspace/real/y.txt"); ok {
		t.Error("write_file followed the link")
	}
	if got, _ := f.sb.File("/workspace/real/x.txt"); got != "original" {
		t.Errorf("edit_file followed the link: %q", got)
	}
	if left := f.sb.Paths(sandbox.StagingDir + "/"); len(left) != 0 {
		t.Errorf("staged files left: %v", left)
	}
}

func TestAFailedWriteIsNotRememberedAsSeenByEditFile(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.SetWriteErr(func(p string) error {
			if strings.HasPrefix(p, sandbox.StagingDir) {
				return errors.New("disk full")
			}
			return nil
		})
		sb.Put("/workspace/n.md", "old")
	})

	// Act
	_, wErr := f.call(context.Background(), "write_file", `{"path":"n.md","content":"fresh"}`)
	_, eErr := f.call(context.Background(), "edit_file", `{"path":"n.md","oldText":"old","newText":"new"}`)

	// Assert
	if wErr == nil || !strings.Contains(wErr.Error(), "write /workspace/n.md") || !strings.Contains(wErr.Error(), "disk full") {
		t.Fatalf("write err %v", wErr)
	}
	if eErr == nil || !strings.Contains(eErr.Error(), "you have not read /workspace/n.md") {
		t.Fatalf("edit err %v", eErr)
	}
}

func TestWriteFileValidatesItsPath(t *testing.T) {
	// Arrange
	f := newFixture(t, nil)

	// Act
	_, empty := f.call(context.Background(), "write_file", `{"path":"  ","content":"x"}`)
	_, nul := f.call(context.Background(), "write_file", `{"path":"a\u0000b","content":"x"}`)

	// Assert
	if empty == nil || !strings.Contains(empty.Error(), "path is required") || nul == nil || !strings.Contains(nul.Error(), "invalid path") {
		t.Fatalf("empty %v nul %v", empty, nul)
	}
	if n := len(f.backend.Created()); n != 0 {
		t.Fatalf("%d sandboxes created for an invalid path", n)
	}
}

func TestWriteHooksAddTheirNoticesAndNeverFailTheWrite(t *testing.T) {
	// Arrange
	var onWrite []string
	f := newFixtureWith(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.py", "x = 1\n") },
		func(o *sandboxtools.Options, _ *sandbox.ManagerOptions) {
			o.OnWrite = func(_ context.Context, _ tools.RunContext, _ sandbox.Sandbox, p string, content []byte) error {
				onWrite = append(onWrite, p+"="+string(content))
				return errors.New("skill install failed")
			}
			o.CheckWrite = func(_ context.Context, _ sandbox.Sandbox, p string) string { return "syntax error in " + p }
			o.AfterPath = func(context.Context, tools.RunContext, sandbox.Sandbox, string) string { return "[dir notes]" }
		})

	// Act
	w, wErr := f.call(context.Background(), "write_file", `{"path":"b.py","content":"y = (\n"}`)
	e, eErr := readThenEdit(t, f, "a.py", `{"path":"a.py","oldText":"x = 1","newText":"x = ("}`)

	// Assert
	if wErr != nil || eErr != nil {
		t.Fatalf("errs %v %v", wErr, eErr)
	}
	if got := decodeMap(t, w)["notice"]; got != "syntax error in /workspace/b.py\n[dir notes]" {
		t.Errorf("write notice %q", got)
	}
	if got := decodeMap(t, e)["notice"]; got != "syntax error in /workspace/a.py\n[dir notes]" {
		t.Errorf("edit notice %q", got)
	}
	if got, _ := f.sb.File("/workspace/b.py"); got != "y = (\n" {
		t.Errorf("file %q", got)
	}
	if len(onWrite) != 2 || onWrite[0] != "/workspace/b.py=y = (\n" || onWrite[1] != "/workspace/a.py=x = (\n" {
		t.Errorf("OnWrite calls %q", onWrite)
	}
}

func TestListFilesBoundsAndErrors(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.Put("/workspace/d1/d2/d3/d4/d5/d6/f.txt", "x")
		sb.Put("/workspace/top.txt", "x")
	})
	paths := func(out string) map[string]bool {
		var r listResult
		_ = json.Unmarshal([]byte(out), &r)
		m := map[string]bool{}
		for _, e := range r.Entries {
			m[e.Path] = true
		}
		return m
	}

	// Act
	deep, err1 := f.call(context.Background(), "list_files", `{"depth":99}`)
	negative, err2 := f.call(context.Background(), "list_files", `{"path":"  ","depth":-3}`)
	_, missing := f.call(context.Background(), "list_files", `{"path":"nope"}`)
	_, nul := f.call(context.Background(), "list_files", `{"path":"a\u0000"}`)

	// Assert: depth is capped at 5, a non-positive depth falls back to 2, a blank path means /workspace.
	d := paths(deep)
	if err1 != nil || !d["/workspace/d1/d2/d3/d4/d5"] || d["/workspace/d1/d2/d3/d4/d5/d6"] {
		t.Fatalf("deep err %v paths %v", err1, d)
	}
	n := paths(negative)
	if err2 != nil || !n["/workspace/top.txt"] || !n["/workspace/d1/d2"] || n["/workspace/d1/d2/d3"] {
		t.Fatalf("negative err %v paths %v", err2, n)
	}
	if missing == nil || !strings.Contains(missing.Error(), "/workspace/nope: no such file or directory") {
		t.Fatalf("missing %v", missing)
	}
	if nul == nil || !strings.Contains(nul.Error(), "invalid path") {
		t.Fatalf("nul %v", nul)
	}
}
