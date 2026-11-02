package teamstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/core/store"
)

// A database that cannot be reached must surface as an error from every operation; in particular
// it must not be mistaken for "not found" (callers treat that as a normal answer) nor yield data.
func TestPGSurfacesDatabaseFailures(t *testing.T) {
	// Arrange: a pool that was closed, so that every statement fails. No server is needed.
	cfg, err := pgxpool.ParseConfig("postgres://harness:harness@127.0.0.1:1/harness?sslmode=disable")
	must(t, err)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	must(t, err)
	pool.Close()
	s := teamstore.NewPG(pool)
	ctx := context.Background()
	str := "x"

	calls := []struct {
		name string
		call func() error
	}{
		{"UpsertUser", func() error { _, err := s.UpsertUser(ctx, store.User{Login: "a", Email: "a@x.io"}); return err }},
		{"GetUser", func() error { _, err := s.GetUser(ctx, "a"); return err }},
		{"GetUsers", func() error { _, err := s.GetUsers(ctx, []string{"a"}); return err }},
		{"GetUserByEmail", func() error { _, err := s.GetUserByEmail(ctx, "a@x.io"); return err }},
		{"ListUsers", func() error { _, err := s.ListUsers(ctx); return err }},
		{"SetDisplayName", func() error { return s.SetDisplayName(ctx, "a", "A") }},
		{"SetAvatar upload", func() error { _, err := s.SetAvatar(ctx, "a", "image/png", []byte("x")); return err }},
		{"SetAvatar remove", func() error { _, err := s.SetAvatar(ctx, "a", "", nil); return err }},
		{"GetAvatar", func() error { _, _, err := s.GetAvatar(ctx, "a"); return err }},
		{"SetConnector", func() error { return s.SetConnector(ctx, store.Connector{Login: "a", Kind: "github", Account: "a"}) }},
		{"GetConnector", func() error { _, err := s.GetConnector(ctx, "a", "github"); return err }},
		{"ListConnectors", func() error { _, err := s.ListConnectors(ctx, "a"); return err }},
		{"DeleteConnector", func() error { return s.DeleteConnector(ctx, "a", "github") }},
		{"FindConnector", func() error { _, err := s.FindConnector(ctx, "github", "a"); return err }},
		{"UserPrefs", func() error { _, err := s.UserPrefs(ctx, "a"); return err }},
		{"SetUserPrefs", func() error { return s.SetUserPrefs(ctx, "a", map[string]any{"a": 1}) }},
		{"SeenThreads", func() error { _, err := s.SeenThreads(ctx, "a"); return err }},
		{"SetThreadSeen", func() error { return s.SetThreadSeen(ctx, "a", "t", time.Now()) }},
		{"CreateProject", func() error { _, err := s.CreateProject(ctx, store.Project{Name: "p"}, "a"); return err }},
		{"GetProject", func() error { _, err := s.GetProject(ctx, "p"); return err }},
		{"UpdateProject", func() error { _, err := s.UpdateProject(ctx, "p", store.ProjectPatch{Name: &str}); return err }},
		{"DeleteProject", func() error { return s.DeleteProject(ctx, "p") }},
		{"ListProjects", func() error { _, err := s.ListProjects(ctx, "a"); return err }},
		{"MemberRole", func() error { _, err := s.MemberRole(ctx, "p", "a"); return err }},
		{"ListMembers", func() error { _, err := s.ListMembers(ctx, "p"); return err }},
		{"SetMember owner", func() error { return s.SetMember(ctx, "p", "a", store.RoleOwner) }},
		{"SetMember member", func() error { return s.SetMember(ctx, "p", "a", store.RoleMember) }},
		{"RemoveMember", func() error { return s.RemoveMember(ctx, "p", "a") }},
		{"CreateInvite", func() error {
			_, err := s.CreateInvite(ctx, store.Invite{ProjectID: "p", Email: "a@x.io", Role: store.RoleMember})
			return err
		}},
		{"ListInvites", func() error { _, err := s.ListInvites(ctx, "p"); return err }},
		{"DeleteInvite", func() error { return s.DeleteInvite(ctx, "p", "a@x.io") }},
		{"SetThreadMeta", func() error { return s.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "t", ProjectID: "p", Owner: "a"}) }},
		{"GetThreadMeta", func() error { _, err := s.GetThreadMeta(ctx, "t"); return err }},
		{"ListProjectThreads", func() error { _, err := s.ListProjectThreads(ctx, "p"); return err }},
		{"SupervisorThread", func() error { _, err := s.SupervisorThread(ctx, "a"); return err }},
		{"DeleteThreadMeta", func() error { return s.DeleteThreadMeta(ctx, "t") }},
		{"Audit", func() error { return s.Audit(ctx, store.AuditEntry{ProjectID: "p", Actor: "a", Action: "x"}) }},
		{"ListAudit", func() error { _, err := s.ListAudit(ctx, "p", 10); return err }},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			err := tc.call()

			// Assert
			if err == nil || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrLastOwner) {
				t.Fatalf("err = %v, want the database failure", err)
			}
		})
	}
}

func TestPGInvalidInputIsRefusedBeforeTouchingTheDatabase(t *testing.T) {
	// Arrange: no usable database at all.
	cfg, err := pgxpool.ParseConfig("postgres://harness:harness@127.0.0.1:1/harness?sslmode=disable")
	must(t, err)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	must(t, err)
	pool.Close()
	s := teamstore.NewPG(pool)
	ctx := context.Background()

	// Act / Assert: a role outside owner/member/viewer is a conflict, whatever the database state.
	wantErr(t, "SetMember", s.SetMember(ctx, "p", "a", "admin"), store.ErrConflict)
	_, err = s.CreateInvite(ctx, store.Invite{ProjectID: "p", Email: "a@x.io", Role: ""})
	wantErr(t, "CreateInvite", err, store.ErrConflict)
	if users, err := s.GetUsers(ctx, nil); err != nil || users == nil || len(users) != 0 {
		t.Fatalf("GetUsers(nil) = %#v, %v", users, err)
	}
}
