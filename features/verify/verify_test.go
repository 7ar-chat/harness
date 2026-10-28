package verify_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/features/verify"
	"github.com/GhaziBenDahmane/harness/redisx"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/memstore"
)

// fakeArtifacts is a thread's artifact store: artifacts, their versions and contents.
type fakeArtifacts struct {
	arts     []*store.Artifact
	versions map[string][]store.ArtifactVersion
	blobs    map[string][]byte
}

func (f *fakeArtifacts) add(a store.Artifact, versions ...fakeVersion) {
	if f.versions == nil {
		f.versions, f.blobs = map[string][]store.ArtifactVersion{}, map[string][]byte{}
	}
	for i, v := range versions {
		sum := sha256.Sum256([]byte(v.data))
		key := hex.EncodeToString(sum[:])
		f.blobs[key] = []byte(v.data)
		f.versions[a.ID] = append(f.versions[a.ID], store.ArtifactVersion{Version: i + 1, SHA256: key, RunID: v.runID,
			SizeBytes: int64(len(v.data)), CreatedAt: time.Unix(int64(1000+i), 0)})
		a.SHA256, a.Version, a.RunID = key, i+1, v.runID
	}
	cp := a
	f.arts = append(f.arts, &cp)
}

type fakeVersion struct{ data, runID string }

func (f *fakeArtifacts) ListArtifacts(_ context.Context, threadID string) ([]*store.Artifact, error) {
	var out []*store.Artifact
	for _, a := range f.arts {
		if a.ThreadID == threadID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeArtifacts) ListArtifactVersions(_ context.Context, id string) ([]store.ArtifactVersion, error) {
	return f.versions[id], nil
}

func (f *fakeArtifacts) Content(_ context.Context, a *store.Artifact, version int) ([]byte, *store.ArtifactVersion, error) {
	for _, v := range f.versions[a.ID] {
		if v.Version == version {
			return f.blobs[v.SHA256], &v, nil
		}
	}
	return nil, nil, store.ErrNotFound
}

type rig struct {
	sb   *sandboxtest.Sandbox
	run  *store.Run
	log  string
	arts *fakeArtifacts
	v    *verify.Verifier
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", "checkers", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// newRig gives a live sandbox of a thread whose commands run with real bash,
// a verifier checking .pptx/.docx with the logging fixture and citations with
// the citations fixture, and the thread's artifact store.
func newRig(t *testing.T, files map[string]string) *rig {
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
		log: filepath.Join(t.TempDir(), "check.log"), arts: &fakeArtifacts{}}
	t.Setenv("CHECK_LOG", r.log)
	check := "bash " + fixture(t, "log_check.sh") + " {file}"
	r.v = verify.New(verify.Options{Sandbox: mgr, Artifacts: r.arts,
		Checkers: map[string]verify.Checker{
			".pptx": {Name: "fit_text", Command: check, Baseline: "--baseline {baseline}"},
			".docx": {Name: "inspect_docx", Command: check},
		},
		Citations: &verify.Checker{Name: "check_citations", Command: "bash " + fixture(t, "citations.sh") + " {sources} {file}"},
	})
	return r
}

func (r *rig) logged(t *testing.T) string {
	t.Helper()
	data, _ := os.ReadFile(r.log)
	return string(data)
}

func TestAnUploadOfTheSameNameIsTheDecksBaseline(t *testing.T) {
	// Review round 2 (N8): a deck under /workspace/outputs/ named like an uploaded deck is checked with fit_text --baseline <the upload as uploaded>, so only the overlaps the agent added fail.
	// Arrange
	r := newRig(t, map[string]string{"/workspace/client_update.pptx": "AGENT EDITED THE UPLOAD IN PLACE",
		"/workspace/outputs/client_update.pptx": "EDITED"})
	r.arts.add(store.Artifact{ID: "u1", ThreadID: r.run.ThreadID, Path: "/workspace/client_update.pptx", Name: "client_update.pptx", Kind: store.ArtifactUpload},
		fakeVersion{"ORIGINAL DECK", ""}, fakeVersion{"AGENT EDITED THE UPLOAD IN PLACE", "run-1"})

	// Act
	res := r.v.Check(context.Background(), r.run, "/workspace/outputs/client_update.pptx")

	// Assert
	if !res.OK || !strings.Contains(r.logged(t), "baseline: ORIGINAL DECK") {
		t.Fatalf("result %+v log %q", res, r.logged(t))
	}
}

func TestTheVersionBeforeTheAgentsChangesIsTheDecksBaseline(t *testing.T) {
	// A deck the user put in /workspace/outputs/ and the agent then changed is checked against the version the user gave (the last one before the agent's changes).
	// Arrange
	r := newRig(t, map[string]string{"/workspace/outputs/steerco.pptx": "AGENT VERSION 2"})
	r.arts.add(store.Artifact{ID: "o1", ThreadID: r.run.ThreadID, Path: "/workspace/outputs/steerco.pptx", Name: "steerco.pptx", Kind: store.ArtifactUpload},
		fakeVersion{"USER DECK", ""}, fakeVersion{"AGENT VERSION 1", "run-1"}, fakeVersion{"AGENT VERSION 2", "run-2"})

	// Act
	res := r.v.Check(context.Background(), r.run, "/workspace/outputs/steerco.pptx")

	// Assert
	if !res.OK || !strings.Contains(r.logged(t), "baseline: USER DECK") {
		t.Fatalf("result %+v log %q", res, r.logged(t))
	}
}

func TestACopyOfAnUploadUnderAnotherNameHasThatUploadAsBaseline(t *testing.T) {
	// A deck whose first version is a byte copy of an upload (cp upload.pptx outputs/deck_v2.pptx, then edits) is checked against that upload.
	// Arrange
	r := newRig(t, map[string]string{"/workspace/outputs/deck_v2.pptx": "EDITED COPY"})
	r.arts.add(store.Artifact{ID: "u1", ThreadID: r.run.ThreadID, Path: "/workspace/deck.pptx", Name: "deck.pptx", Kind: store.ArtifactUpload},
		fakeVersion{"UPLOADED DECK", ""})
	r.arts.add(store.Artifact{ID: "o1", ThreadID: r.run.ThreadID, Path: "/workspace/outputs/deck_v2.pptx", Name: "deck_v2.pptx", Kind: store.ArtifactOutput},
		fakeVersion{"UPLOADED DECK", "run-2"}, fakeVersion{"EDITED COPY", "run-2"})

	// Act
	res := r.v.Check(context.Background(), r.run, "/workspace/outputs/deck_v2.pptx")

	// Assert
	if !res.OK || !strings.Contains(r.logged(t), "baseline: UPLOADED DECK") {
		t.Fatalf("result %+v log %q", res, r.logged(t))
	}
}

func TestADeckTheAgentMadeHasNoBaseline(t *testing.T) {
	// A deck the agent built from scratch (no upload of its name, every version from agent runs) is checked without --baseline.
	// Arrange
	r := newRig(t, map[string]string{"/workspace/outputs/new.pptx": "BUILT"})
	r.arts.add(store.Artifact{ID: "o1", ThreadID: r.run.ThreadID, Path: "/workspace/outputs/new.pptx", Name: "new.pptx", Kind: store.ArtifactOutput},
		fakeVersion{"FIRST BUILD", "run-1"}, fakeVersion{"BUILT", "run-2"})

	// Act
	res := r.v.Check(context.Background(), r.run, "/workspace/outputs/new.pptx")

	// Assert
	if !res.OK || strings.Contains(r.logged(t), "--baseline") || !strings.Contains(r.logged(t), "args: ") {
		t.Fatalf("result %+v log %q", res, r.logged(t))
	}
}

func TestADeliverableCitingPagesOfAnUploadedPdfIsCheckedAgainstIt(t *testing.T) {
	// Review round 2 (A9): when the thread has an uploaded PDF, a document deliverable is also checked by check_citations against it; an invented figure fails the deliverable with the citation checker's detail and next step.
	// Arrange
	r := newRig(t, map[string]string{"/workspace/study.pdf": "%PDF", "/workspace/outputs/brief.docx": "38 % (p. 19) INVENTED"})
	r.arts.add(store.Artifact{ID: "u1", ThreadID: r.run.ThreadID, Path: "/workspace/study.pdf", Name: "study.pdf", Kind: store.ArtifactUpload},
		fakeVersion{"%PDF", ""})

	// Act
	res := r.v.Check(context.Background(), r.run, "/workspace/outputs/brief.docx")

	// Assert
	log := r.logged(t)
	if res.OK || res.Failed || res.Checker != "inspect_docx" || !strings.Contains(log, "/workspace/study.pdf ") || !strings.Contains(log, "/workspace/outputs/brief.docx") ||
		!strings.Contains(res.Summary, "inspect_docx: ok") || !strings.Contains(res.Summary, "check_citations: NOT OK") ||
		!strings.Contains(res.Summary, "38 (p. 19)") || res.NextStep != "use the figure printed on the cited page" {
		t.Fatalf("result %+v log %q", res, log)
	}
}

func TestCitationsAddNothingWhenTheDeliverableCitesNoPage(t *testing.T) {
	// A deliverable without page citations keeps its format checker's result unchanged; one with correct citations says they were checked.
	// Arrange
	r := newRig(t, map[string]string{"/workspace/study.pdf": "%PDF", "/workspace/outputs/memo.docx": "NOCITE",
		"/workspace/outputs/brief.docx": "19.5% (p. 48)"})
	r.arts.add(store.Artifact{ID: "u1", ThreadID: r.run.ThreadID, Path: "/workspace/study.pdf", Name: "study.pdf", Kind: store.ArtifactUpload},
		fakeVersion{"%PDF", ""})

	// Act
	plain := r.v.Check(context.Background(), r.run, "/workspace/outputs/memo.docx")
	cited := r.v.Check(context.Background(), r.run, "/workspace/outputs/brief.docx")

	// Assert
	if !plain.OK || plain.Summary != "inspect_docx: ok (1 checks passed)" {
		t.Fatalf("plain %+v", plain)
	}
	if !cited.OK || !strings.Contains(cited.Summary, "check_citations: ok") {
		t.Fatalf("cited %+v", cited)
	}
}

func TestNoUploadedPdfMeansNoCitationCheck(t *testing.T) {
	// Without a PDF among the thread's uploads, check_citations never runs.
	// Arrange
	r := newRig(t, map[string]string{"/workspace/outputs/brief.docx": "38 % (p. 19) INVENTED"})
	r.arts.add(store.Artifact{ID: "u1", ThreadID: r.run.ThreadID, Path: "/workspace/data.xlsx", Name: "data.xlsx", Kind: store.ArtifactUpload},
		fakeVersion{"PK", ""})

	// Act
	res := r.v.Check(context.Background(), r.run, "/workspace/outputs/brief.docx")

	// Assert
	if !res.OK || strings.Contains(r.logged(t), "citations:") {
		t.Fatalf("result %+v log %q", res, r.logged(t))
	}
}

func TestACitationCheckThatCannotRunKeepsTheFormatVerdict(t *testing.T) {
	// When check_citations crashes, the deliverable keeps its format checker's verdict and the summary says the citations could not be checked.
	// Arrange
	r := newRig(t, map[string]string{"/workspace/study.pdf": "%PDF", "/workspace/outputs/brief.docx": "CRASH"})
	r.arts.add(store.Artifact{ID: "u1", ThreadID: r.run.ThreadID, Path: "/workspace/study.pdf", Name: "study.pdf", Kind: store.ArtifactUpload},
		fakeVersion{"%PDF", ""})

	// Act
	res := r.v.Check(context.Background(), r.run, "/workspace/outputs/brief.docx")

	// Assert
	if !res.OK || res.Failed || !strings.Contains(res.Summary, "citations could not be checked") {
		t.Fatalf("result %+v", res)
	}
}

func TestDefaultCheckersCheckCitationsAndBaselines(t *testing.T) {
	// The built-in configuration runs fit_text with --baseline when an original exists and check_citations from the pdf skill.
	// Arrange
	defaults := verify.DefaultCheckers()

	// Act
	cit := verify.DefaultCitations()

	// Assert
	if defaults[".pptx"].Baseline != "--baseline {baseline}" || cit.Skill != "pdf" ||
		cit.Command != "python3 /skills/pdf/scripts/check_citations.py {sources} {file}" {
		t.Fatalf("pptx %+v citations %+v", defaults[".pptx"], cit)
	}
}
