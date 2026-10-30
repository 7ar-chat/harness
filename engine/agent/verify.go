package agent

import (
	"context"

	"github.com/GhaziBenDahmane/harness/core/store"
)

// Deliverable verification (harness v3 §4): after a tool call changed files
// under /workspace/outputs/, the matching checker runs and its summary is
// appended to the tool result (CUSTOM work.verification); a run that wants to
// stop with an unverified deliverable is reminded once; answers carry
// metadata.verification.

// OutputsDir holds the deliverables.
const OutputsDir = "/workspace/outputs/"

// CustomVerification is the CUSTOM event of a checker result (and of an
// answer's verification summary).
const CustomVerification = "work.verification"

// CheckResult is one checker result.
type CheckResult struct {
	Path    string `json:"path"`
	Checker string `json:"checker"`
	OK      bool   `json:"ok"`
	// Failed: the checker could not run (exit 1, timeout, skill missing): the
	// file is unverified, not known bad.
	Failed   bool   `json:"failed,omitempty"`
	Summary  string `json:"summary"`
	NextStep string `json:"nextStep,omitempty"`
}

// Checker runs the checker of a deliverable (verify.Verifier).
type Checker interface {
	// CheckerFor names the checker of a file ("" when its type has none).
	CheckerFor(path string) string
	// Check runs it in the run's sandbox.
	Check(ctx context.Context, run *store.Run, path string) CheckResult
}

// CheckCommander is optionally implemented by a Checker: the shell command
// that checks a file (named in reminders, so the model can run it itself).
type CheckCommander interface {
	CheckCommand(path string) string
}

// CheckCompleter is optionally implemented by a Checker whose own check does
// more than the file's checker command (CheckCommand): Complete returns the
// runtime's result for p given base, the result of that command run by the
// model on p's final content (final review of round 3, F1: a deck's baseline
// and a document's page citations are still checked). A Checker without it
// takes base as its result.
type CheckCompleter interface {
	Complete(ctx context.Context, run *store.Run, path string, base CheckResult) CheckResult
}

// maxChecksPerCall bounds the checkers run after one tool call (and before
// one model call, for the files changed past that bound).
const maxChecksPerCall = 3
