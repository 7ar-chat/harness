package factorystore

import (
	"fmt"
	"slices"
	"time"

	"github.com/7ar-chat/harness/core/factory"
)

// checkPatch rejects a patch no task could take, before anything is read or locked.
func checkPatch(p factory.Patch) error {
	if p.State != nil && !p.State.Valid() {
		return fmt.Errorf("%w: state %q", factory.ErrInvalid, *p.State)
	}
	return nil
}

// applyPatch applies p to t. Mem and PG share it so that both stores change a
// task in exactly the same way; the callers check IfState and dependencies.
func applyPatch(t *factory.Task, p factory.Patch, now time.Time) {
	apply := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	if p.State != nil {
		t.State = *p.State
		if t.State.Terminal() {
			t.FinishedAt = now
		} else {
			t.FinishedAt = time.Time{}
		}
	}
	apply(&t.BlockedReason, p.BlockedReason)
	apply(&t.Error, p.Error)
	apply(&t.Result, p.Result)
	apply(&t.Title, p.Title)
	apply(&t.Spec, p.Spec)
	apply(&t.ThreadID, p.ThreadID)
	apply(&t.RunID, p.RunID)
	apply(&t.Model, p.Model)
	apply(&t.Branch, p.Branch)
	apply(&t.Policy, p.Policy)
	apply(&t.Feedback, p.Feedback)
	if p.PR != nil {
		t.PR = *p.PR
	}
	if p.Attempts != nil {
		t.Attempts = *p.Attempts
	}
	if p.MaxAttempts != nil {
		t.MaxAttempts = *p.MaxAttempts
	}
	if p.AutoMerge != nil {
		t.AutoMerge = *p.AutoMerge
	}
	if p.Budget != nil {
		t.Budget = *p.Budget
	}
	if p.NotBefore != nil {
		t.NotBefore = *p.NotBefore
	}
	if p.DependsOn != nil {
		t.DependsOn = slices.Clone(*p.DependsOn)
	}
	if p.Labels != nil {
		t.Labels = slices.Clone(*p.Labels)
	}
	if p.AddFingerprint != "" {
		t.Fingerprints = append(slices.Clone(t.Fingerprints), p.AddFingerprint)
		if n := len(t.Fingerprints); n > maxFingerprints {
			t.Fingerprints = slices.Clone(t.Fingerprints[n-maxFingerprints:])
		}
	}
	t.UpdatedAt = now
}
