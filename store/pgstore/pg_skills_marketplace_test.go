package pgstore_test

import (
	"context"
	"errors"
	"github.com/GhaziBenDahmane/harness/store/pgstore"
	"testing"
	"testing/fstest"

	"github.com/GhaziBenDahmane/harness/skills"
	"github.com/GhaziBenDahmane/harness/store"
)

func pgGlobal(t *testing.T, s *pgstore.PG, name string) *store.GlobalSkill {
	t.Helper()
	g, _, err := s.CreateGlobalSkill(context.Background(), store.GlobalSkill{Name: name, Description: "d", CreatedBy: "admin"},
		store.SkillVersion{Version: "1.0.0", Name: name, Description: "d", SHA256: "x", Package: []byte("zip"), CreatedBy: "admin"}, true)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestPGMarketplaceSourcesFollowTheirGlobalSkill(t *testing.T) {
	// A source needs its global skill, is replaced by an upsert and disappears with the skill (ON DELETE CASCADE).
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	g := pgGlobal(t, s, "studio-studio-docx")
	src := store.MarketplaceSource{GlobalSkillID: g.ID, RepositoryURL: "https://git.example.com/a/b.git", Branch: "main",
		SourcePath: "skills/studio/studio-docx", Namespace: "studio", CommitSHA: "abc", DeclaredVersion: "1.0"}

	// Act
	missing := s.UpsertMarketplaceSource(ctx, store.MarketplaceSource{GlobalSkillID: "8f0c7f0e-6a55-4d1e-9a39-5b9b2b8f1c11"})
	errFirst := s.UpsertMarketplaceSource(ctx, src)
	src.Deprecated, src.CommitSHA = true, "def"
	errSecond := s.UpsertMarketplaceSource(ctx, src)
	listed, _ := s.MarketplaceSources(ctx)
	_ = s.DeleteGlobalSkill(ctx, g.ID)
	afterDelete, _ := s.MarketplaceSources(ctx)

	// Assert
	if !errors.Is(missing, store.ErrNotFound) || errFirst != nil || errSecond != nil {
		t.Fatalf("upserts: %v / %v / %v", missing, errFirst, errSecond)
	}
	if len(listed) != 1 || !listed[0].Deprecated || listed[0].CommitSHA != "def" || listed[0].Namespace != "studio" || listed[0].LastSyncedAt.IsZero() {
		t.Fatalf("listed = %+v", listed)
	}
	if len(afterDelete) != 0 {
		t.Fatalf("source outlived its skill: %+v", afterDelete)
	}
}

func TestPGMarketplaceRunsAndGlobalSkillInfo(t *testing.T) {
	// Runs store their counts and issues; UpdateGlobalSkillInfo changes the display name and description.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	g := pgGlobal(t, s, "tls-jira")
	run := &store.MarketplaceRun{Trigger: "scheduled", RepositoryURL: "https://git.example.com/a/b.git", Branch: "main"}

	// Act
	if err := s.CreateMarketplaceRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	run.Status, run.CommitSHA, run.Created, run.Failed = store.MarketplaceRunPartial, "abc", 3, 1
	run.Errors = []store.MarketplaceIssue{{Path: "skills/fc/fc-cypress", Message: "multiple_skill_md"}}
	finishErr := s.FinishMarketplaceRun(ctx, run)
	runs, _ := s.ListMarketplaceRuns(ctx, 5)
	infoErr := s.UpdateGlobalSkillInfo(ctx, g.ID, "Jira", "Jira helper.")
	got, _ := s.GetGlobalSkill(ctx, g.ID)

	// Assert
	if finishErr != nil || run.FinishedAt == nil {
		t.Fatalf("finish: %v %v", finishErr, run.FinishedAt)
	}
	if len(runs) != 1 || runs[0].Status != store.MarketplaceRunPartial || runs[0].Created != 3 || runs[0].Failed != 1 ||
		len(runs[0].Errors) != 1 || runs[0].Errors[0].Path != "skills/fc/fc-cypress" {
		t.Fatalf("runs = %+v", runs)
	}
	if infoErr != nil || got.DisplayName != "Jira" || got.Description != "Jira helper." {
		t.Fatalf("info: %v %+v", infoErr, got)
	}
}

func TestPGMarketplaceSyncEndToEnd(t *testing.T) {
	// The sync over Postgres: create, idempotent re-sync, new version, unpublish of a removed path.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	md := func(name, extra string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte("---\nname: " + name + "\ndescription: " + name + " skill.\n" + extra + "---\n# x\n")}
	}
	repo := fstest.MapFS{
		"skills/studio/studio-pdf/SKILL.md": md("studio-pdf", "tags: [studio-work-default]\n"),
		"skills/tls/jira/SKILL.md":          md("jira", ""),
	}
	reg := skills.NewRegistry(nil, s)
	syncer := &skills.MarketplaceSyncer{Registry: reg, Catalog: s, Market: s, RepositoryURL: "https://git.example.com/a/b.git",
		Branch: "main", Source: skills.FSMarketplaceSource{FS: repo, CommitSHA: "1111111111"}}

	// Act
	first, err1 := syncer.Sync(ctx, skills.MarketplaceTriggerCommand)
	second, err2 := syncer.Sync(ctx, skills.MarketplaceTriggerCommand)
	repo["skills/studio/studio-pdf/SKILL.md"] = md("studio-pdf", "tags: [studio-work-default, pdf]\nversion: \"2.0\"\n")
	delete(repo, "skills/tls/jira/SKILL.md")
	syncer.Source = skills.FSMarketplaceSource{FS: repo, CommitSHA: "2222222222"}
	third, err3 := syncer.Sync(ctx, skills.MarketplaceTriggerCommand)
	userView, _ := reg.List(ctx, "alice")

	// Assert
	if err1 != nil || err2 != nil || err3 != nil {
		t.Fatalf("errors: %v %v %v", err1, err2, err3)
	}
	if first.Created != 2 || second.Unchanged != 2 || third.Updated != 1 || third.Deprecated != 1 || third.Status != store.MarketplaceRunSucceeded {
		t.Fatalf("runs: %+v / %+v / %+v", first, second, third)
	}
	seen := map[string]skills.Skill{}
	for _, sk := range userView {
		seen[sk.Name] = sk
	}
	if pdf := seen["studio-studio-pdf"]; pdf.VersionLabel != "2.0" || !pdf.Allowed || len(pdf.Tags) != 2 {
		t.Fatalf("pdf = %+v", pdf)
	}
	if _, ok := seen["tls-jira"]; ok {
		t.Fatal("removed skill still visible to users")
	}
}
