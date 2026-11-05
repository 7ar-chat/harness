package learning

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/runtime"
)

// Utility is the track record of a memory entry.
type Utility struct{ Uses, Wins, Losses int }

// UtilityOf reads the counters the Learner keeps in the entry's metadata.
func UtilityOf(e *store.MemoryEntry) Utility {
	return Utility{Uses: num(e.Metadata[MetaUses]), Wins: num(e.Metadata[MetaWins]), Losses: num(e.Metadata[MetaLosses])}
}

// Wilson is the Wilson score interval of wins out of n at z standard deviations (1.64 ≈ 90 % one-sided).
func Wilson(wins, n int, z float64) (lo, hi float64) {
	if n == 0 {
		return 0, 1
	}
	p, nf := float64(wins)/float64(n), float64(n)
	d := 1 + z*z/nf
	c := p + z*z/(2*nf)
	m := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf))
	return max(0, (c-m)/d), min(1, (c+m)/d)
}

// CuratorOptions configure a Curator.
type CuratorOptions struct {
	Memory store.MemoryStore
	// Baseline is the success rate of the tasks of a memory key and how many there are. An entry is judged
	// against it, not against 100 %, because most tasks that recall an entry would have failed or passed anyway.
	Baseline func(ctx context.Context, key string) (rate float64, n int, err error)
	// MinUses is the finished tasks an entry needs before it is judged (default 6).
	MinUses int
	// Margin is how far from the baseline the confidence interval must be to act (default 0.1).
	Margin float64
	Now    func() time.Time
}

// Report is what one curation did.
type Report struct {
	Judged   int
	Promoted []string
	Demoted  []string
	Retired  []string
}

// Curator judges the automatically written memory entries by their track record: an entry whose confidence
// interval lies clearly below the baseline loses importance and, when clearly harmful, is retired (soft-deleted, so
// the user can restore it); one clearly above is promoted. Entries the user wrote or pinned are never touched.
type Curator struct{ o CuratorOptions }

// NewCurator returns a Curator.
func NewCurator(o CuratorOptions) *Curator {
	if o.MinUses <= 0 {
		o.MinUses = 6
	}
	if o.Margin <= 0 {
		o.Margin = 0.1
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Curator{o: o}
}

// Job is the periodic curation for the runtime's job runner.
func (c *Curator) Job() runtime.Job {
	return runtime.Job{Name: "factory-learning-curate", Every: 6 * time.Hour, Run: c.Run}
}

// Run is Job's function.
func (c *Curator) Run(ctx context.Context) error {
	r, err := c.Curate(ctx)
	if len(r.Promoted)+len(r.Demoted)+len(r.Retired) > 0 {
		slog.Info("learning: curated memory", "judged", r.Judged, "promoted", len(r.Promoted), "demoted", len(r.Demoted), "retired", len(r.Retired))
	}
	return err
}

// Curate judges every user's learnable entries once.
func (c *Curator) Curate(ctx context.Context) (Report, error) {
	var rep Report
	if c.o.Memory == nil || c.o.Baseline == nil {
		return rep, nil
	}
	users, err := c.o.Memory.ListMemoryUsers(ctx)
	if err != nil {
		return rep, fmt.Errorf("learning: listing memory users: %w", err)
	}
	var errs []error
	for _, key := range users {
		if err := c.curateKey(ctx, key, &rep); err != nil {
			errs = append(errs, err)
		}
	}
	return rep, errors.Join(errs...)
}

// learnable: written by the lessons feature or the end-of-run extraction, not by the user, not pinned.
func learnable(e *store.MemoryEntry) bool {
	if e.Source == store.MemorySourceUser || e.Status != store.MemoryActive {
		return false
	}
	if v, _ := e.Metadata["pinned"].(bool); v {
		return false
	}
	if s, _ := e.Metadata["source"].(string); s == "factory-lessons" {
		return true
	}
	auto, _ := e.Metadata["auto"].(bool)
	return auto
}

func (c *Curator) curateKey(ctx context.Context, key string, rep *Report) error {
	es, err := c.o.Memory.ListMemoryEntries(ctx, key, store.MemoryActive)
	if err != nil {
		return fmt.Errorf("learning: listing memory of %q: %w", key, err)
	}
	base, n, baseErr := 0.0, -1, error(nil)
	for _, e := range es {
		if !learnable(e) {
			continue
		}
		u := UtilityOf(e)
		if u.Wins+u.Losses < c.o.MinUses || num(e.Metadata["curated_uses"]) == u.Uses {
			continue // too little evidence, or nothing new since the last judgement
		}
		if n < 0 {
			if base, n, baseErr = c.o.Baseline(ctx, key); baseErr != nil {
				return fmt.Errorf("learning: baseline of %q: %w", key, baseErr)
			}
		}
		if n < c.o.MinUses {
			return nil // no baseline to compare with yet
		}
		rep.Judged++
		lo, hi := Wilson(u.Wins, u.Wins+u.Losses, 1.64)
		switch {
		case hi < base-c.o.Margin:
			retire := hi < base-2*c.o.Margin
			if err := c.act(ctx, e, retire, false, base, lo, hi, rep); err != nil {
				slog.Warn("learning: demoting an entry failed", "entry", e.ID, "error", err)
			}
		case lo > base+c.o.Margin:
			if err := c.act(ctx, e, false, true, base, lo, hi, rep); err != nil {
				slog.Warn("learning: promoting an entry failed", "entry", e.ID, "error", err)
			}
		}
	}
	return nil
}

func (c *Curator) act(ctx context.Context, e *store.MemoryEntry, retire, promote bool, base, lo, hi float64, rep *Report) error {
	cp := e.Clone()
	if cp.Metadata == nil {
		cp.Metadata = map[string]any{}
	}
	u := UtilityOf(e)
	cp.Metadata["curated_uses"] = u.Uses
	imp := 0.5
	if cp.Importance != nil {
		imp = *cp.Importance
	}
	note := fmt.Sprintf("%d wins/%d losses (interval %.0f–%.0f%%) vs baseline %.0f%%", u.Wins, u.Losses, 100*lo, 100*hi, 100*base)
	now := c.o.Now().UTC()
	switch {
	case retire:
		cp.Status = store.MemoryDeleted
		cp.DeletedAt = &now
		cp.Metadata["retired_reason"] = "learning: " + note
	case promote:
		imp = min(1, imp+0.1)
		cp.Importance = &imp
		cp.Metadata["proven"] = true
		if !hasTag(cp.Tags, "proven") {
			cp.Tags = append(cp.Tags, "proven")
		}
	default:
		imp = max(0.1, imp-0.2)
		cp.Importance = &imp
		cp.Metadata["demoted_reason"] = "learning: " + note
	}
	_, err := c.o.Memory.UpdateMemoryEntry(ctx, cp, store.MemoryLimits{})
	if errors.Is(err, store.ErrMemoryConflict) {
		return nil // changed meanwhile: judged again next time
	}
	if err != nil {
		return err
	}
	switch {
	case retire:
		rep.Retired = append(rep.Retired, e.ID)
	case promote:
		rep.Promoted = append(rep.Promoted, e.ID)
	default:
		rep.Demoted = append(rep.Demoted, e.ID)
	}
	return nil
}

func hasTag(tags []string, t string) bool {
	for _, x := range tags {
		if strings.EqualFold(x, t) {
			return true
		}
	}
	return false
}

// BaselineFromStore computes a Baseline from the dataset of the last window (all time when zero).
func BaselineFromStore(st factory.Store, window time.Duration) func(ctx context.Context, key string) (float64, int, error) {
	return func(ctx context.Context, key string) (float64, int, error) {
		rows, err := Collect(ctx, st, factory.Filter{})
		if err != nil {
			return 0, 0, err
		}
		cut := time.Time{}
		if window > 0 {
			cut = time.Now().Add(-window)
		}
		rate, n := Rate(rows, func(r Row) bool { return r.Key == key && r.Source != factory.SourceDelegate && r.CreatedAt.After(cut) })
		return rate, n, nil
	}
}
