// Package titles gives threads a short model-written title after their first
// completed run (harness v2 §5), unless the user set one.
package titles

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/engine/llm"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
	"github.com/GhaziBenDahmane/harness/store"
)

// MaxChars bounds a generated title.
const MaxChars = 60

const (
	timeout           = 20 * time.Second
	defaultInlineWait = 3 * time.Second
	questionChars     = 2000
	answerChars       = 1000
)

// Instructions is the system prompt of the title call.
const Instructions = `You name conversations. Write a short, specific title (3 to 7 words, at most 60 characters) for the conversation below, in the language of the user's message.
No quotes, no trailing period, no emoji, no "Title:" prefix. Output only the title.`

// Store is the persistence used by the hook.
type Store interface {
	GetThread(ctx context.Context, login, id string) (*store.Thread, error)
	ListMessages(ctx context.Context, threadID string) ([]*store.Message, error)
	SetGeneratedTitle(ctx context.Context, threadID, title string) (bool, error)
}

// Hook generates the title (a runtime.Hook).
type Hook struct {
	Store  Store
	Models llm.Factory
	// InlineWait is how long the run waits for the title (default 3 s).
	InlineWait time.Duration
	// SupportsReasoning reports models that accept reasoningEffort: the title
	// call then runs at low effort (A-25).
	SupportsReasoning func(model string) bool
}

type generated struct {
	title string
	usage agui.Usage
}

var _ runtime.Hook = (*Hook)(nil)

// Finishing implements runtime.Hook: titles the thread and emits work.title.
func (h *Hook) Finishing(ctx context.Context, f *runtime.Finish) func(context.Context) {
	if f.Status != store.StatusCompleted || f.Run.Kind == store.KindCompact {
		return nil
	}
	t, err := h.Store.GetThread(ctx, f.Run.UserLogin, f.Run.ThreadID)
	if err != nil || t.TitleSource != store.TitleAuto {
		return nil
	}
	msgs, err := h.Store.ListMessages(ctx, t.ID)
	if err != nil {
		return nil
	}
	question, answer := "", ""
	for _, m := range msgs {
		switch {
		case m.Role == store.RoleUser && question == "" && strings.TrimSpace(m.Content) != "":
			question = m.Content
		case m.Role == store.RoleAssistant && strings.TrimSpace(m.Content) != "":
			answer = m.Content
		}
	}
	if question == "" {
		return nil
	}
	// The run waits at most InlineWait for the title (then emits work.title);
	// a slower title is saved when it arrives, after the run (no event: the
	// thread list shows it on its next refresh).
	done := make(chan generated, 1)
	gctx := context.WithoutCancel(ctx)
	go func() {
		g, err := h.generate(gctx, f.Run.Model, question, answer)
		if err != nil {
			slog.Warn("titles: generating the title failed", "run_id", f.Run.ID, "error", err)
		}
		done <- g
	}()
	wait := h.InlineWait
	if wait <= 0 {
		wait = defaultInlineWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case g := <-done:
		f.AddUsage(g.usage) // counted in RUN_FINISHED.usage (a title arriving later is not)
		if h.save(gctx, f.Run.ID, t.ID, g.title) {
			_ = f.Emit(ctx, agui.CustomEvent(agui.CustomTitle, map[string]any{"title": g.title}))
		}
	case <-timer.C:
		go func() { h.save(gctx, f.Run.ID, t.ID, (<-done).title) }()
	}
	return nil
}

// save stores a generated title unless the title is no longer auto-derived.
func (h *Hook) save(ctx context.Context, runID, threadID, title string) bool {
	if title == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ok, err := h.Store.SetGeneratedTitle(ctx, threadID, title)
	if err != nil {
		slog.Warn("titles: saving the title failed", "run_id", runID, "error", err)
	}
	return ok
}

func (h *Hook) generate(ctx context.Context, modelName, question, answer string) (generated, error) {
	m, err := h.Models.Model(ctx, modelName)
	if err != nil {
		return generated{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	prompt := "User:\n" + clip(question, questionChars)
	if answer != "" {
		prompt += "\n\nAssistant:\n" + clip(answer, answerChars)
	}
	var opts []model.Option
	if h.SupportsReasoning != nil && h.SupportsReasoning(modelName) {
		opts = llm.WithReasoningEffort("low")
	}
	out, err := m.Generate(ctx, []*schema.Message{schema.SystemMessage(Instructions), schema.UserMessage(prompt)}, opts...)
	if err != nil {
		return generated{}, err
	}
	return generated{title: Clean(out.Content), usage: agent.UsageOf(modelName, out)}, nil
}

// Clean turns a model answer into a title: first line, no quotes or prefix, ≤ MaxChars.
func Clean(s string) string {
	line := ""
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			line = l
			break
		}
	}
	line = strings.TrimSpace(line)
	for _, p := range []string{"Title:", "title:", "Titre :", "Titre:"} {
		line = strings.TrimSpace(strings.TrimPrefix(line, p))
	}
	for prev := ""; prev != line; {
		prev = line
		line = strings.TrimSuffix(strings.Trim(line, "\"'`*#«»“”‘’ "), ".")
	}
	line = strings.Join(strings.Fields(store.CleanText(line)), " ")
	if utf8.RuneCountInString(line) > MaxChars {
		line = strings.TrimSpace(string([]rune(line)[:MaxChars-1])) + "…"
	}
	return line
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
