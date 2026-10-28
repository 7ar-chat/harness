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
	httpserver "github.com/GhaziBenDahmane/harness/hosts/web"
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
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	token := getenv("HARNESS_WEB_TOKEN")
	if !isLoopback(*addr) && token == "" {
		return fail(stderr, fmt.Errorf("refusing to listen on %s without HARNESS_WEB_TOKEN: set a token, or bind a loopback address", *addr))
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
	h, closer, err := buildHarness(cfg, *local)
	if err != nil {
		return fail(stderr, err)
	}
	defer closer.Close()
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = h.Run(hctx) }()

	opts := httpserver.Options{
		Token: token, LocalUser: *user, Models: cfg.LLMModels, DefaultModel: *model,
		AllowedOrigins: splitList(getenv("HARNESS_WEB_ORIGINS")),
	}
	if token == "" { // DNS-rebinding guard for the token-less loopback server
		opts.AllowedHosts = []string{"localhost", "127.0.0.1", "::1"}
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fail(stderr, err)
	}
	srv := &http.Server{Handler: httpserver.New(h, opts), ReadHeaderTimeout: 10 * time.Second}
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
