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
	"strings"
	"time"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/hosts/client"
	httpserver "github.com/GhaziBenDahmane/harness/hosts/web"
	"github.com/GhaziBenDahmane/harness/modelinfo"
	"github.com/GhaziBenDahmane/harness/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
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
	team := fs.Bool("team", getenv("OIDC_ISSUER") != "" || getenv("HARNESS_TEAM") == "1", "team mode: OIDC login, projects, shared threads (env OIDC_ISSUER or HARNESS_TEAM=1)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	token := getenv("HARNESS_WEB_TOKEN")
	oidcCfg, err := oidcFromEnv(getenv, *team)
	if err != nil {
		return fail(stderr, err)
	}
	if !isLoopback(*addr) && token == "" && oidcCfg == nil {
		return fail(stderr, fmt.Errorf("refusing to listen on %s without HARNESS_WEB_TOKEN: set a token, configure OIDC (OIDC_ISSUER), or bind a loopback address", *addr))
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if *local {
		setDefault("SANDBOX_BACKEND", "local")
		setDefault("AGENT_PROFILE", "coding")
	}
	cfg, err := config.Load()
	if err != nil {
		return fail(stderr, err)
	}
	if *model == "" {
		*model = cfg.LLMDefaultModel
	}
	if !cfg.ModelAllowed(*model) {
		return fail(stderr, fmt.Errorf("model %q is not allowed (LLM_MODELS)", *model))
	}
	bt, err := buildWith(ctx, cfg, *local, *team)
	if err != nil {
		return fail(stderr, err)
	}
	h, teamStore, sup, closer := bt.h, bt.team, bt.sup, bt.closer
	defer closer.Close()
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = h.Run(hctx) }()

	opts := httpserver.Options{
		Token: token, LocalUser: *user, Models: cfg.LLMModels, DefaultModel: *model,
		AllowedOrigins: splitList(getenv("HARNESS_WEB_ORIGINS")),
		Team:           teamStore, OIDC: oidcCfg, Supervisor: sup, GitHub: bt.github,
	}
	if *devUsers {
		if !*team || oidcCfg != nil || !isLoopback(*addr) {
			return fail(stderr, errors.New("-dev-users needs -team, no OIDC_ISSUER, and a loopback address"))
		}
		opts.Authenticate = devAuthenticator(teamStore, *user)
	}
	if *team && oidcCfg == nil { // team mode without OIDC: every request acts as -user (local development)
		fmt.Fprintln(stderr, "harness-web: team mode without OIDC_ISSUER: everyone is", *user)
	}
	if mo, ok := config.ModelPriceOptions(); ok { // loaded in the background: the list gains its info once it arrives
		opts.ModelInfo = client.TableLookup(modelinfo.LoadAsync(hctx, mo), cfg.LLMProviderName, cfg.LLMProvider)
	}
	if token == "" { // DNS-rebinding guard for the token-less loopback server
		opts.AllowedHosts = []string{"localhost", "127.0.0.1", "::1"}
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fail(stderr, err)
	}
	handler, err := httpserver.NewServer(h, opts)
	if err != nil {
		return fail(stderr, err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
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

// oidcFromEnv reads OIDC_ISSUER, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET, OIDC_REDIRECT_URL,
// OIDC_SCOPES, OIDC_LOGIN_CLAIM, OIDC_ALLOWED_DOMAINS and OIDC_SESSION_KEY. nil without an issuer.
func oidcFromEnv(getenv func(string) string, team bool) (*httpserver.OIDCConfig, error) {
	issuer := getenv("OIDC_ISSUER")
	if issuer == "" {
		return nil, nil
	}
	if !team {
		return nil, errors.New("OIDC_ISSUER needs team mode (-team)")
	}
	c := &httpserver.OIDCConfig{
		Issuer: issuer, ClientID: getenv("OIDC_CLIENT_ID"), ClientSecret: getenv("OIDC_CLIENT_SECRET"),
		RedirectURL: getenv("OIDC_REDIRECT_URL"), Scopes: splitList(getenv("OIDC_SCOPES")),
		LoginClaim: getenv("OIDC_LOGIN_CLAIM"), AllowedDomains: splitList(getenv("OIDC_ALLOWED_DOMAINS")),
		SessionKey: []byte(getenv("OIDC_SESSION_KEY")),
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
func devAuthenticator(ts store.TeamStore, def string) httpserver.Authenticator {
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
