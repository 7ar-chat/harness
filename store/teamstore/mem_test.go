package teamstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/GhaziBenDahmane/harness/store"
)

func TestProjectsMembersInvites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "team.json")
	m, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.UpsertUser(ctx, store.User{Login: "alice", Email: "Alice@x.io", Name: "Alice"}); err != nil {
		t.Fatal(err)
	}
	p, err := m.CreateProject(ctx, store.Project{Name: "api", RepoURL: "https://github.com/x/api"}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if r, err := m.MemberRole(ctx, p.ID, "alice"); err != nil || r != store.RoleOwner {
		t.Fatalf("owner role = %q, %v", r, err)
	}
	if _, err := m.MemberRole(ctx, p.ID, "bob"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bob is not a member: %v", err)
	}
	// An invite to an unknown email waits for the login of that email.
	if iv, err := m.CreateInvite(ctx, store.Invite{ProjectID: p.ID, Email: "bob@x.io", Role: store.RoleMember, InvitedBy: "alice"}); err != nil || iv == nil {
		t.Fatalf("invite = %v, %v", iv, err)
	}
	if _, err := m.UpsertUser(ctx, store.User{Login: "bob", Email: "BOB@x.io"}); err != nil {
		t.Fatal(err)
	}
	if r, _ := m.MemberRole(ctx, p.ID, "bob"); r != store.RoleMember {
		t.Fatalf("bob role = %q, want member", r)
	}
	if inv, _ := m.ListInvites(ctx, p.ID); len(inv) != 0 {
		t.Fatalf("invite not consumed: %v", inv)
	}
	// An invite to a known user adds at once.
	if iv, err := m.CreateInvite(ctx, store.Invite{ProjectID: p.ID, Email: "alice@x.io", Role: store.RoleOwner}); err != nil || iv != nil {
		t.Fatalf("known email invite = %v, %v", iv, err)
	}
	// The last owner can neither leave nor be demoted.
	if err := m.RemoveMember(ctx, p.ID, "alice"); !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("remove last owner: %v", err)
	}
	if err := m.SetMember(ctx, p.ID, "alice", store.RoleViewer); !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("demote last owner: %v", err)
	}
	// Threads.
	if err := m.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "t1", ProjectID: p.ID, Owner: "alice", CreatedBy: "bob"}); err != nil {
		t.Fatal(err)
	}
	if ths, _ := m.ListProjectThreads(ctx, p.ID); len(ths) != 1 || ths[0].CreatedBy != "bob" {
		t.Fatalf("threads = %+v", ths)
	}
	// Persistence.
	m2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := m2.ListProjects(ctx, "bob"); len(list) != 1 || list[0].Role != store.RoleMember {
		t.Fatalf("reloaded projects = %+v", list)
	}
	if err := m2.DeleteProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.GetThreadMeta(ctx, "t1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("thread link survived the project: %v", err)
	}
}

func TestRoleAtLeast(t *testing.T) {
	if !store.RoleAtLeast(store.RoleOwner, store.RoleMember) || store.RoleAtLeast(store.RoleViewer, store.RoleMember) || store.RoleAtLeast("", store.RoleViewer) {
		t.Fatal("role order")
	}
}
