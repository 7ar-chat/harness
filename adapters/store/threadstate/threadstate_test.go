package threadstate_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/threadstate"
	"github.com/7ar-chat/harness/core/store"
)

func suite(t *testing.T, st store.ThreadStateStore) {
	ctx := context.Background()
	if got, err := st.GetThreadState(ctx, "t1", "k"); err != nil || got != nil {
		t.Fatalf("missing document = %q, %v", got, err)
	}
	set := func(v string) func(json.RawMessage) (json.RawMessage, error) {
		return func(json.RawMessage) (json.RawMessage, error) { return json.RawMessage(v), nil }
	}
	if err := st.UpdateThreadState(ctx, "t1", "k", set(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetThreadState(ctx, "t1", "k"); string(got) != `{"a":1}` && string(got) != `{"a": 1}` {
		t.Fatalf("document = %q", got)
	}
	// an error leaves it alone
	boom := errors.New("boom")
	if err := st.UpdateThreadState(ctx, "t1", "k", func(json.RawMessage) (json.RawMessage, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := st.GetThreadState(ctx, "t1", "k"); len(got) == 0 {
		t.Fatal("a failed update removed the document")
	}
	// updates are serialised: no increment is lost
	if err := st.UpdateThreadState(ctx, "t1", "n", set(`0`)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = st.UpdateThreadState(ctx, "t1", "n", func(old json.RawMessage) (json.RawMessage, error) {
				var n int
				_ = json.Unmarshal(old, &n)
				return json.Marshal(n + 1)
			})
		}()
	}
	wg.Wait()
	if got, _ := st.GetThreadState(ctx, "t1", "n"); string(got) != "20" {
		t.Fatalf("counter = %s, want 20", got)
	}
	// nil deletes; threads are independent
	_ = st.UpdateThreadState(ctx, "t2", "k", set(`"other"`))
	if err := st.UpdateThreadState(ctx, "t1", "k", func(json.RawMessage) (json.RawMessage, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetThreadState(ctx, "t1", "k"); got != nil {
		t.Fatalf("deleted document = %q", got)
	}
	if got, _ := st.GetThreadState(ctx, "t2", "k"); string(got) != `"other"` {
		t.Fatalf("other thread = %q", got)
	}
	if err := st.DeleteThreadState(ctx, "t2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetThreadState(ctx, "t2", "k"); got != nil {
		t.Fatalf("after DeleteThreadState = %q", got)
	}
}

func TestMem(t *testing.T) { suite(t, threadstate.NewMem()) }

func TestFileSurvivesAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "thread_state.json")
	f, err := threadstate.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	suite(t, f)
	_ = f.UpdateThreadState(context.Background(), "t9", "queue", func(json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`["x"]`), nil })

	again, err := threadstate.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := again.GetThreadState(context.Background(), "t9", "queue"); string(got) != `["x"]` {
		t.Fatalf("after reopen = %q", got)
	}
}
