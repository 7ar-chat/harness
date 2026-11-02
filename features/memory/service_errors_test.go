package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/features/memory"
)

var errBoom = errors.New("boom")

func TestErrorsRenderTheirCodeAndPayload(t *testing.T) {
	// Arrange
	r := newFixture(memory.Options{}).svc.Get(context.Background(), "alice", "mem_nope", "", false)
	e := r.Data["error"].(map[string]any)

	// Act
	err := &memory.Error{Code: "x_code", Message: "it broke"}

	// Assert
	if err.Error() != "x_code: it broke" {
		t.Fatalf("Error() = %q", err.Error())
	}
	if e["code"] != "memory_not_found" || e["message"] != "Memory id not found" || e["details"].(map[string]any)["id"] != "mem_nope" {
		t.Fatalf("payload = %v", e)
	}
}

func TestLayersListThePrimaryMemoryThenTheUsersOwn(t *testing.T) {
	// A run has one layer (named after who shares it); a project run adds the user's own layer, unless it is the same memory.
	// Arrange
	ctx := context.Background()
	plain := memory.New(newFixture(memory.Options{}).st, memory.Options{})
	project := memory.New(newFixture(memory.Options{}).st, memory.Options{})
	project.KeyFor = func(_ context.Context, _, thread string) string {
		if thread == "t-p" {
			return "project:p1"
		}
		return "" // an empty key falls back to the login
	}
	project.Shared = func(key string) bool { return strings.HasPrefix(key, "project:") }
	project.UserKeyFor = func(_ context.Context, login, thread string) string {
		if thread == "t-same" {
			return login
		}
		return "pu:" + login
	}

	// Act
	personal := plain.Layers(ctx, "alice", "t")
	inProject := project.Layers(ctx, "alice", "t-p")
	fallback := project.Layers(ctx, "alice", "t-x")
	same := project.Layers(ctx, "alice", "t-same")

	// Assert
	if len(personal) != 1 || personal[0] != (memory.Layer{Key: "alice", Name: "user"}) || plain.SharedMemory(ctx, "alice", "t") {
		t.Fatalf("personal = %v", personal)
	}
	if len(inProject) != 2 || inProject[0] != (memory.Layer{Key: "project:p1", Name: "project"}) || inProject[1] != (memory.Layer{Key: "pu:alice", Name: "me"}) ||
		!project.SharedMemory(ctx, "alice", "t-p") {
		t.Fatalf("project = %v", inProject)
	}
	if fallback[0].Key != "alice" || project.SharedMemory(ctx, "alice", "t-x") {
		t.Fatalf("an empty key must fall back to the login: %v", fallback)
	}
	if len(same) != 1 {
		t.Fatalf("a user layer equal to the primary one is not a second layer: %v", same)
	}
}

func TestEnabledFollowsTheUsersSetting(t *testing.T) {
	// Arrange
	f := newFixture(memory.Options{})
	ctx := context.Background()
	before, _ := f.svc.Enabled(ctx, "alice")
	_ = f.st.SetMemoryEnabled(ctx, "alice", false)

	// Act
	after, err := f.svc.Enabled(ctx, "alice")

	// Assert
	if !before || after || err != nil {
		t.Fatalf("before %v after %v err %v", before, after, err)
	}
}

func TestResolveReportsWhatWentWrong(t *testing.T) {
	// A missing id or path, a deleted entry, a bad path, no target at all and a failing store each get their own code.
	// Arrange
	st, svc, w := newFaulty(memory.Options{})
	ctx := context.Background()
	live := svc.Create(ctx, w, memory.CreateInput{Content: "live", Path: "memories/semantic/notes/live.md"}).Data["memory"].(memory.Entry)
	gone := svc.Create(ctx, w, memory.CreateInput{Content: "gone", Path: "memories/semantic/notes/gone.md"}).Data["memory"].(memory.Entry)
	svc.Delete(ctx, w, gone.ID, "", map[string]any{"reason": "outdated"}, "")
	code := func(r memory.Result) string { return errCode(r) }

	// Act
	byID := svc.Get(ctx, "alice", live.ID, "", false)
	missingPath := svc.Get(ctx, "alice", "", "memories/semantic/notes/none.md", false)
	deletedByID := svc.Get(ctx, "alice", gone.ID, "", false)
	deletedByPath := svc.Get(ctx, "alice", "", "memories/semantic/notes/gone.md", false)
	badPath := svc.Get(ctx, "alice", "", "../etc/passwd", false)
	nothing := svc.Get(ctx, "alice", "  ", "  ", false)
	st.errGet = errBoom
	idFails := svc.Get(ctx, "alice", live.ID, "", false)
	st.errGet, st.errGetPath = nil, errBoom
	pathFails := svc.Get(ctx, "alice", "", "memories/semantic/notes/live.md", false)

	// Assert
	if byID.IsError {
		t.Fatalf("get by id: %v", byID.Data)
	}
	got := map[string]string{"missing path": code(missingPath), "deleted id": code(deletedByID), "deleted path": code(deletedByPath),
		"bad path": code(badPath), "nothing": code(nothing), "id fails": code(idFails), "path fails": code(pathFails)}
	want := map[string]string{"missing path": "memory_not_found", "deleted id": "memory_deleted", "deleted path": "memory_deleted",
		"bad path": "invalid_path", "nothing": "invalid_target", "id fails": "memory_store_failed", "path fails": "memory_store_failed"}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: code %q, want %q", k, got[k], w)
		}
	}
	// The raw storage error is not shown to the model.
	if b, _ := json.Marshal(idFails.Data); strings.Contains(string(b), "boom") {
		t.Fatalf("storage error leaked: %s", b)
	}
}

func TestGetOfAnEntryWithoutImportanceReportsNull(t *testing.T) {
	// With metadata the entry's importance is reported, null when none is set.
	// Arrange
	f := newFixture(memory.Options{})
	imp := 0.7
	with := f.create(t, memory.CreateInput{Content: "with", Importance: &imp})
	without := f.create(t, memory.CreateInput{Content: "without"})

	// Act
	a := f.svc.Get(context.Background(), "alice", with.ID, "", true).Data["memory"].(map[string]any)
	b := f.svc.Get(context.Background(), "alice", without.ID, "", true).Data["memory"].(map[string]any)

	// Assert
	if a["importance"] != 0.7 || b["importance"] != nil {
		t.Fatalf("importance %v / %v", a["importance"], b["importance"])
	}
	if _, has := b["importance"]; !has {
		t.Fatal("importance key missing")
	}
}

func TestSearchRejectsBadFiltersAndReportsStorageFailures(t *testing.T) {
	// Arrange
	st, svc, w := newFaulty(memory.Options{})
	ctx := context.Background()
	svc.Create(ctx, w, memory.CreateInput{Content: "Risk report on Monday", Tags: []string{"7", "report"}})

	// Act
	badScope := svc.Search(ctx, "alice", memory.SearchInput{Query: "risk", Scope: "galaxy"})
	badPrefix := svc.Search(ctx, "alice", memory.SearchInput{Query: "risk", Filters: map[string]any{"path_prefix": "../x"}})
	numericTag := svc.Search(ctx, "alice", memory.SearchInput{Query: "risk", Filters: map[string]any{"tags": []any{7, " ", nil}}})
	stringTags := svc.Search(ctx, "alice", memory.SearchInput{Query: "risk", Filters: map[string]any{"tags": []string{"report", ""}}})
	noMatchTag := svc.Search(ctx, "alice", memory.SearchInput{Query: "risk", Filters: map[string]any{"tags": []any{"absent"}}})
	st.errList = map[string]error{"alice": errBoom}
	failing := svc.Search(ctx, "alice", memory.SearchInput{Query: "risk"})

	// Assert
	if errCode(badScope) != "invalid_scope" || errCode(badPrefix) != "invalid_path" || errCode(failing) != "memory_store_failed" {
		t.Fatalf("codes: %q %q %q", errCode(badScope), errCode(badPrefix), errCode(failing))
	}
	count := func(r memory.Result) int { return len(r.Data["results"].([]map[string]any)) }
	if count(numericTag) != 1 || count(stringTags) != 1 || count(noMatchTag) != 0 {
		t.Fatalf("tag filters: numeric %d strings %d absent %d", count(numericTag), count(stringTags), count(noMatchTag))
	}
}

func TestSearchBoundsTopK(t *testing.T) {
	// top_k is clamped to 1..MaxTopK whatever the caller passes.
	// Arrange
	f := newFixture(memory.Options{})
	for _, c := range []string{"deploy one", "deploy two", "deploy three"} {
		f.create(t, memory.CreateInput{Content: c})
	}
	count := func(k int) int {
		return len(f.svc.Search(context.Background(), "alice", memory.SearchInput{Query: "deploy", TopK: k}).Data["results"].([]map[string]any))
	}

	// Act / Assert
	if count(-5) != 1 || count(2) != 2 || count(1000) != 3 || count(0) != 3 {
		t.Fatalf("top_k -5 → %d, 2 → %d, 1000 → %d, default → %d", count(-5), count(2), count(1000), count(0))
	}
}

func TestCreateValidatesInputs(t *testing.T) {
	// Importance, tags, path and metadata importance are checked; a pre-existing tag is de-duplicated; storage errors map to codes.
	// Arrange
	st, svc, w := newFaulty(memory.Options{})
	ctx := context.Background()
	bad := 1.5
	many := make([]string, memory.MaxTags+1)
	for i := range many {
		many[i] = "t" + strings.Repeat("x", i)
	}
	create := func(in memory.CreateInput) memory.Result { return svc.Create(ctx, w, in) }

	// Act
	codes := map[string]string{
		"importance": errCode(create(memory.CreateInput{Content: "x", Importance: &bad})),
		"many tags":  errCode(create(memory.CreateInput{Content: "x", Tags: many})),
		"long tag":   errCode(create(memory.CreateInput{Content: "x", Tags: []string{strings.Repeat("é", memory.MaxTagChars+1)}})),
		"bad path":   errCode(create(memory.CreateInput{Content: "x", Path: "/abs/path.md"})),
	}
	st.errCreate = store.ErrMemoryLimit
	codes["limit"] = errCode(create(memory.CreateInput{Content: "x"}))
	st.errCreate = errBoom
	codes["store"] = errCode(create(memory.CreateInput{Content: "x"}))
	st.errCreate = nil
	okTags := create(memory.CreateInput{Content: "tagged", Tags: []string{"Go", "go", " ", "GO ", "api"}})

	// Assert
	want := map[string]string{"importance": "invalid_importance", "many tags": "invalid_tags", "long tag": "invalid_tags", "bad path": "invalid_path",
		"limit": "memory_limit", "store": "memory_store_failed"}
	for k, w := range want {
		if codes[k] != w {
			t.Errorf("%s: %q, want %q", k, codes[k], w)
		}
	}
	if tags := okTags.Data["memory"].(memory.Entry).Tags; len(tags) != 2 || tags[0] != "Go" || tags[1] != "api" {
		t.Fatalf("tags = %v", tags)
	}
}

func TestMetadataImportanceIsUsedOnlyWhenItIsANumberBetweenZeroAndOne(t *testing.T) {
	// metadata.importance sets the entry's importance (numbers, ints and JSON numbers); anything else is ignored;
	// an explicit Importance wins over it.
	// Arrange
	f := newFixture(memory.Options{})
	explicit := 0.1
	create := func(meta map[string]any, imp *float64) *float64 {
		e := f.create(t, memory.CreateInput{Content: "fact " + strings.Repeat("x", len(f.svc.Search(context.Background(), "alice", memory.SearchInput{Query: "fact", TopK: 50}).Data["results"].([]map[string]any))), Metadata: meta, Importance: imp})
		return e.Importance
	}

	// Act
	float := create(map[string]any{"importance": 0.9}, nil)
	integer := create(map[string]any{"importance": 1}, nil)
	number := create(map[string]any{"importance": json.Number("0.4")}, nil)
	badNumber := create(map[string]any{"importance": json.Number("abc")}, nil)
	tooBig := create(map[string]any{"importance": 5.0}, nil)
	negative := create(map[string]any{"importance": -0.5}, nil)
	text := create(map[string]any{"importance": "high"}, nil)
	overridden := create(map[string]any{"importance": 0.9}, &explicit)

	// Assert
	if float == nil || *float != 0.9 || integer == nil || *integer != 1 || number == nil || *number != 0.4 {
		t.Fatalf("valid: %v %v %v", float, integer, number)
	}
	if badNumber != nil || tooBig != nil || negative != nil || text != nil {
		t.Fatalf("invalid metadata importance was kept: %v %v %v %v", badNumber, tooBig, negative, text)
	}
	if overridden == nil || *overridden != 0.1 {
		t.Fatalf("explicit importance lost: %v", overridden)
	}
}

func TestTitlesComeFromTheInputThenMetadataThenTheContent(t *testing.T) {
	// An explicit title wins, then metadata.title, else the first line of content; titles are collapsed, redacted and bounded.
	// Arrange
	f := newFixture(memory.Options{})
	explicit := "  Use   staging  token=abc123 "
	long := strings.Repeat("w", memory.MaxTitleChars+50)

	// Act
	a := f.create(t, memory.CreateInput{Content: "# Heading\nbody", Title: &explicit})
	b := f.create(t, memory.CreateInput{Content: "# Heading two\nbody", Metadata: map[string]any{"title": "From  metadata"}})
	c := f.create(t, memory.CreateInput{Content: "# Heading three\nbody"})
	d := f.create(t, memory.CreateInput{Content: "body four", Title: &long})

	// Assert
	if a.Title != "Use staging token= [REDACTED]" || b.Title != "From metadata" || c.Title != "Heading three" {
		t.Fatalf("titles: %q %q %q", a.Title, b.Title, c.Title)
	}
	if n := len([]rune(d.Title)); n != memory.MaxTitleChars {
		t.Fatalf("title length %d", n)
	}
}

func TestUpdateRejectsInvalidChanges(t *testing.T) {
	// Transient paths, empty or oversized content, bad importance, too many tags, strict policy and bad new paths are refused with their codes.
	// Arrange
	st, svc, w := newFaulty(memory.Options{MaxContentBytes: 50})
	ctx := context.Background()
	e := svc.Create(ctx, w, memory.CreateInput{Content: "Prefers tea", Path: "memories/semantic/preferences/tea.md",
		Metadata: map[string]any{"trigger": "durable_preference"}}).Data["memory"].(memory.Entry)
	runs, _ := st.CreateMemoryEntry(ctx, &store.MemoryEntry{ID: "r1", UserLogin: "alice", Path: "runs/2026/summary.md", Title: "r", Content: "run",
		Scope: "personal", Repository: "default", Source: "proxy", Status: store.MemoryActive, ContentHash: "h"}, store.MemoryLimits{})
	strict := memory.New(st, memory.Options{WriteMode: "strict"})
	empty, huge, other := "   ", strings.Repeat("x", 51), "Prefers coffee"
	badImp := -1.0
	tooMany := make([]string, memory.MaxTags+1)
	for i := range tooMany {
		tooMany[i] = "t" + strings.Repeat("y", i)
	}
	badPath, runsPath := "../x.md", "runs/x.md"
	up := func(in memory.UpdateInput) string { return errCode(svc.Update(ctx, w, in)) }

	// Act
	codes := map[string]string{
		"transient ref":    up(memory.UpdateInput{Path: "runs/2026/summary.md", Content: &other}),
		"bad ref":          up(memory.UpdateInput{Path: "../x", Content: &other}),
		"transient target": up(memory.UpdateInput{ID: runs.ID, Content: &other}),
		"empty":            up(memory.UpdateInput{ID: e.ID, Content: &empty}),
		"huge":             up(memory.UpdateInput{ID: e.ID, Content: &huge}),
		"importance":       up(memory.UpdateInput{ID: e.ID, Importance: &badImp}),
		"tags":             up(memory.UpdateInput{ID: e.ID, Tags: &tooMany}),
		"bad new path":     up(memory.UpdateInput{ID: e.ID, NewPath: &badPath}),
		"transient new":    up(memory.UpdateInput{ID: e.ID, NewPath: &runsPath}),
		"not found":        up(memory.UpdateInput{ID: "mem_nope", Content: &other}),
	}

	// Assert
	want := map[string]string{"transient ref": "transient_path", "bad ref": "invalid_path", "transient target": "transient_path", "empty": "invalid_content",
		"huge": "content_too_large", "importance": "invalid_importance", "tags": "invalid_tags", "bad new path": "invalid_path",
		"transient new": "transient_path", "not found": "memory_not_found"}
	for k, w := range want {
		if codes[k] != w {
			t.Errorf("%s: %q, want %q", k, codes[k], w)
		}
	}
	got, _ := st.GetMemoryEntry(ctx, "alice", e.ID)
	if got.Content != "Prefers tea" {
		t.Fatalf("a refused update changed the entry: %q", got.Content)
	}
	// The stored entry carries a durable_preference trigger in its metadata; an update merges it, so strict mode accepts it.
	lunch := "Lunch is at noon today"
	if strictUpdate := strict.Update(ctx, w, memory.UpdateInput{ID: e.ID, Content: &lunch}); strictUpdate.IsError {
		t.Fatalf("strict update with an inherited trigger: %v", strictUpdate.Data)
	}
}

func TestStrictUpdateAndDeleteWithoutATriggerAreBlocked(t *testing.T) {
	// In strict mode an update with no recognizable trigger and a delete with no reason are policy violations that change nothing.
	// Arrange
	st, _, w := newFaulty(memory.Options{})
	ctx := context.Background()
	warn := memory.New(st, memory.Options{})
	strict := memory.New(st, memory.Options{WriteMode: "strict"})
	e := warn.Create(ctx, w, memory.CreateInput{Content: "Lunch at noon"}).Data["memory"].(memory.Entry)
	content := "Lunch at one"

	// Act
	upd := strict.Update(ctx, w, memory.UpdateInput{ID: e.ID, Content: &content})
	del := strict.Delete(ctx, w, e.ID, "", nil, "")
	delWithReason := strict.Delete(ctx, w, e.ID, "", map[string]any{"reason": "outdated"}, "")

	// Assert
	if errCode(upd) != "policy_violation" || errCode(del) != "policy_violation" {
		t.Fatalf("update %v delete %v", upd.Data, del.Data)
	}
	details := del.Data["error"].(map[string]any)["details"].(map[string]any)
	if details["write_mode"] != "strict" || details["trigger"] != nil || details["violations"].([]string)[0] != "delete_trigger_missing" {
		t.Fatalf("details = %v", details)
	}
	if delWithReason.IsError {
		t.Fatalf("a delete with a reason must pass: %v", delWithReason.Data)
	}
}

func TestUpdateChangesTitleImportanceAndPath(t *testing.T) {
	// A derived title follows the content, an explicit or metadata title sticks, importance can be set, taken from metadata or cleared,
	// and the entry can move to a new path.
	// Arrange
	f := newFixture(memory.Options{})
	ctx := context.Background()
	imp := 0.4
	derived := f.create(t, memory.CreateInput{Content: "Old heading line", Importance: &imp})
	custom := f.create(t, memory.CreateInput{Content: "Some body", Title: ptr("Chosen title")})
	next := "New heading line"
	next2 := "Rewritten body"
	newImp := 0.9
	newPath := "memories/semantic/facts/moved.md"

	// Act
	a := f.svc.Update(ctx, f.w, memory.UpdateInput{ID: derived.ID, Content: &next, NewPath: &newPath}).Data["memory"].(memory.Entry)
	b := f.svc.Update(ctx, f.w, memory.UpdateInput{ID: custom.ID, Content: &next2}).Data["memory"].(memory.Entry)
	c := f.svc.Update(ctx, f.w, memory.UpdateInput{ID: custom.ID, Title: ptr("  Renamed   now ")}).Data["memory"].(memory.Entry)
	d := f.svc.Update(ctx, f.w, memory.UpdateInput{ID: custom.ID, Metadata: map[string]any{"title": "Meta title"}}).Data["memory"].(memory.Entry)
	e := f.svc.Update(ctx, f.w, memory.UpdateInput{ID: derived.ID, Importance: &newImp}).Data["memory"].(memory.Entry)
	g := f.svc.Update(ctx, f.w, memory.UpdateInput{ID: derived.ID, Metadata: map[string]any{"importance": 0.2}}).Data["memory"].(memory.Entry)
	h := f.svc.Update(ctx, f.w, memory.UpdateInput{ID: derived.ID, ClearImportance: true}).Data["memory"].(memory.Entry)

	// Assert
	if a.Title != "New heading line" || a.Path != newPath {
		t.Fatalf("derived title %q path %q", a.Title, a.Path)
	}
	if b.Title != "Chosen title" || c.Title != "Renamed now" || d.Title != "Meta title" {
		t.Fatalf("titles: %q %q %q", b.Title, c.Title, d.Title)
	}
	if e.Importance == nil || *e.Importance != 0.9 || g.Importance == nil || *g.Importance != 0.2 || h.Importance != nil {
		t.Fatalf("importance: %v %v %v", e.Importance, g.Importance, h.Importance)
	}
}

func TestUpdateOfAnUnchangedContentIsNotAContentChange(t *testing.T) {
	// Writing the same content back reports no content change, keeps its hash and adds no revision.
	// Arrange
	f := newFixture(memory.Options{})
	m := f.create(t, memory.CreateInput{Content: "Same text"})
	same := "Same text"

	// Act
	r := f.svc.Update(context.Background(), f.w, memory.UpdateInput{ID: m.ID, Content: &same})

	// Assert
	e, _ := f.st.GetMemoryEntry(context.Background(), "alice", m.ID)
	if r.IsError || r.Data["changes"].(map[string]any)["content_changed"] != false {
		t.Fatalf("update = %v", r.Data)
	}
	if _, has := e.Metadata["history"]; has {
		t.Fatalf("a revision was recorded: %v", e.Metadata["history"])
	}
}

func TestUpdateMapsStorageErrors(t *testing.T) {
	// A path clash, a vanished entry, a concurrent change and a storage failure are reported with their own codes,
	// for direct updates and for proposals alike.
	// Arrange
	st, svc, w := newFaulty(memory.Options{})
	ctx := context.Background()
	e := svc.Create(ctx, w, memory.CreateInput{Content: "Prefers tea"}).Data["memory"].(memory.Entry)
	content := "Prefers coffee"
	untrusted := memory.Writer{Login: "alice", Untrusted: true}
	cases := []struct {
		err  error
		want string
	}{{store.ErrMemoryPathExists, "path_exists"}, {store.ErrNotFound, "memory_not_found"}, {store.ErrMemoryConflict, "memory_conflict"}, {errBoom, "memory_store_failed"}}

	for _, c := range cases {
		st.errUpdate = c.err

		// Act
		direct := svc.Update(ctx, w, memory.UpdateInput{ID: e.ID, Content: &content})
		proposal := svc.Update(ctx, untrusted, memory.UpdateInput{ID: e.ID, Content: &content})

		// Assert
		if errCode(direct) != c.want {
			t.Errorf("direct update with %v: %q, want %q", c.err, errCode(direct), c.want)
		}
		wantProposal := c.want
		if c.err == store.ErrMemoryPathExists {
			wantProposal = "memory_store_failed" // a proposal does not move the entry: the store never reports a clash for it
		}
		if errCode(proposal) != wantProposal {
			t.Errorf("proposal with %v: %q, want %q", c.err, errCode(proposal), wantProposal)
		}
	}
}

func TestUpdatingAPendingEntryChangesItInPlace(t *testing.T) {
	// An untrusted update of an entry that is itself pending is not a proposal on top of a proposal: the pending entry is edited and stays pending.
	// Arrange
	f := newFixture(memory.Options{})
	ctx := context.Background()
	untrusted := memory.Writer{Login: "alice", Untrusted: true}
	id := f.svc.Create(ctx, untrusted, memory.CreateInput{Content: "Reports go to Bob"}).Data["memory"].(memory.Entry).ID
	next := "Reports go to Carol"

	// Act
	r := f.svc.Update(ctx, untrusted, memory.UpdateInput{ID: id, Content: &next})

	// Assert
	e, _ := f.st.GetMemoryEntry(ctx, "alice", id)
	if r.IsError || r.Changes[0].Action != "proposed" || r.Data["confirmation"] == nil || r.Data["proposal"] != nil {
		t.Fatalf("update = %v", r.Data)
	}
	if e.Status != store.MemoryPending || e.Content != next || e.Proposal != nil {
		t.Fatalf("entry = %+v", e)
	}
}

func TestPublicProposalShowsWhatIsProposed(t *testing.T) {
	// The proposal object carries the proposed fields; null importance when none; nothing for no proposal.
	// Arrange
	imp := 0.3
	p := &store.MemoryProposal{Path: "memories/semantic/facts/a.md", Title: "T", Content: "c", Tags: nil, Importance: &imp,
		Metadata: map[string]any{"k": "v"}, ContentHash: "h", ProposedAt: now}

	// Act
	got := memory.PublicProposal(p)
	p.Importance = nil
	noImp := memory.PublicProposal(p)

	// Assert
	if memory.PublicProposal(nil) != nil {
		t.Fatal("a nil proposal must stay nil")
	}
	if got["importance"] != 0.3 || noImp["importance"] != nil || got["content_hash"] != "h" || got["proposed_at"] != "2026-10-06T12:00:00Z" ||
		len(got["tags"].([]string)) != 0 || got["metadata"].(map[string]any)["k"] != "v" || got["path"] != "memories/semantic/facts/a.md" || got["title"] != "T" {
		t.Fatalf("proposal = %v", got)
	}
	if _, leaks := got["content"]; leaks {
		t.Fatal("the proposal summary must not carry the content")
	}
}

func TestDeleteRefusesWhatItMust(t *testing.T) {
	// Bad references, transient entries, missing entries and a failing store are errors; deleting nothing leaves the entry.
	// Arrange
	st, svc, w := newFaulty(memory.Options{})
	ctx := context.Background()
	e := svc.Create(ctx, w, memory.CreateInput{Content: "keep me"}).Data["memory"].(memory.Entry)
	runs, _ := st.CreateMemoryEntry(ctx, &store.MemoryEntry{ID: "r1", UserLogin: "alice", Path: "handoff/next.md", Title: "r", Content: "run",
		Scope: "personal", Repository: "default", Source: "proxy", Status: store.MemoryActive, ContentHash: "h"}, store.MemoryLimits{})
	reason := map[string]any{"reason": "outdated"}

	// Act
	transientRef := svc.Delete(ctx, w, "", "runs/x.md", reason, "")
	transientTarget := svc.Delete(ctx, w, runs.ID, "", reason, "")
	missing := svc.Delete(ctx, w, "mem_nope", "", reason, "")
	st.errUpdate = errBoom
	softFails := svc.Delete(ctx, w, e.ID, "", reason, "soft")
	st.errUpdate, st.errDelete = nil, errBoom
	hardFails := svc.Delete(ctx, w, e.ID, "", reason, "hard")

	// Assert
	if errCode(transientRef) != "transient_path" || errCode(transientTarget) != "transient_path" || errCode(missing) != "memory_not_found" ||
		errCode(softFails) != "memory_store_failed" || errCode(hardFails) != "memory_store_failed" {
		t.Fatalf("codes: %q %q %q %q %q", errCode(transientRef), errCode(transientTarget), errCode(missing), errCode(softFails), errCode(hardFails))
	}
	if got, _ := st.GetMemoryEntry(ctx, "alice", e.ID); got.Status != store.MemoryActive {
		t.Fatalf("a failed delete changed the entry: %+v", got)
	}
}

func TestDeleteToleratesAnEntryAlreadyGone(t *testing.T) {
	// If the entry disappeared between the read and the write, the delete still reports success.
	// Arrange
	st, svc, w := newFaulty(memory.Options{})
	ctx := context.Background()
	reason := map[string]any{"reason": "outdated"}
	a := svc.Create(ctx, w, memory.CreateInput{Content: "a note"}).Data["memory"].(memory.Entry)
	b := svc.Create(ctx, w, memory.CreateInput{Content: "b note"}).Data["memory"].(memory.Entry)

	// Act
	st.errDelete = store.ErrNotFound
	hard := svc.Delete(ctx, w, a.ID, "", reason, "hard")
	st.errDelete, st.errUpdate = nil, store.ErrNotFound
	soft := svc.Delete(ctx, w, b.ID, "", reason, "soft")

	// Assert
	if hard.IsError || hard.Data["mode"] != "hard" || soft.IsError || soft.Data["mode"] != "soft" || soft.Changes[0].Entry.Status != store.MemoryDeleted {
		t.Fatalf("hard %v soft %v", hard.Data, soft.Data)
	}
}

func TestDeleteEntryFailsWhenTheEntryChangedSinceItWasRead(t *testing.T) {
	// DeleteEntry works on an entry the caller read: a newer version blocks a soft delete, a changed status blocks a hard one.
	// Arrange
	f := newFixture(memory.Options{})
	ctx := context.Background()
	reason := map[string]any{"reason": "outdated"}
	soft := f.create(t, memory.CreateInput{Content: "soft target"})
	hard := f.svc.Create(ctx, memory.Writer{Login: "alice", Untrusted: true}, memory.CreateInput{Content: "pending target"}).Data["memory"].(memory.Entry)
	softRead, _ := f.st.GetMemoryEntry(ctx, "alice", soft.ID)
	hardRead, _ := f.st.GetMemoryEntry(ctx, "alice", hard.ID)
	newer := "soft target, edited"
	f.svc.Update(ctx, f.w, memory.UpdateInput{ID: soft.ID, Content: &newer})
	if _, cerr := f.svc.Confirm(ctx, "alice", hard.ID, memory.Expect{}); cerr != nil {
		t.Fatal(cerr)
	}

	// Act
	softRes := f.svc.DeleteEntry(ctx, f.w, softRead, reason, "soft")
	hardRes := f.svc.DeleteEntry(ctx, f.w, hardRead, reason, "hard")

	// Assert
	if errCode(softRes) != "memory_conflict" || errCode(hardRes) != "memory_conflict" {
		t.Fatalf("soft %v hard %v", softRes.Data, hardRes.Data)
	}
	if e, err := f.st.GetMemoryEntry(ctx, "alice", hard.ID); err != nil || e.Status != store.MemoryActive {
		t.Fatalf("the confirmed entry was deleted: %+v %v", e, err)
	}
	// With the entry as it is now, the delete goes through.
	fresh, _ := f.st.GetMemoryEntry(ctx, "alice", soft.ID)
	if r := f.svc.DeleteEntry(ctx, f.w, fresh, reason, "soft"); r.IsError || r.Data["deleted"] != true {
		t.Fatalf("delete of the current entry: %v", r.Data)
	}
}

func TestBatchesOfUntrustedWritesCarryTheConfirmationNote(t *testing.T) {
	// After untrusted content a batch create is all proposals (the note says so); a batch where nothing was created has no note.
	// Arrange
	f := newFixture(memory.Options{})
	ctx := context.Background()
	untrusted := memory.Writer{Login: "alice", Untrusted: true}

	// Act
	ok := f.svc.CreateBatch(ctx, untrusted, []memory.CreateInput{{Content: "one proposal"}, {Content: "another proposal"}})
	none := f.svc.CreateBatch(ctx, untrusted, []memory.CreateInput{{Content: ""}})

	// Assert
	if ok.Data["confirmation"] == nil || ok.Changes[0].Action != "proposed" || none.Data["confirmation"] != nil {
		t.Fatalf("ok %v none %v", ok.Data, none.Data)
	}
	if es, _ := f.st.ListMemoryEntries(ctx, "alice", store.MemoryPending); len(es) != 2 {
		t.Fatalf("pending = %d", len(es))
	}
}

func TestConfirmReportsWhatCannotBeKept(t *testing.T) {
	// Missing entries, a proposal whose new path was taken meanwhile, a full memory, a concurrent change and a failing store each have their code;
	// confirming an entry nothing waits on is a no-op unless the caller expects a pending one.
	// Arrange
	st, svc, w := newFaulty(memory.Options{})
	ctx := context.Background()
	untrusted := memory.Writer{Login: "alice", Untrusted: true}
	a := svc.Create(ctx, w, memory.CreateInput{Content: "tea", Path: "memories/semantic/notes/a.md"}).Data["memory"].(memory.Entry)
	svc.Create(ctx, w, memory.CreateInput{Content: "coffee", Path: "memories/semantic/notes/b.md"})
	moved := "memories/semantic/notes/b.md"
	svc.Update(ctx, untrusted, memory.UpdateInput{ID: a.ID, NewPath: &moved})
	pending := svc.Create(ctx, untrusted, memory.CreateInput{Content: "proposed"}).Data["memory"].(memory.Entry)

	// Act
	_, missing := svc.Confirm(ctx, "alice", "mem_nope", memory.Expect{})
	_, clash := svc.Confirm(ctx, "alice", a.ID, memory.Expect{})
	plain, noop := svc.Confirm(ctx, "alice", pending.ID, memory.Expect{ContentHash: "wrong"})
	st.errUpdate = store.ErrNotFound
	_, gone := svc.Confirm(ctx, "alice", pending.ID, memory.Expect{})
	st.errUpdate = store.ErrMemoryConflict
	_, changed := svc.Confirm(ctx, "alice", pending.ID, memory.Expect{})
	st.errUpdate = store.ErrMemoryLimit
	_, full := svc.Confirm(ctx, "alice", pending.ID, memory.Expect{})
	st.errUpdate = errBoom
	_, broken := svc.Confirm(ctx, "alice", pending.ID, memory.Expect{})
	st.errUpdate = nil

	// Assert
	if missing == nil || missing.Code != "memory_not_found" || clash == nil || clash.Code != "path_exists" || plain != nil || noop == nil || noop.Code != "memory_conflict" {
		t.Fatalf("missing %v clash %v plain %v noop %v", missing, clash, plain, noop)
	}
	if gone.Code != "memory_not_found" || changed.Code != "memory_conflict" || full.Code != "memory_limit" || broken.Code != "memory_store_failed" {
		t.Fatalf("store errors: %v %v %v %v", gone, changed, full, broken)
	}
	e, _ := st.GetMemoryEntry(ctx, "alice", a.ID)
	if e.Proposal == nil || e.Path != "memories/semantic/notes/a.md" {
		t.Fatalf("a failed confirm must keep the proposal waiting: %+v", e)
	}
	if _, err := svc.Confirm(ctx, "alice", e.ID, memory.Expect{ContentHash: e.Proposal.ContentHash, Pending: true}); err == nil || err.Code != "path_exists" {
		t.Fatalf("path clash on confirm = %v", err)
	}
}

func TestConfirmWithNothingWaitingReturnsTheEntryUnchanged(t *testing.T) {
	// Arrange
	f := newFixture(memory.Options{})
	ctx := context.Background()
	m := f.create(t, memory.CreateInput{Content: "settled"})
	before, _ := f.st.GetMemoryEntry(ctx, "alice", m.ID)

	// Act
	got, err := f.svc.Confirm(ctx, "alice", m.ID, memory.Expect{})
	_, notPending := f.svc.Confirm(ctx, "alice", m.ID, memory.Expect{Pending: true})

	// Assert
	after, _ := f.st.GetMemoryEntry(ctx, "alice", m.ID)
	if err != nil || got.ID != m.ID || after.Version != before.Version {
		t.Fatalf("got %+v %v version %d→%d", got, err, before.Version, after.Version)
	}
	if notPending == nil || notPending.Code != "memory_not_pending" {
		t.Fatalf("expected memory_not_pending, got %v", notPending)
	}
}

func TestConfirmingAProposalWithoutMetadataStillRecordsTheRevision(t *testing.T) {
	// A proposal written by another process may carry no metadata map: keeping it still records what it replaces.
	// Arrange
	f := newFixture(memory.Options{})
	ctx := context.Background()
	m := f.create(t, memory.CreateInput{Content: "Deploy with make"})
	e, _ := f.st.GetMemoryEntry(ctx, "alice", m.ID)
	e.Metadata = nil
	e.Proposal = &store.MemoryProposal{Path: e.Path, Title: "Deploy", Content: "Deploy with ship.sh", ContentHash: "hash-2", ThreadID: "t7"}
	if _, err := f.st.UpdateMemoryEntry(ctx, e, store.MemoryLimits{}); err != nil {
		t.Fatal(err)
	}

	// Act
	out, cerr := f.svc.Confirm(ctx, "alice", m.ID, memory.Expect{ContentHash: "hash-2"})

	// Assert
	if cerr != nil {
		t.Fatal(cerr)
	}
	h, _ := out.Metadata["history"].([]any)
	if out.Content != "Deploy with ship.sh" || len(h) != 1 || h[0].(map[string]any)["content"] != "Deploy with make" || h[0].(map[string]any)["thread"] != "t7" {
		t.Fatalf("out %+v history %v", out, h)
	}
}

func TestRejectReportsWhatCannotBeDiscarded(t *testing.T) {
	// Discarding needs something waiting; a vanished or concurrently changed entry and storage failures have their codes;
	// a discarded pending entry is gone for good.
	// Arrange
	st, svc, w := newFaulty(memory.Options{})
	ctx := context.Background()
	untrusted := memory.Writer{Login: "alice", Untrusted: true}
	active := svc.Create(ctx, w, memory.CreateInput{Content: "tea"}).Data["memory"].(memory.Entry)
	content := "coffee"
	svc.Update(ctx, untrusted, memory.UpdateInput{ID: active.ID, Content: &content})
	pending := svc.Create(ctx, untrusted, memory.CreateInput{Content: "proposed"}).Data["memory"].(memory.Entry)

	// Act
	_, missing := svc.Reject(ctx, "alice", "mem_nope", memory.Expect{})
	st.errDelete = store.ErrNotFound
	_, pendingGone := svc.Reject(ctx, "alice", pending.ID, memory.Expect{})
	st.errDelete = store.ErrMemoryConflict
	_, pendingChanged := svc.Reject(ctx, "alice", pending.ID, memory.Expect{})
	st.errDelete = errBoom
	_, pendingBroken := svc.Reject(ctx, "alice", pending.ID, memory.Expect{})
	st.errDelete, st.errUpdate = nil, store.ErrNotFound
	_, proposalGone := svc.Reject(ctx, "alice", active.ID, memory.Expect{})
	st.errUpdate = store.ErrMemoryConflict
	_, proposalChanged := svc.Reject(ctx, "alice", active.ID, memory.Expect{})
	st.errUpdate = errBoom
	_, proposalBroken := svc.Reject(ctx, "alice", active.ID, memory.Expect{})
	st.errUpdate = nil
	kept, ok := svc.Reject(ctx, "alice", pending.ID, memory.Expect{})

	// Assert
	codes := []string{missing.Code, pendingGone.Code, pendingChanged.Code, pendingBroken.Code, proposalGone.Code, proposalChanged.Code, proposalBroken.Code}
	want := []string{"memory_not_found", "memory_not_found", "memory_not_pending", "memory_store_failed", "memory_not_found", "memory_conflict", "memory_store_failed"}
	for i := range want {
		if codes[i] != want[i] {
			t.Errorf("case %d: %q, want %q", i, codes[i], want[i])
		}
	}
	if kept != nil || ok != nil {
		t.Fatalf("a discarded pending entry returns nothing: %v %v", kept, ok)
	}
	if _, err := st.GetMemoryEntry(ctx, "alice", pending.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pending entry still stored: %v", err)
	}
	if e, _ := st.GetMemoryEntry(ctx, "alice", active.ID); e.Proposal == nil || e.Content != "tea" {
		t.Fatalf("failed discards must leave the proposal: %+v", e)
	}
}

func TestRestoreReportsWhatCannotBeRestored(t *testing.T) {
	// A missing entry and storage failures are reported; an entry that is not deleted is returned as it is; a concurrent change blocks the restore.
	// Arrange
	st, svc, w := newFaulty(memory.Options{})
	ctx := context.Background()
	m := svc.Create(ctx, w, memory.CreateInput{Content: "tea"}).Data["memory"].(memory.Entry)
	deleted := svc.Create(ctx, w, memory.CreateInput{Content: "coffee"}).Data["memory"].(memory.Entry)
	svc.Delete(ctx, w, deleted.ID, "", map[string]any{"reason": "outdated"}, "")

	// Act
	_, missing := svc.Restore(ctx, "alice", "mem_nope")
	same, noErr := svc.Restore(ctx, "alice", m.ID)
	st.errGet = errBoom
	_, getFails := svc.Restore(ctx, "alice", deleted.ID)
	st.errGet, st.errUpdate = nil, store.ErrMemoryConflict
	_, changed := svc.Restore(ctx, "alice", deleted.ID)
	st.errUpdate = errBoom
	_, updateFails := svc.Restore(ctx, "alice", deleted.ID)

	// Assert
	if missing == nil || missing.Code != "memory_not_found" || noErr != nil || same.ID != m.ID || same.Status != store.MemoryActive {
		t.Fatalf("missing %v same %+v %v", missing, same, noErr)
	}
	if getFails.Code != "memory_store_failed" || changed.Code != "memory_conflict" || updateFails.Code != "memory_store_failed" {
		t.Fatalf("errors: %v %v %v", getFails, changed, updateFails)
	}
	if e, _ := st.GetMemoryEntry(ctx, "alice", deleted.ID); e.Status != store.MemoryDeleted {
		t.Fatalf("failed restores must keep the entry deleted: %+v", e)
	}
}

func TestSearchAllMergesLayersByScoreAndKeepsTheBestTopK(t *testing.T) {
	// Hits of several layers are merged by score (best first), cut to top_k and labelled; an error in a layer is the result;
	// with no hit the trace says so; with one layer it is that layer's own search.
	// Arrange
	f := newFixture(memory.Options{})
	ctx := context.Background()
	team := memory.Writer{Login: "team", Source: store.MemorySourceProxy}
	mine := memory.Writer{Login: "me", Source: store.MemorySourceProxy}
	newEntry(t, f, team, memory.CreateInput{Content: "Deploys use staging then production, deploys are Friday-free", Tags: []string{"decision"}})
	newEntry(t, f, mine, memory.CreateInput{Content: "I deploy"})
	newEntry(t, f, mine, memory.CreateInput{Content: "Deploy notes in the wiki"})
	layers := []memory.Layer{{Key: "team", Name: "project"}, {Key: "me", Name: "me"}}

	// Act
	all := f.svc.SearchAll(ctx, layers, memory.SearchInput{Query: "deploy", TopK: 10})
	top := f.svc.SearchAll(ctx, layers, memory.SearchInput{Query: "deploy", TopK: 2})
	none := f.svc.SearchAll(ctx, layers, memory.SearchInput{Query: "???"})
	bad := f.svc.SearchAll(ctx, layers, memory.SearchInput{Query: "deploy", Scope: "galaxy"})
	single := f.svc.SearchAll(ctx, layers[:1], memory.SearchInput{Query: "deploy"})

	// Assert
	list := all.Data["results"].([]map[string]any)
	if len(list) != 3 {
		t.Fatalf("all = %v", list)
	}
	for i := 1; i < len(list); i++ {
		if list[i-1]["score"].(float64) < list[i]["score"].(float64) {
			t.Fatalf("not sorted by score: %v", list)
		}
	}
	if len(top.Data["results"].([]map[string]any)) != 2 || top.Data["trace"].(map[string]any)["chunk_count"] != 2 {
		t.Fatalf("top = %v", top.Data)
	}
	if tr := none.Data["trace"].(map[string]any); len(none.Data["results"].([]map[string]any)) != 0 || tr["used_memory"] != false || tr["reason"] != "no_match" {
		t.Fatalf("none = %v", none.Data)
	}
	if errCode(bad) != "invalid_scope" {
		t.Fatalf("bad = %v", bad.Data)
	}
	if _, labelled := single.Data["results"].([]map[string]any)[0]["layer"]; labelled {
		t.Fatal("a single-layer search is not relabelled")
	}
}
