package pgstore_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/GhaziBenDahmane/harness/store/pgstore"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/pgstore/migrate"
)

func TestPGMigrate0008TurnsExistingUploadsIntoPublishedVersions(t *testing.T) {
	// A skill uploaded before 0008 gets its published version "<n>.0.0" holding its package.
	// Arrange
	dsn := os.Getenv("WORK_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WORK_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("work_v7_%d", time.Now().UnixNano())
	cfg, _ := pgxpool.ParseConfig(dsn)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); pool.Close() })
	stmts := []string{"CREATE SCHEMA " + schema,
		"CREATE TABLE work_migrations (id SERIAL PRIMARY KEY, name VARCHAR(255) NOT NULL UNIQUE, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())"}
	entries, _ := os.ReadDir("../migrate/migrations")
	for _, e := range entries {
		if e.Name() >= "0008" {
			continue
		}
		sql, err := os.ReadFile("../migrate/migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		stmts = append(stmts, string(sql), "INSERT INTO work_migrations (name) VALUES ('"+e.Name()+"')")
	}
	skillID := uuid.NewString()
	stmts = append(stmts, "INSERT INTO work_skills (id, user_login, name, description, version, sha256, package, files, size_bytes) VALUES ('"+
		skillID+"', 'alice', 'report', 'Reports.', 3, 'abc', 'zipbytes', 1, 8)")
	for _, q := range stmts {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	s := pgstore.NewPG(pool)

	// Act
	err = migrate.Run(ctx, pool, schema)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	metas, err := s.SkillMetas(ctx, "alice")
	if err != nil || metas[skillID].PublishedVersionID == "" || metas[skillID].PublishedVersionID != metas[skillID].LatestVersionID {
		t.Fatalf("metas %+v %v", metas, err)
	}
	v, err := s.GetSkillVersion(ctx, metas[skillID].PublishedVersionID)
	if err != nil || v.Version != "3.0.0" || v.Status != store.SkillPublished || string(v.Package) != "zipbytes" || v.SkillID != skillID {
		t.Fatalf("version %+v %v", v, err)
	}
}

func TestPGSkillVersionsDraftsPublishAndEffectivePackage(t *testing.T) {
	// Uploads are published versions; a draft version does not replace the published package until published; labels are unique.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	u, err := s.UpsertUserSkill(ctx, store.UserSkill{UserLogin: "alice", Name: "report", DisplayName: "Report", Description: "v1",
		SHA256: "s1", Package: []byte("p1"), Files: 1, SizeBytes: 2})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	draft, err := s.AddSkillVersion(ctx, "alice", store.SkillVersion{SkillID: u.ID, Version: "1.1.0", Name: "Report", Description: "v2",
		SHA256: "s2", Package: []byte("p2"), Files: 1, SizeBytes: 2, CreatedBy: "alice"}, false)
	dup := func() error {
		_, err := s.AddSkillVersion(ctx, "alice", store.SkillVersion{SkillID: u.ID, Version: "1.1.0", Name: "R", Description: "d",
			SHA256: "s3", Package: []byte("p3"), CreatedBy: "alice"}, false)
		return err
	}()
	foreign := func() error {
		_, err := s.AddSkillVersion(ctx, "bob", store.SkillVersion{SkillID: u.ID, Version: "9.0.0", Name: "R", Description: "d",
			SHA256: "s3", Package: []byte("p3"), CreatedBy: "bob"}, true)
		return err
	}()
	beforePublish, _ := s.GetUserSkill(ctx, "alice", u.ID)
	pubErr := s.PublishSkillVersion(ctx, "alice", u.ID, draft.ID, false)
	afterPublish, _ := s.GetUserSkill(ctx, "alice", u.ID)
	versions, _ := s.ListSkillVersions(ctx, []string{u.ID})

	// Assert
	if err != nil || draft.Status != store.SkillDraft || !errors.Is(dup, store.ErrConflict) || !errors.Is(foreign, store.ErrNotFound) {
		t.Fatalf("draft %+v %v dup %v foreign %v", draft, err, dup, foreign)
	}
	if string(beforePublish.Package) != "p1" || beforePublish.Description != "v1" {
		t.Fatalf("draft replaced the published package: %+v", beforePublish)
	}
	if pubErr != nil || string(afterPublish.Package) != "p2" || afterPublish.Description != "v2" || afterPublish.SHA256 != "s2" {
		t.Fatalf("publish %v %+v", pubErr, afterPublish)
	}
	if len(versions[u.ID]) != 2 || versions[u.ID][0].Version != "1.1.0" || versions[u.ID][1].Version != "1.0.0" || versions[u.ID][0].Package != nil {
		t.Fatalf("versions %+v", versions[u.ID])
	}
}

func TestPGSkillDraftBatchesAreAtomicAndIdempotent(t *testing.T) {
	// A batch creates every draft or none; the same key replays the stored response; another request with it is a conflict.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	draft := func(name string) store.NewSkillDraft {
		return store.NewSkillDraft{Name: name, DisplayName: name, Description: "d", Version: store.SkillVersion{Version: "0.1.0",
			SourceType: store.SkillSourceGenerated, Name: name, Description: "d", SHA256: "x", Package: []byte("zip"), Files: 1, SizeBytes: 3, CreatedBy: "alice"}}
	}
	idem := store.Idempotency{Login: "alice", Command: "drafts", Key: uuid.NewString(), RequestHash: "h1"}
	respond := func(items []store.DraftItem) []byte {
		return []byte(fmt.Sprintf(`{"n":%d,"id":%q}`, len(items), items[0].SkillID))
	}

	// Act
	body, replayed, err := s.CreateSkillDrafts(ctx, idem, []store.NewSkillDraft{draft("a"), draft("b")}, respond)
	again, replayedAgain, errAgain := s.CreateSkillDrafts(ctx, idem, []store.NewSkillDraft{draft("a"), draft("b")}, respond)
	other := idem
	other.RequestHash = "h2"
	_, _, conflict := s.CreateSkillDrafts(ctx, other, []store.NewSkillDraft{draft("c")}, respond)
	clash := idem
	clash.Key, clash.RequestHash = uuid.NewString(), "h3"
	_, _, nameTaken := s.CreateSkillDrafts(ctx, clash, []store.NewSkillDraft{draft("d"), draft("a")}, respond)
	list, _ := s.ListUserSkills(ctx, "alice")
	metas, _ := s.SkillMetas(ctx, "alice")

	// Assert
	if err != nil || replayed || string(body) == "" || errAgain != nil || !replayedAgain || string(again) != string(body) {
		t.Fatalf("first %s %v %v again %s %v %v", body, replayed, err, again, replayedAgain, errAgain)
	}
	if !errors.Is(conflict, store.ErrIdempotencyConflict) || !errors.Is(nameTaken, store.ErrConflict) {
		t.Fatalf("conflict %v nameTaken %v", conflict, nameTaken)
	}
	if len(list) != 2 {
		t.Fatalf("a failed batch left skills behind: %d", len(list))
	}
	for _, u := range list {
		m := metas[u.ID]
		if m.PublishedVersionID != "" || m.LatestVersionID == "" {
			t.Fatalf("draft %+v meta %+v", u, m)
		}
	}
}

func TestPGGlobalSkillsTagsPublicationAndDeletion(t *testing.T) {
	// Global skills have unique names, tags, a publishable/unpublishable version and cascade their versions on delete.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	v := store.SkillVersion{Version: "1.0.0", Name: "Deck", Description: "d", SHA256: "x", Package: []byte("zip"), Files: 1, SizeBytes: 3, CreatedBy: "admin"}

	// Act
	g, gv, err := s.CreateGlobalSkill(ctx, store.GlobalSkill{Name: "deck", DisplayName: "Deck", Description: "d", Tags: []string{"studio-work"}, CreatedBy: "admin"}, v, true)
	_, _, dup := s.CreateGlobalSkill(ctx, store.GlobalSkill{Name: "deck", Description: "d", CreatedBy: "admin"}, v, false)
	v2 := v
	v2.SkillID, v2.Global, v2.Version = g.ID, true, "2.0.0"
	draft, draftErr := s.AddSkillVersion(ctx, "", v2, false)
	tagErr := s.SetGlobalSkillTags(ctx, g.ID, []string{"studio-work-default"})
	unpubErr := s.UnpublishGlobalSkill(ctx, g.ID)
	afterUnpublish, _ := s.GetGlobalSkill(ctx, g.ID)
	pubErr := s.PublishSkillVersion(ctx, "", g.ID, draft.ID, true)
	afterPublish, _ := s.GetGlobalSkill(ctx, g.ID)
	delErr := s.DeleteGlobalSkill(ctx, g.ID)
	_, gone := s.GetSkillVersion(ctx, gv.ID)

	// Assert
	if err != nil || g.PublishedVersionID != gv.ID || g.LatestVersionID != gv.ID || !errors.Is(dup, store.ErrConflict) {
		t.Fatalf("create %+v %v dup %v", g, err, dup)
	}
	if draftErr != nil || tagErr != nil || unpubErr != nil || afterUnpublish.PublishedVersionID != "" ||
		afterUnpublish.LatestVersionID != draft.ID || afterUnpublish.Tags[0] != "studio-work-default" {
		t.Fatalf("draft %v tag %v unpublish %v %+v", draftErr, tagErr, unpubErr, afterUnpublish)
	}
	if pubErr != nil || afterPublish.PublishedVersionID != draft.ID || delErr != nil || !errors.Is(gone, store.ErrNotFound) {
		t.Fatalf("publish %v %+v delete %v version %v", pubErr, afterPublish, delErr, gone)
	}
}

func TestPGSkillCreationSessionsAndCommandResults(t *testing.T) {
	// Sessions of deleted threads are hidden; command results are stored once per key.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	t1, _ := s.CreateThread(ctx, "alice", "first")
	t2, _ := s.CreateThread(ctx, "alice", "second")
	for _, th := range []*store.Thread{t1, t2} {
		if err := s.CreateSkillCreationSession(ctx, store.SkillCreationSession{ThreadID: th.ID, UserLogin: "alice", PackageRoot: "/workspace/generated-skill"}); err != nil {
			t.Fatal(err)
		}
	}
	idem := store.Idempotency{Login: "alice", Command: "c", Key: "k", RequestHash: "h"}

	// Act
	_, _ = s.DeleteThread(ctx, "alice", t1.ID)
	list, total, err := s.ListSkillCreationSessions(ctx, "alice", 25, 0)
	foreign, foreignErr := s.GetSkillCreationSession(ctx, "bob", t2.ID)
	saveErr := s.SaveCommandResult(ctx, idem, store.CommandResult{StatusCode: 201, Response: []byte(`{"ok":true}`)})
	again := s.SaveCommandResult(ctx, idem, store.CommandResult{StatusCode: 201, Response: []byte(`{}`)})
	got, getErr := s.CommandResult(ctx, "alice", "c", "k")

	// Assert
	if err != nil || total != 1 || len(list) != 1 || list[0].ThreadID != t2.ID || list[0].Mode != "skill_creation" || list[0].Title != "second" {
		t.Fatalf("list %+v %d %v", list, total, err)
	}
	if foreign != nil || !errors.Is(foreignErr, store.ErrNotFound) {
		t.Fatalf("foreign %+v %v", foreign, foreignErr)
	}
	if saveErr != nil || !errors.Is(again, store.ErrConflict) || getErr != nil || got.RequestHash != "h" || string(got.Response) != `{"ok":true}` {
		t.Fatalf("save %v again %v got %+v %v", saveErr, again, got, getErr)
	}
}

func TestPGPublishingLeavesOneVersionPublished(t *testing.T) {
	// Like Work's single published_version pointer: publishing (global or private) demotes the previously published
	// version to draft; unpublishing a global skill leaves none published.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	v := store.SkillVersion{Version: "1.0.0", Name: "Deck", Description: "d", SHA256: "x", Package: []byte("zip"), Files: 1, SizeBytes: 3, CreatedBy: "admin"}
	g, first, err := s.CreateGlobalSkill(ctx, store.GlobalSkill{Name: "deck", DisplayName: "Deck", Description: "d", CreatedBy: "admin"}, v, true)
	if err != nil {
		t.Fatal(err)
	}
	statuses := func(skillID string) map[string]string {
		t.Helper()
		all, err := s.ListSkillVersions(ctx, []string{skillID})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, ver := range all[skillID] {
			out[ver.ID] = ver.Status
		}
		return out
	}
	v2 := v
	v2.SkillID, v2.Global, v2.Version = g.ID, true, "2.0.0"

	// Act
	second, err := s.AddSkillVersion(ctx, "", v2, true)
	if err != nil {
		t.Fatal(err)
	}
	afterAdd := statuses(g.ID)
	if err := s.PublishSkillVersion(ctx, "", g.ID, first.ID, true); err != nil {
		t.Fatal(err)
	}
	afterRepublish := statuses(g.ID)
	if err := s.UnpublishGlobalSkill(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	afterUnpublish := statuses(g.ID)
	u, _ := s.UpsertUserSkill(ctx, store.UserSkill{UserLogin: "alice", Name: "notes", Description: "d", SHA256: "a", Package: []byte("z"), Files: 1, SizeBytes: 1})
	u, _ = s.UpsertUserSkill(ctx, store.UserSkill{UserLogin: "alice", Name: "notes", Description: "d", SHA256: "b", Package: []byte("y"), Files: 1, SizeBytes: 1})
	private := statuses(u.ID)

	// Assert
	if afterAdd[first.ID] != store.SkillDraft || afterAdd[second.ID] != store.SkillPublished {
		t.Fatalf("after a new published version %v", afterAdd)
	}
	if afterRepublish[first.ID] != store.SkillPublished || afterRepublish[second.ID] != store.SkillDraft {
		t.Fatalf("after republishing v1 %v", afterRepublish)
	}
	if afterUnpublish[first.ID] != store.SkillDraft || afterUnpublish[second.ID] != store.SkillDraft {
		t.Fatalf("after unpublish %v", afterUnpublish)
	}
	published := 0
	for _, st := range private {
		if st == store.SkillPublished {
			published++
		}
	}
	if len(private) != 2 || published != 1 {
		t.Fatalf("private re-upload versions %v", private)
	}
}

func TestPGClaimCommandReservesTheKeyUntilSavedOrReleased(t *testing.T) {
	// A claimed key is pending (status 0) and cannot be claimed again; SaveCommandResult completes it once; a released claim can be claimed again.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	idem := store.Idempotency{Login: "alice", Command: "cmd", Key: uuid.NewString(), RequestHash: "h"}
	other := store.Idempotency{Login: "alice", Command: "cmd", Key: uuid.NewString(), RequestHash: "h"}

	// Act
	first, err1 := s.ClaimCommand(ctx, idem)
	second, err2 := s.ClaimCommand(ctx, idem)
	pending, _ := s.CommandResult(ctx, "alice", "cmd", idem.Key)
	saveErr := s.SaveCommandResult(ctx, idem, store.CommandResult{StatusCode: 201, Response: []byte(`{"ok":true}`)})
	again := s.SaveCommandResult(ctx, idem, store.CommandResult{StatusCode: 201, Response: []byte(`{}`)})
	done, _ := s.CommandResult(ctx, "alice", "cmd", idem.Key)
	_, _ = s.ClaimCommand(ctx, other)
	relErr := s.ReleaseCommand(ctx, other)
	reclaimed, _ := s.ClaimCommand(ctx, other)

	// Assert
	if err1 != nil || err2 != nil || !first || second || pending == nil || pending.StatusCode != 0 {
		t.Fatalf("claims %v %v (%v %v) pending %+v", first, second, err1, err2, pending)
	}
	if saveErr != nil || !errors.Is(again, store.ErrConflict) || done.StatusCode != 201 || string(done.Response) != `{"ok":true}` {
		t.Fatalf("save %v again %v done %+v", saveErr, again, done)
	}
	if relErr != nil || !reclaimed {
		t.Fatalf("release %v reclaimed %v", relErr, reclaimed)
	}
}

func TestPGACommandClaimLeftByACrashedRequestExpires(t *testing.T) {
	// A claim (status 0) whose request died is free again after the claim lease; a live claim is not.
	// Arrange
	s, pool := newPGPool(t)
	ctx := context.Background()
	idem := store.Idempotency{Login: "alice", Command: "c", Key: "k", RequestHash: "h"}
	_, _ = s.ClaimCommand(ctx, idem)
	live, _ := s.ClaimCommand(ctx, idem)
	_, _ = pool.Exec(ctx, `UPDATE work_skill_commands SET created_at = now() - interval '1 hour'`)

	// Act
	stale, err := s.ClaimCommand(ctx, idem)

	// Assert
	if live || err != nil || !stale {
		t.Fatalf("live claim taken %v, stale claim taken %v (%v)", live, stale, err)
	}
}
