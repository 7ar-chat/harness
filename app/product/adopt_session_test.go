package product

import (
	"context"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/core/store"
)

func TestFactoryHooksAdoptOnlyProjectSessionsOfARepositoryTheAppReaches(t *testing.T) {
	ctx := context.Background()
	tm := teamstore.NewMem()
	cnUsers(t, tm, "alice")
	withApp, _ := tm.CreateProject(ctx, store.Project{Name: "api", RepoURL: "https://github.com/acme/api", DefaultBranch: "main", GitHubInstallation: 42}, "alice")
	noApp, _ := tm.CreateProject(ctx, store.Project{Name: "web", RepoURL: "https://github.com/acme/web", DefaultBranch: "main"}, "alice")
	noRepo, _ := tm.CreateProject(ctx, store.Project{Name: "notes"}, "alice")
	meta := func(id, project, kind string) {
		if err := tm.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: id, ProjectID: project, Owner: "alice", CreatedBy: "alice", Kind: kind}); err != nil {
			t.Fatal(err)
		}
	}
	meta("t-ok", withApp.ID, store.ThreadChat)
	meta("t-noapp", noApp.ID, store.ThreadChat)
	meta("t-norepo", noRepo.ID, store.ThreadChat)
	meta("t-personal", "", store.ThreadChat)
	meta("t-sup", withApp.ID, store.ThreadSupervisor)
	h := &FactoryHooks{Team: tm}

	for id, want := range map[string]bool{"t-ok": true, "t-noapp": false, "t-norepo": false, "t-personal": false, "t-sup": false, "unknown": false} {
		ad, err := h.AdoptSession(ctx, id)
		if err != nil || (ad != nil) != want {
			t.Errorf("%s: adoption = %v, err = %v, want adoptable %v", id, ad, err, want)
		}
	}
	ad, _ := h.AdoptSession(ctx, "t-ok")
	if ad.Info.Repo != "acme/api" || ad.Info.Branch != branchFor("t-ok") || ad.Info.BaseBranch != "main" || ad.Task.ProjectID != withApp.ID || ad.Task.Owner != "alice" {
		t.Errorf("adoption = %+v", ad)
	}
}
