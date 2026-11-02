package artifacts_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/features/artifacts"
)

// sofficeSandbox is the rig's sandbox answering the snapshot listing and
// converting with a scripted soffice; every command is recorded.
type sofficeSandbox struct {
	sb       *sandboxtest.Sandbox
	mu       sync.Mutex
	commands []string
	exit     int
	pdf      string
	err      error
	noOutput bool
	stderr   string
}

func (r *sbRig) soffice() *sofficeSandbox {
	r.t.Helper()
	s := &sofficeSandbox{sb: r.sandbox(), pdf: "%PDF-converted"}
	s.sb.SetExec(sandboxtest.FindExec(s.sb, func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		s.mu.Lock()
		s.commands = append(s.commands, req.Command)
		exit, err, noOutput, stderr, pdf := s.exit, s.err, s.noOutput, s.stderr, s.pdf
		s.mu.Unlock()
		if !strings.HasPrefix(req.Command, "soffice ") {
			return sandbox.ExecResult{}, nil
		}
		if req.OnStderr != nil && stderr != "" {
			req.OnStderr(stderr)
		}
		if err != nil || exit != 0 {
			return sandbox.ExecResult{ExitCode: exit}, err
		}
		if !noOutput {
			fields := strings.Fields(req.Command)
			src := strings.Trim(fields[len(fields)-1], "'")
			s.sb.Put(strings.TrimSuffix(src, ".docx")+".pdf", pdf)
		}
		return sandbox.ExecResult{}, nil
	}))
	return s
}

func (s *sofficeSandbox) conversions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.commands {
		if strings.HasPrefix(c, "soffice ") {
			n++
		}
	}
	return n
}

func (r *sbRig) docx(content string) *store.Artifact {
	r.t.Helper()
	doc := testdata(r.t, "report.docx")
	if content != "" {
		doc = []byte(content)
	}
	a, err := r.svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: "report.docx", Data: doc})
	if err != nil {
		r.t.Fatal(err)
	}
	return a
}

func TestPreviewServesImagesTextAndPDFsDirectly(t *testing.T) {
	// Images keep their own type, text and HTML are served as text/plain, a PDF as application/pdf, without any conversion.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	img, _ := r.svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: "p.png", Data: pngBytes})
	txt := r.upload("n.txt", "hello")
	html := r.upload("page.html", "<b>x</b>")
	pdf, _ := r.svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: "memo.pdf", Data: testdata(t, "memo.pdf")})

	// Act
	gotImg, errImg := r.svc.Preview(r.ctx, img, 0)
	gotTxt, errTxt := r.svc.Preview(r.ctx, txt, 1)
	gotHTML, errHTML := r.svc.Preview(r.ctx, html, 0)
	gotPDF, errPDF := r.svc.Preview(r.ctx, pdf, 0)

	// Assert
	for _, err := range []error{errImg, errTxt, errHTML, errPDF} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if gotImg.ContentType != "image/png" || gotTxt.ContentType != "text/plain; charset=utf-8" || string(gotTxt.Data) != "hello" ||
		gotHTML.ContentType != "text/plain; charset=utf-8" || gotPDF.ContentType != "application/pdf" {
		t.Fatalf("img %q txt %q html %q pdf %q", gotImg.ContentType, gotTxt.ContentType, gotHTML.ContentType, gotPDF.ContentType)
	}
}

func TestPreviewRefusesUnpreviewableAndMasqueradingFiles(t *testing.T) {
	// A zip or unknown type, a file recorded with a signature type its extension does not claim, and PDF/Office bytes behind an old text record are never served raw.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	zip := r.upload("a.zip", string(zipBytes(t, map[string]string{"a": "b"})))
	agentSB := r.sandbox()
	agentSB.Put("/workspace/memo.txt", string(testdata(t, "memo.pdf"))) // written by the agent: recorded as application/pdf by its signature
	r.snapshot(agentSB)
	renamed := r.art("/workspace/memo.txt")
	legacy := r.upload("old.txt", "plain")
	// A version stored before the signature-aware content type: a text record over PDF bytes.
	_, _, err := r.mem.PutArtifactVersion(r.ctx, store.PutArtifact{ThreadID: r.th.ID, UserLogin: "alice", Path: legacy.Path, Name: legacy.Name,
		MimeType: "text/plain", Kind: store.ArtifactUpload, SizeBytes: 4, SHA256: sum(testdata(t, "memo.pdf")), Force: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = r.mem.PutBlob(r.ctx, sum(testdata(t, "memo.pdf")), testdata(t, "memo.pdf"))
	legacyNow, _ := r.mem.GetArtifact(r.ctx, legacy.ID)

	// Act
	_, zipErr := r.svc.Preview(r.ctx, zip, 0)
	_, renamedErr := r.svc.Preview(r.ctx, renamed, 0)
	_, legacyErr := r.svc.Preview(r.ctx, legacyNow, 0)

	// Assert
	for name, err := range map[string]error{"zip": zipErr, "renamed pdf": renamedErr, "legacy record": legacyErr} {
		if !errors.Is(err, artifacts.ErrNotPreviewable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if renamed.MimeType != "application/pdf" {
		t.Errorf("renamed PDF recorded as %q", renamed.MimeType)
	}
}

func TestPreviewReportsMissingVersionsAndContent(t *testing.T) {
	// An unknown version or a lost blob is an error, not an empty preview.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	a := r.upload("n.txt", "hello")
	_ = r.mem.DeleteBlob(r.ctx, a.SHA256)

	// Act
	_, noVersion := r.svc.Preview(r.ctx, a, 7)
	_, noBlob := r.svc.Preview(r.ctx, a, 1)

	// Assert
	if !errors.Is(noVersion, store.ErrNotFound) || !errors.Is(noBlob, artifacts.ErrBlobNotFound) {
		t.Fatalf("version %v blob %v", noVersion, noBlob)
	}
}

func TestOfficePreviewIsConvertedOnceAndCachedPerOwner(t *testing.T) {
	// An Office file is converted in the owner's sandbox, the PDF cached by content hash and owner; the second preview does not convert again and the scratch files are removed.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	so := r.soffice()
	a := r.docx("")

	// Act
	first, err1 := r.svc.Preview(r.ctx, a, 0)
	second, err2 := r.svc.Preview(r.ctx, a, 0)

	// Assert
	if err1 != nil || err2 != nil || string(first.Data) != "%PDF-converted" || first.ContentType != "application/pdf" || string(second.Data) != string(first.Data) {
		t.Fatalf("previews %v %v", err1, err2)
	}
	if so.conversions() != 1 {
		t.Fatalf("conversions %d, want 1", so.conversions())
	}
	var cleanedProfile, cleanedFiles bool
	for _, c := range so.commands {
		cleanedProfile = cleanedProfile || strings.HasPrefix(c, "rm -rf '/tmp/work-preview-profile-")
		cleanedFiles = cleanedFiles || (strings.HasPrefix(c, "rm -f ") && strings.Contains(c, "/workspace/.work/previews/"+a.SHA256+".docx") &&
			strings.Contains(c, "/workspace/.work/previews/"+a.SHA256+".pdf"))
	}
	if !cleanedProfile || !cleanedFiles {
		t.Fatalf("conversion leftovers not removed (profile %v, files %v): %q", cleanedProfile, cleanedFiles, so.commands)
	}
}

func TestOfficePreviewCacheIsNotSharedAcrossOwners(t *testing.T) {
	// The PDF produced in one user's sandbox is never served to another user with the same content (C-03).
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	so := r.soffice()
	a := r.docx("")
	other := *a
	other.UserLogin = "bob"
	otherThread, _ := r.mem.CreateThread(r.ctx, "bob", "t")
	other.ThreadID = otherThread.ID
	bobSB := sandboxtest.NewSandbox("sbx-bob")
	bobSB.SetExec(sandboxtest.FindExec(bobSB, func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		if strings.HasPrefix(req.Command, "soffice ") {
			fields := strings.Fields(req.Command)
			src := strings.Trim(fields[len(fields)-1], "'")
			bobSB.Put(strings.TrimSuffix(src, ".docx")+".pdf", "%PDF-bob")
		}
		return sandbox.ExecResult{}, nil
	}))
	mgr := &fakeMgr{acquire: func(int) (sandbox.Sandbox, error) { return bobSB, nil }}
	_, err := r.svc.Preview(r.ctx, a, 0)
	if err != nil {
		t.Fatal(err)
	}
	r.svc.Sandbox = mgr

	// Act
	_, bobErr := r.svc.Preview(r.ctx, &other, 0)

	// Assert
	if bobErr != nil || so.conversions() != 1 {
		t.Fatalf("bob preview %v, alice conversions %d", bobErr, so.conversions())
	}
	if got, _ := r.svc.Preview(r.ctx, &other, 0); string(got.Data) != "%PDF-bob" {
		t.Fatalf("bob was served %q", got.Data)
	}
	if mgr.acquires != 1 {
		t.Fatalf("bob's preview must convert in his own sandbox (acquires %d)", mgr.acquires)
	}
}

func TestOfficePreviewFailures(t *testing.T) {
	// A failing soffice, a missing output or an unreadable output is ErrPreviewFailed; a disabled sandbox is ErrSandboxDisabled; nothing is cached after a failure.
	t.Run("non-zero exit", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		so := r.soffice()
		so.exit, so.stderr = 77, strings.Repeat("e", 5000)
		a := r.docx("")

		// Act
		_, err := r.svc.Preview(r.ctx, a, 0)
		so.exit = 0
		_, retry := r.svc.Preview(r.ctx, a, 0)

		// Assert
		if !errors.Is(err, artifacts.ErrPreviewFailed) || retry != nil {
			t.Fatalf("err %v retry %v", err, retry)
		}
	})
	t.Run("exec error", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		so := r.soffice()
		so.err = errors.New("timeout")
		a := r.docx("")

		// Act
		_, err := r.svc.Preview(r.ctx, a, 0)

		// Assert
		if !errors.Is(err, artifacts.ErrPreviewFailed) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("no output produced", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		so := r.soffice()
		so.noOutput = true
		a := r.docx("")

		// Act
		_, err := r.svc.Preview(r.ctx, a, 0)

		// Assert
		if !errors.Is(err, artifacts.ErrPreviewFailed) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("sandbox write failure", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		so := r.soffice()
		so.sb.SetWriteErr(func(string) error { return errors.New("disk full") })
		a := r.docx("")

		// Act
		_, err := r.svc.Preview(r.ctx, a, 0)

		// Assert
		if !errors.Is(err, artifacts.ErrPreviewFailed) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("sandboxes disabled", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		a := r.docx("")
		r.svc.Sandbox = &fakeMgr{disabled: true}
		noMgr := artifacts.New(r.mem, artifacts.PGBlobs{S: r.mem}, nil, artifacts.Options{})

		// Act
		_, errDisabled := r.svc.Preview(r.ctx, a, 0)
		_, errNil := noMgr.Preview(r.ctx, a, 0)

		// Assert
		if !errors.Is(errDisabled, artifacts.ErrSandboxDisabled) || !errors.Is(errNil, artifacts.ErrSandboxDisabled) {
			t.Fatalf("disabled %v nil %v", errDisabled, errNil)
		}
	})
	t.Run("acquire failure", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		a := r.docx("")
		boom := errors.New("quota reached")
		r.svc.Sandbox = &fakeMgr{acquire: func(int) (sandbox.Sandbox, error) { return nil, boom }}

		// Act
		_, err := r.svc.Preview(r.ctx, a, 0)

		// Assert
		if !errors.Is(err, boom) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("conversion lock unavailable", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		r.soffice()
		a := r.docx("")
		r.withStore(&faultStore{Store: r.mem, lockErr: errors.New("lock timeout")}, nil, artifacts.Options{})

		// Act
		_, err := r.svc.Preview(r.ctx, a, 0)

		// Assert
		if !errors.Is(err, artifacts.ErrPreviewFailed) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("cancelled while waiting for a conversion slot", func(t *testing.T) {
		// Arrange: the only slot is held by a preview stuck in soffice
		r := newSBRig(t, artifacts.Options{PreviewConcurrency: 1})
		so := r.soffice()
		started, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		inner := so.sb
		inner.SetExec(sandboxtest.FindExec(inner, func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			if strings.HasPrefix(req.Command, "soffice ") {
				once.Do(func() { close(started) })
				<-release
			}
			return sandbox.ExecResult{}, errors.New("no output")
		}))
		a := r.docx("")
		done := make(chan error, 1)
		go func() { _, err := r.svc.Preview(r.ctx, a, 0); done <- err }()
		<-started
		cancelled, cancel := context.WithCancel(r.ctx)
		cancel()

		// Act
		_, err := r.svc.Preview(cancelled, a, 0)
		close(release)

		// Assert
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
		if holder := <-done; !errors.Is(holder, artifacts.ErrPreviewFailed) {
			t.Fatalf("holder %v", holder)
		}
	})
}

func TestOfficePreviewRetriesOnANewSandboxWhenTheOldOneIsGone(t *testing.T) {
	// A sandbox removed under the conversion is invalidated and the conversion runs again on a fresh one.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	a := r.docx("")
	dying := sandboxtest.NewSandbox("sbx-dying")
	dying.SetWriteErr(func(string) error { return errGone })
	alive := sandboxtest.NewSandbox("sbx-alive")
	alive.SetExec(sandboxtest.FindExec(alive, func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		if strings.HasPrefix(req.Command, "soffice ") {
			fields := strings.Fields(req.Command)
			src := strings.Trim(fields[len(fields)-1], "'")
			alive.Put(strings.TrimSuffix(src, ".docx")+".pdf", "%PDF-second-try")
		}
		return sandbox.ExecResult{}, nil
	}))
	mgr := &fakeMgr{acquire: func(call int) (sandbox.Sandbox, error) {
		if call == 1 {
			return dying, nil
		}
		return alive, nil
	}}
	r.svc.Sandbox = mgr

	// Act
	got, err := r.svc.Preview(r.ctx, a, 0)

	// Assert
	if err != nil || string(got.Data) != "%PDF-second-try" || len(mgr.invalidated) != 1 || mgr.invalidated[0] != "sbx-dying" {
		t.Fatalf("preview %v err %v invalidated %v", got, err, mgr.invalidated)
	}
}

func TestOfficePreviewStillSucceedsWhenCachingFails(t *testing.T) {
	// A preview that cannot be queued for GC or stored is served anyway (just not cached).
	for name, fs := range map[string]func(r *sbRig) (*faultStore, *failBlobs){
		"queue failure": func(r *sbRig) (*faultStore, *failBlobs) {
			return &faultStore{Store: r.mem, queueErr: errors.New("db down")}, nil
		},
		"blob failure": func(r *sbRig) (*faultStore, *failBlobs) {
			return &faultStore{Store: r.mem}, &failBlobs{hookBlobs: newHookBlobs()}
		},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			r := newSBRig(t, artifacts.Options{})
			r.soffice()
			a := r.docx("")
			st, blobs := fs(r)
			var b artifacts.Blobs = artifacts.PGBlobs{S: r.mem}
			if blobs != nil {
				// Content exists; only writing the preview fails.
				_ = blobs.hookBlobs.Put(r.ctx, a.SHA256, testdata(t, "report.docx"))
				blobs.putErr = nil
				b = &previewWriteFails{failBlobs: blobs}
			}
			r.withStore(st, b, artifacts.Options{})

			// Act
			got, err := r.svc.Preview(r.ctx, a, 0)

			// Assert
			if err != nil || string(got.Data) != "%PDF-converted" {
				t.Fatalf("preview %v", err)
			}
		})
	}
}

// previewWriteFails stores content but refuses the cached preview.
type previewWriteFails struct{ *failBlobs }

func (p *previewWriteFails) Put(ctx context.Context, key string, data []byte) error {
	if strings.HasPrefix(key, "preview/") {
		return errors.New("bucket full")
	}
	return p.failBlobs.Put(ctx, key, data)
}

func TestConcurrentOfficePreviewsOfOneThreadConvertOnce(t *testing.T) {
	// Previews of the same content queued on one thread serialize on the conversion lock and the later ones hit the cache.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	so := r.soffice()
	a := r.docx("")
	var wg sync.WaitGroup
	errs := make(chan error, 4)

	// Act
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.svc.Preview(r.ctx, a, 0)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	// Assert
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if so.conversions() != 1 {
		t.Fatalf("conversions %d, want 1", so.conversions())
	}
}

func TestDeliverTypesTheFileByName(t *testing.T) {
	// A delivered file carries the content type of its name.
	// Arrange
	r := newSBRig(t, artifacts.Options{})

	// Act
	d, err := r.svc.Deliver(r.ctx, "report.pdf", []byte("x"))

	// Assert
	if err != nil || d.ContentType != "application/pdf" || string(d.Data) != "x" {
		t.Fatalf("delivery %+v %v", d, err)
	}
}

func TestContentAndVersionResolveVersionsAndSurfaceFailures(t *testing.T) {
	// Version 0 is the latest; older versions stay readable; a failed lookup or lost blob is reported.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "v1")
	r.snapshot(sb)
	sb.Put("/workspace/a.txt", "version two")
	r.snapshot(sb)
	a := r.art("/workspace/a.txt")

	// Act
	latest, vLatest, err0 := r.svc.Content(r.ctx, a, 0)
	first, _, err1 := r.svc.Content(r.ctx, a, 1)
	_, _, errMissing := r.svc.Content(r.ctx, a, 9)
	versions, errVersions := r.svc.ListArtifactVersions(r.ctx, a.ID)
	listed, errList := r.svc.ListArtifacts(r.ctx, r.th.ID)

	// Assert
	if err0 != nil || string(latest) != "version two" || vLatest.Version != 2 || err1 != nil || string(first) != "v1" {
		t.Fatalf("latest %q %v first %q %v", latest, err0, first, err1)
	}
	if !errors.Is(errMissing, store.ErrNotFound) || errVersions != nil || len(versions) != 2 || errList != nil || len(listed) != 1 {
		t.Fatalf("missing %v versions %d %v listed %d %v", errMissing, len(versions), errVersions, len(listed), errList)
	}
}

func TestRestoreVersionWritesTheOldContentAsANewVersion(t *testing.T) {
	// Restoring version 1 writes it into the sandbox and records it as a new latest version (history is kept).
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "first")
	r.snapshot(sb)
	sb.Put("/workspace/a.txt", "second")
	r.snapshot(sb)
	a := r.art("/workspace/a.txt")

	// Act
	got, err := r.svc.RestoreVersion(r.ctx, a, 1)

	// Assert
	if err != nil || got.Version != 3 || got.SHA256 != sum([]byte("first")) || got.SandboxID != sb.ID() {
		t.Fatalf("restored %+v %v", got, err)
	}
	if content, _ := sb.File("/workspace/a.txt"); content != "first" {
		t.Fatalf("sandbox content %q", content)
	}
	if changes := r.snapshot(sb); len(changes) != 0 {
		t.Fatalf("the restored file was seen as a new change: %v", changes)
	}
}

func TestRestoreVersionFailures(t *testing.T) {
	// Unknown versions, a disabled sandbox, a failed acquire, the lock, and a failed write are reported; a sandbox found gone is invalidated.
	boom := errors.New("boom")
	t.Run("unknown version", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		a := r.upload("a.txt", "x")

		// Act
		_, err := r.svc.RestoreVersion(r.ctx, a, 5)

		// Assert
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("sandbox disabled", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		a := r.upload("a.txt", "x")
		r.svc.Sandbox = &fakeMgr{disabled: true}
		noMgr := artifacts.New(r.mem, artifacts.PGBlobs{S: r.mem}, nil, artifacts.Options{})

		// Act
		_, err := r.svc.RestoreVersion(r.ctx, a, 0)
		_, errNil := noMgr.RestoreVersion(r.ctx, a, 0)

		// Assert
		if !errors.Is(err, artifacts.ErrSandboxDisabled) || !errors.Is(errNil, artifacts.ErrSandboxDisabled) {
			t.Fatalf("err %v %v", err, errNil)
		}
	})
	t.Run("acquire failure", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		a := r.upload("a.txt", "x")
		r.svc.Sandbox = &fakeMgr{acquire: func(int) (sandbox.Sandbox, error) { return nil, boom }}

		// Act
		_, err := r.svc.RestoreVersion(r.ctx, a, 0)

		// Assert
		if !errors.Is(err, boom) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("lock failure", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		a := r.upload("a.txt", "x")
		r.sandbox()
		r.withStore(&faultStore{Store: r.mem, lockErr: boom}, nil, artifacts.Options{})

		// Act
		_, err := r.svc.RestoreVersion(r.ctx, a, 0)

		// Assert
		if !errors.Is(err, boom) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("write failure", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		a := r.upload("a.txt", "x")
		sb := r.sandbox()
		sb.SetWriteErr(func(string) error { return boom })

		// Act
		_, err := r.svc.RestoreVersion(r.ctx, a, 0)

		// Assert
		if !errors.Is(err, boom) || r.art("/workspace/a.txt").Version != 1 {
			t.Fatalf("err %v artifact %+v", err, r.art("/workspace/a.txt"))
		}
	})
	t.Run("sandbox gone during the write", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		a := r.upload("a.txt", "x")
		dying := sandboxtest.NewSandbox("sbx-dying")
		dying.SetWriteErr(func(string) error { return errGone })
		mgr := &fakeMgr{acquire: func(int) (sandbox.Sandbox, error) { return dying, nil }}
		r.svc.Sandbox = mgr

		// Act
		_, err := r.svc.RestoreVersion(r.ctx, a, 0)

		// Assert
		if !sandbox.IsGone(err) || len(mgr.invalidated) != 1 || mgr.invalidated[0] != "sbx-dying" {
			t.Fatalf("err %v invalidated %v", err, mgr.invalidated)
		}
	})
}

func TestTreeListsDirectChildrenWithDirectoriesFirst(t *testing.T) {
	// Tree shows the direct files of a directory and its sub-directories (once, with the newest update time), skips deleted files, and sorts directories before files by name.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	sb := r.sandbox()
	for p, c := range map[string]string{
		"/workspace/outputs/b.txt": "b", "/workspace/outputs/a.txt": "a", "/workspace/outputs/charts/one.png": "1",
		"/workspace/outputs/charts/two.png": "2", "/workspace/outputs/data/x.csv": "x", "/workspace/outputs/gone.txt": "g",
		"/workspace/outputs2/other.txt": "o", "/workspace/top.txt": "t",
	} {
		sb.Put(p, c)
	}
	r.snapshot(sb)
	sb.Remove("/workspace/outputs/gone.txt")
	sb.Put("/workspace/outputs/charts/two.png", "22")
	r.snapshot(sb)

	// Act
	entries, err := r.svc.Tree(r.ctx, r.th.ID, "/workspace/outputs/")
	root, rootErr := r.svc.Tree(r.ctx, r.th.ID, "/workspace")
	_, failErr := artifacts.New(&faultStore{Store: r.mem, listErr: errors.New("db down")}, nil, nil, artifacts.Options{}).Tree(r.ctx, r.th.ID, "/workspace")

	// Assert
	var got []string
	for _, e := range entries {
		got = append(got, e.Type+":"+e.Name)
	}
	if err != nil || strings.Join(got, ",") != "directory:charts,directory:data,file:a.txt,file:b.txt" {
		t.Fatalf("entries %v err %v", got, err)
	}
	charts := entries[0]
	two := r.art("/workspace/outputs/charts/two.png")
	if charts.Path != "/workspace/outputs/charts" || !charts.ModifiedAt.Equal(two.UpdatedAt) {
		t.Fatalf("charts %+v want modified %v", charts, two.UpdatedAt)
	}
	if entries[2].Size != 1 || entries[2].Path != "/workspace/outputs/a.txt" {
		t.Fatalf("file entry %+v", entries[2])
	}
	if rootErr != nil || len(root) != 3 || root[0].Name != "outputs" || root[1].Name != "outputs2" || root[2].Name != "top.txt" {
		t.Fatalf("root %+v %v", root, rootErr)
	}
	if failErr == nil {
		t.Fatal("a listing failure must be returned")
	}
}

func TestConversionSlotsBoundTheConcurrentConversions(t *testing.T) {
	// At most PreviewConcurrency conversions run at once in a process.
	// Arrange
	r := newSBRig(t, artifacts.Options{PreviewConcurrency: 2})
	sb := r.sandbox()
	var mu sync.Mutex
	running, peak := 0, 0
	gate := make(chan struct{})
	started := make(chan struct{}, 4)
	sb.SetExec(sandboxtest.FindExec(sb, func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		if !strings.HasPrefix(req.Command, "soffice ") {
			return sandbox.ExecResult{}, nil
		}
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()
		started <- struct{}{}
		<-gate
		mu.Lock()
		running--
		mu.Unlock()
		return sandbox.ExecResult{ExitCode: 1}, nil
	}))
	var arts []*store.Artifact
	for i := range 4 {
		th, _ := r.mem.CreateThread(r.ctx, "alice", "t")
		a, err := r.svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: th.ID, Login: "alice", Filename: "report.docx", Data: append(testdata(t, "report.docx"), byte(i))})
		if err != nil {
			t.Fatal(err)
		}
		arts = append(arts, a)
	}
	r.svc.Sandbox = &fakeMgr{acquire: func(int) (sandbox.Sandbox, error) { return sb, nil }}
	var wg sync.WaitGroup

	// Act
	for _, a := range arts {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = r.svc.Preview(r.ctx, a, 0) }()
	}
	<-started // two previews hold the slots, the others wait for them
	<-started
	close(gate)
	wg.Wait()

	// Assert
	if peak != 2 {
		t.Fatalf("peak concurrent conversions %d, want 2", peak)
	}
}
