package verify_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/7ar-chat/harness/adapters/redisx"
	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/features/verify"
)

// newRigWith is newRig with the verifier's options adjustable: mutate sees the options before the verifier is built.
func newRigWith(t *testing.T, files map[string]string, mutate func(o *verify.Options)) *rig {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	ctx := context.Background()
	th, _ := st.CreateThread(ctx, "alice", "t")
	backend := sandboxtest.NewBackend()
	backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(sandboxtest.FindExec(sb, sandboxtest.ShellExec(sb)))
		for p, c := range files {
			sb.Put(p, c)
		}
	}
	mgr := sandbox.NewManager(backend, st, redisx.NewCoordinator(rdb), sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour})
	if _, err := mgr.Acquire(ctx, th.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	r := &rig{sb: backend.Created()[0], run: &store.Run{ID: "run-2", ThreadID: th.ID, UserLogin: "alice"},
		log: t.TempDir() + "/check.log", arts: &fakeArtifacts{}}
	t.Setenv("CHECK_LOG", r.log)
	check := "bash " + fixture(t, "log_check.sh") + " {file}"
	o := verify.Options{Sandbox: mgr, Artifacts: r.arts,
		Checkers: map[string]verify.Checker{
			".pptx": {Name: "fit_text", Command: check, Baseline: "--baseline {baseline}"},
			".docx": {Name: "inspect_docx", Command: check},
		},
		Citations: &verify.Checker{Name: "check_citations", Command: "bash " + fixture(t, "citations.sh") + " {sources} {file}"},
	}
	if mutate != nil {
		mutate(&o)
	}
	r.v = verify.New(o)
	return r
}

func (r *rig) upload(path, content string) {
	r.arts.add(store.Artifact{ID: "u-" + path, ThreadID: r.run.ThreadID, Path: path, Name: path, Kind: store.ArtifactUpload}, fakeVersion{content, ""})
}

type getManager struct {
	sandbox.Manager
	err error
}

func (m getManager) Get(context.Context, string) (sandbox.Sandbox, error) { return nil, m.err }

type fakeMessages struct {
	msgs []*store.Message
	err  error
}

func (f fakeMessages) ListMessages(context.Context, string) ([]*store.Message, error) {
	return f.msgs, f.err
}

func TestCheckerNamesAndCommandsOfTheDefaultConfiguration(t *testing.T) {
	// Arrange
	v := verify.New(verify.Options{})

	// Act / Assert
	for path, want := range map[string]string{
		"/workspace/outputs/a.docx": "inspect_docx", "/workspace/outputs/A.DOCX": "inspect_docx", "/workspace/outputs/a.xlsx": "recalc",
		"/workspace/outputs/a.py": "py_compile", "/workspace/outputs/a.JSON": "json", "/workspace/outputs/a.csv": "csv",
		"/workspace/outputs/a.png": "make_chart", "/workspace/outputs/a.txt": "", "/workspace/outputs/noext": "",
	} {
		if got := v.CheckerFor(path); got != want {
			t.Errorf("CheckerFor(%s) = %q, want %q", path, got, want)
		}
	}
	if got := v.CheckCommand("/workspace/outputs/it's.docx"); got != `python3 /skills/docx/scripts/inspect_docx.py --check '/workspace/outputs/it'\''s.docx'` {
		t.Errorf("CheckCommand quotes the path badly: %s", got)
	}
	if got := v.CheckCommand("/workspace/outputs/a.csv"); got != "python3 "+verify.ValidatorScript+" '/workspace/outputs/a.csv'" {
		t.Errorf("generic validator command: %s", got)
	}
	if got := v.CheckCommand("/workspace/outputs/a.txt"); got != "" {
		t.Errorf("a type without checker has the command %q", got)
	}
}

func TestACheckThatCannotStartReportsWhyAndIsFailedNotBad(t *testing.T) {
	failedMgr := func(err error) func(o *verify.Options) {
		return func(o *verify.Options) { o.Sandbox = getManager{err: err} }
	}
	cases := []struct {
		name   string
		mutate func(o *verify.Options)
		runMod func(r *store.Run)
		path   string
		want   string
	}{
		{"no checker for the type", nil, nil, "/workspace/outputs/a.txt", ": no checker for this file type"},
		{"no sandbox configured", func(o *verify.Options) { o.Sandbox = nil }, nil, "/workspace/outputs/a.docx", "inspect_docx: could not run (no sandbox)"},
		{"sandbox disabled for the run", nil, func(r *store.Run) { r.Sandbox = store.SandboxOff }, "/workspace/outputs/a.docx", "inspect_docx: could not run (no sandbox)"},
		{"sandbox gone", failedMgr(sandbox.ErrNoSandbox), nil, "/workspace/outputs/a.docx", "inspect_docx: could not run (the sandbox is gone)"},
		{"sandbox error", failedMgr(errors.New("boom")), nil, "/workspace/outputs/a.docx", "inspect_docx: could not run (sandbox unavailable)"},
		{"skill install fails", func(o *verify.Options) {
			o.Checkers[".docx"] = verify.Checker{Name: "inspect_docx", Skill: "docx", Command: "true"}
			o.EnsureSkill = func(context.Context, *store.Run, sandbox.Sandbox, string) error { return errors.New("no network") }
		}, nil, "/workspace/outputs/a.docx", "inspect_docx: could not run (installing the docx skill failed)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			r := newRigWith(t, map[string]string{"/workspace/outputs/a.docx": "x"}, tc.mutate)
			if tc.runMod != nil {
				tc.runMod(r.run)
			}

			// Act
			res := r.v.Check(context.Background(), r.run, tc.path)

			// Assert
			if !res.Failed || res.OK || res.Path != tc.path || !strings.Contains(res.Summary, tc.want) {
				t.Fatalf("result %+v, want summary ~ %q", res, tc.want)
			}
		})
	}
}

func TestTheSkillIsInstalledBeforeItsCheckerRuns(t *testing.T) {
	// Arrange
	var installed []string
	r := newRigWith(t, map[string]string{"/workspace/outputs/a.docx": "ok"}, func(o *verify.Options) {
		o.Checkers[".docx"] = verify.Checker{Name: "inspect_docx", Skill: "docx", Command: "bash " + fixture(t, "log_check.sh") + " {file}"}
		o.EnsureSkill = func(_ context.Context, run *store.Run, _ sandbox.Sandbox, skill string) error {
			installed = append(installed, run.ID+":"+skill)
			if _, err := os.Stat(os.Getenv("CHECK_LOG")); err == nil {
				t.Error("the checker ran before the skill was installed")
			}
			return nil
		}
	})

	// Act
	res := r.v.Check(context.Background(), r.run, "/workspace/outputs/a.docx")

	// Assert
	if !res.OK || len(installed) != 1 || installed[0] != "run-2:docx" {
		t.Fatalf("result %+v installed %v", res, installed)
	}
}

func TestACheckerThatCrashesOrAnswersNothingUsefulIsFailedWithItsStderrTail(t *testing.T) {
	cases := []struct {
		name, script string
		wantPrefix   string
		wantTail     string
	}{
		{"crash with exit 1", `echo "Traceback: boom" >&2; exit 1`, "inspect_docx: could not run (exit 1) Traceback: boom", ""},
		{"no contract output", `echo "hello"; echo "warning" >&2; exit 3`, "inspect_docx: gave no result (exit 3) warning", ""},
		{"only the last 300 bytes of stderr", `printf 'A%.0s' $(seq 1 500) >&2; echo END >&2; exit 1`, "inspect_docx: could not run (exit 1) AAAA", "AEND"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			r := newRigWith(t, map[string]string{"/workspace/outputs/a.docx": "x"}, func(o *verify.Options) {
				o.Checkers[".docx"] = verify.Checker{Name: "inspect_docx", Command: tc.script}
			})

			// Act
			res := r.v.Check(context.Background(), r.run, "/workspace/outputs/a.docx")

			// Assert
			if !res.Failed || res.OK || !strings.HasPrefix(res.Summary, tc.wantPrefix) || !strings.HasSuffix(res.Summary, tc.wantTail) {
				t.Fatalf("result %+v", res)
			}
			if tc.wantTail != "" && len(res.Summary) > len("inspect_docx: could not run (exit 1) ")+300 {
				t.Fatalf("stderr tail not cut: %d bytes", len(res.Summary))
			}
		})
	}
}

func TestAnExecutionErrorIsFailedAndNamesTheCause(t *testing.T) {
	// Arrange
	r := newRigWith(t, map[string]string{"/workspace/outputs/a.docx": "x"}, nil)
	r.sb.SetExec(func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, errors.New(strings.Repeat("e", 300))
	})

	// Act
	res := r.v.Check(context.Background(), r.run, "/workspace/outputs/a.docx")

	// Assert
	if !res.Failed || res.Summary != "inspect_docx: could not run ("+strings.Repeat("e", 200)+"…)" {
		t.Fatalf("result %+v", res)
	}
}

func TestACheckerFailureOfTheFormatCheckerSkipsTheCitationCheck(t *testing.T) {
	// Arrange
	r := newRigWith(t, map[string]string{"/workspace/study.pdf": "%PDF", "/workspace/outputs/brief.docx": "CRASH"}, func(o *verify.Options) {
		o.Checkers[".docx"] = verify.Checker{Name: "inspect_docx", Command: "bash " + fixture(t, "check_doc.sh") + " {file}"}
	})
	r.upload("/workspace/study.pdf", "%PDF")

	// Act
	res := r.v.Check(context.Background(), r.run, "/workspace/outputs/brief.docx")

	// Assert
	if !res.Failed || strings.Contains(r.logged(t), "citations:") {
		t.Fatalf("result %+v log %q", res, r.logged(t))
	}
}

func TestTheNextStepsOfTheFormatAndCitationCheckersAreChained(t *testing.T) {
	// Arrange: the document breaks a format rule and invents a figure.
	files := map[string]string{"/workspace/study.pdf": "%PDF", "/workspace/outputs/brief.docx": "BROKEN INVENTED", "/workspace/outputs/ok.docx": "INVENTED"}
	r := newRigWith(t, files, func(o *verify.Options) {
		o.Checkers[".docx"] = verify.Checker{Name: "inspect_docx", Command: "bash " + fixture(t, "check_doc.sh") + " {file}"}
	})
	r.upload("/workspace/study.pdf", "%PDF")

	// Act
	both := r.v.Check(context.Background(), r.run, "/workspace/outputs/brief.docx")
	citationsOnly := r.v.Check(context.Background(), r.run, "/workspace/outputs/ok.docx")

	// Assert
	if both.OK || both.NextStep != "fix the heading levels, then run the checker again; then use the figure printed on the cited page" {
		t.Fatalf("both failing: %+v", both)
	}
	if !strings.Contains(both.Summary, "headings: Heading 3 directly after Heading 1") || !strings.Contains(both.Summary, "; check_citations: NOT OK") {
		t.Fatalf("summary %q", both.Summary)
	}
	if citationsOnly.OK || citationsOnly.NextStep != "use the figure printed on the cited page" {
		t.Fatalf("citations failing only: %+v", citationsOnly)
	}
}

func TestOnlyLiveUploadedPdfsOtherThanTheDeliverableAreCitationSourcesAtMostFive(t *testing.T) {
	// Arrange
	files := map[string]string{"/workspace/outputs/brief.docx": "text (p. 1)"}
	r := newRigWith(t, files, nil)
	for _, n := range []string{"g", "f", "e", "d", "c", "b", "a"} { // 7 uploaded PDFs present in the sandbox
		files["/workspace/"+n+".pdf"] = "%PDF"
		r.sb.Put("/workspace/"+n+".pdf", "%PDF")
		r.upload("/workspace/"+n+".pdf", "%PDF")
	}
	r.upload("/workspace/missing.pdf", "%PDF") // not in the sandbox (stat fails)
	r.upload("/workspace/notes.txt", "txt")    // not a PDF
	r.sb.Put("/workspace/notes.txt", "txt")
	r.arts.add(store.Artifact{ID: "gone", ThreadID: r.run.ThreadID, Path: "/workspace/deleted.pdf", Kind: store.ArtifactUpload, Deleted: true}, fakeVersion{"%PDF", ""})
	r.sb.Put("/workspace/deleted.pdf", "%PDF")
	r.arts.add(store.Artifact{ID: "out", ThreadID: r.run.ThreadID, Path: "/workspace/outputs/made.pdf", Kind: store.ArtifactOutput}, fakeVersion{"%PDF", "run-1"})
	r.sb.Put("/workspace/outputs/made.pdf", "%PDF")

	// Act
	res := r.v.Check(context.Background(), r.run, "/workspace/outputs/brief.docx")

	// Assert
	log := r.logged(t)
	line := log[strings.Index(log, "citations:"):]
	var at []int
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		i := strings.Index(line, "/workspace/"+n+".pdf")
		if i < 0 {
			t.Fatalf("source %s.pdf missing from %q", n, line)
		}
		at = append(at, i)
	}
	for i := 1; i < len(at); i++ {
		if at[i] <= at[i-1] {
			t.Fatalf("sources are not sorted: %q", line)
		}
	}
	for _, not := range []string{"/f.pdf", "/g.pdf", "missing.pdf", "notes.txt", "deleted.pdf", "made.pdf"} {
		if strings.Contains(line, not) {
			t.Fatalf("%s is a citation source: %q", not, line)
		}
	}
	if !res.OK || !strings.HasSuffix(strings.TrimSpace(line), "/workspace/outputs/brief.docx") {
		t.Fatalf("result %+v line %q", res, line)
	}
}

func TestACitationSkillThatCannotBeInstalledKeepsTheFormatVerdict(t *testing.T) {
	// Arrange
	r := newRigWith(t, map[string]string{"/workspace/outputs/brief.docx": "x"}, func(o *verify.Options) {
		o.Citations = &verify.Checker{Name: "check_citations", Skill: "pdf", Command: "true"}
		o.EnsureSkill = func(context.Context, *store.Run, sandbox.Sandbox, string) error { return errors.New("offline") }
	})
	r.upload("/workspace/study.pdf", "%PDF")
	r.sb.Put("/workspace/study.pdf", "%PDF")

	// Act
	res := r.v.Check(context.Background(), r.run, "/workspace/outputs/brief.docx")

	// Assert
	if !res.OK || res.Failed || !strings.HasSuffix(res.Summary, "; citations could not be checked (installing the pdf skill failed)") {
		t.Fatalf("result %+v", res)
	}
}

func TestCitationOutputWithoutCitationsOrWithNoiseIsReadLeniently(t *testing.T) {
	cases := []struct {
		name, out  string
		wantMerged bool
	}{
		{"citations list empty", `{"ok": true, "file": "f", "checks": [{"name": "c", "ok": true}], "citations": []}`, false},
		{"citations list absent counts as citing", `{"ok": true, "file": "f", "checks": [{"name": "c", "ok": true}]}`, true},
		{"noise and braces before the object", `log {x}` + "\n" + `{"ok": true, "file": "f", "checks": [{"name": "c", "ok": true}], "citations": [{}]}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			r := newRigWith(t, map[string]string{"/workspace/outputs/brief.docx": "x"}, func(o *verify.Options) {
				o.Citations = &verify.Checker{Name: "check_citations", Command: "printf '%s' '" + tc.out + "'"}
			})
			r.upload("/workspace/study.pdf", "%PDF")
			r.sb.Put("/workspace/study.pdf", "%PDF")

			// Act
			res := r.v.Check(context.Background(), r.run, "/workspace/outputs/brief.docx")

			// Assert
			if merged := strings.Contains(res.Summary, "check_citations:"); !res.OK || merged != tc.wantMerged {
				t.Fatalf("result %+v, merged=%v want %v", res, merged, tc.wantMerged)
			}
		})
	}
}

func TestCompleteReusesTheModelsFormatResultAndOnlyAddsWhatItMisses(t *testing.T) {
	base := agent.CheckResult{Path: "/workspace/outputs/brief.docx", Checker: "inspect_docx", OK: true, Summary: "inspect_docx: ok (1 checks passed)"}
	t.Run("a failed model result is final", func(t *testing.T) {
		// Arrange
		r := newRigWith(t, map[string]string{"/workspace/study.pdf": "%PDF", "/workspace/outputs/brief.docx": "INVENTED"}, nil)
		r.upload("/workspace/study.pdf", "%PDF")
		bad := base
		bad.OK, bad.Failed = false, true

		// Act
		got := r.v.Complete(context.Background(), r.run, "/workspace/outputs/brief.docx", bad)

		// Assert
		if got != bad || r.logged(t) != "" {
			t.Fatalf("got %+v log %q", got, r.logged(t))
		}
	})
	t.Run("a type without checker keeps the base", func(t *testing.T) {
		// Arrange
		r := newRigWith(t, nil, nil)
		other := agent.CheckResult{Path: "/workspace/outputs/a.txt", Summary: "s"}

		// Act
		got := r.v.Complete(context.Background(), r.run, "/workspace/outputs/a.txt", other)

		// Assert
		if got != other {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a configured citation checker without any uploaded PDF leaves the base as it is", func(t *testing.T) {
		// Arrange
		r := newRigWith(t, map[string]string{"/workspace/outputs/brief.docx": "INVENTED"}, nil)

		// Act
		got := r.v.Complete(context.Background(), r.run, "/workspace/outputs/brief.docx", base)

		// Assert
		if got.Summary != base.Summary || !got.OK || strings.Contains(r.logged(t), "args:") {
			t.Fatalf("got %+v log %q", got, r.logged(t))
		}
	})
	t.Run("citations are checked without running the format checker again", func(t *testing.T) {
		// Arrange
		r := newRigWith(t, map[string]string{"/workspace/study.pdf": "%PDF", "/workspace/outputs/brief.docx": "INVENTED"}, nil)
		r.upload("/workspace/study.pdf", "%PDF")

		// Act
		got := r.v.Complete(context.Background(), r.run, "/workspace/outputs/brief.docx", base)

		// Assert
		if got.OK || !strings.Contains(got.Summary, "inspect_docx: ok") || !strings.Contains(got.Summary, "check_citations: NOT OK") ||
			strings.Contains(r.logged(t), "args:") || !strings.Contains(r.logged(t), "citations:") {
			t.Fatalf("got %+v log %q", got, r.logged(t))
		}
	})
	t.Run("a deliverable with a baseline is checked again with it", func(t *testing.T) {
		// Arrange
		r := newRigWith(t, map[string]string{"/workspace/deck.pptx": "ORIGINAL", "/workspace/outputs/deck.pptx": "EDITED"}, nil)
		r.upload("/workspace/deck.pptx", "ORIGINAL")
		deck := agent.CheckResult{Path: "/workspace/outputs/deck.pptx", Checker: "fit_text", OK: false, Summary: "fit_text: NOT OK (without baseline)"}

		// Act
		got := r.v.Complete(context.Background(), r.run, "/workspace/outputs/deck.pptx", deck)

		// Assert
		if !got.OK || !strings.Contains(r.logged(t), "baseline: ORIGINAL") {
			t.Fatalf("got %+v log %q", got, r.logged(t))
		}
	})
}

func TestTheBaselineIsWrittenOnceAndNamedByTheHashOfTheOriginal(t *testing.T) {
	// Arrange
	r := newRigWith(t, map[string]string{"/workspace/deck.pptx": "ORIGINAL", "/workspace/outputs/deck.pptx": "EDITED"}, nil)
	r.upload("/workspace/deck.pptx", "ORIGINAL")
	r.sb.Put("/workspace/deck.pptx", "ORIGINAL")

	// Act
	r.v.Check(context.Background(), r.run, "/workspace/outputs/deck.pptx")
	r.v.Check(context.Background(), r.run, "/workspace/outputs/deck.pptx")

	// Assert
	var written []string
	for _, e := range r.sb.Execs() {
		if i := strings.Index(e.Command, verify.BaselinesDir+"/"); i >= 0 {
			written = append(written, e.Command[i:i+len(verify.BaselinesDir)+1+16+len(".pptx")])
		}
	}
	if len(written) != 2 || written[0] != written[1] || !strings.HasSuffix(written[0], ".pptx") {
		t.Fatalf("baseline paths used by the two checks: %v", written)
	}
	n := 0
	for _, d := range r.sb.Mkdirs() {
		if d == verify.BaselinesDir {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the baseline directory was created %d times, want once (the second check reuses the file)", n)
	}
}

func TestADeckWhoseBaselineCannotBeFetchedOrWrittenIsCheckedWithoutOne(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *rig)
	}{
		{"content unavailable", func(r *rig) {
			r.v = rebuild(t, r, &brokenArtifacts{fakeArtifacts: r.arts, contentErr: errors.New("blob gone")})
		}},
		{"versions unavailable", func(r *rig) {
			r.v = rebuild(t, r, &brokenArtifacts{fakeArtifacts: r.arts, versionsErr: errors.New("db down")})
		}},
		{"sandbox write fails", func(r *rig) {
			r.sb.SetWriteErr(func(p string) error {
				if strings.HasPrefix(p, verify.BaselinesDir) {
					return errors.New("read-only filesystem")
				}
				return nil
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			r := newRigWith(t, map[string]string{"/workspace/outputs/deck.pptx": "EDITED"}, nil)
			r.upload("/workspace/deck.pptx", "ORIGINAL")
			tc.setup(r)

			// Act
			res := r.v.Check(context.Background(), r.run, "/workspace/outputs/deck.pptx")

			// Assert
			if !res.OK || strings.Contains(r.logged(t), "--baseline") || !strings.Contains(r.logged(t), "args: ") {
				t.Fatalf("result %+v log %q", res, r.logged(t))
			}
		})
	}
}

// brokenArtifacts fails the reads a test selects.
type brokenArtifacts struct {
	*fakeArtifacts
	contentErr, versionsErr error
}

func (b *brokenArtifacts) ListArtifactVersions(ctx context.Context, id string) ([]store.ArtifactVersion, error) {
	if b.versionsErr != nil {
		return nil, b.versionsErr
	}
	return b.fakeArtifacts.ListArtifactVersions(ctx, id)
}

func (b *brokenArtifacts) Content(ctx context.Context, a *store.Artifact, v int) ([]byte, *store.ArtifactVersion, error) {
	if b.contentErr != nil {
		return nil, nil, b.contentErr
	}
	return b.fakeArtifacts.Content(ctx, a, v)
}

// rebuild gives the rig a verifier on another artifact source (same sandbox, same checkers).
func rebuild(t *testing.T, r *rig, src verify.ArtifactSource) *verify.Verifier {
	t.Helper()
	check := "bash " + fixture(t, "log_check.sh") + " {file}"
	return verify.New(verify.Options{Sandbox: sandboxOf(t, r), Artifacts: src,
		Checkers: map[string]verify.Checker{".pptx": {Name: "fit_text", Command: check, Baseline: "--baseline {baseline}"}}})
}

// sandboxOf is a manager that always hands out the rig's sandbox.
func sandboxOf(t *testing.T, r *rig) sandbox.Manager { return fixedManager{sb: r.sb} }

type fixedManager struct {
	sandbox.Manager
	sb sandbox.Sandbox
}

func (m fixedManager) Get(context.Context, string) (sandbox.Sandbox, error) { return m.sb, nil }

func TestTheBaselineChoiceFavoursTheUsersVersionThenTheUploadOfTheSameName(t *testing.T) {
	t.Run("an upload with only agent versions is used as its first version", func(t *testing.T) {
		// Arrange
		r := newRigWith(t, map[string]string{"/workspace/outputs/deck.pptx": "EDITED"}, nil)
		r.arts.add(store.Artifact{ID: "u", ThreadID: r.run.ThreadID, Path: "/workspace/deck.pptx", Kind: store.ArtifactUpload},
			fakeVersion{"FIRST VERSION", "run-1"}, fakeVersion{"SECOND", "run-1"})

		// Act
		r.v.Check(context.Background(), r.run, "/workspace/outputs/deck.pptx")

		// Assert
		if !strings.Contains(r.logged(t), "baseline: FIRST VERSION") {
			t.Fatalf("log %q", r.logged(t))
		}
	})
	t.Run("a deleted upload is no baseline", func(t *testing.T) {
		// Arrange
		r := newRigWith(t, map[string]string{"/workspace/outputs/deck.pptx": "EDITED"}, nil)
		r.arts.add(store.Artifact{ID: "u", ThreadID: r.run.ThreadID, Path: "/workspace/deck.pptx", Kind: store.ArtifactUpload, Deleted: true}, fakeVersion{"OLD", ""})

		// Act
		r.v.Check(context.Background(), r.run, "/workspace/outputs/deck.pptx")

		// Assert
		if strings.Contains(r.logged(t), "--baseline") {
			t.Fatalf("log %q", r.logged(t))
		}
	})
	t.Run("the most recently updated upload of the name wins", func(t *testing.T) {
		// Arrange
		r := newRigWith(t, map[string]string{"/workspace/outputs/deck.pptx": "EDITED"}, nil)
		r.arts.add(store.Artifact{ID: "u1", ThreadID: r.run.ThreadID, Path: "/workspace/a/deck.pptx", Kind: store.ArtifactUpload, UpdatedAt: time.Unix(100, 0)}, fakeVersion{"OLDER", ""})
		r.arts.add(store.Artifact{ID: "u2", ThreadID: r.run.ThreadID, Path: "/workspace/b/deck.pptx", Kind: store.ArtifactUpload, UpdatedAt: time.Unix(200, 0)}, fakeVersion{"NEWER", ""})

		// Act
		r.v.Check(context.Background(), r.run, "/workspace/outputs/deck.pptx")

		// Assert
		if !strings.Contains(r.logged(t), "baseline: NEWER") {
			t.Fatalf("log %q", r.logged(t))
		}
	})
}

func toolMessage(id, created string, artifacts []any, verification []any) *store.Message {
	md := map[string]any{}
	if artifacts != nil {
		md["artifacts"] = artifacts
	}
	if verification != nil {
		md["verification"] = verification
	}
	return &store.Message{Role: store.RoleTool, ToolCallID: id, Metadata: md}
}

func file(path, change string) map[string]any { return map[string]any{"path": path, "change": change} }
func result(path string, ok bool, summary string) map[string]any {
	return map[string]any{"path": path, "checker": "validate", "ok": ok, "summary": summary}
}

// goalHistory is a thread with an older turn (before the goal) and two turns after it.
func goalHistory(goalAt time.Time) []*store.Message {
	at := func(m *store.Message, d time.Duration) *store.Message { m.CreatedAt = goalAt.Add(d); return m }
	return []*store.Message{
		at(&store.Message{Role: store.RoleUser, Content: "before the goal"}, -time.Hour),
		at(toolMessage("t0", "", []any{file("/workspace/outputs/old.txt", "created")}, []any{result("/workspace/outputs/old.txt", true, "old ok")}), -time.Hour),
		at(&store.Message{Role: store.RoleUser, Content: "turn 1"}, time.Minute),
		at(toolMessage("t1", "", []any{file("/workspace/outputs/a.txt", "created"), file("/workspace/notes.txt", "created")},
			[]any{result("/workspace/outputs/a.txt", true, "a ok")}), time.Minute),
		at(&store.Message{Role: store.RoleUser, Content: "turn 2"}, 2*time.Minute),
		at(toolMessage("t2", "", []any{file("/workspace/outputs/b.txt", "created")}, []any{result("/workspace/outputs/b.txt", false, "b broken")}), 2*time.Minute),
	}
}

func TestEvidenceCoversEveryTurnSinceTheGoalWithCheckerResultsAndText(t *testing.T) {
	// Arrange
	goalAt := time.Unix(10_000, 0)
	r := newRigWith(t, map[string]string{"/workspace/outputs/a.txt": "alpha", "/workspace/outputs/b.txt": strings.Repeat("x", 2500)},
		func(o *verify.Options) { o.Messages = fakeMessages{msgs: goalHistory(goalAt)} })
	g := &store.Goal{CreatedAt: goalAt}

	// Act
	since := r.v.Evidence(context.Background(), r.run, g)
	turn := r.v.Evidence(context.Background(), r.run, nil)

	// Assert
	if len(since) != 2 || since[0].Path != "/workspace/outputs/a.txt" || since[1].Path != "/workspace/outputs/b.txt" {
		t.Fatalf("since the goal: %+v", since)
	}
	a, b := since[0], since[1]
	if a.OK == nil || !*a.OK || a.Summary != "a ok" || a.Checker != "validate" || a.Text != "alpha" {
		t.Errorf("a = %+v", a)
	}
	if b.OK == nil || *b.OK || b.Summary != "b broken" || b.Text != strings.Repeat("x", verify.EvidenceChars)+"…" {
		t.Errorf("b = %+v (text %d chars)", b, len(b.Text))
	}
	if len(turn) != 1 || turn[0].Path != "/workspace/outputs/b.txt" {
		t.Errorf("the run's own turn: %+v", turn)
	}
}

func TestEvidenceKeepsTheLatestEightDeliverablesAndLeavesTextEmptyWithoutASandbox(t *testing.T) {
	// Arrange
	var changed []any
	for _, n := range "abcdefghij" {
		changed = append(changed, file("/workspace/outputs/"+string(n)+".txt", "created"))
	}
	msgs := []*store.Message{{Role: store.RoleUser, Content: "go"}, toolMessage("t", "", changed, nil)}
	v := verify.New(verify.Options{Messages: fakeMessages{msgs: msgs}})

	// Act
	ev := v.Evidence(context.Background(), &store.Run{ThreadID: "t"}, nil)

	// Assert
	if len(ev) != 8 || ev[0].Path != "/workspace/outputs/c.txt" || ev[7].Path != "/workspace/outputs/j.txt" {
		t.Fatalf("evidence %+v", ev)
	}
	for _, e := range ev {
		if e.Text != "" || e.OK != nil || e.Checker != "" {
			t.Fatalf("an unchecked deliverable without sandbox: %+v", e)
		}
	}
}

func TestEvidenceAndDeliverablesAreEmptyWhenMessagesCannotBeRead(t *testing.T) {
	cases := map[string]verify.Options{
		"no message source": {},
		"unreadable":        {Messages: fakeMessages{err: errors.New("db down")}},
		"nothing changed":   {Messages: fakeMessages{msgs: []*store.Message{{Role: store.RoleUser, Content: "hi"}}}},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			v := verify.New(o)
			run := &store.Run{ThreadID: "t"}

			// Act
			ev, evGoal, ds := v.Evidence(context.Background(), run, nil), v.Evidence(context.Background(), run, &store.Goal{}), v.Deliverables(context.Background(), run)

			// Assert
			if len(ev) != 0 || len(evGoal) != 0 || len(ds) != 0 {
				t.Fatalf("evidence %v / %v deliverables %v", ev, evGoal, ds)
			}
		})
	}
}

func TestDeliverablesListsTheTurnsChangedFilesWithTheirLastCheck(t *testing.T) {
	// Arrange
	v := verify.New(verify.Options{Messages: fakeMessages{msgs: goalHistory(time.Unix(10_000, 0))}})

	// Act
	ds := v.Deliverables(context.Background(), &store.Run{ThreadID: "t"})

	// Assert
	if len(ds) != 1 || ds[0].Path != "/workspace/outputs/b.txt" || ds[0].OK == nil || *ds[0].OK || ds[0].Summary != "b broken" {
		t.Fatalf("deliverables %+v", ds)
	}
}
