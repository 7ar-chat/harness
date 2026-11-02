package artifacts_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/runtime"
	"github.com/7ar-chat/harness/features/artifacts"
)

func TestRecordPathRecordsACheckpointRestoredFile(t *testing.T) {
	// A file created or updated outside a run becomes a new version attributed to the sandbox; recording unchanged content is no change; a file that vanished is a deletion.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "one")

	// Act
	created, errCreated := r.svc.RecordPath(r.ctx, sb, r.th.ID, "alice", "/workspace/a.txt")
	unchanged, errUnchanged := r.svc.RecordPath(r.ctx, sb, r.th.ID, "alice", "/workspace/a.txt")
	sb.Put("/workspace/a.txt", "two")
	updated, errUpdated := r.svc.RecordPath(r.ctx, sb, r.th.ID, "alice", "/workspace/./a.txt")
	sb.Remove("/workspace/a.txt")
	deleted, errDeleted := r.svc.RecordPath(r.ctx, sb, r.th.ID, "alice", "/workspace/a.txt")
	again, errAgain := r.svc.RecordPath(r.ctx, sb, r.th.ID, "alice", "/workspace/a.txt")
	unknown, errUnknown := r.svc.RecordPath(r.ctx, sb, r.th.ID, "alice", "/workspace/never.txt")

	// Assert
	for _, err := range []error{errCreated, errUnchanged, errUpdated, errDeleted, errAgain, errUnknown} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if created == nil || created.Change != store.ChangeCreated || created.Artifact.Kind != store.ArtifactOutput || created.Artifact.SandboxID != sb.ID() {
		t.Fatalf("created %+v", created)
	}
	if unchanged != nil || again != nil || unknown != nil {
		t.Fatalf("no-change cases returned %v %v %v", unchanged, again, unknown)
	}
	if updated == nil || updated.Change != store.ChangeUpdated || updated.Artifact.Version != 2 {
		t.Fatalf("updated %+v", updated)
	}
	if deleted == nil || deleted.Change != store.ChangeDeleted || !deleted.Artifact.Deleted {
		t.Fatalf("deleted %+v", deleted)
	}
}

func TestRecordPathIgnoresWhatSnapshotsNeverSave(t *testing.T) {
	// Paths outside /workspace, in skipped directories, directories, and files above the limits are not artifacts.
	// Arrange
	r := newSBRig(t, artifacts.Options{MaxFileBytes: 5})
	r.withStore(r.mem, capBlobs{hookBlobs: newHookBlobs(), max: 4}, artifacts.Options{MaxFileBytes: 5})
	sb := r.sandbox()
	sb.Put("/etc/passwd", "root")
	sb.Put("/workspace/.work/x.txt", "x")
	sb.Put("/workspace/node_modules/m/i.js", "x")
	sb.Put("/workspace/dir/inner.txt", "i")
	sb.Put("/workspace/over-backend-cap.txt", "12345")
	sb.Put("/workspace/over-file-cap.txt", "123456")

	// Act
	var got []*artifacts.Change
	for _, p := range []string{"/etc/passwd", "/workspace", "/workspace/.work/x.txt", "/workspace/node_modules/m/i.js", "/workspace/dir", "/workspace/over-backend-cap.txt", "/workspace/over-file-cap.txt"} {
		c, err := r.svc.RecordPath(r.ctx, sb, r.th.ID, "alice", p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		got = append(got, c)
	}

	// Assert
	for i, c := range got {
		if c != nil {
			t.Errorf("case %d recorded %+v", i, c.Artifact)
		}
	}
	if len(r.all()) != 0 {
		t.Fatalf("artifacts %v", r.all())
	}
}

func TestRecordPathReportsStoreAndSandboxFailures(t *testing.T) {
	// Lock, listing, stat, read and write failures are returned, never swallowed.
	boom := errors.New("boom")
	cases := []struct {
		name  string
		setup func(r *sbRig, sb *sandboxtest.Sandbox) (*artifacts.Service, sandbox.Sandbox)
	}{
		{"lock", func(r *sbRig, sb *sandboxtest.Sandbox) (*artifacts.Service, sandbox.Sandbox) {
			return artifacts.New(&faultStore{Store: r.mem, lockErr: boom}, artifacts.PGBlobs{S: r.mem}, r.mgr, artifacts.Options{}), sb
		}},
		{"listing", func(r *sbRig, sb *sandboxtest.Sandbox) (*artifacts.Service, sandbox.Sandbox) {
			return artifacts.New(&faultStore{Store: r.mem, listErr: boom}, artifacts.PGBlobs{S: r.mem}, r.mgr, artifacts.Options{}), sb
		}},
		{"stat", func(r *sbRig, sb *sandboxtest.Sandbox) (*artifacts.Service, sandbox.Sandbox) {
			return r.svc, &flakySandbox{Sandbox: sb, statErr: func(string) error { return boom }}
		}},
		{"read", func(r *sbRig, sb *sandboxtest.Sandbox) (*artifacts.Service, sandbox.Sandbox) {
			return r.svc, &flakySandbox{Sandbox: sb, readErr: func(string) error { return boom }}
		}},
		{"blob write", func(r *sbRig, sb *sandboxtest.Sandbox) (*artifacts.Service, sandbox.Sandbox) {
			return artifacts.New(r.mem, &failBlobs{hookBlobs: newHookBlobs(), putErr: boom}, r.mgr, artifacts.Options{}), sb
		}},
		{"version write", func(r *sbRig, sb *sandboxtest.Sandbox) (*artifacts.Service, sandbox.Sandbox) {
			return artifacts.New(&faultStore{Store: r.mem, putErr: func(int) error { return boom }}, artifacts.PGBlobs{S: r.mem}, r.mgr, artifacts.Options{}), sb
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			r := newSBRig(t, artifacts.Options{})
			sb := r.sandbox()
			sb.Put("/workspace/a.txt", "x")
			svc, target := c.setup(r, sb)

			// Act
			got, err := svc.RecordPath(r.ctx, target, r.th.ID, "alice", "/workspace/a.txt")

			// Assert
			if !errors.Is(err, boom) || got != nil {
				t.Fatalf("got %v err %v", got, err)
			}
		})
	}
}

func TestRecordPathDeletionFailuresAreReported(t *testing.T) {
	// A vanished file whose bookkeeping fails (materialize, clock, delete) is an error, not a silent no-op.
	boom := errors.New("boom")
	for name, set := range map[string]func(*faultStore){
		"materialize": func(f *faultStore) { f.materializeErr = boom },
		"clock":       func(f *faultStore) { f.clockErr = boom },
		"delete":      func(f *faultStore) { f.deleteErr = boom },
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			r := newSBRig(t, artifacts.Options{})
			fs := &faultStore{Store: r.mem}
			r.withStore(fs, nil, artifacts.Options{})
			sb := r.sandbox()
			sb.Put("/workspace/a.txt", "x")
			r.snapshot(sb)
			sb.Remove("/workspace/a.txt")
			set(fs)

			// Act
			got, err := r.svc.RecordPath(r.ctx, sb, r.th.ID, "alice", "/workspace/a.txt")

			// Assert
			if !errors.Is(err, boom) || got != nil {
				t.Fatalf("got %v err %v", got, err)
			}
		})
	}
}

func TestRecordPathDoesNotRecordAFileThatGrewPastTheCapWhileReading(t *testing.T) {
	// A file whose stat is within the limit but whose content is larger when read is not recorded (truncated content must never become a version).
	// Arrange
	r := newSBRig(t, artifacts.Options{MaxFileBytes: 4})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "1234")
	sb.Put("/workspace/a.txt", "123456789") // grew after the (stale) stat below

	// Act
	got, err := r.svc.RecordPath(r.ctx, &statBeforeGrowth{Sandbox: sb, size: 4}, r.th.ID, "alice", "/workspace/a.txt")

	// Assert
	if err != nil || got != nil || len(r.all()) != 0 {
		t.Fatalf("got %v err %v artifacts %d", got, err, len(r.all()))
	}
}

// statBeforeGrowth reports a stale size: the file grows between the stat and the read.
type statBeforeGrowth struct {
	*sandboxtest.Sandbox
	size int64
}

func (s *statBeforeGrowth) Stat(ctx context.Context, p string) (sandbox.FileInfo, error) {
	info, err := s.Sandbox.Stat(ctx, p)
	info.Size = s.size
	return info, err
}

func (r *sbRig) run(id string) *store.Run {
	return &store.Run{ID: id, ThreadID: r.th.ID, UserLogin: "alice"}
}

func TestCheckAnswerFlagsLinksToFilesThatWereNeverSaved(t *testing.T) {
	// Linking a file with no saved artifact (scratch space, a typo) gets a corrective note; links to saved files, and answers without links, get none.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/outputs/report.docx", "doc")
	sb.Put("/workspace/.work/scratch.csv", "scratch")
	emitted := 0
	emit := func(agui.Event) error { emitted++; return nil }

	// Act
	clean := r.svc.CheckAnswer(r.ctx, r.run("r1"), "Done: [report](workspace-file:///workspace/outputs/report.docx).", emit)
	dangling := r.svc.CheckAnswer(r.ctx, r.run("r1"), "See [data](workspace-file:///workspace/.work/scratch.csv) and [x](workspace-file:///workspace/outputs/missing%20file.csv)", emit)
	none := r.svc.CheckAnswer(r.ctx, r.run("r1"), "All done, no files.", emit)

	// Assert
	if clean != "" || none != "" {
		t.Fatalf("clean %q none %q", clean, none)
	}
	if !strings.Contains(dangling, "/workspace/.work/scratch.csv") || !strings.Contains(dangling, "/workspace/outputs/missing file.csv") ||
		!strings.Contains(dangling, "don't exist as saved files") {
		t.Fatalf("dangling note %q", dangling)
	}
	if emitted != 1 {
		t.Fatalf("the workspace must be snapshotted when the answer links files (events %d)", emitted)
	}
}

func TestCheckAnswerFlagsSavedFilesNamedWithoutALink(t *testing.T) {
	// Saved files named by absolute path, or by the file name of an output of this run, must be linked; names of other runs' files and plain words are left alone.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	run, _, err := r.mem.CreateRun(r.ctx, store.NewRun{RunID: "run-1", ThreadID: r.th.ID, UserLogin: "alice", Message: store.Message{Role: store.RoleUser, Content: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	sb := r.sandbox()
	sb.Put("/workspace/outputs/summary.md", "s")
	sb.Put("/workspace/outputs/chart.png", "c")
	sb.Put("/workspace/notes/todo.txt", "t")
	r.svc.SnapshotRun(r.ctx, run, "", func(agui.Event) error { return nil }) // outputs of run-1
	r.upload("attached.csv", "a")

	// Act
	byName := r.svc.CheckAnswer(r.ctx, run, "I wrote summary.md for you.", nil)
	byPath := r.svc.CheckAnswer(r.ctx, run, "The list is at /workspace/notes/todo.txt.", nil)
	linked := r.svc.CheckAnswer(r.ctx, run, "Here: [summary.md](workspace-file:///workspace/outputs/summary.md) and chart.png", nil)
	partial := r.svc.CheckAnswer(r.ctx, run, "my-summary.md and summary.mdx and chart.png.", nil)
	uploadByName := r.svc.CheckAnswer(r.ctx, run, "I read attached.csv", nil)
	otherRun := r.svc.CheckAnswer(r.ctx, r.run("run-2"), "summary.md", nil)
	both := r.svc.CheckAnswer(r.ctx, run, "[gone](workspace-file:///workspace/outputs/gone.md) and summary.md", nil)

	// Assert
	if !strings.Contains(byName, "/workspace/outputs/summary.md") || !strings.Contains(byName, "without linking them") {
		t.Fatalf("by name %q", byName)
	}
	if !strings.Contains(byPath, "/workspace/notes/todo.txt") || strings.Contains(byPath, "/workspace/.") {
		t.Fatalf("by path %q", byPath)
	}
	if strings.Contains(linked, "summary.md") || !strings.Contains(linked, "chart.png") {
		t.Fatalf("linked %q", linked)
	}
	if strings.Contains(partial, "summary.md") || !strings.Contains(partial, "/workspace/outputs/chart.png") {
		t.Fatalf("partial name matches %q", partial)
	}
	if uploadByName != "" || otherRun != "" {
		t.Fatalf("upload %q other run %q", uploadByName, otherRun)
	}
	if !strings.Contains(both, "don't exist as saved files") || !strings.Contains(both, "without linking them") || !strings.Contains(both, "\n") {
		t.Fatalf("both notes %q", both)
	}
}

func TestCheckAnswerSkipsCompactRunsAndAnUnavailableStore(t *testing.T) {
	// Compaction runs and a nil service are never checked; an artifact store that fails yields no note rather than a wrong one.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	var nilSvc *artifacts.Service
	compact := &store.Run{ID: "c", ThreadID: r.th.ID, Kind: store.KindCompact}
	broken := artifacts.New(&faultStore{Store: r.mem, listErr: errors.New("db down")}, nil, nil, artifacts.Options{})

	// Act
	a := nilSvc.CheckAnswer(r.ctx, r.run("r"), "[x](workspace-file:///workspace/x)", nil)
	b := r.svc.CheckAnswer(r.ctx, compact, "[x](workspace-file:///workspace/x)", nil)
	c := broken.CheckAnswer(r.ctx, r.run("r"), "[x](workspace-file:///workspace/x)", nil)

	// Assert
	if a != "" || b != "" || c != "" {
		t.Fatalf("notes %q %q %q", a, b, c)
	}
}

func TestPromptSectionIsAddedOnlyForRunsWithASandbox(t *testing.T) {
	// The file-reference instruction appears only when the run has sandbox tools.
	// Act
	with := artifacts.PromptSection(context.Background(), agent.Input{Sandbox: true})
	without := artifacts.PromptSection(context.Background(), agent.Input{})

	// Assert
	if with != artifacts.FileReferencePrompt || without != "" {
		t.Fatalf("with %q without %q", with, without)
	}
}

// imageRig uploads attachments and builds the history carrying them.
type imageRig struct {
	*sbRig
	im artifacts.Images
}

func newImageRig(t *testing.T) *imageRig {
	r := newSBRig(t, artifacts.Options{})
	return &imageRig{sbRig: r, im: artifacts.Images{Svc: r.svc, Vision: func(model string) bool { return model == "vision-model" }}}
}

func (r *imageRig) attach(name string, data []byte) store.Attachment {
	r.t.Helper()
	a, err := r.svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: name, Data: data})
	if err != nil {
		r.t.Fatal(err)
	}
	return store.Attachment{ArtifactID: a.ID, Name: a.Name, Path: a.Path, SizeBytes: a.SizeBytes, MimeType: a.MimeType, Version: a.Version}
}

func userMsg(text string, atts ...store.Attachment) *ai.Message {
	m := ai.UserMessage(text)
	if len(atts) > 0 {
		m.Extra = map[string]any{store.ExtraAttachments: atts}
	}
	return m
}

func imageParts(m *ai.Message) int {
	n := 0
	for _, p := range m.Parts {
		if p.Type == ai.PartImage {
			n++
		}
	}
	return n
}

func TestAttachImagesSendsAttachedImagesToVisionModels(t *testing.T) {
	// Image attachments become image parts after the message text for a vision model; non-image attachments and non-vision models leave the history untouched.
	// Arrange
	r := newImageRig(t)
	png := r.attach("shot.png", pngBytes)
	txt := r.attach("notes.txt", []byte("hello"))
	history := []*ai.Message{userMsg("look", png, txt)}
	run := &store.Run{ThreadID: r.th.ID, Model: "vision-model"}

	// Act
	plain := r.im.AttachImages(r.ctx, &store.Run{ThreadID: r.th.ID, Model: "text-model"}, []*ai.Message{userMsg("look", png)})
	got := r.im.AttachImages(r.ctx, run, history)

	// Assert
	if len(plain[0].Parts) != 0 || plain[0].Content != "look" {
		t.Fatalf("non-vision model got parts: %+v", plain[0])
	}
	m := got[0]
	if m.Content != "" || len(m.Parts) != 2 || m.Parts[0].Type != ai.PartText || m.Parts[0].Text != "look" || m.Parts[1].Type != ai.PartImage {
		t.Fatalf("message %+v", m)
	}
	if m.Parts[1].Image.MIMEType != "image/png" || m.Parts[1].Image.Base64Data == "" || m.Parts[1].Extra[agent.ImageAttachmentKey] != png.ArtifactID {
		t.Fatalf("image part %+v", m.Parts[1])
	}
}

func TestAttachImagesLimitsWhatIsSent(t *testing.T) {
	// Only the two most recent messages with images are used, at most six images per request, and never images above 5 MiB, from another thread, or with a lost blob.
	// Arrange
	r := newImageRig(t)
	var history []*ai.Message
	for i := range 3 {
		var atts []store.Attachment
		for j := range 3 {
			atts = append(atts, r.attach(string(rune('a'+i))+string(rune('0'+j))+".png", append([]byte(string(pngBytes)), byte(i), byte(j))))
		}
		history = append(history, userMsg("turn", atts...))
	}
	history = append(history, userMsg("no images"), ai.AssistantMessage("ok", nil))
	run := &store.Run{ThreadID: r.th.ID, Model: "vision-model"}

	// Act
	got := r.im.AttachImages(r.ctx, run, history)

	// Assert
	if imageParts(got[0]) != 0 || imageParts(got[1]) != 3 || imageParts(got[2]) != 3 {
		t.Fatalf("images per message: %d %d %d", imageParts(got[0]), imageParts(got[1]), imageParts(got[2]))
	}

	// Arrange: the budget of six images is shared by the most recent messages
	r2 := newImageRig(t)
	var big []store.Attachment
	for i := range 8 {
		big = append(big, r2.attach("p"+string(rune('0'+i))+".png", append([]byte(string(pngBytes)), byte(i))))
	}

	// Act
	capped := r2.im.AttachImages(r2.ctx, &store.Run{ThreadID: r2.th.ID, Model: "vision-model"}, []*ai.Message{userMsg("many", big...)})

	// Assert
	if imageParts(capped[0]) != agent.MaxRequestImages {
		t.Fatalf("images %d, want %d", imageParts(capped[0]), agent.MaxRequestImages)
	}
}

func TestAttachImagesSkipsImagesItCannotTrust(t *testing.T) {
	// Oversized images, artifacts of another thread, lost content, unknown artifacts and non-vision formats are skipped; a message left without images is unchanged.
	// Arrange
	r := newImageRig(t)
	oversized := r.attach("huge.png", append([]byte(string(pngBytes)), make([]byte, 5<<20)...))
	svg := r.attach("pic.svg", []byte("<svg/>"))
	lost := r.attach("lost.png", append([]byte(string(pngBytes)), 9))
	lostArt, _ := r.mem.GetArtifact(r.ctx, lost.ArtifactID)
	_ = r.mem.DeleteBlob(r.ctx, lostArt.SHA256)
	otherThread, _ := r.mem.CreateThread(r.ctx, "alice", "other")
	foreign, err := r.svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: otherThread.ID, Login: "alice", Filename: "theirs.png", Data: pngBytes})
	if err != nil {
		t.Fatal(err)
	}
	unknown := store.Attachment{ArtifactID: "no-such-artifact", MimeType: "image/png", SizeBytes: 10}
	// Attachment metadata says image/png, the artifact is recorded as text: it is not sent.
	asText := r.upload("really-text.txt", "words")
	textAtt := store.Attachment{ArtifactID: asText.ID, MimeType: "image/png", SizeBytes: asText.SizeBytes}
	history := []*ai.Message{userMsg("m", oversized, svg, lost, store.Attachment{ArtifactID: foreign.ID, MimeType: "image/png", SizeBytes: foreign.SizeBytes}, unknown, textAtt)}

	// Act
	got := r.im.AttachImages(r.ctx, &store.Run{ThreadID: r.th.ID, Model: "vision-model"}, history)

	// Assert
	if len(got[0].Parts) != 0 || got[0].Content != "m" {
		t.Fatalf("message was modified: %+v", got[0])
	}
}

func TestAttachImagesWithoutServiceOrVisionCheckIsANoOp(t *testing.T) {
	// A misconfigured attacher never alters the history.
	// Arrange
	history := []*ai.Message{userMsg("hello")}
	run := &store.Run{Model: "vision-model"}

	// Act
	noSvc := artifacts.Images{Vision: func(string) bool { return true }}.AttachImages(context.Background(), run, history)
	noVision := artifacts.Images{Svc: &artifacts.Service{}}.AttachImages(context.Background(), run, history)

	// Assert
	if len(noSvc) != 1 || len(noVision) != 1 || history[0].Content != "hello" || len(history[0].Parts) != 0 {
		t.Fatalf("history %+v", history)
	}
}

func TestEndOfRunHookSnapshotsOnlyRealRuns(t *testing.T) {
	// Compaction runs and a nil service do not snapshot; a run with a live sandbox and no changes emits nothing and returns no continuation.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "a")
	r.snapshot(sb) // nothing new at the end of the run

	// Act
	compact := artifacts.EndOfRunHook{Svc: r.svc}.Finishing(r.ctx, &runtime.Finish{Run: &store.Run{ID: "c", ThreadID: r.th.ID, Kind: store.KindCompact}})
	nilSvc := artifacts.EndOfRunHook{}.Finishing(r.ctx, &runtime.Finish{Run: r.run("r")})
	idle := artifacts.EndOfRunHook{Svc: r.svc}.Finishing(r.ctx, &runtime.Finish{Run: r.run("r")})

	// Assert
	if compact != nil || nilSvc != nil || idle != nil {
		t.Fatal("the hook must not register a continuation")
	}
}

func TestPurgeJobRunsTheRetentionPurgeOnlyWhenEnabled(t *testing.T) {
	// The sweeper job purges old deleted threads with their blobs; After <= 0 or no service disables it; a purge failure is returned.
	// Arrange
	g := newGCRig(t, nil)
	key := sumOf("old report")
	g.unreferenced("old report")
	g.advance(25 * time.Hour)
	disabled := artifacts.PurgeJob{Svc: g.svc}
	noSvc := artifacts.PurgeJob{After: time.Hour}
	broken := artifacts.PurgeJob{Svc: artifacts.New(&faultStore{Store: g.st, purgeErr: errors.New("db down")}, nil, nil, artifacts.Options{}), After: time.Hour}

	// Act
	errDisabled := disabled.Purge(g.ctx)
	errNoSvc := noSvc.Purge(g.ctx)
	stillThere := g.st.Blob(key)
	errBroken := broken.Purge(g.ctx)
	errRun := artifacts.PurgeJob{Svc: g.svc, After: 24 * time.Hour}.Purge(g.ctx)

	// Assert
	if errDisabled != nil || errNoSvc != nil || !stillThere {
		t.Fatalf("disabled %v no service %v blob kept %v", errDisabled, errNoSvc, stillThere)
	}
	if errBroken == nil || errRun != nil || g.st.Blob(key) {
		t.Fatalf("broken %v run %v blob still stored %v", errBroken, errRun, g.st.Blob(key))
	}
}
