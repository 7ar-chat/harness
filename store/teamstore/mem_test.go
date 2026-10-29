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

func TestProfileAvatarAndConnectors(t *testing.T) {
	ctx := context.Background()
	m := NewMem()
	m.UpsertUser(ctx, store.User{Login: "alice", Name: "Alice A"})
	m.UpsertUser(ctx, store.User{Login: "bob", Name: "Bob B"})
	if err := m.SetDisplayName(ctx, "alice", "Ali"); err != nil {
		t.Fatal(err)
	}
	// A new login refreshes the provider's name but keeps the user's choice.
	m.UpsertUser(ctx, store.User{Login: "alice", Name: "Alice Anderson"})
	u, _ := m.GetUser(ctx, "alice")
	if u.Label() != "Ali" || u.Name != "Alice Anderson" {
		t.Fatalf("user = %+v", u)
	}
	if err := m.SetDisplayName(ctx, "nobody", "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
	v1, _ := m.SetAvatar(ctx, "alice", "image/png", []byte("one"))
	v2, _ := m.SetAvatar(ctx, "alice", "image/png", []byte("two"))
	if v1 == "" || v1 == v2 {
		t.Fatalf("versions %q %q", v1, v2)
	}
	if mime, data, err := m.GetAvatar(ctx, "alice"); err != nil || mime != "image/png" || string(data) != "two" {
		t.Fatalf("avatar = %q %q %v", mime, data, err)
	}
	u, _ = m.GetUser(ctx, "alice")
	if u.Avatar() != "/api/users/alice/avatar?v="+v2 {
		t.Fatalf("avatar url = %q", u.Avatar())
	}
	m.SetAvatar(ctx, "alice", "", nil)
	if _, _, err := m.GetAvatar(ctx, "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("avatar not removed: %v", err)
	}

	c := store.Connector{Login: "alice", Kind: store.ConnectorTelegram, Account: "111", Label: "@a", Data: []byte("sealed")}
	if err := m.SetConnector(ctx, c); err != nil {
		t.Fatal(err)
	}
	if got, err := m.FindConnector(ctx, store.ConnectorTelegram, "111"); err != nil || got.Login != "alice" {
		t.Fatalf("find = %+v, %v", got, err)
	}
	c.Login = "bob"
	if err := m.SetConnector(ctx, c); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("an account linked to two users: %v", err)
	}
	if list, _ := m.ListConnectors(ctx, "alice"); len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	if err := m.DeleteConnector(ctx, "alice", store.ConnectorTelegram); err != nil {
		t.Fatal(err)
	}
	if _, err := m.FindConnector(ctx, store.ConnectorTelegram, "111"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("still found: %v", err)
	}
}
