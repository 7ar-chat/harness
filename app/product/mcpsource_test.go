package product

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/capstore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/capabilities"
	"github.com/7ar-chat/harness/core/store"
)

var msKey = []byte("marketplace-test-secret-key-0123")

// msFixture is a catalog of three entries: "keyless" (on by default, no fields), "optional" (off, an optional key
// sent as a header) and "needs" (off, a required token), a project, and the stores behind the source.
type msFixture struct {
	t       *testing.T
	team    *teamstore.Mem
	caps    *capstore.Mem
	catalog []capabilities.Entry
	box     *secretBox
	src     harness.MCPServersFunc
}

func newMSFixture(t *testing.T) *msFixture {
	t.Helper()
	catalog := []capabilities.Entry{
		{ID: "keyless", Name: "Keyless", URL: "https://keyless.example/mcp", Transport: "http", DefaultEnabled: true, Tools: []string{"search"}},
		{ID: "optional", Name: "Optional", URL: "https://optional.example/mcp", Transport: "sse",
			Headers: map[string]string{"x-api-key": "${api_key}", "Authorization": "Bearer ${api_key}"},
			Fields:  []capabilities.Field{{Key: "api_key", Label: "Key", Secret: true}}},
		{ID: "needs", Name: "Needs", URL: "https://needs.example/mcp", Transport: "http",
			Headers: map[string]string{"Authorization": "Bearer ${token}"},
			Fields:  []capabilities.Field{{Key: "token", Label: "Token", Secret: true, Required: true}}},
	}
	f := &msFixture{t: t, team: teamstore.NewMem(), caps: capstore.NewMem(nil), catalog: catalog}
	var err error
	if f.box, err = newSecretBoxFor(capabilitiesDomain, msKey); err != nil {
		t.Fatal(err)
	}
	if f.src, err = NewMCPSource(f.team, f.caps, catalog, msKey); err != nil {
		t.Fatal(err)
	}
	return f
}

// install stores an install of the scope; secrets are sealed like the API does.
func (f *msFixture) install(scope capabilities.Scope, id string, in capabilities.Install, secrets map[string]string) {
	f.t.Helper()
	in.Scope, in.ScopeID = scope, id
	if secrets != nil {
		plain, _ := json.Marshal(secrets)
		in.Secrets = f.box.seal(plain)
		for k := range secrets {
			in.SecretKeys = append(in.SecretKeys, k)
		}
	}
	if _, err := f.caps.PutInstall(context.Background(), in); err != nil {
		f.t.Fatal(err)
	}
}

// projectThread links a thread to a project; a personal thread has no link at all.
func (f *msFixture) projectThread(thread, project string) {
	f.t.Helper()
	if err := f.team.SetThreadMeta(context.Background(), store.ThreadMeta{ThreadID: thread, ProjectID: project, Owner: "alice", CreatedBy: "alice", Kind: store.ThreadChat}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *msFixture) servers(thread, login string) map[string]config.MCPServer {
	f.t.Helper()
	list, err := f.src(context.Background(), thread, login)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]config.MCPServer{}
	for _, s := range list {
		out[s.Name] = s
	}
	return out
}

func TestADefaultEnabledEntryIsActiveWithoutAnInstallAndAlwaysUntrusted(t *testing.T) {
	// Arrange
	f := newMSFixture(t)

	// Act
	got := f.servers("t-personal", "alice")

	// Assert: only the keyless entry; it carries the catalog's URL, type and default allow-list.
	if len(got) != 1 {
		t.Fatalf("servers = %+v", got)
	}
	s := got["keyless"]
	if s.URL != "https://keyless.example/mcp" || s.Type != "http" || !slices.Equal(s.Tools, []string{"search"}) || !s.Untrusted || s.Headers != nil {
		t.Fatalf("keyless = %+v", s)
	}
}

func TestAProjectThreadUsesTheProjectsInstallsAndNeverTheCallersPersonalOnes(t *testing.T) {
	// Arrange: alice has a personal key for "optional" and a personal custom server; the project has its own key.
	f := newMSFixture(t)
	f.install(capabilities.ScopeUser, "alice", capabilities.Install{CatalogID: "optional", Enabled: true}, map[string]string{"api_key": "ALICE-PERSONAL"})
	f.install(capabilities.ScopeUser, "alice", capabilities.Install{Name: "mine", URL: "https://mine.example/mcp", Transport: "http", Enabled: true}, nil)
	f.install(capabilities.ScopeProject, "p1", capabilities.Install{CatalogID: "optional", Enabled: true}, map[string]string{"api_key": "PROJECT-KEY"})
	f.projectThread("t-shared", "p1")

	// Act
	shared := f.servers("t-shared", "alice")
	personal := f.servers("t-personal", "alice")

	// Assert
	if got := shared["optional"].Headers["x-api-key"]; got != "PROJECT-KEY" {
		t.Fatalf("project thread used key %q", got)
	}
	if _, leaked := shared["mine"]; leaked {
		t.Fatal("a personal custom server reached a project thread")
	}
	for _, s := range shared {
		for _, v := range s.Headers {
			if strings.Contains(v, "ALICE") {
				t.Fatalf("a personal key reached a project thread: %+v", s)
			}
		}
	}
	if got := personal["optional"].Headers["x-api-key"]; got != "ALICE-PERSONAL" {
		t.Fatalf("personal thread used key %q", got)
	}
	if _, ok := personal["mine"]; !ok {
		t.Fatalf("personal thread lost the user's custom server: %+v", personal)
	}
}

func TestAnotherUsersInstallsNeverReachMyThread(t *testing.T) {
	// Arrange
	f := newMSFixture(t)
	f.install(capabilities.ScopeUser, "bob", capabilities.Install{CatalogID: "optional", Enabled: true}, map[string]string{"api_key": "BOB"})

	// Act / Assert
	if _, ok := f.servers("t1", "alice")["optional"]; ok {
		t.Fatal("bob's install is active for alice")
	}
}

func TestAnEntryMissingARequiredFieldIsNotActiveUntilItIsFilled(t *testing.T) {
	// Arrange: enabled, but the token is not set
	f := newMSFixture(t)
	f.install(capabilities.ScopeUser, "alice", capabilities.Install{CatalogID: "needs", Enabled: true}, nil)

	// Act
	before := f.servers("t1", "alice")
	f.install(capabilities.ScopeUser, "alice", capabilities.Install{CatalogID: "needs", Enabled: true}, map[string]string{"token": "tok"})
	after := f.servers("t1", "alice")

	// Assert
	if _, ok := before["needs"]; ok {
		t.Fatal("an entry without its required token is active")
	}
	if after["needs"].Headers["Authorization"] != "Bearer tok" {
		t.Fatalf("needs = %+v", after["needs"])
	}
}

func TestADisabledInstallTurnsADefaultOnEntryOff(t *testing.T) {
	// Arrange
	f := newMSFixture(t)
	f.projectThread("t-shared", "p1")
	f.install(capabilities.ScopeProject, "p1", capabilities.Install{CatalogID: "keyless", Enabled: false}, nil)

	// Act / Assert: off for the project, still on for the user's own sessions
	if _, ok := f.servers("t-shared", "alice")["keyless"]; ok {
		t.Fatal("a disabled default-on entry is still active")
	}
	if _, ok := f.servers("t-personal", "alice")["keyless"]; !ok {
		t.Fatal("the project's decision switched the user's entry off")
	}
}

func TestOptionalKeyHeadersAreSentOnlyWhenTheKeyIsSet(t *testing.T) {
	// Arrange
	f := newMSFixture(t)
	f.install(capabilities.ScopeUser, "alice", capabilities.Install{CatalogID: "optional", Enabled: true}, nil)
	withoutKey := f.servers("t1", "alice")["optional"]
	f.install(capabilities.ScopeUser, "alice", capabilities.Install{CatalogID: "optional", Enabled: true}, map[string]string{"api_key": "K"})

	// Act
	withKey := f.servers("t1", "alice")["optional"]

	// Assert: expansion fills every placeholder; a missing optional value leaves its headers out
	if withoutKey.Name == "" || len(withoutKey.Headers) != 0 {
		t.Fatalf("without a key: %+v", withoutKey)
	}
	if withKey.Headers["x-api-key"] != "K" || withKey.Headers["Authorization"] != "Bearer K" || withKey.Type != "sse" {
		t.Fatalf("with a key: %+v", withKey)
	}
}

func TestTheInstallsAllowListReachesTheServer(t *testing.T) {
	// Arrange: the install narrows the entry's default tools; a custom server has its own list, or none
	f := newMSFixture(t)
	f.install(capabilities.ScopeUser, "alice", capabilities.Install{CatalogID: "keyless", Enabled: true, Tools: []string{"fetch"}}, nil)
	f.install(capabilities.ScopeUser, "alice", capabilities.Install{Name: "mine", URL: "https://mine.example/mcp", Enabled: true, Tools: []string{"a", "b"}}, map[string]string{"X-Token": "t"})
	f.install(capabilities.ScopeUser, "alice", capabilities.Install{Name: "plain", URL: "https://plain.example/mcp", Transport: "sse", Enabled: true}, nil)

	// Act
	got := f.servers("t1", "alice")

	// Assert
	if !slices.Equal(got["keyless"].Tools, []string{"fetch"}) || !slices.Equal(got["mine"].Tools, []string{"a", "b"}) {
		t.Fatalf("tools: keyless %v, mine %v", got["keyless"].Tools, got["mine"].Tools)
	}
	if got["plain"].Tools != nil || got["plain"].Type != "sse" {
		t.Fatalf("a custom server without a list must expose read-only tools only: %+v", got["plain"])
	}
	if mine := got["mine"]; mine.Type != "http" || mine.Headers["X-Token"] != "t" || !mine.Untrusted {
		t.Fatalf("custom server: %+v", mine)
	}
}

func TestServersWhoseSecretsCannotBeOpenedAreSkippedAlone(t *testing.T) {
	// Arrange: one install sealed under another key, one garbage, one fine
	f := newMSFixture(t)
	other, _ := newSecretBoxFor(capabilitiesDomain, []byte("a-different-secret-key-01234567"))
	connector, _ := newSecretBox(msKey) // the connectors' box over the same key must not open them either
	f.install(capabilities.ScopeUser, "alice", capabilities.Install{CatalogID: "optional", Enabled: true, Secrets: other.seal([]byte(`{"api_key":"x"}`)), SecretKeys: []string{"api_key"}}, nil)
	f.install(capabilities.ScopeUser, "alice", capabilities.Install{Name: "sealed-by-connectors", URL: "https://c.example/mcp", Enabled: true, Secrets: connector.seal([]byte(`{"X":"y"}`)), SecretKeys: []string{"X"}}, nil)
	f.install(capabilities.ScopeUser, "alice", capabilities.Install{Name: "notjson", URL: "https://n.example/mcp", Enabled: true, Secrets: f.box.seal([]byte("not json")), SecretKeys: []string{"X"}}, nil)
	f.install(capabilities.ScopeUser, "alice", capabilities.Install{Name: "fine", URL: "https://fine.example/mcp", Enabled: true}, nil)

	// Act
	got := f.servers("t1", "alice")

	// Assert
	for _, name := range []string{"optional", "sealed-by-connectors", "notjson"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s is active although its credentials cannot be opened", name)
		}
	}
	if _, ok := got["fine"]; !ok {
		t.Error("a server with readable credentials was dropped with the broken ones")
	}
	if _, ok := got["keyless"]; !ok {
		t.Error("the default entry was dropped")
	}
}

func TestWithoutASecretKeyOnlyServersWithoutCredentialsRun(t *testing.T) {
	// Arrange
	f := newMSFixture(t)
	src, err := NewMCPSource(f.team, f.caps, f.catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.install(capabilities.ScopeUser, "alice", capabilities.Install{CatalogID: "optional", Enabled: true}, map[string]string{"api_key": "K"})

	// Act
	list, err := src(context.Background(), "t1", "alice")

	// Assert
	if err != nil || len(list) != 1 || list[0].Name != "keyless" {
		t.Fatalf("servers %+v, err %v", list, err)
	}
}

func TestAnOrphanedInstallIsNotActive(t *testing.T) {
	// Arrange: an install of an entry the catalog no longer ships
	f := newMSFixture(t)
	f.install(capabilities.ScopeUser, "alice", capabilities.Install{CatalogID: "retired", Enabled: true}, nil)

	// Act / Assert
	if _, ok := f.servers("t1", "alice")["retired"]; ok {
		t.Fatal("an orphaned install is active")
	}
}

func TestANilCatalogReadsTheStoresCatalogAtEachRun(t *testing.T) {
	// Arrange
	f := newMSFixture(t)
	src, err := NewMCPSource(f.team, f.caps, nil, msKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	empty, _ := src(ctx, "t1", "alice")
	if err := f.caps.SeedCatalog(ctx, f.catalog); err != nil {
		t.Fatal(err)
	}

	// Act
	seeded, _ := src(ctx, "t1", "alice")

	// Assert
	if len(empty) != 0 || len(seeded) != 1 || seeded[0].Name != "keyless" {
		t.Fatalf("before seeding %v, after %v", empty, seeded)
	}
}

func TestNewMCPSourceNeedsBothStores(t *testing.T) {
	if _, err := NewMCPSource(nil, capstore.NewMem(nil), nil, msKey); err == nil {
		t.Error("no team store was accepted")
	}
	if _, err := NewMCPSource(teamstore.NewMem(), nil, nil, msKey); err == nil {
		t.Error("no capability store was accepted")
	}
}

// msFaulty fails the named store operation.
type msFaultyTeam struct {
	store.TeamStore
	err error
}

func (f msFaultyTeam) GetThreadMeta(context.Context, string) (*store.ThreadMeta, error) {
	return nil, f.err
}

type msFaultyCaps struct {
	capabilities.Store
	catalog, installs bool
}

func (f msFaultyCaps) ListCatalog(ctx context.Context) ([]capabilities.Entry, error) {
	if f.catalog {
		return nil, errors.New("boom")
	}
	return f.Store.ListCatalog(ctx)
}

func (f msFaultyCaps) ListInstalls(ctx context.Context, s capabilities.Scope, id string) ([]capabilities.Install, error) {
	if f.installs {
		return nil, errors.New("boom")
	}
	return f.Store.ListInstalls(ctx, s, id)
}

func TestStoreFailuresAreReportedNotMistakenForAnEmptyMarketplace(t *testing.T) {
	// Arrange / Act / Assert: a failing scope lookup must not fall back to the user's installs (it could be a
	// project thread), and a failing store is an error the provider logs.
	mem := capstore.NewMem(nil)
	cases := map[string]struct {
		team store.TeamStore
		caps capabilities.Store
		cat  []capabilities.Entry
	}{
		"thread lookup": {msFaultyTeam{teamstore.NewMem(), errors.New("boom")}, mem, nil},
		"catalog":       {teamstore.NewMem(), msFaultyCaps{Store: mem, catalog: true}, nil},
		"installs":      {teamstore.NewMem(), msFaultyCaps{Store: mem, installs: true}, []capabilities.Entry{}},
	}
	for name, c := range cases {
		src, err := NewMCPSource(c.team, c.caps, c.cat, msKey)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := src(context.Background(), "t1", "alice"); err == nil {
			t.Errorf("%s: the failure was swallowed", name)
		}
	}
	// A run without a user has nothing to look up.
	src, _ := NewMCPSource(teamstore.NewMem(), mem, nil, msKey)
	if list, err := src(context.Background(), "t1", ""); err != nil || list != nil {
		t.Errorf("anonymous run: %v, %v", list, err)
	}
}

func TestACapabilitySecretNeverOpensAsAConnectorTokenOrTheOtherWayRound(t *testing.T) {
	// Arrange
	conn, _ := newSecretBox(msKey)
	caps, _ := newSecretBoxFor(capabilitiesDomain, msKey)

	// Act / Assert
	if _, err := conn.open(caps.seal([]byte("x"))); err == nil {
		t.Error("a capability secret opened as a connector token")
	}
	if _, err := caps.open(conn.seal([]byte("x"))); err == nil {
		t.Error("a connector token opened as a capability secret")
	}
}

func TestSecretKeyFromEnvPrefersTheExplicitKeyThenTheOIDCSessionKey(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"HARNESS_SECRET_KEY": "a", "OIDC_ISSUER": "i", "OIDC_SESSION_KEY": "b"}, "a"},
		{map[string]string{"OIDC_ISSUER": "i", "OIDC_SESSION_KEY": "b"}, "b"},
		{map[string]string{"OIDC_SESSION_KEY": "b"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := string(SecretKeyFromEnv(env(c.env))); got != c.want {
			t.Errorf("%v: key %q, want %q", c.env, got, c.want)
		}
	}
}
