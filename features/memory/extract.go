package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/engine/llm"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
	"github.com/GhaziBenDahmane/harness/store"
)

// Extraction. When a run completes, a model reads it and proposes the durable knowledge it contains:
// decisions and their reason, conventions, constraints, working commands, pitfalls, preferences. Each
// candidate is reconciled with the entries that already look like it (a new fact is added, a changed
// one updates its entry, a known one is skipped) and written as a PENDING proposal: nothing is used
// until a member confirms it in the memory page. This is the "background extraction" of mem0 and
// LangMem with the confirmation step of this harness.

const extractInstructions = `You curate the long-term memory of a software team's coding agent. Below are the memories that already exist and the latest part of a conversation. Propose the durable knowledge in the conversation that a future session should have.

Keep: decisions and why they were taken; project conventions and constraints; commands or steps that worked (or pitfalls that did not); stable facts about the codebase, tools and environment; a person's lasting preferences.
Drop: progress on the current task, anything that only matters today, code or logs, guesses, anything the user did not confirm, and every secret (tokens, passwords, keys).
Write each memory as one or two short self-contained sentences that make sense without the conversation.
If a memory below already says it, return nothing for it. If the conversation changes or corrects one, return an "update" with that memory's id and its full new text.
Return at most 5 items, and an empty list when there is nothing worth keeping (the usual case for a short or routine exchange).

Answer with JSON only:
{"memories":[{"op":"add"|"update","id":"<existing id, for update>","kind":"decisions"|"facts"|"constraints"|"preferences"|"procedures"|"notes","audience":"project"|"me","title":"<=10 words","content":"...","tags":["..."],"importance":0.0-1.0,"reason":"why this is worth keeping"}]}
audience "me" is for the current user's personal preferences; everything else is "project".`

// Extractor is a runtime.Hook that proposes memories after a run.
type Extractor struct {
	Svc               *Service
	Models            llm.Factory
	SupportsReasoning func(model string) bool

	mu   sync.Mutex
	last map[string]time.Time
}

var _ runtime.Hook = (*Extractor)(nil)

const (
	extractTimeout   = 2 * time.Minute
	extractThrottle  = 30 * time.Second
	extractMaxChars  = 12000
	extractMaxOps    = 5
	extractMinChars  = 40
	extractCandidate = 8
)

// Finishing implements runtime.Hook. The work happens in the background: the run does not wait for it.
func (x *Extractor) Finishing(ctx context.Context, f *runtime.Finish) func(context.Context) {
	if x.Svc == nil || !x.Svc.o.Extract || x.Svc.Messages == nil || x.Models == nil ||
		f.Status != store.StatusCompleted || f.Run.Kind == store.KindCompact {
		return nil
	}
	x.mu.Lock()
	if x.last == nil {
		x.last = map[string]time.Time{}
	}
	if t, ok := x.last[f.Run.ThreadID]; ok && time.Since(t) < extractThrottle {
		x.mu.Unlock()
		return nil
	}
	x.last[f.Run.ThreadID] = time.Now()
	x.mu.Unlock()
	run := *f.Run
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), extractTimeout)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("memory: extraction panicked", "panic", r)
			}
		}()
		if err := x.extract(ctx, &run); err != nil {
			x.Svc.stats.extractErrors.Add(1)
			slog.Warn("memory: extraction failed", "run_id", run.ID, "error", err)
		}
	}()
	return nil
}

// extractItem is one proposed operation.
type extractItem struct {
	Op         string   `json:"op"`
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Audience   string   `json:"audience"`
	Title      string   `json:"title"`
	Content    string   `json:"content"`
	Tags       []string `json:"tags"`
	Importance *float64 `json:"importance"`
	Reason     string   `json:"reason"`
}

// transcript renders the run's exchange (from the user's last message) for the extractor.
func transcript(msgs []*store.Message) (text, userText string) {
	start := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if m := msgs[i]; m.Role == store.RoleUser && !m.IsSteer() {
			start = i
			break
		}
	}
	var b strings.Builder
	var users []string
	for _, m := range msgs[start:] {
		switch m.Role {
		case store.RoleUser:
			who := "user"
			if m.AuthorLogin != "" {
				who = "user " + m.AuthorLogin
			}
			if m.AuthorKind == store.AuthorSupervisor {
				who = "assistant on behalf of " + m.OnBehalfOf
			}
			fmt.Fprintf(&b, "%s: %s\n", who, truncateRunes(m.Content, 2000))
			users = append(users, m.Content)
		case store.RoleAssistant:
			if strings.TrimSpace(m.Content) != "" {
				fmt.Fprintf(&b, "agent: %s\n", truncateRunes(m.Content, 2000))
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "agent ran %s(%s)\n", tc.Function.Name, truncateRunes(tc.Function.Arguments, 200))
			}
		case store.RoleTool:
			fmt.Fprintf(&b, "result: %s\n", truncateRunes(strings.Join(strings.Fields(m.Content), " "), 240))
		}
	}
	text = b.String()
	if r := []rune(text); len(r) > extractMaxChars { // keep the end: that is where the outcome is
		text = "…" + string(r[len(r)-extractMaxChars:])
	}
	return text, strings.Join(users, "\n")
}

func (x *Extractor) extract(ctx context.Context, run *store.Run) error {
	svc := x.Svc
	layers := svc.Layers(ctx, run.UserLogin, run.ThreadID)
	if on, err := svc.st.MemoryEnabled(ctx, layers[0].Key); err != nil || !on {
		return err
	}
	msgs, err := svc.Messages(ctx, run.ThreadID)
	if err != nil {
		return err
	}
	text, userText := transcript(msgs)
	if len([]rune(strings.TrimSpace(userText))) < extractMinChars/2 || len([]rune(text)) < extractMinChars {
		svc.stats.extractSkipped.Add(1)
		return nil
	}
	svc.stats.extractRuns.Add(1)

	// Entries that look like what was said, for reconciliation.
	layerOf := map[string]Layer{}
	var all []*store.MemoryEntry
	existing := map[string][]*store.MemoryEntry{} // by key: active and pending, for duplicate checks
	for _, l := range layers {
		es, err := svc.st.ListMemoryEntries(ctx, l.Key, store.MemoryActive, store.MemoryPending)
		if err != nil {
			return err
		}
		existing[l.Key] = es
		for _, e := range es {
			layerOf[e.ID] = l
			if e.Status == store.MemoryActive {
				all = append(all, e)
			}
		}
	}
	var prior strings.Builder
	for _, h := range Rank(all, userText, extractCandidate, svc.now()) {
		fmt.Fprintf(&prior, "- id=%s [%s] %s: %s\n", h.Entry.ID, layerOf[h.Entry.ID].Name, h.Entry.Title, truncateRunes(strings.Join(strings.Fields(h.Entry.Content), " "), 300))
	}
	if prior.Len() == 0 {
		prior.WriteString("(none)\n")
	}

	modelName := svc.o.ExtractModel
	if modelName == "" {
		modelName = run.Model
	}
	m, err := x.Models.Model(ctx, modelName)
	if err != nil {
		return err
	}
	var opts []model.Option
	if x.SupportsReasoning != nil && x.SupportsReasoning(modelName) {
		opts = llm.WithReasoningEffort("low")
	}
	out, err := m.Generate(ctx, []*schema.Message{
		schema.SystemMessage(extractInstructions),
		schema.UserMessage("Existing memories:\n" + prior.String() + "\nConversation:\n" + text),
	}, opts...)
	if err != nil {
		return err
	}
	items := parseExtraction(out.Content)
	if len(items) > extractMaxOps {
		items = items[:extractMaxOps]
	}
	for _, it := range items {
		x.apply(ctx, run, layers, existing, it)
	}
	return nil
}

// parseExtraction reads the model's JSON, tolerating prose or fences around it.
func parseExtraction(s string) []extractItem {
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j <= i {
		return nil
	}
	var doc struct {
		Memories []extractItem `json:"memories"`
	}
	if json.Unmarshal([]byte(s[i:j+1]), &doc) != nil {
		return nil
	}
	return doc.Memories
}

func validKind(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	for _, v := range SemanticKinds {
		if v == k {
			return k
		}
	}
	return "notes"
}

// duplicate reports an entry that already says (nearly) the same.
func duplicate(es []*store.MemoryEntry, content string) bool {
	norm := NormalizeItem(content)
	words := set(Tokenize(norm))
	for _, e := range es {
		if NormalizeItem(e.Content) == norm {
			return true
		}
		other := set(Tokenize(NormalizeItem(e.Content)))
		inter := 0
		for w := range words {
			if other[w] {
				inter++
			}
		}
		if union := len(words) + len(other) - inter; union > 0 && float64(inter)/float64(union) >= 0.8 {
			return true
		}
	}
	return false
}

func (x *Extractor) apply(ctx context.Context, run *store.Run, layers []Layer, existing map[string][]*store.MemoryEntry, it extractItem) {
	svc := x.Svc
	content := strings.TrimSpace(it.Content)
	if content == "" {
		return
	}
	meta := map[string]any{"auto": true, "run_id": run.ID, "reason": truncateRunes(it.Reason, 300)}
	switch strings.ToLower(it.Op) {
	case "update":
		// The id must be an entry of this run's layers: the model's word is not enough.
		for _, l := range layers {
			for _, e := range existing[l.Key] {
				if e.ID != it.ID || e.Status != store.MemoryActive {
					continue
				}
				if duplicate([]*store.MemoryEntry{e}, content) {
					svc.stats.extractSkipped.Add(1)
					return
				}
				w := Writer{Login: l.Key, ThreadID: run.ThreadID, Source: store.MemorySourceProxy, Untrusted: true}
				in := UpdateInput{ID: e.ID, Content: &content, Metadata: meta}
				if r := svc.Update(ctx, w, in); r.IsError {
					slog.Debug("memory: extracted update refused", "id", e.ID, "error", r.Data)
				} else {
					svc.stats.extractProposed.Add(1)
				}
				return
			}
		}
	case "add", "":
		layer := layers[0]
		if strings.EqualFold(it.Audience, "me") && len(layers) > 1 {
			layer = layers[1]
		}
		if duplicate(existing[layer.Key], content) {
			svc.stats.extractSkipped.Add(1)
			return
		}
		kind := validKind(it.Kind)
		meta["kind"] = kind
		title := truncateRunes(strings.TrimSpace(it.Title), MaxTitleChars)
		in := CreateInput{Content: content, Tags: it.Tags, Metadata: meta, Importance: it.Importance,
			Path: DefaultPath(newID(), kind, svc.now())}
		if title != "" {
			in.Title = &title
		}
		w := Writer{Login: layer.Key, ThreadID: run.ThreadID, Source: store.MemorySourceProxy, Untrusted: true}
		if r := svc.Create(ctx, w, in); r.IsError {
			slog.Debug("memory: extracted memory refused", "error", r.Data)
		} else {
			svc.stats.extractProposed.Add(1)
			// remember it so a second identical item of the same answer is not added twice
			for _, ch := range r.Changes {
				if ch.Entry != nil {
					existing[layer.Key] = append(existing[layer.Key], ch.Entry)
				}
			}
		}
	}
}
