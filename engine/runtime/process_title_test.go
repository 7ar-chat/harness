package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/features/titles"
	"github.com/GhaziBenDahmane/harness/store"
)

// untitled creates a thread titled automatically from its first message, like POST /threads/{id}/runs.
func (r *rig) untitled(content string) (*store.Thread, *store.Run) {
	r.t.Helper()
	th, err := r.st.CreateThread(context.Background(), "alice", "")
	if err != nil {
		r.t.Fatal(err)
	}
	return th, r.create(th, store.NewRun{Message: store.Message{Role: store.RoleUser, Content: content}, AutoTitle: content})
}

func TestProcessGeneratesATitleAfterTheFirstCompletedRun(t *testing.T) {
	// An auto-titled thread gets a cleaned model title (work.title before RUN_FINISHED); later runs keep it.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("Les ventes 2025 progressent de 12 %.")},
		llmtest.Turn{Chunks: llmtest.Text(`Title: "Analyse des ventes 2025".`)},
		llmtest.Turn{Chunks: llmtest.Text("Second answer")},
	)
	th, run := r.untitled("peux-tu analyser les ventes 2025 ?")

	// Act
	r.process(run)

	// Assert
	evs := r.events(run.ID)
	tt := types(evs)
	if n := len(tt); tt[n-2] != "CUSTOM:work.title" || find(evs, "CUSTOM:work.title")[0]["value"].(map[string]any)["title"] != "Analyse des ventes 2025" {
		t.Fatalf("types %v", tt)
	}
	got := r.thread(th.ID)
	if got.Title != "Analyse des ventes 2025" || got.TitleSource != store.TitleGenerated {
		t.Fatalf("thread %+v", got)
	}
	if call := r.model.Calls()[1]; call.Messages[0].Content != titles.Instructions {
		t.Fatalf("title call %+v", call)
	}
	second := r.create(got, store.NewRun{Message: store.Message{Role: store.RoleUser, Content: "and 2024?"}})
	r.process(second)
	if len(r.model.Calls()) != 3 || len(find(r.events(second.ID), "CUSTOM:work.title")) != 0 {
		t.Fatalf("title regenerated: %d calls", len(r.model.Calls()))
	}
}

func TestProcessNeverRetitlesUserTitles(t *testing.T) {
	// A title set by the user is never replaced.
	// Arrange
	r := newRig(t)
	th, run := r.untitled("hello")
	title := "Mine"
	_, _ = r.st.UpdateThread(context.Background(), "alice", th.ID, store.ThreadPatch{Title: &title})

	// Act
	r.process(run)

	// Assert
	if got := r.thread(th.ID); got.Title != "Mine" || len(r.model.Calls()) != 1 {
		t.Fatalf("thread %+v calls %d", got, len(r.model.Calls()))
	}
}

func TestProcessKeepsTheAutoTitleWhenTitlingFails(t *testing.T) {
	// A failing title call leaves the auto title and the run completed; the next completed run tries again.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("answer")},
		llmtest.Turn{Err: errors.New("bad request")},
	)
	th, run := r.untitled("hello there")

	// Act
	got := r.process(run)

	// Assert
	if got.Status != store.StatusCompleted || r.thread(th.ID).Title != "hello there" || r.thread(th.ID).TitleSource != store.TitleAuto ||
		len(find(r.events(run.ID), "CUSTOM:work.title")) != 0 {
		t.Fatalf("status %s thread %+v", got.Status, r.thread(th.ID))
	}
}

func TestProcessSavesASlowTitleAfterTheRunWithoutHoldingIt(t *testing.T) {
	// A title call slower than the inline wait does not keep the run open: the run finishes without work.title and the title is saved afterwards.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("Answer")},
		llmtest.Turn{Chunks: llmtest.Text("Slow generated title"), OnCall: func() { time.Sleep(300 * time.Millisecond) }},
	)
	for _, h := range r.opts.Hooks {
		if th, ok := h.(*titles.Hook); ok {
			th.InlineWait = 20 * time.Millisecond
		}
	}
	th, run := r.untitled("hello there")
	start := time.Now()

	// Act
	got := r.process(run)

	// Assert
	if elapsed := time.Since(start); got.Status != store.StatusCompleted || elapsed > 250*time.Millisecond {
		t.Fatalf("status %s after %v", got.Status, elapsed)
	}
	if n := len(find(r.events(run.ID), "CUSTOM:work.title")); n != 0 {
		t.Fatalf("%d work.title events", n)
	}
	waitFor(t, "the generated title", func() bool { return r.thread(th.ID).Title == "Slow generated title" })
}
