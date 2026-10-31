package teamstore_test

// The TeamStore conformance suite: the same behavioural cases run against every
// backend (in-memory, file-backed, Postgres). Only what the store.TeamStore port
// guarantees is asserted here; backend-specific behaviour has its own tests.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/core/store"
)

// eachStore runs fn against a fresh store of every backend (Postgres is skipped when unconfigured).
func eachStore(t *testing.T, fn func(t *testing.T, s store.TeamStore)) {
	t.Helper()
	backends := []struct {
		name string
		open func(t *testing.T) store.TeamStore
	}{
		{"mem", func(*testing.T) store.TeamStore { return teamstore.NewMem() }},
		{"file", func(t *testing.T) store.TeamStore {
			m, err := teamstore.Open(filepath.Join(t.TempDir(), "team.json"))
			if err != nil {
				t.Fatal(err)
			}
			return m
		}},
		{"pg", func(t *testing.T) store.TeamStore { return newPG(t) }},
	}
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) { fn(t, b.open(t)) })
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err = %v, want %v", what, err, want)
	}
}

// pastClock blocks until the wall clock is strictly after at, so that two consecutive writes of the in-memory
// store get distinct timestamps even on a coarse clock. Postgres stamps rows with its own clock and needs no help.
func pastClock(t *testing.T, s store.TeamStore, at time.Time) {
	t.Helper()
	if _, mem := s.(*teamstore.Mem); !mem {
		return
	}
	deadline := time.Now().Add(time.Second)
	for !time.Now().After(at) {
		if time.Now().After(deadline) {
			t.Fatal("clock did not advance")
		}
	}
}

func addUser(t *testing.T, s store.TeamStore, login, email string) *store.User {
	t.Helper()
	u, err := s.UpsertUser(context.Background(), store.User{Login: login, Email: email, Name: strings.ToUpper(login[:1]) + login[1:]})
	must(t, err)
	return u
}

func addProject(t *testing.T, s store.TeamStore, id, name, owner string) *store.Project {
	t.Helper()
	p, err := s.CreateProject(context.Background(), store.Project{ID: id, Name: name}, owner)
	must(t, err)
	return p
}

// roles returns the members of a project as login → role.
func roles(t *testing.T, s store.TeamStore, projectID string) map[string]string {
	t.Helper()
	list, err := s.ListMembers(context.Background(), projectID)
	must(t, err)
	out := map[string]string{}
	for _, m := range list {
		if m.ProjectID != projectID {
			t.Fatalf("member %+v listed under %s", m, projectID)
		}
		out[m.Login] = m.Role
	}
	if len(out) != len(list) {
		t.Fatalf("duplicate members: %+v", list)
	}
	return out
}

func sameRoles(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func TestTeamUpsertUserNormalisesEmailAndKeepsTheProfile(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()

		// Act
		first, err := s.UpsertUser(ctx, store.User{Login: "alice", Email: "  Alice@Example.IO ", Name: "Alice A", AvatarURL: "https://img/a.png"})
		must(t, err)

		// Assert: the email is trimmed and lower-cased, the provider's fields are recorded.
		if first.Login != "alice" || first.Email != "alice@example.io" || first.Name != "Alice A" || first.AvatarURL != "https://img/a.png" {
			t.Fatalf("upserted = %+v", first)
		}
		if first.CreatedAt.IsZero() || first.LastSeen.IsZero() {
			t.Fatalf("timestamps not set: %+v", first)
		}
		got, err := s.GetUser(ctx, "alice")
		must(t, err)
		if got.Email != first.Email || got.Name != first.Name || got.AvatarURL != first.AvatarURL || !got.CreatedAt.Equal(first.CreatedAt) {
			t.Fatalf("stored %+v differs from returned %+v", got, first)
		}

		// Act: the user chose a name and avatar, then logs in again with refreshed provider data and no email.
		must(t, s.SetDisplayName(ctx, "alice", "Ali"))
		version, err := s.SetAvatar(ctx, "alice", "image/png", []byte("png"))
		must(t, err)
		pastClock(t, s, first.LastSeen)
		second, err := s.UpsertUser(ctx, store.User{Login: "alice", Name: "Alice Anderson", AvatarURL: "https://img/b.png"})
		must(t, err)

		// Assert: provider fields are overwritten, the user's own choices and creation time are kept.
		if second.Email != "" || second.Name != "Alice Anderson" || second.AvatarURL != "https://img/b.png" {
			t.Fatalf("provider fields not refreshed: %+v", second)
		}
		if second.DisplayName != "Ali" || second.AvatarVersion != version || second.Label() != "Ali" {
			t.Fatalf("user's own choices lost: %+v", second)
		}
		if !second.CreatedAt.Equal(first.CreatedAt) {
			t.Fatalf("CreatedAt moved: %v -> %v", first.CreatedAt, second.CreatedAt)
		}
		if !second.LastSeen.After(first.LastSeen) {
			t.Fatalf("LastSeen not advanced: %v -> %v", first.LastSeen, second.LastSeen)
		}
		if _, err := s.GetUserByEmail(ctx, "alice@example.io"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("the cleared email still resolves: %v", err)
		}
	})
}

func TestTeamUpsertUserClaimsEveryInviteSentToItsEmail(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange: alice owns two projects and invites bob to both (with different roles) and carol to one.
		ctx := context.Background()
		addUser(t, s, "alice", "alice@x.io")
		addProject(t, s, "p1", "api", "alice")
		addProject(t, s, "p2", "web", "alice")
		for _, iv := range []store.Invite{
			{ProjectID: "p1", Email: "bob@x.io", Role: store.RoleMember, InvitedBy: "alice"},
			{ProjectID: "p2", Email: "BOB@x.io", Role: store.RoleViewer, InvitedBy: "alice"},
			{ProjectID: "p1", Email: "carol@x.io", Role: store.RoleMember, InvitedBy: "alice"},
		} {
			got, err := s.CreateInvite(ctx, iv)
			must(t, err)
			if got == nil {
				t.Fatalf("invite for the unknown %s was not kept pending", iv.Email)
			}
		}

		// Act: bob logs in, with the email in another case.
		_, err := s.UpsertUser(ctx, store.User{Login: "bob", Email: " Bob@X.io"})
		must(t, err)

		// Assert: both memberships exist with the invited roles and only bob's invites are consumed.
		if r, _ := s.MemberRole(ctx, "p1", "bob"); r != store.RoleMember {
			t.Fatalf("p1 role = %q", r)
		}
		if r, _ := s.MemberRole(ctx, "p2", "bob"); r != store.RoleViewer {
			t.Fatalf("p2 role = %q", r)
		}
		p1, _ := s.ListInvites(ctx, "p1")
		p2, _ := s.ListInvites(ctx, "p2")
		if len(p1) != 1 || p1[0].Email != "carol@x.io" || len(p2) != 0 {
			t.Fatalf("remaining invites p1=%+v p2=%+v", p1, p2)
		}

		// Act: logging in again changes nothing.
		_, err = s.UpsertUser(ctx, store.User{Login: "bob", Email: "bob@x.io"})
		must(t, err)
		if r, _ := s.MemberRole(ctx, "p1", "bob"); r != store.RoleMember {
			t.Fatalf("p1 role after relogin = %q", r)
		}
	})
}

func TestTeamUpsertUserWithoutEmailClaimsNothing(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		addUser(t, s, "alice", "alice@x.io")
		addProject(t, s, "p1", "api", "alice")
		_, err := s.CreateInvite(ctx, store.Invite{ProjectID: "p1", Email: "bob@x.io", Role: store.RoleMember, InvitedBy: "alice"})
		must(t, err)

		// Act: a user without email (some providers hide it) logs in.
		_, err = s.UpsertUser(ctx, store.User{Login: "bob"})
		must(t, err)

		// Assert
		if _, err := s.MemberRole(ctx, "p1", "bob"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("membership granted without an email: %v", err)
		}
		if inv, _ := s.ListInvites(ctx, "p1"); len(inv) != 1 {
			t.Fatalf("invite consumed: %+v", inv)
		}
	})
}

func TestTeamUserLookups(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		if list, err := s.ListUsers(ctx); err != nil || len(list) != 0 {
			t.Fatalf("empty store lists %+v, %v", list, err)
		}
		addUser(t, s, "carol", "carol@x.io")
		addUser(t, s, "alice", "Alice@x.io")
		addUser(t, s, "bob", "") // no email

		// Assert: GetUser.
		if u, err := s.GetUser(ctx, "alice"); err != nil || u.Login != "alice" || u.Email != "alice@x.io" {
			t.Fatalf("GetUser = %+v, %v", u, err)
		}
		if _, err := s.GetUser(ctx, "nobody"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown login: %v", err)
		}
		if _, err := s.GetUser(ctx, ""); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("empty login: %v", err)
		}

		// Assert: GetUsers returns the known ones only, by login, and an empty (non-nil) map for no input.
		got, err := s.GetUsers(ctx, []string{"carol", "ghost", "alice", "alice"})
		must(t, err)
		if len(got) != 2 || got["carol"] == nil || got["carol"].Login != "carol" || got["alice"] == nil || got["alice"].Email != "alice@x.io" {
			t.Fatalf("GetUsers = %+v", got)
		}
		for _, in := range [][]string{nil, {}, {"ghost"}} {
			got, err := s.GetUsers(ctx, in)
			if err != nil || got == nil || len(got) != 0 {
				t.Fatalf("GetUsers(%v) = %#v, %v; want an empty map", in, got, err)
			}
		}

		// Assert: GetUserByEmail ignores case and surrounding blanks, and never matches an empty email.
		for _, q := range []string{"carol@x.io", "  CAROL@X.IO "} {
			if u, err := s.GetUserByEmail(ctx, q); err != nil || u.Login != "carol" {
				t.Fatalf("GetUserByEmail(%q) = %+v, %v", q, u, err)
			}
		}
		for _, q := range []string{"", "   ", "ghost@x.io"} {
			if u, err := s.GetUserByEmail(ctx, q); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetUserByEmail(%q) = %+v, %v; want ErrNotFound", q, u, err)
			}
		}

		// Assert: ListUsers holds every user once (order is not part of the port).
		list, err := s.ListUsers(ctx)
		must(t, err)
		logins := []string{}
		for _, u := range list {
			logins = append(logins, u.Login)
		}
		sort.Strings(logins)
		if strings.Join(logins, ",") != "alice,bob,carol" {
			t.Fatalf("ListUsers = %v", logins)
		}
	})
}

func TestTeamReturnedValuesAreCopies(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		addUser(t, s, "alice", "alice@x.io")
		addProject(t, s, "p1", "api", "alice")

		// Act: the caller scribbles over what it got back.
		u, _ := s.GetUser(ctx, "alice")
		u.Name, u.Email = "changed", "changed@x.io"
		list, _ := s.ListUsers(ctx)
		list[0].Name = "changed"
		p, _ := s.GetProject(ctx, "p1")
		p.Name = "changed"
		members, _ := s.ListMembers(ctx, "p1")
		members[0].Role = store.RoleViewer

		// Assert
		if u, _ := s.GetUser(ctx, "alice"); u.Name != "Alice" || u.Email != "alice@x.io" {
			t.Fatalf("user changed through a returned value: %+v", u)
		}
		if p, _ := s.GetProject(ctx, "p1"); p.Name != "api" {
			t.Fatalf("project changed through a returned value: %+v", p)
		}
		if r, _ := s.MemberRole(ctx, "p1", "alice"); r != store.RoleOwner {
			t.Fatalf("membership changed through a returned value: %q", r)
		}
	})
}

func TestTeamDisplayNameAndAvatar(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		_, err := s.UpsertUser(ctx, store.User{Login: "alice", Name: "Alice A", AvatarURL: "https://img/provider.png"})
		must(t, err)
		addUser(t, s, "bob", "")

		// Act / Assert: display name set, replaced, cleared.
		if u, _ := s.GetUser(ctx, "alice"); u.Label() != "Alice A" {
			t.Fatalf("default label = %q", u.Label())
		}
		must(t, s.SetDisplayName(ctx, "alice", "Ali"))
		must(t, s.SetDisplayName(ctx, "alice", "Ally"))
		if u, _ := s.GetUser(ctx, "alice"); u.DisplayName != "Ally" || u.Label() != "Ally" {
			t.Fatalf("after replace: %+v", u)
		}
		must(t, s.SetDisplayName(ctx, "alice", ""))
		if u, _ := s.GetUser(ctx, "alice"); u.DisplayName != "" || u.Label() != "Alice A" {
			t.Fatalf("after clearing: %+v", u)
		}
		wantErr(t, "display name of an unknown user", s.SetDisplayName(ctx, "nobody", "x"), store.ErrNotFound)

		// Act / Assert: the avatar is stored with its mime type and identified by a content version.
		if _, _, err := s.GetAvatar(ctx, "alice"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("avatar before upload: %v", err)
		}
		img := []byte{0x89, 'P', 'N', 'G', 0, 1, 2}
		v1, err := s.SetAvatar(ctx, "alice", "image/png", img)
		must(t, err)
		img[0] = 0 // the store must not alias the caller's buffer
		mime, data, err := s.GetAvatar(ctx, "alice")
		must(t, err)
		if mime != "image/png" || string(data) != "\x89PNG\x00\x01\x02" {
			t.Fatalf("avatar = %q %q", mime, data)
		}
		data[1] = 0 // nor hand out its own
		if _, again, _ := s.GetAvatar(ctx, "alice"); string(again) != "\x89PNG\x00\x01\x02" {
			t.Fatalf("avatar changed through a returned buffer: %q", again)
		}
		u, _ := s.GetUser(ctx, "alice")
		if v1 == "" || u.AvatarVersion != v1 || u.Avatar() != "/api/users/alice/avatar?v="+v1 {
			t.Fatalf("version %q, user %+v, url %q", v1, u, u.Avatar())
		}

		// Replacing changes the version (so caches refresh) and the mime type; the same bytes keep their version.
		v2, err := s.SetAvatar(ctx, "alice", "image/jpeg", []byte("jpeg bytes"))
		must(t, err)
		v2again, err := s.SetAvatar(ctx, "alice", "image/jpeg", []byte("jpeg bytes"))
		must(t, err)
		if v2 == "" || v2 == v1 || v2again != v2 {
			t.Fatalf("versions v1=%q v2=%q again=%q", v1, v2, v2again)
		}
		if mime, data, _ := s.GetAvatar(ctx, "alice"); mime != "image/jpeg" || string(data) != "jpeg bytes" {
			t.Fatalf("replaced avatar = %q %q", mime, data)
		}
		if _, _, err := s.GetAvatar(ctx, "bob"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("bob inherited alice's avatar: %v", err)
		}

		// Empty data removes it: the provider's picture shows again.
		v, err := s.SetAvatar(ctx, "alice", "", nil)
		must(t, err)
		if v != "" {
			t.Fatalf("removal returned version %q", v)
		}
		if _, _, err := s.GetAvatar(ctx, "alice"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("avatar not removed: %v", err)
		}
		if u, _ := s.GetUser(ctx, "alice"); u.AvatarVersion != "" || u.Avatar() != "https://img/provider.png" {
			t.Fatalf("after removal: %+v", u)
		}
		if v, err := s.SetAvatar(ctx, "alice", "", []byte{}); err != nil || v != "" {
			t.Fatalf("removing a missing avatar = %q, %v", v, err)
		}

		// Unknown users never get an avatar, uploaded or removed.
		if _, err := s.SetAvatar(ctx, "nobody", "image/png", img); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("avatar of an unknown user: %v", err)
		}
		if _, err := s.SetAvatar(ctx, "nobody", "", nil); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("avatar removal of an unknown user: %v", err)
		}
		if _, _, err := s.GetAvatar(ctx, "nobody"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetAvatar of an unknown user: %v", err)
		}
	})
}

func TestTeamConnectors(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		addUser(t, s, "alice", "")
		addUser(t, s, "bob", "")
		gh := store.Connector{Login: "alice", Kind: store.ConnectorGitHub, Account: "alice-gh", Label: "@alice", Data: []byte("sealed-1")}
		tg := store.Connector{Login: "alice", Kind: store.ConnectorTelegram, Account: "111", Label: "@ali", Data: nil}

		// Act: connectors of one user, listed in kind order whatever the insertion order.
		must(t, s.SetConnector(ctx, tg))
		must(t, s.SetConnector(ctx, gh))

		// Assert
		list, err := s.ListConnectors(ctx, "alice")
		must(t, err)
		if len(list) != 2 || list[0].Kind != store.ConnectorGitHub || list[1].Kind != store.ConnectorTelegram {
			t.Fatalf("list = %+v", list)
		}
		got, err := s.GetConnector(ctx, "alice", store.ConnectorGitHub)
		must(t, err)
		if got.Login != "alice" || got.Account != "alice-gh" || got.Label != "@alice" || string(got.Data) != "sealed-1" || got.CreatedAt.IsZero() {
			t.Fatalf("github connector = %+v", got)
		}
		if got, _ := s.GetConnector(ctx, "alice", store.ConnectorTelegram); len(got.Data) != 0 {
			t.Fatalf("telegram data = %q", got.Data)
		}
		if list, err := s.ListConnectors(ctx, "bob"); err != nil || len(list) != 0 {
			t.Fatalf("bob's connectors = %+v, %v", list, err)
		}
		for _, c := range []struct{ login, kind string }{{"bob", store.ConnectorGitHub}, {"alice", "slack"}, {"nobody", store.ConnectorGitHub}} {
			if _, err := s.GetConnector(ctx, c.login, c.kind); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetConnector(%s, %s): %v", c.login, c.kind, err)
			}
		}

		// Act: setting again replaces the connector of that kind (new account, label and data), no duplicate.
		gh2 := store.Connector{Login: "alice", Kind: store.ConnectorGitHub, Account: "alice-gh2", Label: "@alice2", Data: []byte("sealed-2")}
		must(t, s.SetConnector(ctx, gh2))

		// Assert
		if list, _ := s.ListConnectors(ctx, "alice"); len(list) != 2 {
			t.Fatalf("replacing duplicated the connector: %+v", list)
		}
		if got, _ := s.GetConnector(ctx, "alice", store.ConnectorGitHub); got.Account != "alice-gh2" || got.Label != "@alice2" || string(got.Data) != "sealed-2" {
			t.Fatalf("replaced connector = %+v", got)
		}
		if _, err := s.FindConnector(ctx, store.ConnectorGitHub, "alice-gh"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("the replaced account is still found: %v", err)
		}
		if got, err := s.FindConnector(ctx, store.ConnectorGitHub, "alice-gh2"); err != nil || got.Login != "alice" {
			t.Fatalf("FindConnector = %+v, %v", got, err)
		}
		// Re-saving the same account for the same user is not a conflict.
		must(t, s.SetConnector(ctx, gh2))
		// Find is per kind: telegram chat 111 is not a github account.
		if got, err := s.FindConnector(ctx, store.ConnectorTelegram, "111"); err != nil || got.Login != "alice" {
			t.Fatalf("telegram find = %+v, %v", got, err)
		}
		if _, err := s.FindConnector(ctx, store.ConnectorGitHub, "111"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a telegram account found as github: %v", err)
		}

		// Assert: a provider account belongs to one user per kind, and a refused link changes nothing.
		stolen := store.Connector{Login: "bob", Kind: store.ConnectorTelegram, Account: "111", Label: "@bob"}
		wantErr(t, "linking an account owned by someone else", s.SetConnector(ctx, stolen), store.ErrConflict)
		if _, err := s.GetConnector(ctx, "bob", store.ConnectorTelegram); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a refused link was stored: %v", err)
		}
		if got, _ := s.FindConnector(ctx, store.ConnectorTelegram, "111"); got.Login != "alice" {
			t.Fatalf("account moved to %s", got.Login)
		}
		// The same account id under another kind is a different account.
		must(t, s.SetConnector(ctx, store.Connector{Login: "bob", Kind: store.ConnectorGitHub, Account: "111", Label: "@bob"}))

		// Assert: unknown users cannot link anything.
		wantErr(t, "connector of an unknown user", s.SetConnector(ctx, store.Connector{Login: "ghost", Kind: store.ConnectorGitHub, Account: "ghost-gh"}), store.ErrNotFound)

		// Act: deleting frees the account for others.
		must(t, s.DeleteConnector(ctx, "alice", store.ConnectorTelegram))
		wantErr(t, "deleting twice", s.DeleteConnector(ctx, "alice", store.ConnectorTelegram), store.ErrNotFound)
		wantErr(t, "deleting for an unknown user", s.DeleteConnector(ctx, "nobody", store.ConnectorTelegram), store.ErrNotFound)
		if _, err := s.FindConnector(ctx, store.ConnectorTelegram, "111"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("deleted account still found: %v", err)
		}
		must(t, s.SetConnector(ctx, stolen))
		if got, _ := s.FindConnector(ctx, store.ConnectorTelegram, "111"); got.Login != "bob" {
			t.Fatalf("freed account owned by %q", got.Login)
		}
		if got, _ := s.GetConnector(ctx, "alice", store.ConnectorGitHub); got == nil || got.Account != "alice-gh2" {
			t.Fatalf("deleting telegram touched github: %+v", got)
		}
	})
}

func TestTeamUserPrefsAreReplacedNotMerged(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		addUser(t, s, "alice", "")
		addUser(t, s, "bob", "")

		// Assert: none set → an empty, usable map, for known and unknown logins.
		for _, l := range []string{"alice", "ghost"} {
			prefs, err := s.UserPrefs(ctx, l)
			if err != nil || prefs == nil || len(prefs) != 0 {
				t.Fatalf("UserPrefs(%s) = %#v, %v", l, prefs, err)
			}
		}

		// Act
		in := map[string]any{"theme": "dark", "sidebar": true, "volume": float64(7), "supervisor": map[string]any{"watch": true, "stuckMinutes": float64(30)}}
		must(t, s.SetUserPrefs(ctx, "alice", in))
		in["theme"] = "mutated after the call"

		// Assert: values round trip, the store keeps its own copy and each user has their own.
		got, err := s.UserPrefs(ctx, "alice")
		must(t, err)
		sup, _ := got["supervisor"].(map[string]any)
		if got["theme"] != "dark" || got["sidebar"] != true || got["volume"] != float64(7) || sup["watch"] != true || sup["stuckMinutes"] != float64(30) || len(got) != 4 {
			t.Fatalf("prefs = %#v", got)
		}
		got["theme"] = "mutated after the read"
		if again, _ := s.UserPrefs(ctx, "alice"); again["theme"] != "dark" {
			t.Fatalf("prefs changed through a returned map: %#v", again)
		}
		if other, _ := s.UserPrefs(ctx, "bob"); len(other) != 0 {
			t.Fatalf("bob sees alice's prefs: %#v", other)
		}

		// Act: setting again replaces everything.
		must(t, s.SetUserPrefs(ctx, "alice", map[string]any{"lang": "fr"}))

		// Assert
		if got, _ := s.UserPrefs(ctx, "alice"); len(got) != 1 || got["lang"] != "fr" {
			t.Fatalf("prefs after replace = %#v", got)
		}
		must(t, s.SetUserPrefs(ctx, "alice", nil))
		if got, err := s.UserPrefs(ctx, "alice"); err != nil || got == nil || len(got) != 0 {
			t.Fatalf("prefs after clearing = %#v, %v", got, err)
		}
	})
}

func TestTeamSeenThreadsNeverMoveBackwards(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		addUser(t, s, "alice", "")
		addUser(t, s, "bob", "")
		t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
		t2, t3 := t1.Add(time.Hour), t1.Add(2*time.Hour)
		if got, err := s.SeenThreads(ctx, "alice"); err != nil || got == nil || len(got) != 0 {
			t.Fatalf("nothing seen yet: %#v, %v", got, err)
		}

		// Act: a later visit advances it, an earlier or equal one is ignored (replays, another device).
		must(t, s.SetThreadSeen(ctx, "alice", "t1", t2))
		must(t, s.SetThreadSeen(ctx, "alice", "t1", t1))
		must(t, s.SetThreadSeen(ctx, "alice", "t1", t2))
		must(t, s.SetThreadSeen(ctx, "alice", "t2", t1))
		must(t, s.SetThreadSeen(ctx, "bob", "t1", t1))

		// Assert
		alice, _ := s.SeenThreads(ctx, "alice")
		bob, _ := s.SeenThreads(ctx, "bob")
		if len(alice) != 2 || !alice["t1"].Equal(t2) || !alice["t2"].Equal(t1) || len(bob) != 1 || !bob["t1"].Equal(t1) {
			t.Fatalf("alice %v, bob %v", alice, bob)
		}

		// Act
		must(t, s.SetThreadSeen(ctx, "alice", "t1", t3))
		// Assert: a time zone does not change the instant.
		if got, _ := s.SeenThreads(ctx, "alice"); !got["t1"].Equal(t3) {
			t.Fatalf("t1 seen at %v, want %v", got["t1"], t3)
		}
		must(t, s.SetThreadSeen(ctx, "alice", "t1", t2.In(time.FixedZone("x", 5*3600))))
		if got, _ := s.SeenThreads(ctx, "alice"); !got["t1"].Equal(t3) {
			t.Fatalf("an older instant in another zone moved it back to %v", got["t1"])
		}
	})
}

func TestTeamSeenStateGoesWithItsThread(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange: alice saw one thread of a project, one chat of no project and one of another project.
		ctx := context.Background()
		addUser(t, s, "alice", "")
		addUser(t, s, "bob", "")
		addProject(t, s, "p1", "api", "alice")
		addProject(t, s, "p2", "web", "alice")
		for _, m := range []store.ThreadMeta{
			{ThreadID: "gone", ProjectID: "p1", Owner: "alice", CreatedBy: "alice"},
			{ThreadID: "kept-p2", ProjectID: "p2", Owner: "alice", CreatedBy: "alice"},
			{ThreadID: "solo", Owner: "alice", CreatedBy: "alice"},
		} {
			must(t, s.SetThreadMeta(ctx, m))
		}
		at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		for _, l := range []string{"alice", "bob"} {
			for _, id := range []string{"gone", "kept-p2", "solo", "legacy"} {
				must(t, s.SetThreadSeen(ctx, l, id, at))
			}
		}

		// Act: deleting the project drops the seen state of its threads, for every user.
		must(t, s.DeleteProject(ctx, "p1"))

		// Assert
		for _, l := range []string{"alice", "bob"} {
			got, _ := s.SeenThreads(ctx, l)
			if len(got) != 3 || got["gone"].IsZero() == false {
				t.Fatalf("%s seen after project delete = %v", l, got)
			}
		}

		// Act: deleting a thread link drops its seen state for every user; a thread without link keeps it.
		must(t, s.DeleteThreadMeta(ctx, "solo"))

		// Assert
		for _, l := range []string{"alice", "bob"} {
			got, _ := s.SeenThreads(ctx, l)
			if len(got) != 2 || got["kept-p2"].IsZero() || got["legacy"].IsZero() {
				t.Fatalf("%s seen after thread delete = %v", l, got)
			}
		}
	})
}

func TestTeamCreateProject(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()

		// Act: explicit id; the owner argument, not the struct, decides the creator.
		p, err := s.CreateProject(ctx, store.Project{ID: "proj-1", Name: "api", Description: "the api", RepoURL: "https://github.com/x/api",
			DefaultBranch: "main", GitHubInstallation: 4242, CreatedBy: "mallory"}, "alice")
		must(t, err)

		// Assert
		if p.ID != "proj-1" || p.Name != "api" || p.Description != "the api" || p.RepoURL != "https://github.com/x/api" ||
			p.DefaultBranch != "main" || p.GitHubInstallation != 4242 || p.CreatedBy != "alice" {
			t.Fatalf("created = %+v", p)
		}
		if p.CreatedAt.IsZero() || !p.UpdatedAt.Equal(p.CreatedAt) {
			t.Fatalf("timestamps: created %v updated %v", p.CreatedAt, p.UpdatedAt)
		}
		got, err := s.GetProject(ctx, "proj-1")
		must(t, err)
		if *got != *p {
			t.Fatalf("stored %+v differs from returned %+v", got, p)
		}
		if r, err := s.MemberRole(ctx, "proj-1", "alice"); err != nil || r != store.RoleOwner {
			t.Fatalf("creator role = %q, %v", r, err)
		}
		if !sameRoles(roles(t, s, "proj-1"), map[string]string{"alice": store.RoleOwner}) {
			t.Fatalf("members = %v", roles(t, s, "proj-1"))
		}

		// Act / Assert: a duplicate id is a conflict and leaves the first project untouched.
		_, err = s.CreateProject(ctx, store.Project{ID: "proj-1", Name: "hijack"}, "bob")
		wantErr(t, "duplicate project id", err, store.ErrConflict)
		if got, _ := s.GetProject(ctx, "proj-1"); got.Name != "api" || got.CreatedBy != "alice" {
			t.Fatalf("project changed by a refused create: %+v", got)
		}
		if _, err := s.MemberRole(ctx, "proj-1", "bob"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("the refused creator became a member: %v", err)
		}

		// Act / Assert: no id → a fresh unique one.
		a, err := s.CreateProject(ctx, store.Project{Name: "one"}, "alice")
		must(t, err)
		b, err := s.CreateProject(ctx, store.Project{Name: "one"}, "alice")
		must(t, err)
		if a.ID == "" || b.ID == "" || a.ID == b.ID || a.ID == "proj-1" {
			t.Fatalf("generated ids %q %q", a.ID, b.ID)
		}
		if got, err := s.GetProject(ctx, a.ID); err != nil || got.Name != "one" {
			t.Fatalf("generated project = %+v, %v", got, err)
		}
		if _, err := s.GetProject(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown project: %v", err)
		}
	})
}

func TestTeamUpdateProjectChangesOnlyThePatchedFields(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		created, err := s.CreateProject(ctx, store.Project{ID: "p1", Name: "api", Description: "old", RepoURL: "https://github.com/x/api",
			DefaultBranch: "main", GitHubInstallation: 7}, "alice")
		must(t, err)
		addProject(t, s, "p2", "other", "alice")
		str := func(v string) *string { return &v }
		num := func(v int64) *int64 { return &v }
		pastClock(t, s, created.UpdatedAt)

		// Act: rename only.
		got, err := s.UpdateProject(ctx, "p1", store.ProjectPatch{Name: str("api v2")})
		must(t, err)

		// Assert
		if got.Name != "api v2" || got.Description != "old" || got.RepoURL != "https://github.com/x/api" || got.DefaultBranch != "main" || got.GitHubInstallation != 7 {
			t.Fatalf("after rename: %+v", got)
		}
		if got.ID != "p1" || got.CreatedBy != "alice" || !got.CreatedAt.Equal(created.CreatedAt) {
			t.Fatalf("identity changed: %+v", got)
		}
		if !got.UpdatedAt.After(created.UpdatedAt) {
			t.Fatalf("UpdatedAt not bumped: %v -> %v", created.UpdatedAt, got.UpdatedAt)
		}
		if stored, _ := s.GetProject(ctx, "p1"); *stored != *got {
			t.Fatalf("stored %+v differs from returned %+v", stored, got)
		}

		// Act: several fields, including explicit clearing to the zero value (a non-nil pointer to "" or 0 clears).
		got, err = s.UpdateProject(ctx, "p1", store.ProjectPatch{Description: str(""), RepoURL: str(""), DefaultBranch: str("trunk"), GitHubInstallation: num(0)})
		must(t, err)

		// Assert
		if got.Name != "api v2" || got.Description != "" || got.RepoURL != "" || got.DefaultBranch != "trunk" || got.GitHubInstallation != 0 {
			t.Fatalf("after clearing: %+v", got)
		}

		// Act: an empty patch changes no field.
		same, err := s.UpdateProject(ctx, "p1", store.ProjectPatch{})
		must(t, err)
		if same.Name != got.Name || same.DefaultBranch != "trunk" || same.Description != "" {
			t.Fatalf("empty patch changed fields: %+v", same)
		}
		if other, _ := s.GetProject(ctx, "p2"); other.Name != "other" {
			t.Fatalf("another project was patched: %+v", other)
		}

		// Assert: unknown id.
		_, err = s.UpdateProject(ctx, "missing", store.ProjectPatch{Name: str("x")})
		wantErr(t, "update of an unknown project", err, store.ErrNotFound)
	})
}

func TestTeamDeleteProjectCascades(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange: p1 with a second member, a pending invite and two threads; p2 and a project-less thread must survive.
		ctx := context.Background()
		addUser(t, s, "alice", "alice@x.io")
		addUser(t, s, "bob", "bob@x.io")
		addProject(t, s, "p1", "api", "alice")
		addProject(t, s, "p2", "web", "alice")
		must(t, s.SetMember(ctx, "p1", "bob", store.RoleMember))
		must(t, s.SetMember(ctx, "p2", "bob", store.RoleViewer))
		for _, p := range []string{"p1", "p2"} {
			_, err := s.CreateInvite(ctx, store.Invite{ProjectID: p, Email: "dave@x.io", Role: store.RoleMember, InvitedBy: "alice"})
			must(t, err)
		}
		for _, m := range []store.ThreadMeta{
			{ThreadID: "t1", ProjectID: "p1", Owner: "alice", CreatedBy: "alice"},
			{ThreadID: "t2", ProjectID: "p1", Owner: "bob", CreatedBy: "bob"},
			{ThreadID: "t3", ProjectID: "p2", Owner: "alice", CreatedBy: "alice"},
			{ThreadID: "free", Owner: "alice", CreatedBy: "alice"},
		} {
			must(t, s.SetThreadMeta(ctx, m))
		}

		// Act
		must(t, s.DeleteProject(ctx, "p1"))

		// Assert: everything attached to p1 is gone.
		if _, err := s.GetProject(ctx, "p1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("project survived: %v", err)
		}
		if got := roles(t, s, "p1"); len(got) != 0 {
			t.Fatalf("members survived: %v", got)
		}
		if _, err := s.MemberRole(ctx, "p1", "bob"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("membership survived: %v", err)
		}
		if inv, _ := s.ListInvites(ctx, "p1"); len(inv) != 0 {
			t.Fatalf("invites survived: %+v", inv)
		}
		for _, id := range []string{"t1", "t2"} {
			if _, err := s.GetThreadMeta(ctx, id); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("thread link %s survived: %v", id, err)
			}
		}
		if th, _ := s.ListProjectThreads(ctx, "p1"); len(th) != 0 {
			t.Fatalf("threads survived: %+v", th)
		}
		if list, _ := s.ListProjects(ctx, "bob"); len(list) != 1 || list[0].ID != "p2" || list[0].Role != store.RoleViewer {
			t.Fatalf("bob's projects = %+v", list)
		}

		// Assert: the rest is untouched.
		if _, err := s.GetProject(ctx, "p2"); err != nil {
			t.Fatalf("p2: %v", err)
		}
		if got := roles(t, s, "p2"); !sameRoles(got, map[string]string{"alice": store.RoleOwner, "bob": store.RoleViewer}) {
			t.Fatalf("p2 members = %v", got)
		}
		if inv, _ := s.ListInvites(ctx, "p2"); len(inv) != 1 {
			t.Fatalf("p2 invites = %+v", inv)
		}
		for _, id := range []string{"t3", "free"} {
			if _, err := s.GetThreadMeta(ctx, id); err != nil {
				t.Fatalf("thread link %s: %v", id, err)
			}
		}

		// Assert: a deleted project is gone for good, and its id is reusable.
		wantErr(t, "second delete", s.DeleteProject(ctx, "p1"), store.ErrNotFound)
		wantErr(t, "delete of an unknown project", s.DeleteProject(ctx, "missing"), store.ErrNotFound)
		if _, err := s.CreateProject(ctx, store.Project{ID: "p1", Name: "again"}, "bob"); err != nil {
			t.Fatalf("recreating the id: %v", err)
		}
		if got := roles(t, s, "p1"); !sameRoles(got, map[string]string{"bob": store.RoleOwner}) {
			t.Fatalf("recreated project inherited members: %v", got)
		}
	})
}

func TestTeamListProjectsByNameWithTheCallersRole(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		addProject(t, s, "id-1", "zeta", "alice")
		addProject(t, s, "id-2", "alpha", "alice")
		addProject(t, s, "id-3", "mid", "bob")
		addProject(t, s, "id-4", "bobs-only", "bob")
		must(t, s.SetMember(ctx, "id-3", "alice", store.RoleViewer))

		// Act
		list, err := s.ListProjects(ctx, "alice")
		must(t, err)

		// Assert
		type row struct{ name, role string }
		var got []row
		for _, pr := range list {
			got = append(got, row{pr.Name, pr.Role})
			if pr.ID == "" || pr.CreatedBy == "" || pr.CreatedAt.IsZero() {
				t.Fatalf("incomplete project %+v", pr)
			}
		}
		want := []row{{"alpha", store.RoleOwner}, {"mid", store.RoleViewer}, {"zeta", store.RoleOwner}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("projects = %v, want %v", got, want)
		}
		if list[1].ID != "id-3" || list[1].CreatedBy != "bob" {
			t.Fatalf("mid = %+v", list[1].Project)
		}
		if none, err := s.ListProjects(ctx, "stranger"); err != nil || len(none) != 0 {
			t.Fatalf("stranger's projects = %+v, %v", none, err)
		}

		// Act: leaving a project removes it from the list.
		must(t, s.RemoveMember(ctx, "id-3", "alice"))
		if list, _ := s.ListProjects(ctx, "alice"); len(list) != 2 {
			t.Fatalf("after leaving: %+v", list)
		}
	})
}

func TestTeamMembersAndTheLastOwnerRule(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		addProject(t, s, "p1", "api", "alice")
		addProject(t, s, "p2", "web", "carol")

		// Assert: adding, changing, reading roles.
		must(t, s.SetMember(ctx, "p1", "bob", store.RoleViewer))
		if r, err := s.MemberRole(ctx, "p1", "bob"); err != nil || r != store.RoleViewer {
			t.Fatalf("bob = %q, %v", r, err)
		}
		must(t, s.SetMember(ctx, "p1", "bob", store.RoleMember))
		must(t, s.SetMember(ctx, "p1", "bob", store.RoleMember))
		if got := roles(t, s, "p1"); !sameRoles(got, map[string]string{"alice": store.RoleOwner, "bob": store.RoleMember}) {
			t.Fatalf("members = %v", got)
		}
		if _, err := s.MemberRole(ctx, "p1", "carol"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("carol of another project: %v", err)
		}
		if _, err := s.MemberRole(ctx, "missing", "alice"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("role in an unknown project: %v", err)
		}
		if got, err := s.ListMembers(ctx, "missing"); err != nil || len(got) != 0 {
			t.Fatalf("members of an unknown project = %+v, %v", got, err)
		}
		list, _ := s.ListMembers(ctx, "p1")
		for _, m := range list {
			if m.AddedAt.IsZero() {
				t.Fatalf("member without AddedAt: %+v", m)
			}
		}

		// Assert: invalid roles are refused and change nothing; unknown projects are not found.
		for _, role := range []string{"", "admin", "OWNER"} {
			wantErr(t, "role "+role, s.SetMember(ctx, "p1", "bob", role), store.ErrConflict)
			wantErr(t, "new member, role "+role, s.SetMember(ctx, "p1", "dave", role), store.ErrConflict)
		}
		if r, _ := s.MemberRole(ctx, "p1", "bob"); r != store.RoleMember {
			t.Fatalf("bob = %q after refused changes", r)
		}
		if _, err := s.MemberRole(ctx, "p1", "dave"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("dave added with an invalid role: %v", err)
		}
		wantErr(t, "member of an unknown project", s.SetMember(ctx, "missing", "bob", store.RoleMember), store.ErrNotFound)
		wantErr(t, "owner of an unknown project", s.SetMember(ctx, "missing", "bob", store.RoleOwner), store.ErrNotFound)
		if got := roles(t, s, "missing"); len(got) != 0 {
			t.Fatalf("members created for an unknown project: %v", got)
		}

		// Assert: the last owner can neither be demoted nor removed, and nothing changes.
		wantErr(t, "demoting the last owner to member", s.SetMember(ctx, "p1", "alice", store.RoleMember), store.ErrLastOwner)
		wantErr(t, "demoting the last owner to viewer", s.SetMember(ctx, "p1", "alice", store.RoleViewer), store.ErrLastOwner)
		wantErr(t, "removing the last owner", s.RemoveMember(ctx, "p1", "alice"), store.ErrLastOwner)
		if r, _ := s.MemberRole(ctx, "p1", "alice"); r != store.RoleOwner {
			t.Fatalf("alice = %q after refused changes", r)
		}
		must(t, s.SetMember(ctx, "p1", "alice", store.RoleOwner)) // staying owner is not a demotion

		// Act: promote a second owner, then the first may step down.
		must(t, s.SetMember(ctx, "p1", "bob", store.RoleOwner))
		must(t, s.SetMember(ctx, "p1", "alice", store.RoleViewer))

		// Assert: bob is now the last owner.
		if got := roles(t, s, "p1"); !sameRoles(got, map[string]string{"alice": store.RoleViewer, "bob": store.RoleOwner}) {
			t.Fatalf("members = %v", got)
		}
		wantErr(t, "removing the new last owner", s.RemoveMember(ctx, "p1", "bob"), store.ErrLastOwner)
		wantErr(t, "demoting the new last owner", s.SetMember(ctx, "p1", "bob", store.RoleMember), store.ErrLastOwner)

		// Act / Assert: members leave; owners may leave once another remains.
		must(t, s.RemoveMember(ctx, "p1", "alice"))
		if _, err := s.MemberRole(ctx, "p1", "alice"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("alice still a member: %v", err)
		}
		must(t, s.SetMember(ctx, "p1", "alice", store.RoleOwner))
		must(t, s.RemoveMember(ctx, "p1", "bob"))
		if got := roles(t, s, "p1"); !sameRoles(got, map[string]string{"alice": store.RoleOwner}) {
			t.Fatalf("members = %v", got)
		}

		// Assert: removing someone who is not a member, or from an unknown project.
		wantErr(t, "removing a stranger", s.RemoveMember(ctx, "p1", "bob"), store.ErrNotFound)
		wantErr(t, "removing from an unknown project", s.RemoveMember(ctx, "missing", "alice"), store.ErrNotFound)
		// Roles are per project: carol's ownership of p2 was never at stake.
		if r, _ := s.MemberRole(ctx, "p2", "carol"); r != store.RoleOwner {
			t.Fatalf("carol = %q", r)
		}
	})
}

func TestTeamInvites(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		addUser(t, s, "alice", "alice@x.io")
		addProject(t, s, "p1", "api", "alice")
		addProject(t, s, "p2", "web", "alice")

		// Act: invite an email nobody has logged in with.
		iv, err := s.CreateInvite(ctx, store.Invite{ProjectID: "p1", Email: "  Bob@X.io ", Role: store.RoleMember, InvitedBy: "alice"})
		must(t, err)

		// Assert
		if iv == nil || iv.ProjectID != "p1" || iv.Email != "bob@x.io" || iv.Role != store.RoleMember || iv.InvitedBy != "alice" || iv.CreatedAt.IsZero() {
			t.Fatalf("invite = %+v", iv)
		}
		list, _ := s.ListInvites(ctx, "p1")
		if len(list) != 1 || list[0].Email != "bob@x.io" || list[0].Role != store.RoleMember || list[0].InvitedBy != "alice" || list[0].ProjectID != "p1" {
			t.Fatalf("listed = %+v", list)
		}
		if other, err := s.ListInvites(ctx, "p2"); err != nil || len(other) != 0 {
			t.Fatalf("p2 invites = %+v, %v", other, err)
		}
		if got := roles(t, s, "p1"); len(got) != 1 {
			t.Fatalf("an invite created a membership: %v", got)
		}

		// Act: inviting the same email again (other case) replaces the pending invite.
		again, err := s.CreateInvite(ctx, store.Invite{ProjectID: "p1", Email: "BOB@x.io", Role: store.RoleViewer, InvitedBy: "carol"})
		must(t, err)

		// Assert
		list, _ = s.ListInvites(ctx, "p1")
		if again == nil || again.Role != store.RoleViewer || len(list) != 1 || list[0].Role != store.RoleViewer || list[0].InvitedBy != "carol" {
			t.Fatalf("after replace: returned %+v, listed %+v", again, list)
		}

		// Assert: invalid role and unknown project are refused and store nothing.
		for _, role := range []string{"", "admin"} {
			_, err := s.CreateInvite(ctx, store.Invite{ProjectID: "p1", Email: "eve@x.io", Role: role})
			wantErr(t, "invite with role "+role, err, store.ErrConflict)
		}
		_, err = s.CreateInvite(ctx, store.Invite{ProjectID: "missing", Email: "eve@x.io", Role: store.RoleMember})
		wantErr(t, "invite to an unknown project", err, store.ErrNotFound)
		if list, _ := s.ListInvites(ctx, "p1"); len(list) != 1 {
			t.Fatalf("refused invites were stored: %+v", list)
		}
		if list, _ := s.ListInvites(ctx, "missing"); len(list) != 0 {
			t.Fatalf("invites stored for an unknown project: %+v", list)
		}

		// Act: an email that already belongs to a user is added at once and no invite remains (nil is returned).
		addProject(t, s, "p3", "docs", "bob")
		got, err := s.CreateInvite(ctx, store.Invite{ProjectID: "p3", Email: " ALICE@x.io", Role: store.RoleMember, InvitedBy: "bob"})
		must(t, err)

		// Assert
		if got != nil {
			t.Fatalf("known email returned a pending invite: %+v", got)
		}
		if r, err := s.MemberRole(ctx, "p3", "alice"); err != nil || r != store.RoleMember {
			t.Fatalf("alice in p3 = %q, %v", r, err)
		}
		if list, _ := s.ListInvites(ctx, "p3"); len(list) != 0 {
			t.Fatalf("a pending invite was left for a known user: %+v", list)
		}
		// Inviting again with another role updates the membership.
		if got, err := s.CreateInvite(ctx, store.Invite{ProjectID: "p3", Email: "alice@x.io", Role: store.RoleOwner}); err != nil || got != nil {
			t.Fatalf("second invite = %+v, %v", got, err)
		}
		if r, _ := s.MemberRole(ctx, "p3", "alice"); r != store.RoleOwner {
			t.Fatalf("alice in p3 = %q after re-invite", r)
		}

		// Act / Assert: deleting ignores case and blanks, is per project, and reports a missing invite.
		_, err = s.CreateInvite(ctx, store.Invite{ProjectID: "p2", Email: "bob@x.io", Role: store.RoleMember})
		must(t, err)
		wantErr(t, "delete in the wrong project", s.DeleteInvite(ctx, "p3", "bob@x.io"), store.ErrNotFound)
		must(t, s.DeleteInvite(ctx, "p1", "  BOB@X.IO "))
		if list, _ := s.ListInvites(ctx, "p1"); len(list) != 0 {
			t.Fatalf("p1 invites after delete = %+v", list)
		}
		if list, _ := s.ListInvites(ctx, "p2"); len(list) != 1 {
			t.Fatalf("p2 invite lost: %+v", list)
		}
		wantErr(t, "deleting twice", s.DeleteInvite(ctx, "p1", "bob@x.io"), store.ErrNotFound)
		wantErr(t, "deleting from an unknown project", s.DeleteInvite(ctx, "missing", "bob@x.io"), store.ErrNotFound)
	})
}

func TestTeamThreadMeta(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		addProject(t, s, "p1", "api", "alice")
		addProject(t, s, "p2", "web", "alice")

		// Act: the kind defaults to chat; a thread may belong to no project.
		must(t, s.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "t1", ProjectID: "p1", Owner: "alice", CreatedBy: "bob"}))
		must(t, s.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "solo", Owner: "alice", CreatedBy: "alice"}))

		// Assert
		got, err := s.GetThreadMeta(ctx, "t1")
		must(t, err)
		if got.ThreadID != "t1" || got.ProjectID != "p1" || got.Owner != "alice" || got.CreatedBy != "bob" || got.Kind != store.ThreadChat || got.CreatedAt.IsZero() {
			t.Fatalf("t1 = %+v", got)
		}
		solo, err := s.GetThreadMeta(ctx, "solo")
		must(t, err)
		if solo.ProjectID != "" || solo.Kind != store.ThreadChat || solo.Owner != "alice" {
			t.Fatalf("solo = %+v", solo)
		}
		if th, _ := s.ListProjectThreads(ctx, "p1"); len(th) != 1 || th[0].ThreadID != "t1" {
			t.Fatalf("p1 threads = %+v", th)
		}
		if _, err := s.GetThreadMeta(ctx, "unknown"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown thread: %v", err)
		}
		if th, err := s.ListProjectThreads(ctx, "p2"); err != nil || len(th) != 0 {
			t.Fatalf("p2 threads = %+v, %v", th, err)
		}

		// Act: linking again replaces the link (idempotent), without making the thread look newer.
		pastClock(t, s, got.CreatedAt)
		must(t, s.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "t1", ProjectID: "p2", Owner: "carol", CreatedBy: "carol", Kind: store.ThreadSupervisor}))
		must(t, s.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "t1", ProjectID: "p2", Owner: "carol", CreatedBy: "carol", Kind: store.ThreadSupervisor}))

		// Assert
		moved, _ := s.GetThreadMeta(ctx, "t1")
		if moved.ProjectID != "p2" || moved.Owner != "carol" || moved.CreatedBy != "carol" || moved.Kind != store.ThreadSupervisor {
			t.Fatalf("relinked = %+v", moved)
		}
		if !moved.CreatedAt.Equal(got.CreatedAt) {
			t.Fatalf("re-linking changed the creation time: %v -> %v", got.CreatedAt, moved.CreatedAt)
		}
		if th, _ := s.ListProjectThreads(ctx, "p1"); len(th) != 0 {
			t.Fatalf("p1 still lists the moved thread: %+v", th)
		}
		if th, _ := s.ListProjectThreads(ctx, "p2"); len(th) != 1 || th[0].ThreadID != "t1" {
			t.Fatalf("p2 threads = %+v", th)
		}

		// Act / Assert: deleting is idempotent and also fine for unknown threads.
		must(t, s.DeleteThreadMeta(ctx, "t1"))
		must(t, s.DeleteThreadMeta(ctx, "t1"))
		must(t, s.DeleteThreadMeta(ctx, "never-existed"))
		if _, err := s.GetThreadMeta(ctx, "t1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("deleted thread: %v", err)
		}
		if _, err := s.GetThreadMeta(ctx, "solo"); err != nil {
			t.Fatalf("solo was deleted with another thread: %v", err)
		}
	})
}

func TestTeamProjectThreadsAreListedNewestFirst(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		addProject(t, s, "p1", "api", "alice")
		addProject(t, s, "p2", "web", "alice")

		// Act
		var prev time.Time
		for _, id := range []string{"a", "b", "c"} {
			pastClock(t, s, prev)
			must(t, s.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: id, ProjectID: "p1", Owner: "alice", CreatedBy: "alice"}))
			m, err := s.GetThreadMeta(ctx, id)
			must(t, err)
			prev = m.CreatedAt
		}
		must(t, s.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "elsewhere", ProjectID: "p2", Owner: "alice", CreatedBy: "alice"}))
		must(t, s.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "a", ProjectID: "p1", Owner: "alice", CreatedBy: "alice"})) // idempotent: "a" stays the oldest

		// Assert
		list, err := s.ListProjectThreads(ctx, "p1")
		must(t, err)
		ids := []string{}
		for _, m := range list {
			ids = append(ids, m.ThreadID)
		}
		if strings.Join(ids, ",") != "c,b,a" {
			t.Fatalf("p1 threads = %v, want newest first c,b,a", ids)
		}
	})
}

func TestTeamSupervisorThreadIsLookedUpByItsOwner(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		addProject(t, s, "p1", "api", "alice")
		must(t, s.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "chat", ProjectID: "p1", Owner: "alice", CreatedBy: "alice"}))

		// Assert: a chat thread is not a supervisor thread.
		if _, err := s.SupervisorThread(ctx, "alice"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("supervisor without one: %v", err)
		}

		// Act: the supervisor thread belongs to no project; alice owns it, bob created it.
		must(t, s.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "sup", Owner: "alice", CreatedBy: "bob", Kind: store.ThreadSupervisor}))

		// Assert
		got, err := s.SupervisorThread(ctx, "alice")
		must(t, err)
		if got.ThreadID != "sup" || got.Owner != "alice" || got.CreatedBy != "bob" || got.Kind != store.ThreadSupervisor || got.ProjectID != "" {
			t.Fatalf("supervisor = %+v", got)
		}
		for _, l := range []string{"bob", "nobody", ""} {
			if _, err := s.SupervisorThread(ctx, l); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("SupervisorThread(%q): %v", l, err)
			}
		}
		must(t, s.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "sup-bob", Owner: "bob", CreatedBy: "bob", Kind: store.ThreadSupervisor}))
		if got, err := s.SupervisorThread(ctx, "bob"); err != nil || got.ThreadID != "sup-bob" {
			t.Fatalf("bob's supervisor = %+v, %v", got, err)
		}

		// Act / Assert: deleting the link makes it unknown again.
		must(t, s.DeleteThreadMeta(ctx, "sup"))
		if _, err := s.SupervisorThread(ctx, "alice"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("deleted supervisor: %v", err)
		}
	})
}

func TestTeamAuditIsListedNewestFirstPerProject(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		detail := map[string]any{"email": "bob@x.io", "role": "member", "force": true, "count": float64(3), "nested": map[string]any{"k": "v"}}

		// Act
		must(t, s.Audit(ctx, store.AuditEntry{ProjectID: "p1", ThreadID: "t1", Actor: "alice", OnBehalfOf: "bob", Action: "member.invite", Detail: detail}))
		must(t, s.Audit(ctx, store.AuditEntry{ProjectID: "p2", Actor: "alice", Action: "project.create"}))
		for i := range 4 {
			must(t, s.Audit(ctx, store.AuditEntry{ProjectID: "p1", Actor: "carol", Action: fmt.Sprintf("a%d", i)}))
		}

		// Assert: entries come back whole and the detail round trips.
		all, err := s.ListAudit(ctx, "p1", 10)
		must(t, err)
		if len(all) != 5 {
			t.Fatalf("p1 audit = %+v", all)
		}
		first := all[4]
		if first.Action != "member.invite" || first.ProjectID != "p1" || first.ThreadID != "t1" || first.Actor != "alice" || first.OnBehalfOf != "bob" || first.At.IsZero() || first.ID == 0 {
			t.Fatalf("first entry = %+v", first)
		}
		nested, _ := first.Detail["nested"].(map[string]any)
		if len(first.Detail) != 5 || first.Detail["email"] != "bob@x.io" || first.Detail["force"] != true || first.Detail["count"] != float64(3) || nested["k"] != "v" {
			t.Fatalf("detail = %#v", first.Detail)
		}
		if last := all[0]; last.Action != "a3" || len(last.Detail) != 0 || last.OnBehalfOf != "" || last.ThreadID != "" {
			t.Fatalf("newest entry = %+v", last)
		}

		// Assert: newest first (ids strictly decreasing), limited, and per project.
		for i := 1; i < len(all); i++ {
			if all[i].ID >= all[i-1].ID {
				t.Fatalf("not newest first: %d then %d", all[i-1].ID, all[i].ID)
			}
		}
		top, _ := s.ListAudit(ctx, "p1", 2)
		if len(top) != 2 || top[0].Action != "a3" || top[1].Action != "a2" {
			t.Fatalf("limit 2 = %+v", top)
		}
		if p2, _ := s.ListAudit(ctx, "p2", 10); len(p2) != 1 || p2[0].Action != "project.create" {
			t.Fatalf("p2 audit = %+v", p2)
		}
		if none, err := s.ListAudit(ctx, "p3", 10); err != nil || len(none) != 0 {
			t.Fatalf("p3 audit = %+v, %v", none, err)
		}
	})
}

func TestTeamAuditLimitDefaultsToOneHundred(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		for i := range 105 {
			must(t, s.Audit(ctx, store.AuditEntry{ProjectID: "p1", Actor: "alice", Action: fmt.Sprintf("n%03d", i)}))
		}

		// Act / Assert: a missing or negative limit means 100, the newest ones.
		for _, limit := range []int{0, -1} {
			list, err := s.ListAudit(ctx, "p1", limit)
			must(t, err)
			if len(list) != 100 || list[0].Action != "n104" || list[99].Action != "n005" {
				t.Fatalf("limit %d: %d entries, first %q last %q", limit, len(list), list[0].Action, list[len(list)-1].Action)
			}
		}
		if list, _ := s.ListAudit(ctx, "p1", 500); len(list) != 105 {
			t.Fatalf("limit 500 returned %d entries", len(list))
		}
		if list, _ := s.ListAudit(ctx, "p1", 1); len(list) != 1 || list[0].Action != "n104" {
			t.Fatalf("limit 1 = %+v", list)
		}
	})
}

// ---- concurrency: the invariants the stores promise under parallel callers ----

func TestTeamTwoOwnersDemotingEachOtherNeverLeaveTheProjectOwnerless(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		addProject(t, s, "p1", "api", "alice")
		must(t, s.SetMember(ctx, "p1", "bob", store.RoleOwner))

		// Act: each owner steps the other one down at the same time.
		start := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i, target := range []string{"alice", "bob"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = s.SetMember(ctx, "p1", target, store.RoleViewer)
			}()
		}
		close(start)
		wg.Wait()

		// Assert: exactly one demotion went through; the other was refused and one owner is left.
		var done, refused int
		for _, err := range errs {
			switch {
			case err == nil:
				done++
			case errors.Is(err, store.ErrLastOwner):
				refused++
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		owners := 0
		for _, role := range roles(t, s, "p1") {
			if role == store.RoleOwner {
				owners++
			}
		}
		if done != 1 || refused != 1 || owners != 1 {
			t.Fatalf("demoted %d, refused %d, owners left %d", done, refused, owners)
		}
	})
}

func TestTeamRacingForTheSameIdentityHasASingleWinner(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		const n = 8
		for i := range n {
			addUser(t, s, fmt.Sprintf("u%d", i), "")
		}

		// Act: n callers create the same project id, and n users link the same telegram chat.
		start := make(chan struct{})
		projectErrs, connErrs := make([]error, n), make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				login := fmt.Sprintf("u%d", i)
				_, projectErrs[i] = s.CreateProject(ctx, store.Project{ID: "shared", Name: "p of " + login}, login)
				connErrs[i] = s.SetConnector(ctx, store.Connector{Login: login, Kind: store.ConnectorTelegram, Account: "chat-1"})
			}()
		}
		close(start)
		wg.Wait()

		// Assert: one winner each, everybody else got ErrConflict, and the winners are consistent.
		count := func(errs []error) (winner int) {
			winner = -1
			for i, err := range errs {
				switch {
				case err == nil:
					if winner >= 0 {
						t.Fatalf("two winners: %d and %d", winner, i)
					}
					winner = i
				case !errors.Is(err, store.ErrConflict):
					t.Fatalf("caller %d: %v", i, err)
				}
			}
			if winner < 0 {
				t.Fatal("no winner")
			}
			return winner
		}
		pw, cw := count(projectErrs), count(connErrs)
		p, _ := s.GetProject(ctx, "shared")
		if p.CreatedBy != fmt.Sprintf("u%d", pw) || p.Name != "p of u"+fmt.Sprint(pw) {
			t.Fatalf("project %+v, winner u%d", p, pw)
		}
		if got := roles(t, s, "shared"); len(got) != 1 || got[fmt.Sprintf("u%d", pw)] != store.RoleOwner {
			t.Fatalf("members = %v, winner u%d", got, pw)
		}
		if c, err := s.FindConnector(ctx, store.ConnectorTelegram, "chat-1"); err != nil || c.Login != fmt.Sprintf("u%d", cw) {
			t.Fatalf("connector = %+v, %v; winner u%d", c, err, cw)
		}
	})
}

func TestTeamParallelWritersLoseNothing(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		const n = 12
		addProject(t, s, "p1", "api", "owner")
		base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
		for i := range n {
			addUser(t, s, fmt.Sprintf("u%d", i), "")
		}

		// Act: n users, concurrently, join the project, mark one shared thread seen at different times and write audit.
		start := make(chan struct{})
		var wg sync.WaitGroup
		errc := make(chan error, 4*n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				login := fmt.Sprintf("u%d", i)
				errc <- s.SetMember(ctx, "p1", login, store.RoleMember)
				errc <- s.SetThreadSeen(ctx, login, "t1", base.Add(time.Duration(i)*time.Minute))
				errc <- s.SetUserPrefs(ctx, login, map[string]any{"i": float64(i)})
				errc <- s.Audit(ctx, store.AuditEntry{ProjectID: "p1", Actor: login, Action: "join"})
			}()
		}
		close(start)
		wg.Wait()
		close(errc)

		// Assert
		for err := range errc {
			must(t, err)
		}
		if got := roles(t, s, "p1"); len(got) != n+1 {
			t.Fatalf("%d members, want %d", len(got), n+1)
		}
		for i := range n {
			login := fmt.Sprintf("u%d", i)
			seen, _ := s.SeenThreads(ctx, login)
			prefs, _ := s.UserPrefs(ctx, login)
			if !seen["t1"].Equal(base.Add(time.Duration(i)*time.Minute)) || prefs["i"] != float64(i) {
				t.Fatalf("%s: seen %v prefs %v", login, seen, prefs)
			}
		}
		audit, _ := s.ListAudit(ctx, "p1", 1000)
		ids := map[int64]bool{}
		for _, e := range audit {
			ids[e.ID] = true
		}
		if len(audit) != n || len(ids) != n {
			t.Fatalf("audit has %d entries with %d distinct ids, want %d", len(audit), len(ids), n)
		}
	})
}

func TestTeamSeenRacingWritersConvergeOnTheLatest(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		// Arrange
		ctx := context.Background()
		addUser(t, s, "alice", "")
		base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
		const n = 16

		// Act: several devices report visits in arbitrary order.
		start := make(chan struct{})
		var wg sync.WaitGroup
		errc := make(chan error, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errc <- s.SetThreadSeen(ctx, "alice", "t1", base.Add(time.Duration((i*7)%n)*time.Minute))
			}()
		}
		close(start)
		wg.Wait()
		close(errc)

		// Assert
		for err := range errc {
			must(t, err)
		}
		got, _ := s.SeenThreads(ctx, "alice")
		if want := base.Add((n - 1) * time.Minute); !got["t1"].Equal(want) {
			t.Fatalf("seen = %v, want the latest %v", got["t1"], want)
		}
	})
}
