package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
)

// wbUnset removes a variable for the test: the code under test distinguishes "unset" from "empty".
func wbUnset(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "") // registers the restore of the original value
		_ = os.Unsetenv(k)
	}
}

// wbEnv is a self-contained configuration: one model, no sandbox, no shared infrastructure, private directories.
// The variables that run sets by default are cleared first so that the test restores them afterwards.
func wbEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("LLM_DEFAULT_MODEL", "gpt-test")
	t.Setenv("LOCAL_STATE_DIR", t.TempDir())
	t.Setenv("LOCAL_WORKSPACE", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HAR_HOME", filepath.Join(t.TempDir(), "har"))
	for _, k := range []string{"SANDBOX_BACKEND", "OPENSANDBOX_URL", "AGENT_PROFILE", "MEMORY_IN_CODING", "HARNESS_MEMORY_EXTRACT", "HARNESS_ALLOW_LOCAL_SANDBOX"} {
		t.Setenv(k, "")
	}
	wbUnset(t, "POSTGRES_HOST", "REDIS_HOST", "GITHUB_APP_ID")
}

// wbBuf is a bytes.Buffer safe to read while run writes to it.
type wbBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *wbBuf) Write(p []byte) (int, error) { w.mu.Lock(); defer w.mu.Unlock(); return w.b.Write(p) }
func (w *wbBuf) String() string              { w.mu.Lock(); defer w.mu.Unlock(); return w.b.String() }

type wbServer struct {
	URL    string
	stderr *wbBuf
	cancel context.CancelFunc
	done   chan int
	once   sync.Once
	code   int
}

// wbStart runs the command until the test ends and returns the address it announced.
func wbStart(t *testing.T, genv map[string]string, args ...string) *wbServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &wbServer{stderr: &wbBuf{}, cancel: cancel, done: make(chan int, 1)}
	pr, pw := io.Pipe()
	go func() {
		code := run(ctx, append([]string{"-addr", "127.0.0.1:0"}, args...), env(genv), pw, s.stderr)
		pw.Close()
		s.done <- code
	}()
	t.Cleanup(func() { s.Stop(t) })
	line, _ := bufio.NewReader(pr).ReadString('\n')
	if !strings.HasPrefix(line, "harness-web: http://") {
		cancel()
		t.Fatalf("the server did not start: stdout %q stderr %q", line, s.stderr.String())
	}
	s.URL = strings.Fields(strings.TrimPrefix(line, "harness-web: "))[0]
	return s
}

// Stop ends the server and returns its exit code.
func (s *wbServer) Stop(t *testing.T) int {
	t.Helper()
	s.once.Do(func() {
		s.cancel()
		select {
		case s.code = <-s.done:
		case <-time.After(15 * time.Second):
			t.Error("the server did not stop")
			s.code = -1
		}
	})
	return s.code
}

func wbDo(t *testing.T, method, url, host string, hdr map[string]string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

var (
	wbKeyOnce sync.Once
	wbKeyPEM  []byte
)

// wbPrivateKeyPEM is a throw-away GitHub App key.
func wbPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	wbKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		wbKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	})
	return wbKeyPEM
}

// --- configuration readers ---

func TestOIDCFromEnvRequiresTeamModeAndTheClientSettings(t *testing.T) {
	full := map[string]string{
		"OIDC_ISSUER": "https://id.example.com", "OIDC_CLIENT_ID": "cid", "OIDC_CLIENT_SECRET": "sec",
		"OIDC_REDIRECT_URL": "https://app.example.com/auth/callback", "OIDC_SCOPES": "groups, offline_access ,",
		"OIDC_LOGIN_CLAIM": "email", "OIDC_ALLOWED_DOMAINS": "a.com,b.com", "OIDC_SESSION_KEY": "0123456789abcdef",
	}
	without := func(k string) map[string]string {
		m := map[string]string{}
		for kk, v := range full {
			if kk != k {
				m[kk] = v
			}
		}
		return m
	}

	cases := []struct {
		name    string
		env     map[string]string
		team    bool
		wantNil bool
		wantErr string
	}{
		{"no issuer means no login, even in team mode", nil, true, true, ""},
		{"an issuer without team mode is refused", full, false, false, "team mode"},
		{"client id is required", without("OIDC_CLIENT_ID"), true, false, "OIDC_CLIENT_ID"},
		{"redirect url is required", without("OIDC_REDIRECT_URL"), true, false, "OIDC_REDIRECT_URL"},
		{"complete", full, true, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			got, err := oidcFromEnv(env(c.env), c.team)

			// Assert
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || (got == nil) != c.wantNil {
				t.Fatalf("got %+v, %v", got, err)
			}
			if got == nil {
				return
			}
			if got.Issuer != full["OIDC_ISSUER"] || got.ClientID != "cid" || got.ClientSecret != "sec" || got.LoginClaim != "email" ||
				got.RedirectURL != full["OIDC_REDIRECT_URL"] || string(got.SessionKey) != "0123456789abcdef" ||
				strings.Join(got.Scopes, "|") != "groups|offline_access" || strings.Join(got.AllowedDomains, "|") != "a.com|b.com" {
				t.Errorf("config = %+v", got)
			}
		})
	}
}

func TestSecretKeyPrefersTheExplicitKeyThenTheSessionKey(t *testing.T) {
	// Arrange
	sess, err := oidcFromEnv(env(map[string]string{
		"OIDC_ISSUER": "https://i", "OIDC_CLIENT_ID": "c", "OIDC_REDIRECT_URL": "https://a/cb", "OIDC_SESSION_KEY": "session-key-0123456",
	}), true)
	if err != nil {
		t.Fatal(err)
	}
	noKey := *sess
	noKey.SessionKey = nil

	// Act / Assert
	if got := secretKey(env(map[string]string{"HARNESS_SECRET_KEY": "explicit"}), sess); string(got) != "explicit" {
		t.Errorf("explicit key lost to the session key: %q", got)
	}
	if got := secretKey(env(nil), sess); string(got) != "session-key-0123456" {
		t.Errorf("session key = %q", got)
	}
	if got := secretKey(env(nil), &noKey); got != nil {
		t.Errorf("an empty session key became a secret: %q", got)
	}
	if got := secretKey(env(nil), nil); got != nil {
		t.Errorf("no key configured = %q", got)
	}
}

func TestSplitListEnvOrAndSentryEnvironment(t *testing.T) {
	if got := splitList(" a, ,b ,,c "); strings.Join(got, "|") != "a|b|c" {
		t.Errorf("splitList = %q", got)
	}
	if got := splitList(""); got != nil {
		t.Errorf("splitList(\"\") = %q", got)
	}
	if envOr(env(map[string]string{"K": "v"}), "K", "d") != "v" || envOr(env(nil), "K", "d") != "d" {
		t.Error("envOr does not prefer the set value over the default")
	}
	for _, c := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"nothing set", nil, "development"},
		{"production flag", map[string]string{"HARNESS_PRODUCTION": "1"}, "production"},
		{"explicit name beats the production flag", map[string]string{"SENTRY_ENVIRONMENT": "staging", "HARNESS_PRODUCTION": "1"}, "staging"},
	} {
		if got := sentryEnvironment(env(c.env)); got != c.want {
			t.Errorf("%s: sentryEnvironment = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestErrorTrackerIsOffWithoutADSNRejectsABadOneAndTagsEventsWithEnvironmentAndRelease(t *testing.T) {
	// Arrange: a stand-in tracker recording the envelopes it receives
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				return
			}
			body = zr
		}
		raw, _ := io.ReadAll(body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
	}))
	defer srv.Close()
	dsn := strings.Replace(srv.URL, "http://", "http://publickey@", 1) + "/1"

	// Act
	none, noneErr := errorTracker(env(nil))
	_, badErr := errorTracker(env(map[string]string{"SENTRY_DSN": "not a dsn"}))
	tr, err := errorTracker(env(map[string]string{"SENTRY_DSN": dsn, "HARNESS_PRODUCTION": "1", "SENTRY_RELEASE": "rel-42"}))
	if err != nil || tr == nil {
		t.Fatalf("tracker %v err %v", tr, err)
	}
	slog.New(tr.Handler(slog.NewTextHandler(io.Discard, nil))).Error("boom", "run", "r1")
	flushed := tr.Flush(5 * time.Second)

	// Assert
	if none != nil || noneErr != nil {
		t.Errorf("no DSN: %v, %v; want nil, nil", none, noneErr)
	}
	if badErr == nil {
		t.Error("a malformed DSN was accepted")
	}
	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(bodies, "\n")
	if !flushed || !strings.Contains(all, `"environment":"production"`) || !strings.Contains(all, "rel-42") || !strings.Contains(all, "boom") {
		t.Errorf("flushed=%v, the event lacks environment/release/message:\n%s", flushed, all)
	}
}

func TestGitHubFromEnv(t *testing.T) {
	pemKey := string(wbPrivateKeyPEM(t))
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyFile, []byte(pemKey), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		env     map[string]string
		wantNil bool
		wantErr string
	}{
		{"no app configured", nil, true, ""},
		{"app id without a key", map[string]string{"GITHUB_APP_ID": "1"}, false, "GITHUB_APP_PRIVATE_KEY"},
		{"key that is not PEM", map[string]string{"GITHUB_APP_ID": "1", "GITHUB_APP_PRIVATE_KEY": "garbage"}, false, "PEM"},
		{"key file that does not exist", map[string]string{"GITHUB_APP_ID": "1", "GITHUB_APP_PRIVATE_KEY_FILE": filepath.Join(t.TempDir(), "missing")}, false, "GITHUB_APP_PRIVATE_KEY_FILE"},
		{"inline key", map[string]string{"GITHUB_APP_ID": "1", "GITHUB_APP_SLUG": "my-app", "GITHUB_APP_PRIVATE_KEY": pemKey, "GITHUB_APP_CLIENT_ID": "cid", "GITHUB_APP_CLIENT_SECRET": "csec"}, false, ""},
		{"key file wins over a broken inline key", map[string]string{"GITHUB_APP_ID": "1", "GITHUB_APP_PRIVATE_KEY": "garbage", "GITHUB_APP_PRIVATE_KEY_FILE": keyFile}, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			app, err := githubFromEnv(env(c.env))

			// Assert
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || (app == nil) != c.wantNil {
				t.Fatalf("app %v err %v", app, err)
			}
			if app != nil && c.env["GITHUB_APP_CLIENT_ID"] != "" && (app.ClientID != "cid" || app.ClientSecret != "csec" || app.Slug != "my-app") {
				t.Errorf("app = %+v", app)
			}
		})
	}
}

type wbFailingTeam struct {
	store.TeamStore
	err error
}

func (f wbFailingTeam) UpsertUser(context.Context, store.User) (*store.User, error) {
	return nil, f.err
}

func TestDevAuthenticatorActsAsTheHeaderUserAndRegistersThemOnce(t *testing.T) {
	// Arrange
	ts := teamstore.NewMem()
	if _, err := ts.UpsertUser(context.Background(), store.User{Login: "carol", Name: "Carol Custom", Email: "carol@corp.example"}); err != nil {
		t.Fatal(err)
	}
	auth := devAuthenticator(ts, "local")
	req := func(login string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if login != "" {
			r.Header.Set("X-Harness-User", login)
		}
		return r
	}

	// Act
	bob, errBob := auth(req("  BoB "))
	def, errDef := auth(req(""))
	carol, errCarol := auth(req("carol"))

	// Assert
	if errBob != nil || bob != "bob" || errDef != nil || def != "local" || errCarol != nil || carol != "carol" {
		t.Fatalf("logins %q %q %q, errors %v %v %v", bob, def, carol, errBob, errDef, errCarol)
	}
	u, err := ts.GetUser(context.Background(), "bob")
	if err != nil || u.Name != "Bob" || u.Email != "bob@dev.local" {
		t.Errorf("bob registered as %+v, %v", u, err)
	}
	if u, _ := ts.GetUser(context.Background(), "local"); u == nil || u.Name != "Local" {
		t.Errorf("the default user was not registered: %+v", u)
	}
	if u, _ := ts.GetUser(context.Background(), "carol"); u.Name != "Carol Custom" || u.Email != "carol@corp.example" {
		t.Errorf("a known user was overwritten: %+v", u)
	}
}

func TestDevAuthenticatorReportsAStoreFailure(t *testing.T) {
	boom := errors.New("disk full")
	auth := devAuthenticator(wbFailingTeam{TeamStore: teamstore.NewMem(), err: boom}, "local")

	login, err := auth(httptest.NewRequest(http.MethodGet, "/", nil))

	if !errors.Is(err, boom) || login != "" {
		t.Fatalf("login %q err %v; a user that could not be recorded must not be authenticated", login, err)
	}
}

func TestMultiCloserClosesEveryResourceAndReportsTheFirstError(t *testing.T) {
	// Arrange
	var closed []string
	mk := func(name string, err error) closerFunc {
		return func() error { closed = append(closed, name); return err }
	}
	first, second := errors.New("first"), errors.New("second")
	m := multiCloser{mk("a", nil), mk("b", first), mk("c", second)}
	released := false

	// Act
	err := m.Close()
	_ = poolCloser{func() { released = true }}.Close()

	// Assert
	if err != first || strings.Join(closed, "") != "abc" {
		t.Fatalf("err %v, closed %v: every resource must be released and the first error reported", err, closed)
	}
	if !released {
		t.Error("the pool was not closed")
	}
	if err := (multiCloser{}).Close(); err != nil {
		t.Errorf("empty closer: %v", err)
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// --- building the harness ---

func wbConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestBuildHarnessKeepsThreadsOnDiskOnlyWhenDurable(t *testing.T) {
	// Arrange
	wbEnv(t)
	ctx := context.Background()
	cfg := wbConfig(t)

	// Act: a durable harness saves a thread, a second one over the same directory sees it
	h1, c1, err := buildHarness(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	th, err := h1.Store.CreateThread(ctx, "u", "kept")
	if err != nil {
		t.Fatal(err)
	}
	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}
	h2, c2, err := buildHarness(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	_, errDurable := h2.Store.GetThread(ctx, "u", th.ID)
	// a memory harness shares nothing with the first
	h3, c3, err := buildHarness(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c3.Close()
	_, errMemory := h3.Store.GetThread(ctx, "u", th.ID)

	// Assert
	if errDurable != nil {
		t.Errorf("the durable thread did not survive: %v", errDurable)
	}
	if !errors.Is(errMemory, store.ErrNotFound) {
		t.Errorf("a non-durable harness found the thread: %v", errMemory)
	}
}

func TestBuildWithTeamModeKeepsTeamStateBesideTheSessionsAndWiresTheSupervisor(t *testing.T) {
	// Arrange
	wbEnv(t)
	dir := t.TempDir()
	t.Setenv("LOCAL_STATE_DIR", dir)
	ctx := context.Background()

	// Act
	b, err := buildWith(ctx, wbConfig(t), false, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.team.UpsertUser(ctx, store.User{Login: "ann"}); err != nil {
		t.Fatal(err)
	}
	if err := b.closer.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := teamstore.Open(filepath.Join(dir, "team.json"))

	// Assert
	if b.sup == nil || b.team == nil || b.bus != nil || b.github != nil {
		t.Errorf("wiring: sup %v team %v bus %v github %v", b.sup, b.team, b.bus, b.github)
	}
	if err != nil {
		t.Fatal(err)
	}
	if u, err := reopened.GetUser(ctx, "ann"); err != nil || u.Login != "ann" {
		t.Errorf("the team store was not persisted to team.json: %v %v", u, err)
	}
}

func TestBuildWithTeamModeWithoutAStateDirectoryUsesTheUserDirectory(t *testing.T) {
	// Arrange
	wbEnv(t)
	home := filepath.Join(t.TempDir(), "har")
	t.Setenv("LOCAL_STATE_DIR", "")
	t.Setenv("HAR_HOME", home)
	t.Setenv("HARNESS_NO_USER_DIR", "")
	ctx := context.Background()

	// Act
	b, err := buildWith(ctx, wbConfig(t), false, true)
	if err != nil {
		t.Fatal(err)
	}
	_, upErr := b.team.UpsertUser(ctx, store.User{Login: "ann"})
	_ = b.closer.Close()

	// Assert: the sessions and team.json share the directory the sessions use
	if upErr != nil {
		t.Fatal(upErr)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".har", "sessions", "team.json")); err != nil {
		t.Errorf("team.json not beside the sessions: %v", err)
	}
}

func TestBuildWithReportsEachUnusableDependency(t *testing.T) {
	notAFile := func(t *testing.T) string {
		p := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	closedPort := func(t *testing.T) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		return strings.Split(ln.Addr().String(), ":")[1]
	}
	cases := []struct {
		name    string
		setup   func(t *testing.T)
		durable bool
		team    bool
		want    string
	}{
		{"state directory is a file", func(t *testing.T) { t.Setenv("LOCAL_STATE_DIR", notAFile(t)) }, true, false, "session store"},
		{"corrupt team file", func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("LOCAL_STATE_DIR", dir)
			if err := os.WriteFile(filepath.Join(dir, "team.json"), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, false, true, "team store"},
		{"unreachable Postgres", func(t *testing.T) {
			t.Setenv("POSTGRES_HOST", "127.0.0.1")
			t.Setenv("POSTGRES_PORT", closedPort(t))
			t.Setenv("POSTGRES_SSLMODE", "disable")
			t.Setenv("PING_DB_AT_STARTUP", "true")
		}, false, true, "Postgres"},
		{"unreachable Redis", func(t *testing.T) {
			t.Setenv("REDIS_HOST", "127.0.0.1")
			t.Setenv("REDIS_PORT", closedPort(t))
			t.Setenv("PING_DB_AT_STARTUP", "true")
		}, false, true, "Redis"},
		{"GitHub App without a key", func(t *testing.T) { t.Setenv("GITHUB_APP_ID", "7") }, false, true, "GITHUB_APP_PRIVATE_KEY"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			wbEnv(t)
			c.setup(t)

			// Act
			b, err := buildWith(context.Background(), wbConfig(t), c.durable, c.team)

			// Assert
			if err == nil || !strings.Contains(err.Error(), c.want) || b != nil {
				t.Fatalf("built %v, err %v; want an error mentioning %q", b, err, c.want)
			}
		})
	}
}

func TestBuildWithSharesTheFeedBetweenReplicasThroughRedisInTeamMode(t *testing.T) {
	// Arrange
	wbEnv(t)
	mr := miniredis.RunT(t)
	host, port, _ := net.SplitHostPort(mr.Addr())
	t.Setenv("REDIS_HOST", host)
	t.Setenv("REDIS_PORT", port)
	t.Setenv("PING_DB_AT_STARTUP", "true")

	// Act
	team, err := buildWith(context.Background(), wbConfig(t), false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer team.closer.Close()
	t.Setenv("LOCAL_STATE_DIR", t.TempDir())                               // the first replica holds the lock of its own directory
	solo, err := buildWith(context.Background(), wbConfig(t), true, false) // Redis only matters with a team store
	if err != nil {
		t.Fatal(err)
	}
	defer solo.closer.Close()

	// Assert
	if team.bus == nil {
		t.Error("team mode with REDIS_HOST has no feed bus")
	}
	if solo.bus != nil {
		t.Error("a single-user server connected to Redis")
	}
}

func TestBuildWithGivesEachTeamThreadItsOwnLocalWorkspace(t *testing.T) {
	// Arrange
	wbEnv(t)
	t.Setenv("SANDBOX_BACKEND", "local")
	state := t.TempDir()
	t.Setenv("LOCAL_STATE_DIR", state)
	ctx := context.Background()
	cfg := wbConfig(t)
	be, err := teamLocalBackend(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Act
	a, errA := be.Create(ctx, sandbox.CreateOptions{})
	b, errB := be.Create(ctx, sandbox.CreateOptions{})
	built, errBuild := buildWith(ctx, cfg, false, true)
	if errBuild == nil {
		defer built.closer.Close()
	}

	// Assert
	if errA != nil || errB != nil || errBuild != nil {
		t.Fatalf("errors %v %v %v", errA, errB, errBuild)
	}
	if err := a.WriteFile(ctx, "/workspace/only-a.txt", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Stat(ctx, "/workspace/only-a.txt"); err == nil {
		t.Error("two sandboxes share a workspace; a team's sessions must not see each other's files")
	}
	if _, err := os.Stat(filepath.Join(state, a.ID(), "workspace", "only-a.txt")); err != nil {
		t.Errorf("the workspace is not under the state directory: %v", err)
	}
}

// --- run ---

func TestRunRejectsBadInvocationsBeforeServing(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cases := []struct {
		name string
		env  map[string]string
		osv  map[string]string
		args []string
		code int
		want string
	}{
		{"help", nil, nil, []string{"-h"}, 0, "-dev-users"},
		{"unknown flag", nil, nil, []string{"-nope"}, 2, "-nope"},
		{"OIDC without team mode", map[string]string{"OIDC_ISSUER": "https://id.example"}, nil, []string{"-team=false"}, 1, "team mode"},
		{"OIDC missing its client", map[string]string{"OIDC_ISSUER": "https://id.example"}, nil, nil, 1, "OIDC_CLIENT_ID"},
		{"malformed error tracker DSN", map[string]string{"SENTRY_DSN": "nonsense"}, nil, nil, 1, "errtrack"},
		{"invalid configuration", nil, map[string]string{"LLM_PROVIDER": "magic"}, nil, 1, "LLM_PROVIDER"},
		{"model outside LLM_MODELS", nil, nil, []string{"-model", "other"}, 1, `model "other" is not allowed`},
		{"local sandbox in team mode", nil, nil, []string{"-local", "-team"}, 1, "SANDBOX_BACKEND=local"},
		{"dev users without team mode", nil, nil, []string{"-dev-users"}, 1, "-dev-users"},
		{"dev users on a public address", map[string]string{"HARNESS_WEB_TOKEN": "t"}, nil, []string{"-team", "-dev-users", "-addr", "0.0.0.0:0"}, 1, "-dev-users"},
		{"address already in use", nil, nil, []string{"-addr", busy.Addr().String()}, 1, "address already in use"},
		{"client config that is not a DSN", map[string]string{"SENTRY_BROWSER_DSN": "nope"}, nil, nil, 1, "not a DSN"},
		{"half of the analytics settings", map[string]string{"UMAMI_URL": "https://umami.example"}, nil, nil, 1, "Umami"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			wbEnv(t)
			for k, v := range c.osv {
				t.Setenv(k, v)
			}
			var out, errb bytes.Buffer
			args := c.args
			if !hasFlag(args, "-addr") {
				args = append([]string{"-addr", "127.0.0.1:0"}, args...)
			}

			// Act
			code := run(context.Background(), args, env(c.env), &out, &errb)

			// Assert
			if code != c.code || !strings.Contains(errb.String(), c.want) || (c.code != 0 && out.Len() != 0) {
				t.Fatalf("exit %d stdout %q stderr %q, want exit %d mentioning %q", code, out.String(), errb.String(), c.code, c.want)
			}
		})
	}
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func TestServerWithoutACredentialAnswersOnlyToLoopbackHostNames(t *testing.T) {
	// Arrange
	wbEnv(t)
	s := wbStart(t, nil)

	// Act
	okStatus, _, hdr := wbDo(t, "GET", s.URL+"healthz", "", nil)
	rebinding, body, _ := wbDo(t, "GET", s.URL+"healthz", "attacker.example", nil)

	// Assert
	if okStatus != 200 || hdr.Get("Content-Security-Policy") == "" {
		t.Errorf("healthz %d, CSP %q", okStatus, hdr.Get("Content-Security-Policy"))
	}
	if rebinding != http.StatusForbidden || !strings.Contains(body, "forbidden_host") {
		t.Errorf("a foreign Host header got %d %s", rebinding, body)
	}
	if !strings.Contains(s.stderr.String(), "no sandbox") {
		t.Errorf("no warning that the agent has no tools: %q", s.stderr.String())
	}
}

func TestServerWithATokenRequiresItAndServesUnderAnyHostName(t *testing.T) {
	// Arrange
	wbEnv(t)
	s := wbStart(t, map[string]string{"HARNESS_WEB_TOKEN": "s3cret"}, "-user", "ann")

	// Act
	noToken, _, _ := wbDo(t, "GET", s.URL+"api/models", "", nil)
	wrong, _, _ := wbDo(t, "GET", s.URL+"api/models", "", map[string]string{"Authorization": "Bearer nope"})
	good, body, _ := wbDo(t, "GET", s.URL+"api/models", "proxy.example.com", map[string]string{"Authorization": "Bearer s3cret"})

	// Assert
	if noToken != 401 || wrong != 401 {
		t.Errorf("unauthenticated requests got %d and %d", noToken, wrong)
	}
	if good != 200 || !strings.Contains(body, "gpt-test") {
		t.Errorf("authenticated request through a public name: %d %s", good, body)
	}
}

func TestServerPublishesTheBrowserErrorAndAnalyticsSettings(t *testing.T) {
	// Arrange
	wbEnv(t)
	s := wbStart(t, map[string]string{
		"SENTRY_BROWSER_DSN": "https://pub@errors.example.com/7", "SENTRY_RELEASE": "r9", "HARNESS_PRODUCTION": "1",
		"UMAMI_URL": "https://umami.example.com", "UMAMI_WEBSITE_ID": "site-1",
	})

	// Act
	status, body, hdr := wbDo(t, "GET", s.URL+"api/client-config", "", nil)

	// Assert
	var got struct {
		Sentry struct{ DSN, Environment, Release string }
		Umami  struct{ ScriptURL, WebsiteID string }
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil || status != 200 {
		t.Fatalf("%d %s %v", status, body, err)
	}
	if got.Sentry.DSN != "https://pub@errors.example.com/7" || got.Sentry.Environment != "production" || got.Sentry.Release != "r9" ||
		got.Umami.ScriptURL != "https://umami.example.com/script.js" || got.Umami.WebsiteID != "site-1" {
		t.Errorf("client config = %s", body)
	}
	if csp := hdr.Get("Content-Security-Policy"); !strings.Contains(csp, "https://errors.example.com") || !strings.Contains(csp, "https://umami.example.com") {
		t.Errorf("CSP does not allow the configured origins: %s", csp)
	}
}

func TestServerListsModelInfoOnceThePriceTableHasLoaded(t *testing.T) {
	// Arrange
	wbEnv(t)
	prices := filepath.Join(t.TempDir(), "prices.json")
	if err := os.WriteFile(prices, []byte(`{"gpt-test":{"litellm_provider":"openai","mode":"chat","max_input_tokens":4321,"input_cost_per_token":0.5,"output_cost_per_token":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_MODEL_PRICES", prices)
	t.Setenv("HARNESS_NO_USER_DIR", "1")
	s := wbStart(t, nil)

	// Act: the table loads in the background; poll until it is served
	var window int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && window == 0 {
		_, body, _ := wbDo(t, "GET", s.URL+"api/models", "", nil)
		var m struct {
			Info map[string]struct{ ContextWindow int }
		}
		_ = json.Unmarshal([]byte(body), &m)
		window = m.Info["gpt-test"].ContextWindow
		if window == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Assert
	if window != 4321 {
		t.Fatalf("context window of gpt-test = %d, want 4321", window)
	}
}

func TestDevUsersTeamServerActsAsTheNamedUser(t *testing.T) {
	// Arrange
	wbEnv(t)
	s := wbStart(t, nil, "-team", "-dev-users", "-user", "dana")

	// Act
	_, bob, _ := wbDo(t, "GET", s.URL+"api/me", "", map[string]string{"X-Harness-User": "Bob"})
	_, dana, _ := wbDo(t, "GET", s.URL+"api/me", "", nil)

	// Assert
	var me struct{ User struct{ Login, Name string } }
	if err := json.Unmarshal([]byte(bob), &me); err != nil || me.User.Login != "bob" || me.User.Name != "Bob" {
		t.Errorf("header user: %s (%v)", bob, err)
	}
	if err := json.Unmarshal([]byte(dana), &me); err != nil || me.User.Login != "dana" {
		t.Errorf("default user: %s (%v)", dana, err)
	}
}

func TestTeamServerWithoutOIDCActsAsTheLocalUserAndSaysSo(t *testing.T) {
	// Arrange: team mode switched on through the environment rather than the flag
	wbEnv(t)
	s := wbStart(t, map[string]string{"HARNESS_TEAM": "1"}, "-user", "erin")

	// Act
	_, body, _ := wbDo(t, "GET", s.URL+"api/me", "", nil)

	// Assert
	if !strings.Contains(body, `"login":"erin"`) {
		t.Errorf("me = %s", body)
	}
	if !strings.Contains(s.stderr.String(), "everyone is erin") {
		t.Errorf("no notice that the server has no login: %q", s.stderr.String())
	}
	if got := os.Getenv("MEMORY_IN_CODING"); got != "true" {
		t.Errorf("team mode did not enable shared memory, MEMORY_IN_CODING=%q", got)
	}
}

func TestConnectorsNeedASecretKeyAndTheGitHubAppNeedsItsOAuthSettings(t *testing.T) {
	type connector struct {
		Kind       string
		Configured bool
		Message    string
	}
	list := func(t *testing.T, s *wbServer) map[string]connector {
		_, body, _ := wbDo(t, "GET", s.URL+"api/connectors", "", nil)
		var cs []connector
		if err := json.Unmarshal([]byte(body), &cs); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		by := map[string]connector{}
		for _, c := range cs {
			by[c.Kind] = c
		}
		return by
	}

	t.Run("no secret key", func(t *testing.T) {
		wbEnv(t)
		s := wbStart(t, nil, "-team", "-dev-users")

		got := list(t, s)

		if got["github"].Configured || !strings.Contains(got["github"].Message, "HARNESS_SECRET_KEY") {
			t.Errorf("github = %+v", got["github"])
		}
	})
	t.Run("secret key but no app", func(t *testing.T) {
		wbEnv(t)
		s := wbStart(t, map[string]string{"HARNESS_SECRET_KEY": "k-0123456789"}, "-team", "-dev-users")

		got := list(t, s)

		if got["github"].Configured || !strings.Contains(got["github"].Message, "GITHUB_APP_CLIENT_ID") {
			t.Errorf("github = %+v", got["github"])
		}
		if got["telegram"].Configured || !strings.Contains(got["telegram"].Message, "TELEGRAM_BOT_TOKEN") {
			t.Errorf("telegram = %+v", got["telegram"])
		}
	})
	t.Run("app with OAuth credentials", func(t *testing.T) {
		wbEnv(t)
		t.Setenv("GITHUB_APP_ID", "99")
		t.Setenv("GITHUB_APP_SLUG", "my-app")
		t.Setenv("GITHUB_APP_PRIVATE_KEY", string(wbPrivateKeyPEM(t)))
		t.Setenv("GITHUB_APP_CLIENT_ID", "cid")
		t.Setenv("GITHUB_APP_CLIENT_SECRET", "csec")
		s := wbStart(t, map[string]string{"HARNESS_SECRET_KEY": "k-0123456789"}, "-team", "-dev-users")

		got := list(t, s)

		if !got["github"].Configured {
			t.Errorf("github = %+v", got["github"])
		}
	})
}

func TestTeamServerWithOIDCDelegatesLoginToTheIdentityProvider(t *testing.T) {
	// Arrange: a minimal OpenID Connect discovery document
	var idp *httptest.Server
	idp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": idp.URL, "authorization_endpoint": idp.URL + "/authorize", "token_endpoint": idp.URL + "/token",
			"jwks_uri": idp.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	}))
	defer idp.Close()
	wbEnv(t)
	s := wbStart(t, map[string]string{
		"OIDC_ISSUER": idp.URL, "OIDC_CLIENT_ID": "cid", "OIDC_CLIENT_SECRET": "sec",
		"OIDC_REDIRECT_URL": "https://app.example.com/auth/callback", "OIDC_SESSION_KEY": "0123456789abcdef0123",
	})

	// Act
	anon, _, _ := wbDo(t, "GET", s.URL+"api/models", "", nil)
	login, _, hdr := wbDo(t, "GET", s.URL+"auth/login", "", nil)
	foreignHost, _, _ := wbDo(t, "GET", s.URL+"healthz", "app.example.com", nil)

	// Assert
	if anon != http.StatusUnauthorized {
		t.Errorf("an anonymous request got %d", anon)
	}
	if loc := hdr.Get("Location"); login != http.StatusFound && login != http.StatusSeeOther || !strings.HasPrefix(loc, idp.URL+"/authorize") || !strings.Contains(loc, "client_id=cid") {
		t.Errorf("login: %d -> %q", login, loc)
	}
	if foreignHost != 200 {
		t.Errorf("a login-protected server must be reachable under its public name, got %d", foreignHost)
	}
	if strings.Contains(s.stderr.String(), "everyone is") {
		t.Errorf("the no-login notice appeared although OIDC is configured: %q", s.stderr.String())
	}
}

func TestTeamServerFailsToStartWhenTheIdentityProviderIsUnreachable(t *testing.T) {
	// Arrange
	wbEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close()
	var out, errb bytes.Buffer

	// Act
	code := run(context.Background(), []string{"-addr", "127.0.0.1:0"}, env(map[string]string{
		"OIDC_ISSUER": dead, "OIDC_CLIENT_ID": "cid", "OIDC_REDIRECT_URL": "https://app.example.com/auth/callback",
	}), &out, &errb)

	// Assert
	if code != 1 || !strings.Contains(errb.String(), "OIDC discovery") {
		t.Fatalf("exit %d stderr %q", code, errb.String())
	}
}
