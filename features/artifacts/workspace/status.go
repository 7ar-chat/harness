package workspace

import (
	"context"
	"errors"
	"time"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/store"
)

// Availability values of the workspace status (Work's vocabulary).
const (
	AvailabilityReady    = "ready"
	AvailabilityStopped  = "stopped"
	AvailabilityMissing  = "missing"
	AvailabilityStarting = "starting"
)

// RestoreBlockedReason is Work's message while a run is active.
const RestoreBlockedReason = "Workspace restore is unavailable while the conversation is still preparing or running."

// Status is the workspace status of a thread (GET …/workspace/status).
type Status struct {
	Availability string `json:"availability"`
	Status       string `json:"status"`
	Ready        bool   `json:"ready"`
	CanRestore   bool   `json:"canRestore"`
	// RestoreBlockedReason is set while a run of the thread is queued or running.
	RestoreBlockedReason *string `json:"restoreBlockedReason"`
	RootPath             string  `json:"rootPath"`
	// HasHistory reports an archived checkpoint history (restored with the files).
	HasHistory bool `json:"hasHistory"`
	// HistoryNotSaved is set when the latest checkpoint history could not be
	// archived (a new sandbox would get an older history, or none).
	HistoryNotSaved *HistoryIssue `json:"historyNotSaved"`
}

// HistoryIssue explains an unsaved checkpoint history.
type HistoryIssue struct {
	// Reason is too_large (over HARNESS_GIT_ARCHIVE_MAX_BYTES) or failed.
	Reason    string `json:"reason"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	At        string `json:"at"`
}

// saved reports whether the thread has files or history to restore.
func (s *Service) saved(ctx context.Context, threadID string) (files, history bool, err error) {
	if s.Artifacts != nil {
		list, err := s.Artifacts.Store.ListArtifacts(ctx, threadID)
		if err != nil {
			return false, false, err
		}
		for _, a := range list {
			if !a.Deleted {
				files = true
				break
			}
		}
	}
	if s.Store != nil && !s.Opts.Disabled {
		_, err := s.Store.WorkspaceGit(ctx, threadID)
		switch {
		case err == nil:
			history = true
		case !errors.Is(err, store.ErrNotFound):
			return false, false, err
		}
	}
	return files, history, nil
}

// Status computes the workspace status: ready with a live sandbox, stopped
// when it expired but files or history are saved, missing otherwise,
// starting while the sandbox is transiently unavailable.
func (s *Service) Status(ctx context.Context, t *store.Thread) (*Status, error) {
	st := &Status{RootPath: sandbox.Workspace}
	files, history, err := s.saved(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	st.HasHistory = history
	if s.Store != nil && !s.Opts.Disabled {
		i, err := s.Store.WorkspaceGitIssue(ctx, t.ID)
		switch {
		case err == nil:
			st.HistoryNotSaved = &HistoryIssue{Reason: i.Reason, SizeBytes: i.SizeBytes, At: i.UpdatedAt.UTC().Format(time.RFC3339)}
		case !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
	}
	enabled := s.Sandbox != nil && s.Sandbox.Enabled()
	switch {
	case !enabled:
		st.Availability = AvailabilityMissing
	default:
		_, err := s.Sandbox.Get(ctx, t.ID)
		switch {
		case err == nil:
			st.Availability = AvailabilityReady
		case errors.Is(err, sandbox.ErrNoSandbox):
			if files || history {
				st.Availability = AvailabilityStopped
			} else {
				st.Availability = AvailabilityMissing
			}
		case errors.Is(err, sandbox.ErrUnavailable):
			st.Availability = AvailabilityStarting
		default:
			return nil, err
		}
	}
	st.Status, st.Ready = st.Availability, st.Availability == AvailabilityReady
	if t.ActiveRun != nil && !store.IsTerminal(t.ActiveRun.Status) {
		reason := RestoreBlockedReason
		st.RestoreBlockedReason = &reason
	}
	st.CanRestore = enabled && (files || history) && st.RestoreBlockedReason == nil
	return st, nil
}

// ErrRestoreBlocked means a run of the thread is active (409).
var ErrRestoreBlocked = errors.New(RestoreBlockedReason)

// RestoreDiagnostic explains an explicit restore (Work's restore_diagnostic).
type RestoreDiagnostic struct {
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// RestoreOutcome is the answer of POST …/workspace/restore.
type RestoreOutcome struct {
	*Status
	Restored          bool              `json:"restored"`
	RestoreDiagnostic RestoreDiagnostic `json:"restoreDiagnostic"`
}

// Restore recreates the thread's sandbox when it is not running: the new
// sandbox gets the archived history and the saved files (the OnCreate restore).
func (s *Service) Restore(ctx context.Context, t *store.Thread) (*RestoreOutcome, error) {
	st, err := s.Status(ctx, t)
	if err != nil {
		return nil, err
	}
	if st.RestoreBlockedReason != nil {
		return nil, ErrRestoreBlocked
	}
	if st.Ready {
		return &RestoreOutcome{Status: st, RestoreDiagnostic: RestoreDiagnostic{Status: "skipped", Reason: "already_ready",
			Message: "Workspace is already ready."}}, nil
	}
	if s.Sandbox == nil || !s.Sandbox.Enabled() {
		return nil, sandbox.ErrDisabled
	}
	if _, err := s.Sandbox.Acquire(sandbox.Quiet(ctx), t.ID, t.UserLogin); err != nil { // the notice goes to the agent (C-18)
		return nil, err
	}
	after, err := s.Status(ctx, t)
	if err != nil {
		return nil, err
	}
	return &RestoreOutcome{Status: after, Restored: true, RestoreDiagnostic: RestoreDiagnostic{Status: "restored", Reason: "restored",
		Message: "Workspace restored."}}, nil
}
