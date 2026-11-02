package product

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

func TestNewGitHubAppAcceptsPKCS1AndPKCS8AndRejectsOthers(t *testing.T) {
	// Arrange
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	ecKey := mustECKeyPKCS8(t)
	for name, tc := range map[string]struct {
		pem     []byte
		wantErr string
	}{
		"PKCS#1":         {pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), ""},
		"PKCS#8":         {pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), ""},
		"not PEM":        {[]byte("-----nope"), "not PEM"},
		"not an RSA key": {pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecKey}), "not RSA"},
		"garbage in PEM": {pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("garbage")}), "parsing the private key"},
	} {
		// Act
		app, err := NewGitHubApp("1", "slug", tc.pem)

		// Assert
		switch {
		case tc.wantErr == "" && (err != nil || app == nil):
			t.Errorf("%s: %v", name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err = %v, want %q", name, err, tc.wantErr)
		}
	}
}

func TestGitHubAppDefaultsToGitHubDotCom(t *testing.T) {
	// Arrange
	g := &GitHubApp{}

	// Act / Assert
	if g.api() != "https://api.github.com" || g.web() != "https://github.com" {
		t.Fatalf("defaults %q %q", g.api(), g.web())
	}
	g.API, g.WebBase = "http://api.test/", "http://web.test///"
	if g.api() != "http://api.test" || g.web() != "http://web.test" {
		t.Fatalf("trailing slashes kept: %q %q", g.api(), g.web())
	}
	custom := &http.Client{Timeout: time.Second}
	g.HTTP = custom
	if g.client() != custom {
		t.Fatal("the configured HTTP client is ignored")
	}
}

func TestGitHubAppErrorsAreSurfaced(t *testing.T) {
	// Arrange
	gh := newCnGH(t)
	app, _ := testApp(t, gh.srv.URL)
	ctx := context.Background()

	// Act / Assert: 404 means "not installed", any other failure is a real error (not mistaken for "not installed").
	gh.set(func(g *cnGH) { g.appStatus = 500 })
	if _, err := app.RepoInstallation(ctx, "acme", "api"); err == nil || errors.Is(err, ErrNotInstalled) {
		t.Fatalf("500 reported as %v", err)
	}
	gh.set(func(g *cnGH) { g.appStatus, g.appInstall = 0, 0 })
	if _, err := app.RepoInstallation(ctx, "acme", "api"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("404 reported as %v", err)
	}
	gh.set(func(g *cnGH) { g.tokenStatus = 403 })
	if tok, err := app.InstallationToken(ctx, 5); err == nil || tok != "" {
		t.Fatalf("token despite 403: %q, %v", tok, err)
	}
	// A failed mint is not cached: the next call asks again and succeeds once GitHub recovers.
	gh.set(func(g *cnGH) { g.tokenStatus = 0 })
	if tok, err := app.InstallationToken(ctx, 5); err != nil || tok != "ghs_5" {
		t.Fatalf("after recovery: %q, %v", tok, err)
	}
	// An unreachable API fails instead of hanging or returning a zero id.
	gh.srv.Close()
	if _, err := app.RepoInstallation(ctx, "acme", "api"); err == nil {
		t.Fatal("unreachable GitHub gave an installation")
	}
}

func TestGitHubStatusMessagesGuideTheUser(t *testing.T) {
	// Arrange
	s := &server{opt: Options{GitHub: &GitHubApp{Slug: "my-app"}}}
	none := &server{}
	for name, tc := range map[string]struct {
		srv       *server
		project   store.Project
		connected bool
		msg       string
		install   bool
	}{
		"no app configured":     {none, store.Project{RepoURL: "https://github.com/a/b"}, false, "no GitHub App", false},
		"no repository":         {s, store.Project{}, false, "Set the repository URL first", false},
		"not a github.com repo": {s, store.Project{RepoURL: "https://gitlab.com/a/b"}, false, "Only github.com", false},
		"app not installed":     {s, store.Project{RepoURL: "https://github.com/a/b"}, false, "Install the GitHub App", true},
		"connected":             {s, store.Project{RepoURL: "https://github.com/a/b", GitHubInstallation: 9}, true, "", false},
	} {
		// Act
		st := tc.srv.githubState(&tc.project)

		// Assert
		if st.Connected != tc.connected || !strings.Contains(st.Message, tc.msg) || (st.InstallURL != "") != tc.install {
			t.Errorf("%s: %+v", name, st)
		}
		if tc.install && st.InstallURL != "https://github.com/apps/my-app/installations/new" {
			t.Errorf("%s: install url %q", name, st.InstallURL)
		}
	}
}

func TestProjectGitHubConnectRoutes(t *testing.T) {
	// Arrange
	r, gh, tm := cnGitHubRig(t, nil)
	mk := func(repo string) string {
		return r.json("POST", "/api/projects", "alice", `{"name":"p","repoUrl":"`+repo+`"}`, 201)["id"].(string)
	}
	api, other, noURL := mk("https://github.com/acme/api"), mk("https://gitlab.com/acme/api"), mk("")
	r.json("PUT", "/api/projects/"+api+"/members/bob", "alice", `{"role":"member"}`, 204)
	installation := func(id string) int64 {
		p, _ := tm.GetProject(context.Background(), id)
		return p.GitHubInstallation
	}

	// Act / Assert: only owners connect; members can read the status.
	r.json("POST", "/api/projects/"+api+"/github/connect", "bob", `{}`, 403)
	r.json("POST", "/api/projects/"+api+"/github/connect", "carol", `{}`, 404)
	if st := r.json("GET", "/api/projects/"+api+"/github", "bob", "", 200); st["connected"] != false || st["repo"] != "acme/api" {
		t.Fatalf("status = %v", st)
	}
	st := r.json("POST", "/api/projects/"+api+"/github/connect", "alice", `{}`, 200)
	if st["connected"] != true || st["repo"] != "acme/api" || installation(api) != 77 {
		t.Fatalf("connect = %v, installation %d", st, installation(api))
	}
	// A repository outside GitHub or without a URL just reports its status; nothing is stored.
	for _, id := range []string{other, noURL} {
		if st := r.json("POST", "/api/projects/"+id+"/github/connect", "alice", `{}`, 200); st["connected"] != false || st["message"] == nil {
			t.Fatalf("status for %s = %v", id, st)
		}
	}
	// The App uninstalled from the repository disconnects the project.
	gh.set(func(g *cnGH) { g.appInstall = 0 })
	if st := r.json("POST", "/api/projects/"+api+"/github/connect", "alice", `{}`, 200); st["connected"] != false || installation(api) != 0 {
		t.Fatalf("after uninstall = %v, installation %d", st, installation(api))
	}
	// A GitHub outage is a 502 and keeps the previous state.
	gh.set(func(g *cnGH) { g.appInstall, g.appStatus = 77, 500 })
	r.json("POST", "/api/projects/"+api+"/github/connect", "alice", `{}`, 502)
	gh.set(func(g *cnGH) { g.appStatus = 0 })
	tm.failOn("UpdateProject", errCnBoom)
	r.json("POST", "/api/projects/"+api+"/github/connect", "alice", `{}`, 500)
}

func TestProjectGitHubConnectWithoutAppReportsStatus(t *testing.T) {
	// Arrange
	r := newTeamRig(t)

	// Act
	st := r.json("POST", "/api/projects/"+r.project+"/github/connect", "alice", `{}`, 200)

	// Assert
	if st["configured"] != false || st["connected"] != false || !strings.Contains(st["message"].(string), "no GitHub App") {
		t.Fatalf("status = %v", st)
	}
}

func TestFileContentReadsRawFilesAndMapsStatuses(t *testing.T) {
	// Arrange
	gh := newCnGH(t)
	app, _ := testApp(t, gh.srv.URL)
	ctx := context.Background()
	gh.set(func(g *cnGH) { g.fileBody = "# Rules\n" })

	// Act
	body, err := app.FileContent(ctx, 5, "acme", "api", "AGENTS.md", "feat/x y")

	// Assert
	if err != nil || string(body) != "# Rules\n" {
		t.Fatalf("content = %q, %v", body, err)
	}
	if refs := gh.fileRefs; len(refs) != 1 || refs[0] != "ref=feat%2Fx+y" {
		t.Fatalf("ref query = %v", refs)
	}
	if _, err := app.FileContent(ctx, 5, "acme", "api", "AGENTS.md", ""); err != nil || gh.fileRefs[1] != "" {
		t.Fatalf("default branch: %v %v", err, gh.fileRefs)
	}
	gh.set(func(g *cnGH) { g.fileStatus = 404 })
	if _, err := app.FileContent(ctx, 5, "acme", "api", "AGENTS.md", ""); !errors.Is(err, errNoRepoFile) {
		t.Fatalf("404 = %v", err)
	}
	gh.set(func(g *cnGH) { g.fileStatus = 500 })
	if _, err := app.FileContent(ctx, 5, "acme", "api", "AGENTS.md", ""); err == nil || errors.Is(err, errNoRepoFile) || !strings.Contains(err.Error(), "500") {
		t.Fatalf("500 = %v", err)
	}
	// Without an installation token there is nothing to read with.
	gh.set(func(g *cnGH) { g.tokenStatus = 401 })
	if _, err := app.FileContent(ctx, 6, "acme", "api", "AGENTS.md", ""); err == nil {
		t.Fatal("read without a token")
	}
	gh.srv.Close()
	if _, err := app.FileContent(ctx, 5, "acme", "api", "AGENTS.md", ""); err == nil {
		t.Fatal("unreachable GitHub gave content")
	}
}

func mustECKeyPKCS8(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return der
}
