package product

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// appGH is a GitHub that answers the read-only calls the factory preflight makes.
type appGH struct {
	mu      sync.Mutex
	pub     *rsa.PublicKey
	t       *testing.T
	status  map[string]int // path -> forced status
	seen    []string       // "METHOD path authKind" per request
	tokRepo [][]string     // repositories each token was limited to
}

func newAppGH(t *testing.T) (*appGH, *GitHubApp) {
	t.Helper()
	g := &appGH{t: t, status: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(srv.Close)
	app, pub := testApp(t, srv.URL)
	g.pub = pub
	return g, app
}

func (g *appGH) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	kind := "installation"
	if strings.Count(bearer, ".") == 2 {
		kind = "app"
		verifyJWT(g.t, bearer, g.pub)
	}
	g.seen = append(g.seen, r.Method+" "+r.URL.EscapedPath()+" "+kind)
	if st := g.status[r.URL.EscapedPath()]; st != 0 {
		w.WriteHeader(st)
		return
	}
	switch p := r.URL.EscapedPath(); {
	case p == "/app":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"slug": "my-app", "name": "My App",
			"permissions": map[string]string{"contents": "write", "pull_requests": "write", "checks": "read", "metadata": "read"},
			"events":      []string{"issues", "pull_request"},
		})
	case strings.HasSuffix(p, "/access_tokens"):
		var body struct {
			Repositories []string `json:"repositories"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.tokRepo = append(g.tokRepo, body.Repositories)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_x","expires_at":"2099-01-01T00:00:00Z","permissions":{"contents":"write","pull_requests":"write","metadata":"read"}}`))
	case p == "/repos/acme/api":
		// An installation token reads the repository with a user-style "permissions" that is all false.
		_, _ = w.Write([]byte(`{"full_name":"acme/api","default_branch":"main","private":true,"permissions":{"admin":false,"push":false,"pull":false}}`))
	case p == "/repos/acme/api/branches/main":
		_, _ = w.Write([]byte(`{"name":"main","protected":true,"commit":{"sha":"abc123"}}`))
	case p == "/repos/acme/api/branches/feat/x%20y":
		_, _ = w.Write([]byte(`{"name":"feat/x y","protected":false,"commit":{"sha":"def456"}}`))
	default:
		http.NotFound(w, r)
	}
}

func TestInfoReadsPermissionsAndEventsAsTheApp(t *testing.T) {
	// Arrange
	gh, app := newAppGH(t)

	// Act
	info, err := app.Info(context.Background())

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if info.Slug != "my-app" || info.Name != "My App" {
		t.Fatalf("identity = %+v", info)
	}
	if info.Permissions["contents"] != "write" || info.Permissions["checks"] != "read" || len(info.Permissions) != 4 {
		t.Fatalf("permissions = %v", info.Permissions)
	}
	if len(info.Events) != 2 || info.Events[0] != "issues" {
		t.Fatalf("events = %v", info.Events)
	}
	if len(gh.seen) != 1 || gh.seen[0] != "GET /app app" {
		t.Fatalf("requests = %v", gh.seen)
	}
}

func TestInfoSurfacesGitHubFailures(t *testing.T) {
	// Arrange
	gh, app := newAppGH(t)
	gh.status["/app"] = 401

	// Act
	info, err := app.Info(context.Background())

	// Assert
	if err == nil || info != nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("info = %v, err = %v", info, err)
	}
}

func TestRepoDetailsUsesATokenLimitedToTheRepository(t *testing.T) {
	// Arrange
	gh, app := newAppGH(t)

	// Act
	d, err := app.RepoDetails(context.Background(), 42, "acme", "api")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	want := RepoDetails{FullName: "acme/api", DefaultBranch: "main", Private: true, CanPush: true, CanAdmin: false}
	if *d != want {
		t.Fatalf("details = %+v, want %+v", *d, want)
	}
	if len(gh.tokRepo) != 1 || len(gh.tokRepo[0]) != 1 || gh.tokRepo[0][0] != "api" {
		t.Fatalf("token limited to %v", gh.tokRepo)
	}
	if last := gh.seen[len(gh.seen)-1]; last != "GET /repos/acme/api installation" {
		t.Fatalf("last request = %q", last)
	}
}

func TestRepoDetailsFailsOnTokenOrRepositoryErrors(t *testing.T) {
	// Arrange
	gh, app := newAppGH(t)
	ctx := context.Background()

	// Act / Assert: the repository read fails
	gh.status["/repos/acme/api"] = 500
	if d, err := app.RepoDetails(ctx, 42, "acme", "api"); err == nil || d != nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("500: %v, %v", d, err)
	}
	// the token cannot be minted
	gh.status["/app/installations/43/access_tokens"] = 403
	if d, err := app.RepoDetails(ctx, 43, "acme", "api"); err == nil || d != nil {
		t.Fatalf("no token: %v, %v", d, err)
	}
}

func TestBranchReportsProtectionAndHeadAndMapsMissing(t *testing.T) {
	// Arrange
	_, app := newAppGH(t)
	ctx := context.Background()

	// Act
	main, err := app.Branch(ctx, 42, "acme", "api", "main")
	slashed, err2 := app.Branch(ctx, 42, "acme", "api", "feat/x y")
	_, errMissing := app.Branch(ctx, 42, "acme", "api", "nope")

	// Assert
	if err != nil || *main != (BranchInfo{Name: "main", Protected: true, SHA: "abc123"}) {
		t.Fatalf("main = %+v, %v", main, err)
	}
	if err2 != nil || *slashed != (BranchInfo{Name: "feat/x y", Protected: false, SHA: "def456"}) {
		t.Fatalf("slashed = %+v, %v", slashed, err2)
	}
	if !errors.Is(errMissing, ErrBranchNotFound) {
		t.Fatalf("missing = %v", errMissing)
	}
}

func TestBranchFailsOnTokenOrServerErrors(t *testing.T) {
	// Arrange
	gh, app := newAppGH(t)
	ctx := context.Background()
	gh.status["/repos/acme/api/branches/main"] = 502

	// Act / Assert
	if b, err := app.Branch(ctx, 42, "acme", "api", "main"); err == nil || b != nil || errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("502: %v, %v", b, err)
	}
	gh.status["/app/installations/44/access_tokens"] = 403
	if b, err := app.Branch(ctx, 44, "acme", "api", "main"); err == nil || b != nil {
		t.Fatalf("no token: %v, %v", b, err)
	}
}

func TestInstallationGrantCanComparesLevelsAndTreatsMissingAsNothing(t *testing.T) {
	g := InstallationGrant{"contents": "write", "metadata": "read", "administration": "admin"}
	cases := []struct {
		perm, level string
		want        bool
	}{
		{"contents", "read", true}, {"contents", "write", true}, {"contents", "admin", false},
		{"metadata", "read", true}, {"metadata", "write", false},
		{"administration", "write", true}, {"issues", "read", false},
		{"contents", "bogus", false}, {"", "read", false},
	}
	for _, c := range cases {
		if got := g.Can(c.perm, c.level); got != c.want {
			t.Errorf("Can(%q, %q) = %v, want %v", c.perm, c.level, got, c.want)
		}
	}
	if InstallationGrant(nil).Can("contents", "read") {
		t.Error("a token with no grant can do something")
	}
}

func TestRepoDetailsReportsWhatTheTokenMayDoNotWhatTheRepositoryFieldSays(t *testing.T) {
	// Arrange: the repository answers with an all-false user-style "permissions" (as it does for an installation
	// token); only the token's grant says the App may write contents.
	_, app := newAppGH(t)

	// Act
	d, err := app.RepoDetails(context.Background(), 42, "acme", "api")

	// Assert
	if err != nil || !d.CanPush || d.CanAdmin {
		t.Errorf("details = %+v, err = %v; want CanPush from the grant and no admin", d, err)
	}
}
