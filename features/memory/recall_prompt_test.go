package memory_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/features/memory"
	"github.com/7ar-chat/harness/prompts"
)

func TestIsPinnedByMetadataOrTag(t *testing.T) {
	// Arrange
	pinnedMeta := entry("a", "p", "x", now)
	pinnedMeta.Metadata = map[string]any{"pinned": true}
	pinnedTag := entry("b", "p", "x", now, " Pinned ")
	notPinned := entry("c", "p", "x", now, "fact")
	notPinned.Metadata = map[string]any{"pinned": false}
	stringy := entry("d", "p", "x", now)
	stringy.Metadata = map[string]any{"pinned": "true"}

	// Act / Assert
	if !memory.IsPinned(pinnedMeta) || !memory.IsPinned(pinnedTag) {
		t.Fatal("pinned entries not recognized")
	}
	if memory.IsPinned(notPinned) || memory.IsPinned(stringy) {
		t.Fatal("only pinned=true (boolean) or the pinned tag pins an entry")
	}
}

func TestPinnedEntriesAreOrderedSharedFirstThenByImportanceThenRecency(t *testing.T) {
	// Pinned entries come shared layer before personal, then more important first, then most recent first.
	// Arrange
	f := newFixture(memory.Options{})
	team := memory.Writer{Login: "team", Source: store.MemorySourceProxy}
	mine := memory.Writer{Login: "me", Source: store.MemorySourceProxy}
	hi, lo := 0.9, 0.2
	pin := map[string]any{"pinned": true}
	newEntry(t, f, mine, memory.CreateInput{Title: ptr("mine-first"), Content: "personal pinned", Metadata: pin, Importance: &hi})
	newEntry(t, f, team, memory.CreateInput{Title: ptr("team-low-old"), Content: "team low old", Metadata: pin, Importance: &lo})
	newEntry(t, f, team, memory.CreateInput{Title: ptr("team-low-new"), Content: "team low new", Metadata: pin, Importance: &lo})
	newEntry(t, f, team, memory.CreateInput{Title: ptr("team-high"), Content: "team high", Metadata: pin, Importance: &hi})
	layers := []memory.Layer{{Key: "team", Name: "project"}, {Key: "me", Name: "me"}}

	// Act
	text, info := f.svc.RecallBlock(context.Background(), layers, "")

	// Assert
	order := []string{"team-high", "team-low-new", "team-low-old", "mine-first"}
	last := -1
	for _, title := range order {
		i := strings.Index(text, title)
		if i < 0 || i < last {
			t.Fatalf("%q out of order in:\n%s", title, text)
		}
		last = i
	}
	if len(info.Pinned) != 4 || info.Fallback || len(info.Relevant) != 0 {
		t.Fatalf("info = %+v", info)
	}
}

func TestPinnedEntriesAreTruncatedAndBudgeted(t *testing.T) {
	// A long pinned entry is cut with a pointer to memory_get; once the pinned budget is spent later pinned entries are dropped.
	// Arrange
	f := newFixture(memory.Options{})
	pin := map[string]any{"pinned": true}
	long := strings.Repeat("a long sentence goes here. ", 100) // > PinnedEntryMax
	for i := range 5 {
		newEntry(t, f, f.w, memory.CreateInput{Title: ptr(fmt.Sprintf("filler-%d", i)), Content: strings.Repeat(fmt.Sprintf("filler %d text ", i), 40), Metadata: pin})
	}
	// The newest of equally important pinned entries comes first.
	id := newEntry(t, f, f.w, memory.CreateInput{Title: ptr("Long"), Content: long, Metadata: pin})
	layers := []memory.Layer{{Key: "alice", Name: "user"}}

	// Act
	text, info := f.svc.RecallBlock(context.Background(), layers, "")

	// Assert
	if !strings.Contains(text, "(memory_get "+id+" for the rest)") {
		t.Fatalf("long entry not cut with a pointer:\n%s", text)
	}
	if len(info.Pinned) < 2 || len(info.Pinned) >= 6 {
		t.Fatalf("budget not applied: %d of 6 pinned entries recalled", len(info.Pinned))
	}
	if n := len([]rune(text)); n > memory.MaxRecallTotal+200 || info.Chars != n {
		t.Fatalf("block of %d chars, info says %d", n, info.Chars)
	}
}

func TestPinnedEntryWithoutATitleIsLabelledByItsPath(t *testing.T) {
	// Arrange
	st, svc, _ := newFaulty(memory.Options{})
	e := seed(st, "alice", "m1", "memories/semantic/facts/x.md", "Always run the linter")
	e.Title, e.Tags = "  ", []string{"pinned"}
	_, _ = st.UpdateMemoryEntry(context.Background(), e, store.MemoryLimits{})

	// Act
	text, _ := svc.RecallBlock(context.Background(), []memory.Layer{{Key: "alice", Name: "user"}}, "")

	// Assert
	if !strings.Contains(text, "[user] memories/semantic/facts/x.md\n  Always run the linter") {
		t.Fatalf("recall = %s", text)
	}
}

func TestRecallKeepsWorkingWhenALayerCannotBeRead(t *testing.T) {
	// A failing layer is skipped: the others are still recalled.
	// Arrange
	st, svc, _ := newFaulty(memory.Options{})
	seed(st, "team", "m1", "memories/semantic/decisions/a.md", "Releases ship from main")
	st.errList = map[string]error{"me": errBoom}

	// Act
	text, info := svc.RecallBlock(context.Background(), []memory.Layer{{Key: "team", Name: "project"}, {Key: "me", Name: "me"}}, "releases")

	// Assert
	if len(info.Relevant) != 1 || !strings.Contains(text, "Releases ship from main") {
		t.Fatalf("recall = %s %+v", text, info)
	}
}

func TestRecallOfNothingIsEmpty(t *testing.T) {
	// Arrange
	f := newFixture(memory.Options{})

	// Act
	text, info := f.svc.RecallBlock(context.Background(), []memory.Layer{{Key: "alice", Name: "user"}}, "anything at all")
	noLayers, _ := f.svc.RecallBlock(context.Background(), nil, "anything")

	// Assert
	if text != "" || noLayers != "" || info.Chars != 0 || info.Fallback {
		t.Fatalf("text %q info %+v", text, info)
	}
}

func TestRecallLimitsRelevantEntriesAndTheTotalSize(t *testing.T) {
	// At most RelevantItems entries are recalled for a question, and the block never exceeds its total budget.
	// Arrange
	f := newFixture(memory.Options{})
	for i := range 12 {
		newEntry(t, f, f.w, memory.CreateInput{Content: fmt.Sprintf("Deploy procedure number %d: %s", i, strings.Repeat("deploy details ", 12))})
	}

	// Act
	text, info := f.svc.RecallBlock(context.Background(), []memory.Layer{{Key: "alice", Name: "user"}}, "how do we deploy?")

	// Assert
	if len(info.Relevant) != memory.RelevantItems {
		t.Fatalf("relevant = %d, want %d", len(info.Relevant), memory.RelevantItems)
	}
	if n := len([]rune(text)); n > memory.MaxRecallTotal+200 {
		t.Fatalf("block of %d chars", n)
	}
	seen := map[string]bool{}
	for _, id := range info.Relevant {
		if seen[id] {
			t.Fatalf("entry %s recalled twice", id)
		}
		seen[id] = true
	}
}

func TestRecallGateLetsInflectedWordsInButNotStopwords(t *testing.T) {
	// Important entries pass the score threshold whatever the question, so the word gate decides: "deploys" matches an entry about
	// "deploy", while a question made of stop words and short words matches nothing.
	// Arrange
	f := newFixture(memory.Options{})
	top := 1.0
	newEntry(t, f, f.w, memory.CreateInput{Content: "We deploy every second Tuesday.", Importance: &top})
	newEntry(t, f, f.w, memory.CreateInput{Content: "The coffee machine is broken again.", Importance: &top})
	layers := []memory.Layer{{Key: "alice", Name: "user"}}

	// Act
	text, inflected := f.svc.RecallBlock(context.Background(), layers, "when are the deploys?")
	_, stop := f.svc.RecallBlock(context.Background(), layers, "what about the is it")

	// Assert
	if len(inflected.Relevant) != 1 || !strings.Contains(text, "deploy every") || strings.Contains(text, "coffee") {
		t.Fatalf("inflected = %+v\n%s", inflected, text)
	}
	if len(stop.Relevant) != 0 {
		t.Fatalf("a question of stop words matched: %+v", stop)
	}
}

func TestRecallFallsBackToRecentTypedEntriesOfThePrimaryMemory(t *testing.T) {
	// With no match, the fallback lists the primary layer's latest decisions/facts; another layer's entries are not part of it.
	// Arrange
	f := newFixture(memory.Options{})
	team := memory.Writer{Login: "team", Source: store.MemorySourceProxy}
	mine := memory.Writer{Login: "me", Source: store.MemorySourceProxy}
	newEntry(t, f, team, memory.CreateInput{Content: "Releases ship from main.", Path: "memories/semantic/decisions/a.md"})
	newEntry(t, f, mine, memory.CreateInput{Content: "I like tabs.", Path: "memories/semantic/preferences/b.md"})
	layers := []memory.Layer{{Key: "team", Name: "project"}, {Key: "me", Name: "me"}}

	// Act
	text, info := f.svc.RecallBlock(context.Background(), layers, "zzz qqq")

	// Assert
	if !info.Fallback || !strings.Contains(text, "Releases ship from main") || strings.Contains(text, "tabs") {
		t.Fatalf("fallback = %+v\n%s", info, text)
	}
}

func agentInput(login, thread, variant string) agent.Input {
	return agent.Input{Run: &store.Run{ID: "r1", ThreadID: thread, UserLogin: login}, Variant: variant}
}

func TestPromptSectionPicksThePolicyOfTheMemory(t *testing.T) {
	// The memory policy is part of the prompt when the variant has it and the user's memory is on; a shared memory has its own text.
	// Arrange
	ctx := context.Background()
	f := newFixture(memory.Options{})
	shared := memory.New(f.st, memory.Options{})
	shared.KeyFor = func(context.Context, string, string) string { return "project:p1" }
	shared.Shared = func(string) bool { return true }
	off := newFixture(memory.Options{})
	_ = off.st.SetMemoryEnabled(ctx, "alice", false)

	// Act
	personal := memory.PromptSection(f.svc)(ctx, agentInput("alice", "t", prompts.Default))
	project := memory.PromptSection(shared)(ctx, agentInput("alice", "t", ""))
	disabled := memory.PromptSection(off.svc)(ctx, agentInput("alice", "t", prompts.Default))
	st, svc, _ := newFaulty(memory.Options{})
	st.errEnabled = errBoom
	broken := memory.PromptSection(svc)(ctx, agentInput("alice", "t", prompts.Default))

	// Assert
	if personal != prompts.MemoryPolicy || project != prompts.ProjectMemoryPolicy {
		t.Fatalf("personal %q project %q", personal, project)
	}
	if disabled != "" || broken != "" {
		t.Fatalf("disabled %q broken %q: no policy without memory", disabled, broken)
	}
}

func TestRecallSectionInjectsTheRecallOfTheLastQuestion(t *testing.T) {
	// RecallSection is empty unless recall injection is on and the memory enabled; otherwise it recalls with the thread's last user message and counts it.
	// Arrange
	ctx := context.Background()
	f := newFixture(memory.Options{RecallInjection: true})
	f.svc.Messages = func(context.Context, string) ([]*store.Message, error) {
		return []*store.Message{
			{Role: store.RoleUser, Content: "how do the database migrations run?"},
			{Role: store.RoleAssistant, Content: "With goose."},
			{Role: store.RoleUser, Content: "steer: use postgres", Metadata: map[string]any{"steer": true}},
		}, nil
	}
	newEntry(t, f, f.w, memory.CreateInput{Content: "Database migrations are applied with goose.", Metadata: map[string]any{"pinned": true}})
	newEntry(t, f, f.w, memory.CreateInput{Content: "Database migrations run in CI before deploy."})
	off := newFixture(memory.Options{})
	disabled := newFixture(memory.Options{RecallInjection: true})
	_ = disabled.st.SetMemoryEnabled(ctx, "alice", false)

	// Act
	text := memory.RecallSection(f.svc)(ctx, agentInput("alice", "t1", ""))
	offText := memory.RecallSection(off.svc)(ctx, agentInput("alice", "t1", ""))
	disabledText := memory.RecallSection(disabled.svc)(ctx, agentInput("alice", "t1", ""))

	// Assert
	if !strings.Contains(text, "goose") || !strings.Contains(text, "run in CI") {
		t.Fatalf("recall = %s", text)
	}
	if offText != "" || disabledText != "" {
		t.Fatalf("off %q disabled %q", offText, disabledText)
	}
	if st := f.svc.Stats(); st.RecallRuns != 1 || st.RecallItems != 2 || st.RecallPinned != 1 || st.RecallChars == 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestRecallQueryIgnoresSteersAndReadErrors(t *testing.T) {
	// The query is the last real user message: steers and empty messages are skipped; a failing message source means no query (the pinned/fallback recall remains).
	// Arrange
	ctx := context.Background()
	f := newFixture(memory.Options{RecallInjection: true})
	newEntry(t, f, f.w, memory.CreateInput{Content: "Database migrations are applied with goose.", Path: "memories/semantic/facts/db.md"})
	newEntry(t, f, f.w, memory.CreateInput{Content: "Coffee machine on floor two.", Path: "memories/semantic/notes/coffee.md"})
	msgs := []*store.Message{
		{Role: store.RoleUser, Content: "database migrations?"},
		{Role: store.RoleUser, Content: "coffee machine please", Metadata: map[string]any{"steer": true}},
		{Role: store.RoleUser, Content: "   "},
	}
	f.svc.Messages = func(context.Context, string) ([]*store.Message, error) { return msgs, nil }

	// Act
	withSteer := memory.RecallSection(f.svc)(ctx, agentInput("alice", "t", ""))
	f.svc.Messages = func(context.Context, string) ([]*store.Message, error) { return nil, errors.New("down") }
	failing := memory.RecallSection(f.svc)(ctx, agentInput("alice", "t", ""))
	f.svc.Messages = nil
	none := memory.RecallSection(f.svc)(ctx, agentInput("alice", "t", ""))

	// Assert
	if !strings.Contains(withSteer, "goose") || strings.Contains(withSteer, "- [user] memories/semantic/notes/coffee.md") {
		t.Fatalf("with steer = %s", withSteer)
	}
	if !strings.Contains(failing, "goose") || !strings.Contains(failing, "Coffee") { // the fallback lists both, there is no query
		t.Fatalf("failing = %s", failing)
	}
	if failing != none {
		t.Fatalf("no query must behave the same whether the source fails or is absent:\n%s\n%s", failing, none)
	}
}

func TestRecallContextDropsItemsBeyondTheBudgetAndKeepsOrder(t *testing.T) {
	// Items are whitespace-collapsed and kept in order until the next would exceed MaxRecallChars.
	// Arrange
	items := []string{"first   item\nwith   breaks", strings.Repeat("x", memory.MaxRecallChars), "third item"}

	// Act
	block := memory.RecallContext(items)

	// Assert
	if !strings.Contains(block, "- first item with breaks") || strings.Contains(block, "xxxx") {
		t.Fatalf("block = %q", block)
	}
	if strings.Contains(block, "third item") {
		t.Fatal("items after the one that did not fit must be dropped, not reordered in")
	}
}
