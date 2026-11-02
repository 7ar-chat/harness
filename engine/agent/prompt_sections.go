package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"github.com/7ar-chat/harness/prompts"
)

// PromptSection adds a section to a run's system prompt ("" adds nothing).
// Sections are appended in order after the base prompt; in.Variant tells
// which prompt variant they render for (see prompts).
type PromptSection func(ctx context.Context, in Input) string

type taintKey struct{}

// ReportUntrusted is called by a prompt section that put untrusted text (an
// attached user or global skill) into the system prompt: the run's context
// is then untrusted from its start, so memory writes are only proposed (A-11).
func ReportUntrusted(ctx context.Context) {
	if f, ok := ctx.Value(taintKey{}).(*atomic.Bool); ok {
		f.Store(true)
	}
}

// Part keeps a section only in the variants that include the optional part p.
func Part(p prompts.Part, s PromptSection) PromptSection {
	return func(ctx context.Context, in Input) string {
		if !prompts.Has(in.Variant, p) {
			return ""
		}
		return s(ctx, in)
	}
}

func (a *Agent) render(ctx context.Context, in Input, list []PromptSection) []string {
	var out []string
	for _, s := range list {
		if s == nil {
			continue
		}
		if text := strings.TrimSpace(s(ctx, in)); text != "" {
			out = append(out, text)
		}
	}
	return out
}

// variantOf is the prompt variant of a model (Harness.PromptVariant, else prompts.Default).
func (a *Agent) variantOf(model string) string {
	if f := a.Harness.PromptVariant; f != nil {
		return prompts.Resolve(f(model))
	}
	return prompts.Default
}

// buildPrompt renders the run's system prompt (static sections, then the
// dynamic context sections) and marks the context untrusted when a section
// injected untrusted text.
func (st *runState) buildPrompt(ctx context.Context, now time.Time) {
	st.in.Variant = st.a.variantOf(st.in.Run.Model)
	tainted := &atomic.Bool{}
	sctx := context.WithValue(ctx, taintKey{}, tainted)
	vision := st.a.Harness.Vision != nil && st.a.Harness.Vision(st.in.Run.Model)
	st.h.prompt = prompts.Prompt{Variant: st.in.Variant, Now: now, FirstName: st.in.FirstName, Sandbox: st.in.Sandbox, Vision: vision,
		Sections: st.a.render(sctx, st.in, append(append([]PromptSection(nil), st.a.Harness.Sections...), func(_ context.Context, in Input) string { return planModeSection(in) })), Context: st.a.render(sctx, st.in, st.a.Harness.Context)}
	st.h.systemBase = st.h.prompt.Render("")
	if tail := st.a.render(sctx, st.in, st.a.Harness.Tail); len(tail) > 0 {
		st.h.tail = "<system-reminder>\n" + strings.Join(tail, "\n") + "\n</system-reminder>"
	}
	if tainted.Load() {
		st.h.untrusted = true
	}
}
