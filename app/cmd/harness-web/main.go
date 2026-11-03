// Command harness-web serves the harness over HTTP: the JSON + SSE API of
// hosts/web and its built-in browser UI.
//
//	harness-web -local                 # in a repository: coding agent on this directory, UI on http://127.0.0.1:8080
//	HARNESS_WEB_ADDR=0.0.0.0:8080 HARNESS_WEB_TOKEN=secret harness-web   # remote sandbox, reachable by others
//
// Environment: HARNESS_WEB_ADDR (default 127.0.0.1:8080), HARNESS_WEB_TOKEN (bearer
// token for the API; required for a non-loopback address), HARNESS_WEB_ORIGINS
// (comma-separated CORS allowlist), plus the harness's own LLM_*, SANDBOX_*, … settings.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/7ar-chat/harness/adapters/errtrack"
	"github.com/7ar-chat/harness/app/product"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/hosts/httpapi"
	"github.com/7ar-chat/harness/modelinfo"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("harness-web", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", envOr(getenv, "HARNESS_WEB_ADDR", "127.0.0.1:8080"), "listen address (env HARNESS_WEB_ADDR)")
	model := fs.String("model", "", "default model id (default LLM_DEFAULT_MODEL)")
	local := fs.Bool("local", false, "work on the current directory: SANDBOX_BACKEND=local, the coding profile, durable sessions")
	user := fs.String("user", "local", "user login of the single local user")
	devUsers := fs.Bool("dev-users", false, "team mode on a loopback address: act as the user named by the X-Harness-User header (development only)")
	check := fs.String("factory-check", "", "check the agent factory's setup against GitHub for this repository (owner/name) and exit: read-only, prints [ OK ]/[WARN]/[FAIL] per check, exit 1 on a failure")
	team := fs.Bool("team", getenv("OIDC_ISSUER") != "" || getenv("GITHUB_LOGIN_CLIENT_ID") != "" || getenv("HARNESS_TEAM") == "1", "team mode: login, projects, shared threads (env OIDC_ISSUER, GITHUB_LOGIN_CLIENT_ID or HARNESS_TEAM=1)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *check != "" {
		return factoryCheck(ctx, *check, getenv, stdout, stderr)
	}
	token := getenv("HARNESS_WEB_TOKEN")
	oidcCfg, err := oidcFromEnv(getenv, *team)
	if err != nil {
		return fail(stderr, err)
	}
	ghLogin, err := githubLoginFromEnv(getenv, *team)
	if err != nil {
		return fail(stderr, err)
	}
	hasLogin := oidcCfg != nil || ghLogin != nil
	if !isLoopback(*addr) && token == "" && !hasLogin {
		return fail(stderr, fmt.Errorf("refusing to listen on %s without HARNESS_WEB_TOKEN: set a token, configure a login (OIDC_ISSUER or GITHUB_LOGIN_CLIENT_ID), or bind a loopback address", *addr))
	}
	mailer, err := mailerFromEnv(getenv)
	if err != nil {
		return fail(stderr, err)
	}
	logger := slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn})
	tracker, err := errorTracker(getenv)
	if err != nil {
		return fail(stderr, err)
	}
	if tracker != nil {
		defer tracker.Flush(2 * time.Second)
		slog.SetDefault(slog.New(tracker.Handler(logger)))
	} else {
		slog.SetDefault(slog.New(logger))
	}
	if *local {
		setDefault("SANDBOX_BACKEND", "local")
		setDefault("AGENT_PROFILE", "coding")
	}
	if *team { // a team's agents share what they learn: memory on in the coding profile, and proposed after each run
		setDefault("MEMORY_IN_CODING", "true")
		setDefault("HARNESS_MEMORY_EXTRACT", "true")
	}
	cfg, err := config.Load()
	if err != nil {
		return fail(stderr, err)
	}
	if err := checkLocalSandbox(cfg, *team, *addr, getenv); err != nil {
		return fail(stderr, err)
	}
	if *model == "" {
		*model = cfg.LLMDefaultModel
	}
	bt, err := buildWith(ctx, cfg, *local, *team)
	if err != nil {
		return fail(stderr, err)
	}
	h, teamStore, sup, closer := bt.h, bt.team, bt.sup, bt.closer
	defer closer.Close()
	if !cfg.ModelAllowed(*model) { // after the build: LLM_DISCOVER_MODELS widens the list there
		return fail(stderr, fmt.Errorf("model %q is not allowed (LLM_MODELS)", *model))
	}
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = h.Run(hctx) }()

	opts := httpapi.Options{
		Token: token, LocalUser: *user, Models: cfg.LLMModels, DefaultModel: *model,
		AllowedOrigins: splitList(getenv("HARNESS_WEB_ORIGINS")),
		Client: httpapi.ClientConfig{
			SentryDSN: getenv("SENTRY_BROWSER_DSN"), SentryEnv: sentryEnvironment(getenv), SentryRel: getenv("SENTRY_RELEASE"),
			UmamiURL: getenv("UMAMI_URL"), UmamiID: getenv("UMAMI_WEBSITE_ID"),
		},
	}
	prod := product.Options{
		Team: teamStore, OIDC: oidcCfg, GitHubLogin: ghLogin, Admins: splitList(getenv("HARNESS_ADMINS")), Mail: mailer, Supervisor: sup, GitHub: bt.github, Bus: bt.bus,
		PublicURL: getenv("HARNESS_PUBLIC_URL"), SecretKey: secretKey(getenv, oidcCfg),
	}
	if bt.factory != nil {
		prod.Factory = *bt.factory
	}
	if bt.caps != nil {
		prod.Capabilities, prod.MCPTester = bt.caps, mcpTester("web")
	}
	if tok := getenv("TELEGRAM_BOT_TOKEN"); tok != "" && *team {
		prod.Telegram = &product.Telegram{Token: tok, Username: strings.TrimPrefix(getenv("TELEGRAM_BOT_USERNAME"), "@")}
	}
	if *devUsers {
		if !*team || hasLogin || !isLoopback(*addr) {
			return fail(stderr, errors.New("-dev-users needs -team, no OIDC_ISSUER or GITHUB_LOGIN_CLIENT_ID, and a loopback address"))
		}
		opts.Authenticate = devAuthenticator(teamStore, *user)
	}
	if *team && !hasLogin { // team mode without a login: every request acts as -user (local development)
		fmt.Fprintln(stderr, "harness-web: team mode without OIDC_ISSUER or GITHUB_LOGIN_CLIENT_ID: everyone is", *user)
	}
	if mo, ok := config.ModelPriceOptions(); ok { // loaded in the background: the list gains its info once it arrives
		opts.ModelInfo = client.TableLookup(modelinfo.LoadAsync(hctx, mo), cfg.LLMProviderName, cfg.LLMProvider)
	}
	opts.AllowedHosts = allowedHosts(token, hasLogin)
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fail(stderr, err)
	}
	handler, err := product.New(h, opts, prod)
	if err != nil {
		return fail(stderr, err)
	}
	// net/http logs a panicking handler through ErrorLog; slog.NewLogLogger sends it to the default slog handler at
	// Error level, so it reaches the tracker as well.
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)}
	fmt.Fprintf(stdout, "harness-web: http://%s/  (model %s)\n", ln.Addr(), *model)
	if !cfg.SandboxEnabled() {
		fmt.Fprintln(stderr, "(no sandbox: use -local, SANDBOX_BACKEND=local or OPENSANDBOX_URL; the agent has no tools)")
	}
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fail(stderr, err)
	}
	return 0
}

// allowedHosts is the Host-header allow-list of the DNS-rebinding guard. Only the server with no credential at all
// (no token, no login) needs it, and it is then reachable only on loopback; a server behind a token or a login is
// served under its public name, so the guard must not apply.
func allowedHosts(token string, login bool) []string {
	if token == "" && !login {
		return []string{"localhost", "127.0.0.1", "::1"}
	}
	return nil
}

// secretKey is the key that encrypts connector tokens: HARNESS_SECRET_KEY, else the OIDC session key.
func secretKey(getenv func(string) string, oidc *product.OIDCConfig) []byte {
	if k := getenv("HARNESS_SECRET_KEY"); k != "" {
		return []byte(k)
	}
	if oidc != nil && len(oidc.SessionKey) > 0 {
		return oidc.SessionKey
	}
	if k := getenv("HARNESS_SESSION_KEY"); k != "" {
		return []byte(k)
	}
	return nil
}

// sessionKey signs the login cookie: HARNESS_SESSION_KEY, else OIDC_SESSION_KEY (random at start when neither is set).
func sessionKey(getenv func(string) string) []byte {
	if k := getenv("HARNESS_SESSION_KEY"); k != "" {
		return []byte(k)
	}
	return []byte(getenv("OIDC_SESSION_KEY"))
}

// githubLoginFromEnv reads GITHUB_LOGIN_CLIENT_ID and GITHUB_LOGIN_CLIENT_SECRET, the OAuth App behind "Continue with
// GitHub" (its callback URL is $HARNESS_PUBLIC_URL/auth/signin/github/callback). nil without a client id.
func githubLoginFromEnv(getenv func(string) string, team bool) (*product.GitHubLoginConfig, error) {
	id := getenv("GITHUB_LOGIN_CLIENT_ID")
	if id == "" {
		return nil, nil
	}
	if !team {
		return nil, errors.New("GITHUB_LOGIN_CLIENT_ID needs team mode (-team)")
	}
	if getenv("GITHUB_LOGIN_CLIENT_SECRET") == "" {
		return nil, errors.New("GITHUB_LOGIN_CLIENT_ID needs GITHUB_LOGIN_CLIENT_SECRET")
	}
	if getenv("HARNESS_PUBLIC_URL") == "" {
		return nil, errors.New("GITHUB_LOGIN_CLIENT_ID needs HARNESS_PUBLIC_URL (https://host): the OAuth callback is built from it")
	}
	return &product.GitHubLoginConfig{ClientID: id, ClientSecret: getenv("GITHUB_LOGIN_CLIENT_SECRET"), SessionKey: sessionKey(getenv)}, nil
}

// mailerFromEnv reads SMTP_HOST, SMTP_PORT (default 587; 465 is TLS from the start), SMTP_USER, SMTP_PASSWORD and SMTP_FROM:
// the mail server the admins are told about new sign-ups through. nil without a host.
func mailerFromEnv(getenv func(string) string) (product.Mailer, error) {
	host := getenv("SMTP_HOST")
	if host == "" {
		return nil, nil
	}
	port := 0
	if p := getenv("SMTP_PORT"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("SMTP_PORT %q is not a port", p)
		}
		port = n
	}
	m, err := product.NewSMTPMailer(product.SMTPConfig{Host: host, Port: port, Username: getenv("SMTP_USER"), Password: getenv("SMTP_PASSWORD"), From: getenv("SMTP_FROM")})
	if err != nil {
		return nil, fmt.Errorf("SMTP_HOST: %w (SMTP_FROM must be an address such as noreply@example.com)", err)
	}
	return m, nil
}

// oidcFromEnv reads OIDC_ISSUER, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET, OIDC_REDIRECT_URL,
// OIDC_SCOPES, OIDC_LOGIN_CLAIM, OIDC_ALLOWED_DOMAINS, OIDC_SESSION_KEY and OIDC_REQUIRE_APPROVAL (1: new accounts wait
// for an admin). nil without an issuer.
func oidcFromEnv(getenv func(string) string, team bool) (*product.OIDCConfig, error) {
	issuer := getenv("OIDC_ISSUER")
	if issuer == "" {
		return nil, nil
	}
	if !team {
		return nil, errors.New("OIDC_ISSUER needs team mode (-team)")
	}
	c := &product.OIDCConfig{
		Issuer: issuer, ClientID: getenv("OIDC_CLIENT_ID"), ClientSecret: getenv("OIDC_CLIENT_SECRET"),
		RedirectURL: getenv("OIDC_REDIRECT_URL"), Scopes: splitList(getenv("OIDC_SCOPES")),
		LoginClaim: getenv("OIDC_LOGIN_CLAIM"), AllowedDomains: splitList(getenv("OIDC_ALLOWED_DOMAINS")),
		SessionKey: sessionKey(getenv), RequireApproval: getenv("OIDC_REQUIRE_APPROVAL") == "1",
	}
	if c.ClientID == "" || c.RedirectURL == "" {
		return nil, errors.New("OIDC_ISSUER needs OIDC_CLIENT_ID and OIDC_REDIRECT_URL (https://host/auth/callback)")
	}
	return c, nil
}

// isLoopback reports whether addr binds only the loopback interface.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(getenv func(string) string, k, def string) string {
	if v := getenv(k); v != "" {
		return v
	}
	return def
}

// errorTracker connects the error tracker named by SENTRY_DSN (Sentry, Rustrak, GlitchTip…): every slog.Error of the
// process is then reported to it. SENTRY_ENVIRONMENT and SENTRY_RELEASE tag every event (see sentryEnvironment).
// nil without a DSN. The browser reports to its own project through SENTRY_BROWSER_DSN, so server and UI errors
// land in separate projects.
func errorTracker(getenv func(string) string) (*errtrack.Tracker, error) {
	dsn := getenv("SENTRY_DSN")
	if dsn == "" {
		return nil, nil
	}
	return errtrack.New(errtrack.Options{DSN: dsn, Environment: sentryEnvironment(getenv), Release: getenv("SENTRY_RELEASE")})
}

// sentryEnvironment is SENTRY_ENVIRONMENT, else "production" under HARNESS_PRODUCTION, else "development".
func sentryEnvironment(getenv func(string) string) string {
	if env := getenv("SENTRY_ENVIRONMENT"); env != "" {
		return env
	}
	if getenv("HARNESS_PRODUCTION") != "" {
		return "production"
	}
	return "development"
}

func setDefault(k, v string) {
	if os.Getenv(k) == "" {
		_ = os.Setenv(k, v)
	}
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "harness-web:", err)
	return 1
}

// devAuthenticator acts as the user in the X-Harness-User header (else def),
// recording them as known users. Only for loopback development servers.
func devAuthenticator(ts store.TeamStore, def string) httpapi.Authenticator {
	return func(r *http.Request) (string, error) {
		login := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Harness-User")))
		if login == "" {
			login = def
		}
		if _, err := ts.GetUser(r.Context(), login); err != nil {
			name := strings.ToUpper(login[:1]) + login[1:]
			if _, err := ts.UpsertUser(r.Context(), store.User{Login: login, Name: name, Email: login + "@dev.local"}); err != nil {
				return "", err
			}
		}
		return login, nil
	}
}

// checkLocalSandbox refuses the local sandbox backend where more than one person (or the network) can reach the
// agent: its commands run on this machine with this user's rights, with no isolation beyond the approval policy.
// A shared server needs a real sandbox (SANDBOX_BACKEND=opensandbox); HARNESS_ALLOW_LOCAL_SANDBOX=1 accepts the risk.
func checkLocalSandbox(cfg *config.Config, team bool, addr string, getenv func(string) string) error {
	if cfg.SandboxBackendName() != config.BackendLocal || getenv("HARNESS_ALLOW_LOCAL_SANDBOX") == "1" {
		return nil
	}
	if team || !isLoopback(addr) {
		return errors.New("SANDBOX_BACKEND=local runs the agent's commands on this machine without isolation: " +
			"not allowed in team mode or on a non-loopback address; use SANDBOX_BACKEND=opensandbox, or set HARNESS_ALLOW_LOCAL_SANDBOX=1 to accept the risk")
	}
	return nil
}
