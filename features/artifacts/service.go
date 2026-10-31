package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/syncx"
	"github.com/7ar-chat/harness/engine/tools"
)

// CustomArtifact is the CUSTOM event announcing an artifact change.
const CustomArtifact = "work.artifact"

// CustomArtifactSkipped is the CUSTOM event announcing a workspace file that is
// not saved as an artifact: {path, sizeBytes, maxBytes, reason: "too_large", toolCallId?}.
const CustomArtifactSkipped = "work.artifact_skipped"

// Errors mapped to HTTP codes by the API.
var (
	ErrNotPreviewable  = errors.New("this file type cannot be previewed")
	ErrPreviewFailed   = errors.New("the preview could not be generated")
	ErrTypeMismatch    = errors.New("file extension, declared type and content disagree")
	ErrTooLarge        = errors.New("the file exceeds the storage limit")
	ErrSandboxDisabled = errors.New("no sandbox is configured")
)

// Options are the limits of the contract (§2).
type Options struct {
	MaxFileBytes    int64         // snapshot: files above are skipped (default 50 MiB)
	MaxFiles        int           // snapshot: at most this many files (default 500, newest first)
	RestoreMaxFiles int           // workspace restore (default 200)
	RestoreMaxBytes int64         // workspace restore (default 500 MiB)
	PreviewTimeout  time.Duration // soffice conversion (default 120 s)
	// PreviewConcurrency bounds the soffice conversions of this process (default 2).
	PreviewConcurrency int
	// LockWait bounds the wait for a thread's artifact lock (default 2 min).
	LockWait time.Duration
	// BlobGCGrace is how long a content blob must have had no reference before
	// the retention purge deletes it (default 24 h, HARNESS_BLOB_GC_GRACE_HOURS).
	BlobGCGrace time.Duration
}

// DefaultBlobGCGrace is the default Options.BlobGCGrace.
const DefaultBlobGCGrace = 24 * time.Hour

// Service implements artifacts on top of the metadata store, a blob store and the sandbox manager.
type Service struct {
	Store   store.ArtifactStore
	Blobs   Blobs
	Sandbox sandbox.Manager
	Opts    Options

	locks   syncx.KeyedMutex
	semOnce sync.Once
	convert chan struct{} // process-wide soffice slots
	// skippedSeen remembers the too-large files already announced (thread, path, size, mtime).
	skippedSeen syncx.LRU[string, struct{}]
}

// Skipped is a workspace file a snapshot did not save (above MaxFileBytes or
// the blob backend's cap): files over the limit are refused, not versioned by
// reference (roadmap 5.8).
type Skipped struct {
	Path      string
	SizeBytes int64
	MaxBytes  int64
	mtime     float64
}

// SkippedEvent builds the CUSTOM work.artifact_skipped event.
func SkippedEvent(s Skipped, toolCallID string) agui.Event {
	v := map[string]any{"path": s.Path, "sizeBytes": s.SizeBytes, "maxBytes": s.MaxBytes, "reason": "too_large"}
	if toolCallID != "" {
		v["toolCallId"] = toolCallID
	}
	return agui.CustomEvent(CustomArtifactSkipped, v)
}

// New returns a service with defaults applied.
func New(st store.ArtifactStore, blobs Blobs, mgr sandbox.Manager, o Options) *Service {
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = 50 << 20
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = 500
	}
	if o.RestoreMaxFiles <= 0 {
		o.RestoreMaxFiles = 200
	}
	if o.RestoreMaxBytes <= 0 {
		o.RestoreMaxBytes = 500 << 20
	}
	if o.PreviewTimeout <= 0 {
		o.PreviewTimeout = 120 * time.Second
	}
	if o.PreviewConcurrency <= 0 {
		o.PreviewConcurrency = 2
	}
	if o.LockWait <= 0 {
		o.LockWait = 2 * time.Minute
	}
	if o.BlobGCGrace <= 0 {
		o.BlobGCGrace = DefaultBlobGCGrace
	}
	return &Service{Store: st, Blobs: blobs, Sandbox: mgr, Opts: o}
}

// lockThread serializes the artifact changes of a thread across every api and
// runtime process: snapshot diff-and-apply, uploads, version restores and the
// workspace restore (in-process mutex first, then the Postgres advisory lock).
func (s *Service) lockThread(ctx context.Context, threadID string) (func(), error) {
	return s.lockAll(ctx, "work:artifacts:"+threadID, s.Opts.LockWait)
}

func (s *Service) lockAll(ctx context.Context, name string, wait time.Duration) (func(), error) {
	if wait <= 0 {
		wait = 2 * time.Minute
	}
	start := time.Now()
	release, err := s.locks.LockWait(ctx, name, wait) // bounded by the wait and ctx (C-23)
	if err != nil {
		return nil, fmt.Errorf("artifact lock %s: %w", name, err)
	}
	unlock, err := s.Store.AdvisoryLock(ctx, name, max(wait-time.Since(start), time.Millisecond))
	if err != nil {
		release()
		return nil, fmt.Errorf("artifact lock %s: %w", name, err)
	}
	return func() { unlock(); release() }, nil
}

// conversionSlot takes one of the process-wide soffice slots.
func (s *Service) conversionSlot(ctx context.Context) (func(), error) {
	s.semOnce.Do(func() {
		n := s.Opts.PreviewConcurrency
		if n <= 0 {
			n = 2
		}
		s.convert = make(chan struct{}, n)
	})
	select {
	case s.convert <- struct{}{}:
		return func() { <-s.convert }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Event builds the CUSTOM work.artifact event; toolCallID is the call whose
// result produced the change ("" for end-of-run snapshots, then omitted).
func Event(a *store.Artifact, change, toolCallID string) agui.Event {
	v := map[string]any{
		"artifactId": a.ID, "path": a.Path, "name": a.Name, "version": a.Version, "sizeBytes": a.SizeBytes,
		"mimeType": a.MimeType, "kind": a.Kind, "change": change,
	}
	if toolCallID != "" {
		v["toolCallId"] = toolCallID
	}
	return agui.CustomEvent(CustomArtifact, v)
}

// MessageItems is the metadata.artifacts form of changes (stored on messages for reloads).
func MessageItems(changes []Change) []any {
	out := make([]any, 0, len(changes))
	for _, c := range changes {
		out = append(out, map[string]any{"artifactId": c.Artifact.ID, "path": c.Artifact.Path, "name": c.Artifact.Name,
			"version": c.Artifact.Version, "change": c.Change})
	}
	return out
}

// Change is one artifact change found by a snapshot.
type Change struct {
	Artifact *store.Artifact
	Change   string
}

// RunRef identifies the run a snapshot is attributed to.
type RunRef struct {
	ThreadID string
	RunID    string
	Login    string
}

type listed struct {
	path  string
	size  int64
	mtime float64
}

// skippedDirs are the directory names the snapshot listing skips anywhere
// under /workspace (with /workspace/.work); files there are never artifacts.
var skippedDirs = map[string]bool{"node_modules": true, ".git": true, "__pycache__": true, ".venv": true}

// InSkippedDir reports a workspace path inside a directory snapshots skip
// (uploads and checkpoint restores there would be marked deleted; C-22).
func InSkippedDir(p string) bool {
	rel := strings.TrimPrefix(path.Clean(p), sandbox.Workspace+"/")
	if rel == ".work" || strings.HasPrefix(rel, ".work/") {
		return true
	}
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		if skippedDirs[part] {
			return true
		}
	}
	return false
}

// listCommand lists /workspace in one call: "<size> <mtime> <path>" per file,
// skipping /workspace/.work and the skippedDirs.
const listCommand = `find /workspace -mindepth 1 \( -path /workspace/.work -o -name node_modules -o -name .git -o -name __pycache__ -o -name .venv \) -prune -o -type f -printf '%s %T@ %p\n' 2>/dev/null`

const maxListingBytes = 8 << 20

// list returns the workspace files; complete=false when the listing may miss files.
func (s *Service) list(ctx context.Context, sb sandbox.Sandbox) ([]listed, bool, error) {
	var out bytes.Buffer
	truncated := false
	res, err := sb.Exec(ctx, sandbox.ExecRequest{
		Command: listCommand, Cwd: sandbox.Workspace, Timeout: 60 * time.Second,
		OnStdout: func(t string) {
			if out.Len()+len(t) > maxListingBytes {
				truncated = true
				return
			}
			out.WriteString(t)
		},
	})
	if err != nil {
		return nil, false, err
	}
	var files []listed
	for _, line := range strings.Split(out.String(), "\n") {
		parts := strings.SplitN(strings.TrimRight(line, "\r"), " ", 3)
		if len(parts) != 3 {
			continue
		}
		size, err1 := strconv.ParseInt(parts[0], 10, 64)
		mtime, err2 := strconv.ParseFloat(parts[1], 64)
		p := path.Clean(parts[2])
		if err1 != nil || err2 != nil || !strings.HasPrefix(p, sandbox.Workspace+"/") || strings.HasPrefix(p, sandbox.Workspace+"/.work/") {
			continue
		}
		files = append(files, listed{path: p, size: size, mtime: mtime})
	}
	return files, res.ExitCode == 0 && !truncated, nil
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// putBlob stores content unless it is already stored. Reusing stored content
// first refreshes its last reference, so the blob GC cannot collect it before
// the version referencing it is recorded (and a collection in flight finishes
// first: the check below then sees the content gone and stores it again).
func (s *Service) putBlob(ctx context.Context, key string, data []byte) error {
	if err := s.Store.TouchBlob(ctx, key); err != nil {
		return err
	}
	if ok, err := s.Blobs.Has(ctx, key); err == nil && ok {
		return nil
	}
	// Queued before it is stored: content whose version row is never written
	// (lock timeout, failure, crash) is collected after the grace (C-25).
	if err := s.Store.QueueBlob(ctx, key); err != nil {
		return err
	}
	return s.Blobs.Put(ctx, key, data)
}

// put records content at p (retrying a concurrent creation of the same path).
func (s *Service) put(ctx context.Context, in store.PutArtifact) (*store.Artifact, string, error) {
	a, change, err := s.Store.PutArtifactVersion(ctx, in)
	if errors.Is(err, store.ErrConflict) {
		a, change, err = s.Store.PutArtifactVersion(ctx, in)
	}
	return a, change, err
}

// Snapshot diffs /workspace against the thread's artifacts and stores every new
// or changed file as a new version; files gone from the workspace are marked
// deleted — only those known to be in this sandbox (seen in a listing or
// written there) and unchanged since the listing started.
func (s *Service) Snapshot(ctx context.Context, sb sandbox.Sandbox, ref RunRef) ([]Change, error) {
	changes, _, err := s.snapshot(ctx, sb, ref)
	return changes, err
}

// limitBytes is the size above which a workspace file is not saved.
func (s *Service) limitBytes() int64 {
	limit := s.Opts.MaxFileBytes
	if m := s.Blobs.MaxBytes(); m > 0 && m < limit {
		limit = m
	}
	return limit
}

func (s *Service) snapshot(ctx context.Context, sb sandbox.Sandbox, ref RunRef) ([]Change, []Skipped, error) {
	unlock, err := s.lockThread(ctx, ref.ThreadID)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	since, err := s.Store.Clock(ctx)
	if err != nil {
		return nil, nil, err
	}
	files, complete, err := s.list(ctx, sb)
	if err != nil {
		return nil, nil, fmt.Errorf("list workspace: %w", err)
	}
	existing, err := s.Store.ListArtifacts(ctx, ref.ThreadID)
	if err != nil {
		return nil, nil, err
	}
	sbID := sb.ID()
	byPath := map[string]*store.Artifact{}
	for _, a := range existing {
		byPath[a.Path] = a
	}
	present := map[string]bool{}
	for _, f := range files {
		present[f.path] = true
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].mtime > files[j].mtime })
	var eligible []listed
	var skipped []Skipped
	limit := s.limitBytes()
	for _, f := range files {
		if f.size > limit {
			skipped = append(skipped, Skipped{Path: f.path, SizeBytes: f.size, MaxBytes: limit, mtime: f.mtime})
			continue
		}
		eligible = append(eligible, f)
	}
	if len(eligible) > s.Opts.MaxFiles {
		slog.Warn("artifacts: workspace has too many files, keeping the newest", "thread_id", ref.ThreadID, "files", len(eligible), "max", s.Opts.MaxFiles)
		eligible = eligible[:s.Opts.MaxFiles]
	}
	var changes []Change
	var seen []string // unchanged artifacts first seen in this sandbox
	for _, f := range eligible {
		a := byPath[f.path]
		if a != nil && !a.Deleted && a.SizeBytes == f.size && a.SourceMtime == f.mtime {
			if a.SandboxID != sbID {
				seen = append(seen, a.ID)
			}
			continue
		}
		data, size, err := sb.ReadFile(ctx, f.path, s.Opts.MaxFileBytes)
		if errors.Is(err, sandbox.ErrNotFound) {
			delete(present, f.path)
			continue
		}
		if err != nil {
			slog.Warn("artifacts: reading workspace file failed", "thread_id", ref.ThreadID, "path", f.path, "error", err)
			present[f.path] = true
			continue
		}
		if size > int64(len(data)) {
			continue // grew past the cap meanwhile
		}
		sum := sha(data)
		if a == nil || a.Deleted || a.SHA256 != sum {
			if err := s.putBlob(ctx, sum, data); err != nil {
				slog.Error("artifacts: storing blob failed", "thread_id", ref.ThreadID, "path", f.path, "error", err)
				continue
			}
		}
		name := path.Base(f.path)
		got, change, err := s.put(ctx, store.PutArtifact{ThreadID: ref.ThreadID, UserLogin: ref.Login, Path: f.path, Name: name,
			MimeType: ContentMime(name, data), Kind: store.ArtifactOutput, SizeBytes: int64(len(data)), SHA256: sum, RunID: ref.RunID,
			Mtime: f.mtime, SandboxID: sbID})
		if err != nil {
			slog.Error("artifacts: recording version failed", "thread_id", ref.ThreadID, "path", f.path, "error", err)
			continue
		}
		if change != "" {
			changes = append(changes, Change{Artifact: got, Change: change})
		}
	}
	if err := s.Store.MarkArtifactsMaterialized(ctx, sbID, seen); err != nil {
		slog.Warn("artifacts: recording materialized files failed", "thread_id", ref.ThreadID, "error", err)
	}
	if complete {
		for _, a := range existing {
			if a.Deleted || present[a.Path] || a.SandboxID != sbID {
				continue // not in the listing, or never materialized in this sandbox (pending restore/upload)
			}
			got, err := s.Store.MarkArtifactDeleted(ctx, a.ID, ref.RunID, sbID, since)
			if errors.Is(err, store.ErrConflict) {
				continue // changed meanwhile (e.g. restored or re-uploaded)
			}
			if err != nil {
				slog.Error("artifacts: marking deletion failed", "artifact_id", a.ID, "error", err)
				continue
			}
			changes = append(changes, Change{Artifact: got, Change: store.ChangeDeleted})
		}
	}
	return changes, skipped, nil
}

// SnapshotRun snapshots the thread's live sandbox (never creates one), emits a
// work.artifact event per change and returns the changes.
func (s *Service) SnapshotRun(ctx context.Context, run *store.Run, toolCallID string, emit func(agui.Event) error) []Change {
	if s == nil || s.Sandbox == nil || !s.Sandbox.Enabled() {
		return nil
	}
	// A sandbox found gone is forgotten; the retry then finds no sandbox
	// (Get never creates one): there is nothing left to snapshot.
	var changes []Change
	var skipped []Skipped
	err := sandbox.Retry(ctx, s.Sandbox, run.ThreadID, func(ctx context.Context) (sandbox.Sandbox, error) {
		return s.Sandbox.Get(ctx, run.ThreadID)
	}, func(sb sandbox.Sandbox) error {
		var err error
		changes, skipped, err = s.snapshot(ctx, sb, RunRef{ThreadID: run.ThreadID, RunID: run.ID, Login: run.UserLogin})
		return err
	})
	switch {
	case errors.Is(err, sandbox.ErrNoSandbox):
	case errors.Is(err, sandbox.ErrUnavailable):
		slog.Warn("artifacts: sandbox unavailable for snapshot", "run_id", run.ID, "error", err)
	case err != nil:
		slog.Warn("artifacts: snapshot failed", "run_id", run.ID, "error", err)
	}
	for _, c := range changes {
		if err := emit(Event(c.Artifact, c.Change, toolCallID)); err != nil {
			slog.Warn("artifacts: emitting event failed", "run_id", run.ID, "error", err)
			break
		}
	}
	for _, sk := range skipped {
		key := fmt.Sprintf("%s\x00%s\x00%d\x00%f", run.ThreadID, sk.Path, sk.SizeBytes, sk.mtime)
		if _, seen := s.skippedSeen.Get(key); seen {
			continue // announced once per file state
		}
		s.skippedSeen.Add(key, struct{}{})
		slog.Info("artifacts: workspace file too large to save", "run_id", run.ID, "path", sk.Path, "size_bytes", sk.SizeBytes, "max_bytes", sk.MaxBytes)
		if err := emit(SkippedEvent(sk, toolCallID)); err != nil {
			slog.Warn("artifacts: emitting event failed", "run_id", run.ID, "error", err)
			break
		}
	}
	return changes
}

// AfterTool implements agent.ToolHook: snapshot after write_file, edit_file
// and execute_command. The work.artifact events are queued with
// tools.AfterResult (written right after the call's TOOL_CALL_RESULT, with the
// tools' own events); the changes go to the tool message metadata.
func (s *Service) AfterTool(ctx context.Context, run *store.Run, toolCallID, toolName string) map[string]any {
	switch toolName {
	case "write_file", "edit_file", "execute_command":
		queue := func(ev agui.Event) error { tools.AfterResult(ctx, ev); return nil }
		if changes := s.SnapshotRun(ctx, run, toolCallID, queue); len(changes) > 0 {
			return map[string]any{"artifacts": MessageItems(changes)}
		}
	}
	return nil
}

// ProducedPaths lists the paths of the thread's live output artifacts (the
// goal evaluator's "files produced").
func (s *Service) ProducedPaths(ctx context.Context, threadID string) ([]string, error) {
	all, err := s.Store.ListArtifacts(ctx, threadID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range all {
		if !a.Deleted && a.Kind == store.ArtifactOutput {
			out = append(out, a.Path)
		}
	}
	return out, nil
}

// SnapshotEnd snapshots the workspace at the end of a run (detached from
// cancellation); changes are recorded on the run's last assistant message.
func (s *Service) SnapshotEnd(ctx context.Context, run *store.Run, emit func(agui.Event) error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	changes := s.SnapshotRun(ctx, run, "", emit)
	if len(changes) == 0 {
		return
	}
	if err := s.Store.AppendRunArtifacts(ctx, run.ID, MessageItems(changes)); err != nil {
		slog.Warn("artifacts: recording end-of-run changes failed", "run_id", run.ID, "error", err)
	}
}

// RestoreWorkspace writes the latest version of every non-deleted artifact back
// into a new sandbox (files a previous restore skipped first, oldest skipped
// first, then the newest; ≤ RestoreMaxFiles / RestoreMaxBytes) and returns the
// notice for the model ("" when there was nothing to restore). Skipped files
// stay pending: they are never taken for deletions and go first next time.
func (s *Service) RestoreWorkspace(ctx context.Context, threadID string, sb sandbox.Sandbox, recreated bool) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	// Never without the lock (C-21): a restore racing a snapshot or a version
	// restore could bring back stale content or false deletions. Without it,
	// nothing is written; every file stays pending for the next restore.
	locked := true
	if unlock, err := s.lockThread(ctx, threadID); err != nil {
		slog.Warn("artifacts: thread lock unavailable; the workspace restore is postponed", "thread_id", threadID, "error", err)
		locked = false
	} else {
		defer unlock()
	}
	all, err := s.Store.ListArtifacts(ctx, threadID)
	if err != nil {
		slog.Error("artifacts: listing for restore failed", "thread_id", threadID, "error", err)
		return ""
	}
	var live []*store.Artifact
	for _, a := range all {
		if !a.Deleted {
			live = append(live, a)
		}
	}
	sort.SliceStable(live, func(i, j int) bool {
		si, sj := live[i].RestoreSkippedAt, live[j].RestoreSkippedAt
		switch {
		case si != nil && sj != nil:
			return si.Before(*sj)
		case si != nil || sj != nil:
			return si != nil
		}
		return live[i].UpdatedAt.After(live[j].UpdatedAt)
	})
	var restored []*store.Artifact
	var skipped []*store.Artifact
	var total int64
	for _, a := range live {
		if !locked || len(restored) >= s.Opts.RestoreMaxFiles || total+a.SizeBytes > s.Opts.RestoreMaxBytes {
			skipped = append(skipped, a)
			continue
		}
		data, err := s.Blobs.Get(ctx, a.SHA256)
		if err == nil {
			err = sb.WriteFile(ctx, a.Path, data)
		}
		if err != nil {
			slog.Warn("artifacts: restoring file failed", "thread_id", threadID, "path", a.Path, "error", err)
			skipped = append(skipped, a)
			continue
		}
		total += a.SizeBytes
		restored = append(restored, a)
	}
	s.recordRestore(ctx, threadID, sb, restored, skipped)
	if len(restored) == 0 && len(skipped) == 0 {
		return ""
	}
	var b strings.Builder
	if recreated {
		b.WriteString("Notice: the sandbox stopped or stayed unresponsive and has been recreated. ")
	} else {
		b.WriteString("Notice: this conversation's sandbox was restarted. ")
	}
	fmt.Fprintf(&b, "%d saved file(s) were restored into /workspace from the conversation's artifacts (other files, installed packages and running processes are gone).", len(restored))
	if len(skipped) > 0 {
		shown := make([]string, 0, 20)
		for _, a := range skipped {
			if len(shown) == 20 {
				break
			}
			shown = append(shown, a.Path)
		}
		reason := fmt.Sprintf("limit %d files / %d MiB or a read/write failure", s.Opts.RestoreMaxFiles, s.Opts.RestoreMaxBytes>>20)
		if !locked {
			reason = "the conversation's files were busy"
		}
		fmt.Fprintf(&b, " Still pending (not written into /workspace: %s; they are kept in the conversation's artifacts and restored first when the sandbox is next restarted, so do not recreate or delete them): %s",
			reason, strings.Join(shown, ", "))
		if len(skipped) > len(shown) {
			fmt.Fprintf(&b, " and %d more", len(skipped)-len(shown))
		}
		b.WriteString(".")
	}
	return b.String()
}

// recordRestore marks the restored files as materialized in sb (with their new
// mtimes, so the next snapshot does not re-download them) and the skipped ones as pending.
func (s *Service) recordRestore(ctx context.Context, threadID string, sb sandbox.Sandbox, restored, skipped []*store.Artifact) {
	var unlisted []string
	if len(restored) > 0 {
		files, _, err := s.list(ctx, sb)
		sizes := map[string]listed{}
		if err == nil {
			for _, f := range files {
				sizes[f.path] = f
			}
		}
		for _, a := range restored {
			f, ok := sizes[a.Path]
			if !ok || f.size != a.SizeBytes {
				unlisted = append(unlisted, a.ID)
				continue
			}
			if _, _, err := s.put(ctx, store.PutArtifact{ThreadID: threadID, UserLogin: a.UserLogin, Path: a.Path, Name: a.Name,
				MimeType: a.MimeType, Kind: a.Kind, SizeBytes: a.SizeBytes, SHA256: a.SHA256, RunID: a.RunID, Mtime: f.mtime,
				SandboxID: sb.ID()}); err != nil {
				unlisted = append(unlisted, a.ID)
			}
		}
	}
	if err := s.Store.MarkArtifactsMaterialized(ctx, sb.ID(), unlisted); err != nil {
		slog.Warn("artifacts: recording restored files failed", "thread_id", threadID, "error", err)
	}
	ids := make([]string, 0, len(skipped))
	for _, a := range skipped {
		ids = append(ids, a.ID)
	}
	if err := s.Store.MarkArtifactsRestoreSkipped(ctx, ids); err != nil {
		slog.Warn("artifacts: recording skipped files failed", "thread_id", threadID, "error", err)
	}
}

// Version resolves a version (0 = latest) of an artifact.
func (s *Service) Version(ctx context.Context, a *store.Artifact, version int) (*store.ArtifactVersion, error) {
	if version <= 0 {
		version = a.Version
	}
	return s.Store.GetArtifactVersion(ctx, a.ID, version)
}

// Content returns the bytes of an artifact version (0 = latest).
func (s *Service) Content(ctx context.Context, a *store.Artifact, version int) ([]byte, *store.ArtifactVersion, error) {
	v, err := s.Version(ctx, a, version)
	if err != nil {
		return nil, nil, err
	}
	data, err := s.Blobs.Get(ctx, v.SHA256)
	if err != nil {
		return nil, nil, err
	}
	return data, v, nil
}

// ListArtifacts returns every artifact of a thread, deleted included
// (verify.ArtifactSource: the originals of decks, the uploaded PDFs).
func (s *Service) ListArtifacts(ctx context.Context, threadID string) ([]*store.Artifact, error) {
	return s.Store.ListArtifacts(ctx, threadID)
}

// ListArtifactVersions returns the stored versions of an artifact (verify.ArtifactSource).
func (s *Service) ListArtifactVersions(ctx context.Context, artifactID string) ([]store.ArtifactVersion, error) {
	return s.Store.ListArtifactVersions(ctx, artifactID)
}

// Delivery is a file ready to be served.
type Delivery struct {
	Data        []byte
	ContentType string
}

// Deliver returns a file as is, with the content type of its name.
func (s *Service) Deliver(_ context.Context, name string, data []byte) (*Delivery, error) {
	return &Delivery{Data: data, ContentType: MimeType(name)}, nil
}

// previewKey is the cached preview of content sum rendered for owner: the PDF
// is produced in the owner's own sandbox (which the owner and the agent
// control), so it is only ever served back to that owner (C-03). Preview keys
// are queued for the blob GC and refreshed on every hit (an idle cache).
func previewKey(owner, sum string) string {
	return "preview/" + sandbox.UserTag(owner) + "/" + sum + ".pdf"
}

// legacyPreviewKey is the global preview cache key of earlier versions
// (deleted with its content blob).
func legacyPreviewKey(sum string) string { return "preview/" + sum + ".pdf" }

// Preview returns the preview of a version: PDF (Office files converted in the
// sandbox, cached per source sha256), images
// as-is and text/html as text/plain.
func (s *Service) Preview(ctx context.Context, a *store.Artifact, version int) (*Delivery, error) {
	kind := PreviewableAs(a.Name, a.MimeType)
	if kind == "none" {
		return nil, ErrNotPreviewable
	}
	data, v, err := s.Content(ctx, a, version)
	if err != nil {
		return nil, err
	}
	if kind != "pdf" && isDocument(a.Name, MimeType(a.Name), data) {
		// PDF/Office bytes behind a text or image extension: never served raw
		// (the version may predate the signature-aware content type).
		return nil, ErrNotPreviewable
	}
	switch kind {
	case "image":
		return &Delivery{Data: data, ContentType: MimeType(a.Name)}, nil
	case "text", "html":
		return &Delivery{Data: data, ContentType: "text/plain; charset=utf-8"}, nil
	}
	pdf := data
	if isOffice(a.Name) {
		if pdf, err = s.officePDF(ctx, a, v.SHA256, data); err != nil {
			return nil, err
		}
	}
	return s.Deliver(ctx, strings.TrimSuffix(a.Name, path.Ext(a.Name))+".pdf", pdf)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

const previewDir = "/workspace/.work/previews"

// officePDF converts an Office file with LibreOffice in the thread's sandbox
// (acquiring one when needed); conversions of a thread are serialized. The
// order bounds what a waiting preview holds (C-24): a process-wide soffice
// slot first (in memory), then the sandbox, and only then the per-thread
// conversion lock (an advisory-lock connection), so at most
// PreviewConcurrency previews per process hold one.
func (s *Service) officePDF(ctx context.Context, a *store.Artifact, sum string, data []byte) ([]byte, error) {
	key := previewKey(a.UserLogin, sum)
	if pdf, ok := s.cachedPreview(ctx, key); ok {
		return pdf, nil
	}
	if s.Sandbox == nil || !s.Sandbox.Enabled() {
		return nil, ErrSandboxDisabled
	}
	release, err := s.conversionSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	var pdf []byte
	err = sandbox.Retry(ctx, s.Sandbox, a.ThreadID, func(ctx context.Context) (sandbox.Sandbox, error) {
		// Quiet: a restore notice of a sandbox created here goes to the agent's next call (C-18).
		return s.Sandbox.Acquire(sandbox.Quiet(ctx), a.ThreadID, a.UserLogin)
	}, func(sb sandbox.Sandbox) error {
		// One conversion per thread sandbox at a time, across api replicas.
		unlock, err := s.lockAll(ctx, "work:convert:"+a.ThreadID, s.Opts.PreviewTimeout+30*time.Second)
		if err != nil {
			slog.Warn("artifacts: conversion lock unavailable", "artifact_id", a.ID, "error", err)
			return ErrPreviewFailed
		}
		defer unlock()
		if cached, ok := s.cachedPreview(ctx, key); ok {
			pdf = cached
			return nil
		}
		pdf, err = s.convertIn(ctx, sb, a, sum, data)
		if err != nil {
			return err
		}
		if err := s.Store.QueueBlob(ctx, key); err != nil {
			slog.Warn("artifacts: queueing preview for GC failed; not cached", "artifact_id", a.ID, "error", err)
		} else if err := s.Blobs.Put(ctx, key, pdf); err != nil {
			slog.Warn("artifacts: caching preview failed", "artifact_id", a.ID, "error", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pdf, nil
}

// cachedPreview returns a cached preview, refreshing its GC reference first
// (a collection in flight finishes first; the read then misses).
func (s *Service) cachedPreview(ctx context.Context, key string) ([]byte, bool) {
	if err := s.Store.TouchBlob(ctx, key); err != nil {
		return nil, false
	}
	pdf, err := s.Blobs.Get(ctx, key)
	return pdf, err == nil
}

// convertIn runs LibreOffice on one file in sb. A sandbox found gone keeps its
// cause in the error (ErrPreviewFailed wrapping it), so the caller can retry.
func (s *Service) convertIn(ctx context.Context, sb sandbox.Sandbox, a *store.Artifact, sum string, data []byte) ([]byte, error) {
	src := previewDir + "/" + sum + strings.ToLower(path.Ext(a.Name))
	out := previewDir + "/" + sum + ".pdf"
	if err := sb.WriteFile(ctx, src, data); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPreviewFailed, err)
	}
	cctx, cancel := context.WithTimeout(ctx, s.Opts.PreviewTimeout+10*time.Second)
	defer cancel()
	var stderr strings.Builder
	// One LibreOffice profile per conversion: parallel conversions never share one (review round 1, #7).
	profile := "/tmp/work-preview-profile-" + uuid.NewString()
	defer func() {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer rcancel()
		_, _ = sb.Exec(rctx, sandbox.ExecRequest{Command: "rm -rf " + shellQuote(profile), Cwd: sandbox.Workspace, Timeout: 10 * time.Second})
	}()
	res, err := sb.Exec(cctx, sandbox.ExecRequest{
		Command: "soffice --headless --norestore -env:UserInstallation=file://" + profile + " --convert-to pdf --outdir " +
			shellQuote(previewDir) + " " + shellQuote(src),
		Cwd: sandbox.Workspace, Timeout: s.Opts.PreviewTimeout,
		OnStderr: func(t string) {
			if stderr.Len() < 4096 {
				stderr.WriteString(t)
			}
		},
	})
	if err != nil || res.ExitCode != 0 {
		slog.Warn("artifacts: office conversion failed", "artifact_id", a.ID, "exit_code", res.ExitCode, "error", err, "stderr", stderr.String())
		if sandbox.IsGone(err) {
			return nil, fmt.Errorf("%w: %w", ErrPreviewFailed, err)
		}
		return nil, ErrPreviewFailed
	}
	pdf, size, err := sb.ReadFile(ctx, out, s.Opts.MaxFileBytes)
	if sandbox.IsGone(err) {
		return nil, fmt.Errorf("%w: %w", ErrPreviewFailed, err)
	}
	if err != nil || size > int64(len(pdf)) {
		return nil, ErrPreviewFailed
	}
	_, _ = sb.Exec(context.WithoutCancel(ctx), sandbox.ExecRequest{Command: "rm -f " + shellQuote(src) + " " + shellQuote(out), Cwd: sandbox.Workspace, Timeout: 10 * time.Second})
	return pdf, nil
}

// RestoreVersion writes a version back into the sandbox (creating it if
// needed) and records it as a new version.
func (s *Service) RestoreVersion(ctx context.Context, a *store.Artifact, version int) (*store.Artifact, error) {
	data, v, err := s.Content(ctx, a, version)
	if err != nil {
		return nil, err
	}
	if s.Sandbox == nil || !s.Sandbox.Enabled() {
		return nil, ErrSandboxDisabled
	}
	sb, err := s.Sandbox.Acquire(sandbox.Quiet(ctx), a.ThreadID, a.UserLogin)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lockThread(ctx, a.ThreadID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := sb.WriteFile(ctx, a.Path, data); err != nil {
		if sandbox.IsGone(err) {
			// Not retried under the thread lock (a replacement's restore takes it): the next request gets a new sandbox.
			s.Sandbox.Invalidate(ctx, a.ThreadID, sb.ID())
		}
		return nil, err
	}
	got, _, err := s.put(ctx, store.PutArtifact{ThreadID: a.ThreadID, UserLogin: a.UserLogin, Path: a.Path, Name: a.Name,
		MimeType: ContentMime(a.Name, data), Kind: a.Kind, SizeBytes: v.SizeBytes, SHA256: v.SHA256, Force: true, SandboxID: sb.ID()})
	return got, err
}

// FileEntry is a v1 files-API entry built from artifacts.
type FileEntry struct {
	Name, Path, Type string
	Size             int64
	ModifiedAt       time.Time
}

// Tree lists the direct children of dir from the thread's non-deleted artifacts.
func (s *Service) Tree(ctx context.Context, threadID, dir string) ([]FileEntry, error) {
	all, err := s.Store.ListArtifacts(ctx, threadID)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(dir, "/") + "/"
	dirs := map[string]*FileEntry{}
	var out []FileEntry
	for _, a := range all {
		if a.Deleted || !strings.HasPrefix(a.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(a.Path, prefix)
		if first, _, nested := strings.Cut(rest, "/"); nested {
			d := dirs[first]
			if d == nil {
				d = &FileEntry{Name: first, Path: prefix + first, Type: "directory", ModifiedAt: a.UpdatedAt}
				dirs[first] = d
			}
			if a.UpdatedAt.After(d.ModifiedAt) {
				d.ModifiedAt = a.UpdatedAt
			}
			continue
		}
		out = append(out, FileEntry{Name: a.Name, Path: a.Path, Type: "file", Size: a.SizeBytes, ModifiedAt: a.UpdatedAt})
	}
	for _, d := range dirs {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type == "directory"
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// blobCollector is a blob backend living in the metadata database, which
// collects queued blobs in one statement (PGBlobs).
type blobCollector interface {
	CollectUnreferenced(ctx context.Context, keys []string, grace time.Duration) ([]string, error)
}

// collect deletes the candidates still collectable, returning how many went.
func (s *Service) collect(ctx context.Context, keys []string, grace time.Duration) (int, error) {
	if c, ok := s.Blobs.(blobCollector); ok {
		done, err := c.CollectUnreferenced(ctx, keys, grace)
		if err != nil {
			return 0, fmt.Errorf("collecting unreferenced blobs: %w", err)
		}
		return len(done), nil
	}
	n := 0
	for _, k := range keys {
		ok, err := s.Store.CollectBlob(ctx, k, grace, func(ctx context.Context) error {
			if err := s.Blobs.Delete(ctx, k); err != nil {
				return err
			}
			if err := s.Blobs.Delete(ctx, legacyPreviewKey(k)); err != nil {
				slog.Warn("artifacts: deleting preview blob failed", "key", k, "error", err)
			}
			return nil
		})
		if err != nil {
			slog.Warn("artifacts: deleting blob failed", "key", k, "error", err)
			continue
		}
		if ok {
			n++
		}
	}
	return n, nil
}

const (
	purgeThreadBatch = 100
	purgeBlobBatch   = 500
)

// Purge hard-deletes the threads soft-deleted more than olderThan ago (with
// their runs, messages, events, artifacts and versions), then deletes from the
// blob backend, with its cached preview, every queued content blob that no
// artifact version references and that had no reference for Opts.BlobGCGrace.
// The deletion is conditional on both still holding: in the same statement as
// the queue row for the database backend, after the queue row is removed (and
// before that removal commits) for an external backend.
func (s *Service) Purge(ctx context.Context, olderThan time.Duration) (threads, blobs int, err error) {
	for {
		n, err := s.Store.PurgeDeletedThreads(ctx, olderThan, purgeThreadBatch)
		threads += n
		if err != nil {
			return threads, blobs, fmt.Errorf("purging deleted threads: %w", err)
		}
		if n < purgeThreadBatch {
			break
		}
	}
	grace := s.Opts.BlobGCGrace
	for {
		keys, err := s.Store.BlobCandidates(ctx, grace, purgeBlobBatch)
		if err != nil {
			return threads, blobs, fmt.Errorf("listing unreferenced blobs: %w", err)
		}
		n, err := s.collect(ctx, keys, grace)
		blobs += n
		if err != nil {
			return threads, blobs, err
		}
		if n == 0 || len(keys) < purgeBlobBatch {
			break
		}
	}
	if threads > 0 || blobs > 0 {
		slog.Info("artifacts: retention purge", "threads", threads, "blobs", blobs)
	}
	return threads, blobs, nil
}
