package product

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
)

func testApp(t *testing.T, api string) (*GitHubApp, *rsa.PublicKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	app, err := NewGitHubApp("12345", "my-app", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
	if err != nil {
		t.Fatal(err)
	}
	app.API = api
	return app, &key.PublicKey
}

func verifyJWT(t *testing.T, tok string, pub *rsa.PublicKey) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt = %q", tok)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, 5, sum[:], sig); err != nil {
		t.Fatalf("bad jwt signature: %v", err)
	}
	var claims map[string]any
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(b, &claims)
	return claims
}

func TestGitHubAppFindsInstallationAndMintsCachedTokens(t *testing.T) {
	var tokens atomic.Int32
	var pub *rsa.PublicKey
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		claims := verifyJWT(t, bearer, pub)
		if claims["iss"] != "12345" {
			t.Errorf("iss = %v", claims["iss"])
		}
		switch {
		case r.URL.Path == "/repos/acme/api/installation":
			json.NewEncoder(w).Encode(map[string]any{"id": 42})
		case r.URL.Path == "/app/installations/42/access_tokens" && r.Method == "POST":
			tokens.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"token": "ghs_secret", "expires_at": time.Now().Add(time.Hour)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	var app *GitHubApp
	app, pub = testApp(t, api.URL)

	ctx := context.Background()
	if id, err := app.RepoInstallation(ctx, "acme", "api"); err != nil || id != 42 {
		t.Fatalf("installation = %d, %v", id, err)
	}
	if _, err := app.RepoInstallation(ctx, "acme", "other"); err != ErrNotInstalled {
		t.Fatalf("uninstalled repo: %v", err)
	}
	for i := 0; i < 3; i++ {
		if tok, err := app.InstallationToken(ctx, 42); err != nil || tok != "ghs_secret" {
			t.Fatalf("token = %q, %v", tok, err)
		}
	}
	if tokens.Load() != 1 {
		t.Fatalf("token minted %d times, want 1 (cached)", tokens.Load())
	}
}

func TestParseRepo(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/api":      "acme/api",
		"https://github.com/acme/api.git":  "acme/api",
		"https://github.com/acme/api/":     "acme/api",
		"git@github.com:acme/api.git":      "acme/api",
		"ssh://git@github.com/acme/api":    "acme/api",
		"https://gitlab.com/acme/api":      "",
		"https://github.com/acme":          "",
		"https://github.com/acme/api/tree": "",
		"":                                 "",
	} {
		o, r, ok := ParseRepo(in)
		got := ""
		if ok {
			got = o + "/" + r
		}
		if got != want {
			t.Errorf("ParseRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeSandbox records the commands it is asked to run.
type fakeSandbox struct {
	sandbox.Sandbox
	reqs []sandbox.ExecRequest
	exit int
	out  string
}

func (f *fakeSandbox) ID() string { return "fake" }
func (f *fakeSandbox) Exec(_ context.Context, r sandbox.ExecRequest) (sandbox.ExecResult, error) {
	f.reqs = append(f.reqs, r)
	if r.OnStderr != nil {
		r.OnStderr(f.out)
	}
	return sandbox.ExecResult{ExitCode: f.exit}, nil
}

func repoRig(t *testing.T, repoURL string, installation int64) (*RepoCheckout, string) {
	t.Helper()
	tm := teamstore.NewMem()
	ctx := context.Background()
	p, _ := tm.CreateProject(ctx, store.Project{Name: "api", RepoURL: repoURL, DefaultBranch: "main", GitHubInstallation: installation}, "alice")
	_ = tm.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "11111111-2222-3333-4444-555555555555", ProjectID: p.ID, Owner: "alice", CreatedBy: "alice"})
	return &RepoCheckout{Team: tm}, "11111111-2222-3333-4444-555555555555"
}

func TestCheckoutClonesOnAThreadBranchWithoutPuttingTheTokenInTheCommand(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_supersecret", "expires_at": time.Now().Add(time.Hour)})
	}))
	defer api.Close()
	rc, thread := repoRig(t, "https://github.com/acme/api", 42)
	rc.GitHub, _ = testApp(t, api.URL)

	sb := &fakeSandbox{}
	note := rc.setup(context.Background(), thread, sb)

	if len(sb.reqs) != 2 {
		t.Fatalf("expected the clone and the credential write, got %d commands", len(sb.reqs))
	}
	clone := sb.reqs[0]
	if strings.Contains(clone.Command, "ghs_supersecret") || strings.Contains(sb.reqs[1].Command, "ghs_supersecret") {
		t.Fatal("the token is in a command line")
	}
	if clone.Env["GH_TOKEN"] != "ghs_supersecret" || clone.Env["REPO_URL"] != "https://github.com/acme/api.git" ||
		clone.Env["THREAD_BRANCH"] != "har/11111111" || clone.Env["REPO_BRANCH"] != "main" {
		t.Fatalf("env = %v", clone.Env)
	}
	if !strings.Contains(note, "har/11111111") {
		t.Fatalf("note = %q", note)
	}
}

func TestCheckoutOfAPublicRepoNeedsNoToken(t *testing.T) {
	rc, thread := repoRig(t, "https://example.com/team/repo.git", 0)
	sb := &fakeSandbox{}
	rc.setup(context.Background(), thread, sb)
	if len(sb.reqs) != 1 || sb.reqs[0].Env["GH_TOKEN"] != "" || sb.reqs[0].Env["REPO_URL"] != "https://example.com/team/repo.git" {
		t.Fatalf("reqs = %+v", sb.reqs)
	}
}

func TestCheckoutRefusesUnsafeInputsAndRedactsFailures(t *testing.T) {
	rc, thread := repoRig(t, "https://user:pass@example.com/team/repo.git", 0)
	sb := &fakeSandbox{}
	if note := rc.setup(context.Background(), thread, sb); len(sb.reqs) != 0 || !strings.Contains(note, "not checked out") {
		t.Fatalf("credentials in URL accepted: %q %v", note, sb.reqs)
	}
	rc2, thread2 := repoRig(t, "file:///etc", 0)
	if note := rc2.setup(context.Background(), thread2, sb); len(sb.reqs) != 0 || !strings.Contains(note, "not checked out") {
		t.Fatalf("non-https URL accepted: %q", note)
	}
	// A failing clone reports its output with the token removed.
	if got := redact("fatal: could not read https://x:ghs_abc@github.com", "ghs_abc"); strings.Contains(got, "ghs_abc") {
		t.Fatalf("redact = %q", got)
	}
}

// brokerSandbox is a fakeSandbox with a credential broker.
type brokerSandbox struct {
	fakeSandbox
	sets []sandbox.CredentialSet
	err  error
}

func (b *brokerSandbox) SetCredentials(_ context.Context, set sandbox.CredentialSet) error {
	b.sets = append(b.sets, set)
	return b.err
}

func TestInstallationTokenIsLimitedToTheProjectsRepository(t *testing.T) {
	var bodies []map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_t" + string(rune('0'+len(bodies))), "expires_at": time.Now().Add(time.Hour)})
	}))
	defer api.Close()
	app, _ := testApp(t, api.URL)
	ctx := context.Background()

	scoped, _ := app.InstallationToken(ctx, 42, "api")
	other, _ := app.InstallationToken(ctx, 42, "web")
	all, _ := app.InstallationToken(ctx, 42)
	again, _ := app.InstallationToken(ctx, 42, "api")

	if repos, _ := bodies[0]["repositories"].([]any); len(repos) != 1 || repos[0] != "api" {
		t.Fatalf("scoped request body = %v", bodies[0])
	}
	if _, has := bodies[2]["repositories"]; has {
		t.Fatalf("an unscoped request must not name repositories: %v", bodies[2])
	}
	// Tokens of different scopes are different tokens; the same scope is served from the cache.
	if len(bodies) != 3 || scoped == other || scoped == all || again != scoped {
		t.Fatalf("tokens %q %q %q %q after %d requests", scoped, other, all, again, len(bodies))
	}
}

func TestBrokeredCheckoutKeepsTheTokenOutOfTheSandbox(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_supersecret", "expires_at": time.Now().Add(time.Hour)})
	}))
	defer api.Close()
	rc, thread := repoRig(t, "https://github.com/acme/api", 42)
	rc.GitHub, _ = testApp(t, api.URL)
	sb := &brokerSandbox{}

	note := rc.setup(context.Background(), thread, sb)

	if len(sb.reqs) != 1 {
		t.Fatalf("expected only the clone, got %d commands (no credential file write)", len(sb.reqs))
	}
	for _, r := range sb.reqs {
		if strings.Contains(r.Command, "ghs_supersecret") || r.Env["GH_TOKEN"] != "" {
			t.Fatalf("the token reached the sandbox: %+v", r)
		}
	}
	if len(sb.sets) != 1 || !strings.Contains(note, "har/11111111") {
		t.Fatalf("broker calls %d, note %q", len(sb.sets), note)
	}
	set := sb.sets[0]
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_supersecret"))
	if len(set.Credentials) != 2 || set.Credentials[0].Value != basic || set.Credentials[1].Value != "ghs_supersecret" {
		t.Fatalf("credentials = %+v", set.Credentials)
	}
	// Each binding is limited to this repository's paths on its host, so the token is useless anywhere else.
	for _, b := range set.Bindings {
		for _, p := range b.Paths {
			if !strings.Contains(p, "/acme/api") {
				t.Errorf("binding %s path %q is not limited to acme/api", b.Name, p)
			}
		}
	}
	if set.Bindings[0].Host != "github.com" || set.Bindings[0].Auth != sandbox.AuthBasic ||
		set.Bindings[1].Host != "api.github.com" || set.Bindings[1].Auth != sandbox.AuthBearer {
		t.Fatalf("bindings = %+v", set.Bindings)
	}
}

func TestBrokeredRefreshRenewsTheVaultAndNeverWritesAFile(t *testing.T) {
	n := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_" + string(rune('a'+n)), "expires_at": time.Now().Add(time.Hour)})
	}))
	defer api.Close()
	rc, _ := repoRig(t, "https://github.com/acme/api", 42)
	rc.GitHub, _ = testApp(t, api.URL)
	p := &store.Project{ID: "p", RepoURL: "https://github.com/acme/api", GitHubInstallation: 42}
	sb := &brokerSandbox{}

	ok := rc.writeCredentials(context.Background(), sb, p, "ghs_new")

	if !ok || len(sb.sets) != 1 || len(sb.reqs) != 0 {
		t.Fatalf("ok=%v broker calls=%d commands=%d", ok, len(sb.sets), len(sb.reqs))
	}
}

func TestCheckoutNeverFallsBackToPuttingTheTokenInTheSandboxWhenTheBrokerFails(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_supersecret", "expires_at": time.Now().Add(time.Hour)})
	}))
	defer api.Close()
	rc, thread := repoRig(t, "https://github.com/acme/api", 42)
	rc.GitHub, _ = testApp(t, api.URL)
	sb := &brokerSandbox{err: context.DeadlineExceeded}

	rc.setup(context.Background(), thread, sb)

	// A broker that cannot take the token must not turn into a credential file or an environment variable: the
	// clone runs unauthenticated (and fails for a private repository) instead of exposing the secret.
	for _, r := range sb.reqs {
		if r.Env["GH_TOKEN"] != "" || strings.Contains(r.Command, "ghs_supersecret") || strings.Contains(r.Command, "git-credentials") {
			t.Fatalf("the token reached the sandbox after a broker failure: %+v", r)
		}
	}
	if len(sb.reqs) != 1 {
		t.Fatalf("commands = %d, want just the clone", len(sb.reqs))
	}
}
