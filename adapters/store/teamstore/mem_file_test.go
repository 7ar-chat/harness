package teamstore_test

// Behaviour only the file-backed Mem has: persistence across Open, load and save failures.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/core/store"
)

func openMem(t *testing.T, path string) *teamstore.Mem {
	t.Helper()
	m, err := teamstore.Open(path)
	must(t, err)
	return m
}

func TestMemOpenRestoresEveryKindOfState(t *testing.T) {
	// Arrange: one store holding a bit of everything.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "team.json")
	m := openMem(t, path)
	alice, err := m.UpsertUser(ctx, store.User{Login: "alice", Email: "Alice@x.io", Name: "Alice A", AvatarURL: "https://img/a.png"})
	must(t, err)
	addUser(t, m, "bob", "bob@x.io")
	must(t, m.SetDisplayName(ctx, "alice", "Ali"))
	version, err := m.SetAvatar(ctx, "alice", "image/png", []byte("png"))
	must(t, err)
	must(t, m.SetConnector(ctx, store.Connector{Login: "alice", Kind: store.ConnectorTelegram, Account: "111", Label: "@ali", Data: []byte("sealed")}))
	must(t, m.SetUserPrefs(ctx, "alice", map[string]any{"theme": "dark", "n": float64(2)}))
	seen := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	must(t, m.SetThreadSeen(ctx, "alice", "t1", seen))
	project, err := m.CreateProject(ctx, store.Project{ID: "p1", Name: "api", RepoURL: "https://github.com/x/api", GitHubInstallation: 9}, "alice")
	must(t, err)
	must(t, m.SetMember(ctx, "p1", "bob", store.RoleViewer))
	_, err = m.CreateInvite(ctx, store.Invite{ProjectID: "p1", Email: "carol@x.io", Role: store.RoleMember, InvitedBy: "alice"})
	must(t, err)
	must(t, m.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "t1", ProjectID: "p1", Owner: "alice", CreatedBy: "bob"}))
	must(t, m.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "sup", Owner: "alice", CreatedBy: "alice", Kind: store.ThreadSupervisor}))
	must(t, m.Audit(ctx, store.AuditEntry{ProjectID: "p1", Actor: "alice", Action: "first", Detail: map[string]any{"k": "v"}}))
	must(t, m.Audit(ctx, store.AuditEntry{ProjectID: "p1", Actor: "alice", Action: "second"}))

	// Act
	r := openMem(t, path)

	// Assert: users, profile, avatar.
	got, err := r.GetUser(ctx, "alice")
	must(t, err)
	if !reflect.DeepEqual(got.Label(), "Ali") || got.Email != "alice@x.io" || got.AvatarVersion != version || !got.CreatedAt.Equal(alice.CreatedAt) {
		t.Fatalf("reloaded alice = %+v", got)
	}
	if mime, data, err := r.GetAvatar(ctx, "alice"); err != nil || mime != "image/png" || string(data) != "png" {
		t.Fatalf("avatar = %q %q %v", mime, data, err)
	}
	if u, err := r.GetUserByEmail(ctx, "bob@x.io"); err != nil || u.Login != "bob" {
		t.Fatalf("bob by email = %+v, %v", u, err)
	}
	// Connectors, prefs, seen.
	if c, err := r.FindConnector(ctx, store.ConnectorTelegram, "111"); err != nil || c.Login != "alice" || string(c.Data) != "sealed" || c.Label != "@ali" {
		t.Fatalf("connector = %+v, %v", c, err)
	}
	if prefs, _ := r.UserPrefs(ctx, "alice"); prefs["theme"] != "dark" || prefs["n"] != float64(2) {
		t.Fatalf("prefs = %v", prefs)
	}
	if s, _ := r.SeenThreads(ctx, "alice"); !s["t1"].Equal(seen) {
		t.Fatalf("seen = %v", s)
	}
	// Projects, members, invites, threads.
	if p, err := r.GetProject(ctx, "p1"); err != nil || *p != *project {
		t.Fatalf("project = %+v, %v", p, err)
	}
	if got := roles(t, r, "p1"); !sameRoles(got, map[string]string{"alice": store.RoleOwner, "bob": store.RoleViewer}) {
		t.Fatalf("members = %v", got)
	}
	if inv, _ := r.ListInvites(ctx, "p1"); len(inv) != 1 || inv[0].Email != "carol@x.io" || inv[0].Role != store.RoleMember {
		t.Fatalf("invites = %+v", inv)
	}
	if th, _ := r.GetThreadMeta(ctx, "t1"); th == nil || th.CreatedBy != "bob" || th.ProjectID != "p1" {
		t.Fatalf("thread = %+v", th)
	}
	if sup, err := r.SupervisorThread(ctx, "alice"); err != nil || sup.ThreadID != "sup" {
		t.Fatalf("supervisor = %+v, %v", sup, err)
	}
	// The reloaded invite is still claimed by its email.
	addUser(t, r, "carol", "carol@x.io")
	if role, _ := r.MemberRole(ctx, "p1", "carol"); role != store.RoleMember {
		t.Fatalf("carol = %q", role)
	}
	// The audit log and its id counter continue where they stopped.
	audit, _ := r.ListAudit(ctx, "p1", 10)
	if len(audit) != 2 || audit[1].Action != "first" || audit[1].Detail["k"] != "v" {
		t.Fatalf("audit = %+v", audit)
	}
	must(t, r.Audit(ctx, store.AuditEntry{ProjectID: "p1", Actor: "alice", Action: "third"}))
	audit, _ = r.ListAudit(ctx, "p1", 10)
	if len(audit) != 3 || audit[0].Action != "third" || audit[0].ID <= audit[1].ID {
		t.Fatalf("audit ids after reload = %+v", audit)
	}
}

func TestMemEveryMutationIsPersistedAtOnce(t *testing.T) {
	// Arrange
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "team.json")
	m := openMem(t, path)
	addUser(t, m, "alice", "alice@x.io")
	addProject(t, m, "p1", "api", "alice")
	must(t, m.SetMember(ctx, "p1", "bob", store.RoleMember))
	must(t, m.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "t1", ProjectID: "p1", Owner: "alice", CreatedBy: "alice"}))
	must(t, m.SetThreadSeen(ctx, "alice", "t1", time.Now()))
	_, err := m.CreateInvite(ctx, store.Invite{ProjectID: "p1", Email: "carol@x.io", Role: store.RoleMember})
	must(t, err)
	must(t, m.SetConnector(ctx, store.Connector{Login: "alice", Kind: store.ConnectorGitHub, Account: "a"}))
	_, err = m.SetAvatar(ctx, "alice", "image/png", []byte("x"))
	must(t, err)

	// Act: undo each of the above on the live store, and compare a reload with the live view every time.
	steps := []struct {
		name string
		do   func() error
		view func(s *teamstore.Mem) any
	}{
		{"remove member", func() error { return m.RemoveMember(ctx, "p1", "bob") }, func(s *teamstore.Mem) any { return roles(t, s, "p1") }},
		{"delete invite", func() error { return m.DeleteInvite(ctx, "p1", "carol@x.io") }, func(s *teamstore.Mem) any { v, _ := s.ListInvites(ctx, "p1"); return len(v) }},
		{"delete connector", func() error { return m.DeleteConnector(ctx, "alice", store.ConnectorGitHub) }, func(s *teamstore.Mem) any { v, _ := s.ListConnectors(ctx, "alice"); return len(v) }},
		{"remove avatar", func() error { _, err := m.SetAvatar(ctx, "alice", "", nil); return err }, func(s *teamstore.Mem) any { _, _, err := s.GetAvatar(ctx, "alice"); return err != nil }},
		{"update project", func() error {
			n := "renamed"
			_, err := m.UpdateProject(ctx, "p1", store.ProjectPatch{Name: &n})
			return err
		}, func(s *teamstore.Mem) any { p, _ := s.GetProject(ctx, "p1"); return p.Name }},
		{"delete thread link", func() error { return m.DeleteThreadMeta(ctx, "t1") }, func(s *teamstore.Mem) any {
			_, err := s.GetThreadMeta(ctx, "t1")
			seen, _ := s.SeenThreads(ctx, "alice")
			return fmt.Sprint(err, len(seen))
		}},
		{"delete project", func() error { return m.DeleteProject(ctx, "p1") }, func(s *teamstore.Mem) any { _, err := s.GetProject(ctx, "p1"); return err }},
	}
	for _, st := range steps {
		must(t, st.do())

		// Assert
		if live, reloaded := st.view(m), st.view(openMem(t, path)); !reflect.DeepEqual(live, reloaded) {
			t.Fatalf("%s: live view %v, reloaded view %v", st.name, live, reloaded)
		}
	}
}

func TestMemOpenWithoutAFileStartsEmptyAndCreatesItOnTheFirstChange(t *testing.T) {
	// Arrange: the directory of the file does not exist yet either.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "dir", "team.json")

	// Act
	m := openMem(t, path)

	// Assert: reading creates nothing.
	if users, _ := m.ListUsers(ctx); len(users) != 0 {
		t.Fatalf("users = %+v", users)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file created by Open: %v", err)
	}

	// Act
	addUser(t, m, "alice", "")

	// Assert: the first change writes the file (owner-only, no temp file left behind).
	info, err := os.Stat(path)
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want only the state file", len(entries))
	}
	if u, err := openMem(t, path).GetUser(ctx, "alice"); err != nil || u.Login != "alice" {
		t.Fatalf("reloaded = %+v, %v", u, err)
	}
}

func TestMemOpenAcceptsAnEmptyJSONObjectAndStaysUsable(t *testing.T) {
	// Arrange: a file written by an older version may lack whole sections.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "team.json")
	must(t, os.WriteFile(path, []byte(`{}`), 0o600))

	// Act
	m := openMem(t, path)

	// Assert: every kind of write works (no nil-map panic).
	addUser(t, m, "alice", "alice@x.io")
	addProject(t, m, "p1", "api", "alice")
	must(t, m.SetUserPrefs(ctx, "alice", map[string]any{"a": 1.0}))
	must(t, m.SetThreadSeen(ctx, "alice", "t", time.Now()))
	must(t, m.SetConnector(ctx, store.Connector{Login: "alice", Kind: store.ConnectorGitHub, Account: "a"}))
	_, err := m.SetAvatar(ctx, "alice", "image/png", []byte("x"))
	must(t, err)
	_, err = m.CreateInvite(ctx, store.Invite{ProjectID: "p1", Email: "bob@x.io", Role: store.RoleMember})
	must(t, err)
	must(t, m.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "t", ProjectID: "p1", Owner: "alice", CreatedBy: "alice"}))
	must(t, m.Audit(ctx, store.AuditEntry{ProjectID: "p1", Actor: "alice", Action: "x"}))
}

func TestMemOpenRefusesAnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "corrupt.json")
	must(t, os.WriteFile(corrupt, []byte(`{"users": {"alice": `), 0o600))
	wrongShape := filepath.Join(dir, "wrong.json")
	must(t, os.WriteFile(wrongShape, []byte(`{"users": ["alice"]}`), 0o600))

	for name, path := range map[string]string{
		"truncated JSON":      corrupt,
		"wrong JSON shape":    wrongShape,
		"path is a directory": dir,
	} {
		t.Run(name, func(t *testing.T) {
			// Act
			m, err := teamstore.Open(path)

			// Assert: a damaged state is reported, never silently replaced by an empty one (the next save would erase it).
			if err == nil || m != nil {
				t.Fatalf("Open = %v, %v; want an error", m, err)
			}
		})
	}
	if b, _ := os.ReadFile(corrupt); string(b) != `{"users": {"alice": ` {
		t.Fatalf("Open rewrote the damaged file: %q", b)
	}
}

func TestMemWithoutAPathNeverWritesAFile(t *testing.T) {
	// Arrange: a working directory in which any stray file would show.
	dir := t.TempDir()
	t.Chdir(dir)
	ctx := context.Background()

	for name, m := range map[string]*teamstore.Mem{"NewMem": teamstore.NewMem(), `Open("")`: openMem(t, "")} {
		// Act
		addUser(t, m, "alice", "alice@x.io")
		addProject(t, m, "p1", "api", "alice")
		must(t, m.Audit(ctx, store.AuditEntry{ProjectID: "p1", Actor: "alice", Action: "x"}))

		// Assert
		if u, err := m.GetUser(ctx, "alice"); err != nil || u.Login != "alice" {
			t.Fatalf("%s: %+v, %v", name, u, err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("files written: %v", entries)
	}
}

func TestMemMutationsReportASaveFailure(t *testing.T) {
	// Arrange: the state file can be created, then its directory is replaced by a plain file so that saving fails.
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "team.json")
	m := openMem(t, path)
	addUser(t, m, "alice", "alice@x.io")
	addProject(t, m, "p1", "api", "alice")
	must(t, os.RemoveAll(filepath.Dir(path)))
	must(t, os.WriteFile(filepath.Dir(path), []byte("not a directory"), 0o600))
	str := "x"

	// Ordered: DeleteProject goes last, the others need the project.
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"UpsertUser", func() error { _, err := m.UpsertUser(ctx, store.User{Login: "bob"}); return err }},
		{"SetDisplayName", func() error { return m.SetDisplayName(ctx, "alice", "Ali") }},
		{"SetAvatar", func() error { _, err := m.SetAvatar(ctx, "alice", "image/png", []byte("x")); return err }},
		{"SetConnector", func() error {
			return m.SetConnector(ctx, store.Connector{Login: "alice", Kind: "github", Account: "a"})
		}},
		{"SetUserPrefs", func() error { return m.SetUserPrefs(ctx, "alice", map[string]any{"a": 1}) }},
		{"SetThreadSeen", func() error { return m.SetThreadSeen(ctx, "alice", "t", time.Now()) }},
		{"CreateProject", func() error { _, err := m.CreateProject(ctx, store.Project{Name: "web"}, "alice"); return err }},
		{"UpdateProject", func() error { _, err := m.UpdateProject(ctx, "p1", store.ProjectPatch{Name: &str}); return err }},
		{"SetMember", func() error { return m.SetMember(ctx, "p1", "bob", store.RoleMember) }},
		{"CreateInvite", func() error {
			_, err := m.CreateInvite(ctx, store.Invite{ProjectID: "p1", Email: "z@x.io", Role: "member"})
			return err
		}},
		{"SetThreadMeta", func() error { return m.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "t", Owner: "alice"}) }},
		{"DeleteThreadMeta", func() error { return m.DeleteThreadMeta(ctx, "t") }},
		{"Audit", func() error { return m.Audit(ctx, store.AuditEntry{ProjectID: "p1", Actor: "alice", Action: "x"}) }},
		{"DeleteProject", func() error { return m.DeleteProject(ctx, "p1") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Act / Assert
			if err := tc.call(); err == nil || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
				t.Fatalf("err = %v, want the failure to write the state file", err)
			}
		})
	}
}

func TestMemSaveFailureWhenTheTempFileCannotBeCreated(t *testing.T) {
	// Arrange: the temp file's name is taken by a directory.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "team.json")
	must(t, os.Mkdir(path+".tmp", 0o700))
	m := openMem(t, path)

	// Act
	_, err := m.UpsertUser(ctx, store.User{Login: "alice"})

	// Assert
	if err == nil {
		t.Fatal("the failed write was not reported")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a state file appeared despite the failure: %v", statErr)
	}
}

func TestMemFileConcurrentMutationsAreAllPersisted(t *testing.T) {
	// Arrange
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "team.json")
	m := openMem(t, path)
	addProject(t, m, "p1", "api", "owner")
	const n = 16

	// Act: parallel writers of different kinds, all saving to the same file.
	start := make(chan struct{})
	var wg sync.WaitGroup
	errc := make(chan error, 3*n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			login := fmt.Sprintf("u%d", i)
			_, err := m.UpsertUser(ctx, store.User{Login: login, Email: login + "@x.io"})
			errc <- err
			errc <- m.SetMember(ctx, "p1", login, store.RoleMember)
			errc <- m.Audit(ctx, store.AuditEntry{ProjectID: "p1", Actor: login, Action: "join"})
		}()
	}
	close(start)
	wg.Wait()
	close(errc)

	// Assert: the file read back by a new process holds all of it, and the file itself is valid.
	for err := range errc {
		must(t, err)
	}
	r := openMem(t, path)
	if got := roles(t, r, "p1"); len(got) != n+1 {
		t.Fatalf("reloaded %d members, want %d", len(got), n+1)
	}
	if users, _ := r.ListUsers(ctx); len(users) != n {
		t.Fatalf("reloaded %d users, want %d", len(users), n)
	}
	audit, _ := r.ListAudit(ctx, "p1", 1000)
	ids := map[int64]bool{}
	for _, e := range audit {
		ids[e.ID] = true
	}
	if len(audit) != n || len(ids) != n {
		t.Fatalf("reloaded %d audit entries with %d distinct ids", len(audit), len(ids))
	}
}

func TestMemAuditKeepsOnlyTheLatestFiveThousandEntries(t *testing.T) {
	// Arrange
	ctx := context.Background()
	m := teamstore.NewMem()
	const total = 5003

	// Act
	for i := range total {
		must(t, m.Audit(ctx, store.AuditEntry{ProjectID: "p1", Actor: "alice", Action: fmt.Sprintf("a%d", i)}))
	}

	// Assert: the oldest fell off, the newest are intact and ids keep growing.
	all, err := m.ListAudit(ctx, "p1", total)
	must(t, err)
	if len(all) != 5000 || all[0].Action != "a5002" || all[0].ID != total || all[4999].Action != "a3" {
		t.Fatalf("kept %d entries, newest %q (id %d), oldest %q", len(all), all[0].Action, all[0].ID, all[len(all)-1].Action)
	}
}
