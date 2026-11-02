package artifacts_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/syncx"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/artifacts"
)

// withStore rebuilds the rig's service over a (fault-injecting) store.
func (r *sbRig) withStore(st store.ArtifactStore, blobs artifacts.Blobs, o artifacts.Options) *artifacts.Service {
	if blobs == nil {
		blobs = artifacts.PGBlobs{S: r.mem}
	}
	r.svc = artifacts.New(st, blobs, r.mgr, o)
	return r.svc
}

func TestSnapshotListingIgnoresMalformedAndScratchLines(t *testing.T) {
	// Only well-formed "<size> <mtime> <path>" lines under /workspace (outside .work) become artifacts; paths with spaces survive.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	sb := r.sandbox()
	for p, c := range map[string]string{
		"/workspace/a.txt": "a", "/workspace/my file.txt": "b", "/workspace/bad-size.txt": "c", "/workspace/bad-mtime.txt": "d",
		"/workspace/.work/scratch.txt": "e", "/outside/c.txt": "f",
	} {
		sb.Put(p, c)
	}
	scriptList(sb, func() (string, int) {
		return listLine(1, 1, "/workspace/a.txt") + "garbage\n" + "x 1.0 /workspace/bad-size.txt\n" + "1 y /workspace/bad-mtime.txt\n" +
			listLine(1, 2, "/workspace/my file.txt") + listLine(1, 3, "/workspace/.work/scratch.txt") + listLine(1, 4, "/outside/c.txt") +
			"1 5.0 relative.txt\r\n", 0
	})

	// Act
	changes := r.snapshot(sb)

	// Assert
	got := changePaths(changes)
	if len(got) != 2 || got["/workspace/a.txt"] != store.ChangeCreated || got["/workspace/my file.txt"] != store.ChangeCreated {
		t.Fatalf("changes %v", got)
	}
}

func TestSnapshotNeverDeletesOnAnIncompleteListing(t *testing.T) {
	// A failing find (non-zero exit) or an over-long listing may miss files: nothing is marked deleted then; a complete listing deletes what is gone.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/keep.txt", "k")
	sb.Put("/workspace/gone.txt", "g")
	r.snapshot(sb)
	sb.Remove("/workspace/gone.txt")
	exit := 1
	pad := ""
	scriptList(sb, func() (string, int) { return listLine(1, 7, "/workspace/keep.txt") + pad, exit })

	// Act
	failed := r.snapshot(sb)
	failedState := r.art("/workspace/gone.txt").Deleted
	exit, pad = 0, strings.Repeat("x", 9<<20) // above the 8 MiB listing cap, no newline
	truncated := r.snapshot(sb)
	truncatedState := r.art("/workspace/gone.txt").Deleted
	pad = ""
	complete := r.snapshot(sb)

	// Assert
	if len(failed) != 0 || failedState || len(truncated) != 0 || truncatedState {
		t.Fatalf("incomplete listing deleted a file: %v %v / %v %v", failed, failedState, truncated, truncatedState)
	}
	if got := changePaths(complete); len(got) != 1 || got["/workspace/gone.txt"] != store.ChangeDeleted {
		t.Fatalf("complete listing changes %v", got)
	}
}

func TestSnapshotListFailureIsReported(t *testing.T) {
	// A listing command that cannot run fails the snapshot (nothing is changed).
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	sb := r.sandbox()
	boom := errors.New("exec channel closed")
	sb.SetExec(func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, boom
	})

	// Act
	_, err := r.svc.Snapshot(r.ctx, sb, r.ref())

	// Assert
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "list workspace") {
		t.Fatalf("err %v", err)
	}
}

func TestSnapshotStoreFailuresFailTheSnapshotBeforeAnyChange(t *testing.T) {
	// A lock, clock or listing failure of the metadata store is returned; the in-process lock is released so the next snapshot works.
	boom := errors.New("db down")
	cases := []struct {
		name  string
		set   func(*faultStore)
		unset func(*faultStore)
	}{
		{"advisory lock", func(f *faultStore) { f.lockErr = boom }, func(f *faultStore) { f.lockErr = nil }},
		{"clock", func(f *faultStore) { f.clockErr = boom }, func(f *faultStore) { f.clockErr = nil }},
		{"artifact listing", func(f *faultStore) { f.listErr = boom }, func(f *faultStore) { f.listErr = nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			r := newSBRig(t, artifacts.Options{})
			fs := &faultStore{Store: r.mem}
			r.withStore(fs, nil, artifacts.Options{})
			sb := r.sandbox()
			sb.Put("/workspace/a.txt", "a")
			c.set(fs)

			// Act
			_, err := r.svc.Snapshot(r.ctx, sb, r.ref())
			c.unset(fs)
			changes, retryErr := r.svc.Snapshot(r.ctx, sb, r.ref())

			// Assert
			if !errors.Is(err, boom) {
				t.Fatalf("err %v", err)
			}
			if retryErr != nil || len(changes) != 1 {
				t.Fatalf("retry after the failure: %v %v", changes, retryErr)
			}
		})
	}
}

func TestSnapshotWaitsForTheThreadLockOnlyAsLongAsAllowed(t *testing.T) {
	// A snapshot queued behind another one of the same thread gives up at the lock wait or when its context ends.
	// Arrange
	r := newSBRig(t, artifacts.Options{LockWait: 20 * time.Millisecond})
	sb := r.sandbox()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	sb.SetExec(func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
		once.Do(func() { close(started) })
		<-release
		return sandbox.ExecResult{}, nil
	})
	done := make(chan error, 1)
	go func() { _, err := r.svc.Snapshot(r.ctx, sb, r.ref()); done <- err }()
	<-started
	cancelled, cancel := context.WithCancel(r.ctx)
	cancel()

	// Act
	_, timeout := r.svc.Snapshot(r.ctx, sb, r.ref())
	_, ctxErr := r.svc.Snapshot(cancelled, sb, r.ref())
	close(release)

	// Assert
	if !errors.Is(timeout, syncx.ErrLockTimeout) {
		t.Fatalf("timeout err %v", timeout)
	}
	if !errors.Is(ctxErr, context.Canceled) {
		t.Fatalf("cancelled err %v", ctxErr)
	}
	if err := <-done; err != nil {
		t.Fatalf("holder: %v", err)
	}
}

func TestSnapshotKeepsTheNewestFilesWhenThereAreTooMany(t *testing.T) {
	// Above MaxFiles, the most recently modified files are saved and the oldest are left out.
	// Arrange
	r := newSBRig(t, artifacts.Options{MaxFiles: 2})
	sb := r.sandbox()
	for _, n := range []string{"old", "mid", "new"} {
		sb.Put("/workspace/"+n+".txt", n)
	}
	scriptList(sb, func() (string, int) {
		return listLine(3, 1, "/workspace/old.txt") + listLine(3, 3, "/workspace/new.txt") + listLine(3, 2, "/workspace/mid.txt"), 0
	})

	// Act
	got := changePaths(r.snapshot(sb))

	// Assert
	if len(got) != 2 || got["/workspace/new.txt"] == "" || got["/workspace/mid.txt"] == "" {
		t.Fatalf("saved %v", got)
	}
}

func TestSnapshotDoesNotRereadUnchangedFilesAndAdoptsThemInANewSandbox(t *testing.T) {
	// A file with the recorded size and mtime is not downloaded again; in a different sandbox it is marked as present there, so a later removal is a deletion.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	sbA := r.sandbox()
	sbA.Put("/workspace/a.txt", "same")
	r.snapshot(sbA)
	sbB := sandboxtest.NewSandbox("sbx-other")
	sbB.SetExec(sandboxtest.FindExec(sbB, nil))
	sbB.Put("/workspace/a.txt", "same")
	reads := 0
	flaky := &flakySandbox{Sandbox: sbB, readErr: func(string) error { reads++; return nil }}

	// Act
	again := r.snapshotWith(flaky)
	adopted := r.art("/workspace/a.txt").SandboxID
	sbB.Remove("/workspace/a.txt")
	removed := r.snapshotWith(flaky)

	// Assert
	if len(again) != 0 || reads != 0 || adopted != "sbx-other" {
		t.Fatalf("again %v reads %d adopted in %q", again, reads, adopted)
	}
	if got := changePaths(removed); got["/workspace/a.txt"] != store.ChangeDeleted {
		t.Fatalf("removal in the adopting sandbox: %v", got)
	}
}

func (r *sbRig) snapshotWith(sb sandbox.Sandbox) []artifacts.Change {
	r.t.Helper()
	changes, err := r.svc.Snapshot(r.ctx, sb, r.ref())
	if err != nil {
		r.t.Fatal(err)
	}
	return changes
}

func TestSnapshotDoesNotDeleteFilesOfAnotherSandbox(t *testing.T) {
	// A file never materialized in this sandbox (a pending restore or upload) is not taken for a deletion.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	r.upload("pending.txt", "waiting for a sandbox")
	sb := r.sandbox() // a fresh sandbox: the upload was never written there

	// Act
	changes := r.snapshot(sb)

	// Assert
	if len(changes) != 0 || r.art("/workspace/pending.txt").Deleted {
		t.Fatalf("pending upload deleted: %v", changes)
	}
}

func TestSnapshotHandlesFilesThatChangeWhileItRuns(t *testing.T) {
	// A file that vanishes before it is read counts as deleted; an unreadable one keeps its artifact; one that grew past the cap is left alone.
	// Arrange
	r := newSBRig(t, artifacts.Options{MaxFileBytes: 100})
	sb := r.sandbox()
	sb.Put("/workspace/vanishing.txt", "v1")
	sb.Put("/workspace/unreadable.txt", "u1")
	r.snapshot(sb)
	sb.Put("/workspace/vanishing.txt", "v2")
	sb.Put("/workspace/unreadable.txt", "u2")
	sb.Put("/workspace/growing.txt", strings.Repeat("g", 200))
	scriptList(sb, func() (string, int) {
		return listLine(2, 11, "/workspace/vanishing.txt") + listLine(2, 12, "/workspace/unreadable.txt") + listLine(50, 13, "/workspace/growing.txt") +
			listLine(2, 14, "/workspace/new-unreadable.txt"), 0
	})
	flaky := &flakySandbox{Sandbox: sb, readErr: func(p string) error {
		switch p {
		case "/workspace/vanishing.txt":
			return sandbox.ErrNotFound
		case "/workspace/unreadable.txt", "/workspace/new-unreadable.txt":
			return errors.New("i/o error")
		}
		return nil
	}}

	// Act
	got := changePaths(r.snapshotWith(flaky))

	// Assert
	if len(got) != 1 || got["/workspace/vanishing.txt"] != store.ChangeDeleted {
		t.Fatalf("changes %v", got)
	}
	if a := r.art("/workspace/unreadable.txt"); a.Deleted || a.Version != 1 {
		t.Fatalf("unreadable file artifact %+v", a)
	}
	if r.art("/workspace/growing.txt") != nil || r.art("/workspace/new-unreadable.txt") != nil {
		t.Fatal("a file that could not be read completely was saved")
	}
}

func TestSnapshotSkipsOnlyTheFileWhoseStorageFails(t *testing.T) {
	// A blob or version write failure loses that file's change, not the rest of the snapshot; a concurrent creation of the same path is retried once.
	boom := errors.New("storage down")
	t.Run("blob write failure", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		blobs := &failBlobs{hookBlobs: newHookBlobs(), putErr: boom}
		r.withStore(r.mem, blobs, artifacts.Options{})
		sb := r.sandbox()
		sb.Put("/workspace/a.txt", "a")

		// Act
		changes := r.snapshot(sb)

		// Assert
		if len(changes) != 0 || len(r.all()) != 0 {
			t.Fatalf("changes %v artifacts %d", changes, len(r.all()))
		}
	})
	t.Run("version write failure", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		fs := &faultStore{Store: r.mem, putErr: func(call int) error {
			if call == 1 {
				return boom
			}
			return nil
		}}
		r.withStore(fs, nil, artifacts.Options{})
		sb := r.sandbox()
		sb.Put("/workspace/a.txt", "a")
		sb.Put("/workspace/b.txt", "b")

		// Act
		changes := r.snapshot(sb)

		// Assert
		if len(changes) != 1 || len(r.all()) != 1 {
			t.Fatalf("changes %v artifacts %d", changes, len(r.all()))
		}
	})
	t.Run("conflict is retried once", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		fs := &faultStore{Store: r.mem, putErr: func(call int) error {
			if call == 1 {
				return store.ErrConflict
			}
			return nil
		}}
		r.withStore(fs, nil, artifacts.Options{})
		sb := r.sandbox()
		sb.Put("/workspace/a.txt", "a")

		// Act
		changes := r.snapshot(sb)

		// Assert
		if len(changes) != 1 || changes[0].Change != store.ChangeCreated || fs.puts != 2 {
			t.Fatalf("changes %v puts %d", changes, fs.puts)
		}
	})
	t.Run("materialization failure keeps the changes", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		r.withStore(&faultStore{Store: r.mem, materializeErr: boom}, nil, artifacts.Options{})
		sb := r.sandbox()
		sb.Put("/workspace/a.txt", "a")

		// Act
		changes := r.snapshot(sb)

		// Assert
		if len(changes) != 1 {
			t.Fatalf("changes %v", changes)
		}
	})
}

func TestSnapshotDeletionIsDroppedWhenTheArtifactChangedMeanwhile(t *testing.T) {
	// A file gone from /workspace is only marked deleted if its artifact did not change since the snapshot started (a concurrent restore or upload wins); a store error also leaves it.
	boom := errors.New("db down")
	t.Run("artifact changed during the listing", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		sb := r.sandbox()
		sb.Put("/workspace/a.txt", "a1")
		r.snapshot(sb)
		sb.Remove("/workspace/a.txt")
		scriptList(sb, func() (string, int) {
			cur := r.art("/workspace/a.txt")
			_, _, err := r.mem.PutArtifactVersion(r.ctx, store.PutArtifact{ThreadID: r.th.ID, UserLogin: "alice", Path: cur.Path, Name: cur.Name,
				MimeType: cur.MimeType, Kind: cur.Kind, SizeBytes: 2, SHA256: sum([]byte("a2")), Force: true, SandboxID: sb.ID()})
			if err != nil {
				t.Error(err)
			}
			return "", 0
		})

		// Act
		changes := r.snapshot(sb)

		// Assert
		if len(changes) != 0 || r.art("/workspace/a.txt").Deleted {
			t.Fatalf("changes %v deleted %v", changes, r.art("/workspace/a.txt").Deleted)
		}
	})
	t.Run("store failure", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		fs := &faultStore{Store: r.mem}
		r.withStore(fs, nil, artifacts.Options{})
		sb := r.sandbox()
		sb.Put("/workspace/a.txt", "a1")
		r.snapshot(sb)
		sb.Remove("/workspace/a.txt")
		fs.deleteErr = boom

		// Act
		changes := r.snapshot(sb)

		// Assert
		if len(changes) != 0 || r.art("/workspace/a.txt").Deleted {
			t.Fatalf("changes %v", changes)
		}
	})
}

func TestSnapshotReportsFilesAboveTheBackendCapWithThatCap(t *testing.T) {
	// The skipped-file event carries the smaller of the file limit and the blob backend's cap.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	r.withStore(r.mem, capBlobs{hookBlobs: newHookBlobs(), max: 10}, artifacts.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/big.bin", strings.Repeat("x", 20))
	sb.Put("/workspace/ok.bin", "tiny")
	var events []agui.Event
	run := &store.Run{ID: "r1", ThreadID: r.th.ID, UserLogin: "alice"}

	// Act
	changes := r.svc.SnapshotRun(r.ctx, run, "", func(ev agui.Event) error { events = append(events, ev); return nil })

	// Assert
	var skipped map[string]any
	for _, ev := range events {
		if ev["name"] == artifacts.CustomArtifactSkipped {
			skipped = ev["value"].(map[string]any)
		}
	}
	if len(changes) != 1 || skipped == nil || skipped["maxBytes"] != int64(10) || skipped["sizeBytes"] != int64(20) {
		t.Fatalf("changes %d skipped %v", len(changes), skipped)
	}
	if _, ok := skipped["toolCallId"]; ok {
		t.Fatalf("empty tool call id must be omitted: %v", skipped)
	}
}

func TestSnapshotRunDoesNothingWithoutALiveSandbox(t *testing.T) {
	// No service, no manager, a disabled manager, a thread without sandbox or an unavailable one: no changes, no events, no sandbox created.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	run := &store.Run{ID: "r1", ThreadID: r.th.ID, UserLogin: "alice"}
	calls := 0
	emit := func(agui.Event) error { calls++; return nil }
	var nilSvc *artifacts.Service
	noMgr := artifacts.New(r.mem, artifacts.PGBlobs{S: r.mem}, nil, artifacts.Options{})
	disabled := artifacts.New(r.mem, artifacts.PGBlobs{S: r.mem}, &fakeMgr{disabled: true}, artifacts.Options{})
	unavailable := artifacts.New(r.mem, artifacts.PGBlobs{S: r.mem}, &fakeMgr{get: func(int) (sandbox.Sandbox, error) { return nil, sandbox.ErrUnavailable }}, artifacts.Options{})
	other := artifacts.New(r.mem, artifacts.PGBlobs{S: r.mem}, &fakeMgr{get: func(int) (sandbox.Sandbox, error) { return nil, errors.New("boom") }}, artifacts.Options{})

	// Act
	results := [][]artifacts.Change{
		nilSvc.SnapshotRun(r.ctx, run, "", emit), noMgr.SnapshotRun(r.ctx, run, "", emit), disabled.SnapshotRun(r.ctx, run, "", emit),
		r.svc.SnapshotRun(r.ctx, run, "", emit), unavailable.SnapshotRun(r.ctx, run, "", emit), other.SnapshotRun(r.ctx, run, "", emit),
	}

	// Assert
	for i, res := range results {
		if len(res) != 0 {
			t.Errorf("case %d returned %v", i, res)
		}
	}
	if calls != 0 || len(r.backend.Created()) != 0 {
		t.Fatalf("events %d sandboxes created %d", calls, len(r.backend.Created()))
	}
}

func TestSnapshotRunRetriesOnANewSandboxWhenTheSandboxIsGone(t *testing.T) {
	// A sandbox found gone during the snapshot is invalidated and the snapshot runs again on the sandbox the manager hands out next.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	dying := sandboxtest.NewSandbox("sbx-dying")
	dying.Die()
	alive := sandboxtest.NewSandbox("sbx-alive")
	alive.SetExec(sandboxtest.FindExec(alive, nil))
	alive.Put("/workspace/a.txt", "a")
	mgr := &fakeMgr{get: func(call int) (sandbox.Sandbox, error) {
		if call == 1 {
			return dying, nil
		}
		return alive, nil
	}}
	svc := artifacts.New(r.mem, artifacts.PGBlobs{S: r.mem}, mgr, artifacts.Options{})
	run := &store.Run{ID: "r1", ThreadID: r.th.ID, UserLogin: "alice"}

	// Act
	changes := svc.SnapshotRun(r.ctx, run, "", func(agui.Event) error { return nil })

	// Assert
	if len(changes) != 1 || len(mgr.invalidated) != 1 || mgr.invalidated[0] != "sbx-dying" {
		t.Fatalf("changes %v invalidated %v", changes, mgr.invalidated)
	}
}

func TestSnapshotRunStopsEmittingAfterAnEventFails(t *testing.T) {
	// A failing event sink stops the event stream, but the changes are still saved and returned.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "a")
	sb.Put("/workspace/b.txt", "b")
	run := &store.Run{ID: "r1", ThreadID: r.th.ID, UserLogin: "alice"}
	calls := 0
	emit := func(agui.Event) error { calls++; return errors.New("client gone") }

	// Act
	changes := r.svc.SnapshotRun(r.ctx, run, "call-1", emit)

	// Assert
	if len(changes) != 2 || calls != 1 {
		t.Fatalf("changes %d emit calls %d", len(changes), calls)
	}
	// Arrange: only skipped files this time
	r2 := newSBRig(t, artifacts.Options{MaxFileBytes: 4})
	sb2 := r2.sandbox()
	sb2.Put("/workspace/big1.txt", "too large")
	sb2.Put("/workspace/big2.txt", "too large")
	calls = 0

	// Act
	changes = r2.svc.SnapshotRun(r2.ctx, &store.Run{ID: "r2", ThreadID: r2.th.ID, UserLogin: "alice"}, "", emit)

	// Assert
	if len(changes) != 0 || calls != 1 {
		t.Fatalf("changes %d emit calls %d", len(changes), calls)
	}
}

// afterEmitter collects the events queued after a tool result.
type afterEmitter struct {
	mu     sync.Mutex
	events []agui.Event
}

func (e *afterEmitter) ToolOutput(string, string, string) {}
func (e *afterEmitter) EmitAfterResult(ev agui.Event) {
	e.mu.Lock()
	e.events = append(e.events, ev)
	e.mu.Unlock()
}

func TestAfterToolSnapshotsOnlyAfterFileChangingTools(t *testing.T) {
	// write_file, edit_file and execute_command snapshot and report the changes (events queued after the result, items for the message); other tools never touch the sandbox.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "a")
	run := &store.Run{ID: "r1", ThreadID: r.th.ID, UserLogin: "alice"}
	em := &afterEmitter{}
	ctx := tools.WithCall(r.ctx, "call-9", em)

	// Act
	readOnly := r.svc.AfterTool(ctx, run, "call-8", "read_file")
	meta := r.svc.AfterTool(ctx, run, "call-9", "execute_command")
	quiet := r.svc.AfterTool(ctx, run, "call-10", "write_file")

	// Assert
	if readOnly != nil || quiet != nil || len(r.all()) != 1 {
		t.Fatalf("read_file %v, unchanged write %v, artifacts %d", readOnly, quiet, len(r.all()))
	}
	items, _ := meta["artifacts"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["path"] != "/workspace/a.txt" || items[0].(map[string]any)["change"] != store.ChangeCreated {
		t.Fatalf("metadata %v", meta)
	}
	if len(em.events) != 1 || em.events[0]["value"].(map[string]any)["toolCallId"] != "call-9" {
		t.Fatalf("queued events %v", em.events)
	}
}

func TestSnapshotEndRecordsChangesOnTheRunsMessageEvenWhenCancelled(t *testing.T) {
	// The end-of-run snapshot ignores the run's cancellation and attaches its changes to the run's assistant message; a failure to record them is not fatal.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "a")
	run, _, err := r.mem.CreateRun(r.ctx, store.NewRun{RunID: "run-1", ThreadID: r.th.ID, UserLogin: "alice", Message: store.Message{Role: store.RoleUser, Content: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(r.ctx)
	cancel()
	var events []agui.Event

	// Act
	r.svc.SnapshotEnd(cancelled, run, func(ev agui.Event) error { events = append(events, ev); return nil })

	// Assert
	msgs, _ := r.mem.ListMessages(r.ctx, r.th.ID)
	var recorded []any
	for _, m := range msgs {
		if m.Role == store.RoleAssistant {
			recorded, _ = m.Metadata["artifacts"].([]any)
		}
	}
	if len(events) != 1 || len(recorded) != 1 {
		t.Fatalf("events %d recorded %v", len(events), recorded)
	}

	// Arrange: recording fails
	r2 := newSBRig(t, artifacts.Options{})
	r2.withStore(&faultStore{Store: r2.mem, appendErr: errors.New("db down")}, nil, artifacts.Options{})
	sb2 := r2.sandbox()
	sb2.Put("/workspace/b.txt", "b")
	events = nil

	// Act
	r2.svc.SnapshotEnd(r2.ctx, &store.Run{ID: "run-2", ThreadID: r2.th.ID, UserLogin: "alice"}, func(ev agui.Event) error { events = append(events, ev); return nil })

	// Assert
	if len(events) != 1 || r2.art("/workspace/b.txt") == nil {
		t.Fatalf("events %d", len(events))
	}
}

func TestProducedPathsListsLiveOutputsOnly(t *testing.T) {
	// The goal evaluator's "files produced" are the non-deleted outputs: uploads and deleted files are excluded.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	r.upload("attached.txt", "u")
	sb := r.sandbox()
	sb.Put("/workspace/outputs/kept.md", "k")
	sb.Put("/workspace/outputs/removed.md", "r")
	r.snapshot(sb)
	sb.Remove("/workspace/outputs/removed.md")
	r.snapshot(sb)

	// Act
	paths, err := r.svc.ProducedPaths(r.ctx, r.th.ID)
	_, failErr := artifacts.New(&faultStore{Store: r.mem, listErr: errors.New("db down")}, nil, nil, artifacts.Options{}).ProducedPaths(r.ctx, r.th.ID)

	// Assert
	if err != nil || len(paths) != 1 || paths[0] != "/workspace/outputs/kept.md" {
		t.Fatalf("paths %v err %v", paths, err)
	}
	if failErr == nil {
		t.Fatal("a listing failure must be returned")
	}
}

func TestSkippedAndArtifactEventsCarryTheirFields(t *testing.T) {
	// The CUSTOM events and message items expose the artifact identity and omit an empty tool call id.
	// Arrange
	a := &store.Artifact{ID: "id1", Path: "/workspace/a.txt", Name: "a.txt", Version: 3, SizeBytes: 5, MimeType: "text/plain", Kind: store.ArtifactOutput}

	// Act
	withCall := artifacts.Event(a, store.ChangeUpdated, "call-1")["value"].(map[string]any)
	without := artifacts.Event(a, store.ChangeCreated, "")["value"].(map[string]any)
	items := artifacts.MessageItems([]artifacts.Change{{Artifact: a, Change: store.ChangeDeleted}})

	// Assert
	if withCall["toolCallId"] != "call-1" || withCall["version"] != 3 || withCall["change"] != store.ChangeUpdated {
		t.Fatalf("event %v", withCall)
	}
	if _, ok := without["toolCallId"]; ok {
		t.Fatalf("empty tool call id kept: %v", without)
	}
	m := items[0].(map[string]any)
	if len(items) != 1 || m["artifactId"] != "id1" || m["version"] != 3 || m["change"] != store.ChangeDeleted {
		t.Fatalf("items %v", items)
	}
}
