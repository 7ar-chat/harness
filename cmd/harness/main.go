// Command harness is the terminal client of the harness library: one
// in-process harness, one thread.
//
//	LLM_PROVIDER=openai LLM_API_KEY=… LLM_MODELS=gpt-5 \
//	OPENSANDBOX_URL=http://localhost:8090 harness -model gpt-5
//
// Modes:
//
//	harness                  interactive terminal UI (when stdin is a terminal)
//	harness -p "prompt"      print mode: the answer on stdout, tool activity on stderr
//	echo prompt | harness    print mode, the prompt read from stdin
//	harness -p … -json       print mode emitting the AG-UI events as JSON lines
//	harness auth login|list|remove   manage the API keys of ~/.har/auth.json
//	harness config path|init          show / create the configuration directories
//	harness trust [path] | trust list | untrust [path]   let a project add allow rules and MCP servers
//
// Remote mode attaches the same terminal UI (and print mode) to a running
// harness-web instead of an in-process engine; the server owns the model
// provider, the sandbox and the conversations:
//
//	harness -remote http://127.0.0.1:8080                  # HARNESS_REMOTE_TOKEN is the bearer token
//	harness -remote http://host:8080 -p "summarise the repo"
//
// Print mode exits 0 on success, 1 when the run failed or was cancelled and 2
// when it ended waiting for an answer or an approval.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/filestore"
	"github.com/7ar-chat/harness/coding/gitsafe"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/features/permissions"
	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/hosts/inproc"
	"github.com/7ar-chat/harness/hosts/tui"
	"github.com/7ar-chat/harness/modelinfo"
)

func main() {
	stat, _ := os.Stdin.Stat()
	isTTY := stat != nil && stat.Mode()&os.ModeCharDevice != 0
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, isTTY, newHarness)
	stop()
	os.Exit(code)
}

// factory builds the harness of a run of the command.
// st is the session store (nil: in memory).
type factory func(cfg *config.Config, st harness.Store) (*harness.Harness, error)

func newHarness(cfg *config.Config, st harness.Store) (*harness.Harness, error) {
	if err := cfg.RequireLLMKey(); err != nil {
		return nil, err
	}
	return harness.New(harness.Options{Config: cfg, Store: st, Version: "cli"})
}

type options struct {
	model, user, prompt string
	provider            string
	json, auto          bool
	ephemeral, cont     bool
	resume              string
	list, undo          bool
	remote              string
	configDump          bool
	discover            func(context.Context) ([]string, error)         // model discovery of /model (in-process only)
	connector           tui.Connector                                   // /connect (in-process only)
	status              func(context.Context) ([]tui.StatusItem, error) // /status and /mcp (in-process only)
	info                func(string) (client.ModelInfo, bool)           // model table lookup, also for discovered models (in-process only)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, isTTY bool, mk factory) int {
	if code, ok := subcommand(args, stdin, stdout, stderr, isTTY); ok {
		return code
	}
	var o options
	fs := flag.NewFlagSet("harness", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.model, "model", "", "model id (default LLM_DEFAULT_MODEL)")
	fs.StringVar(&o.provider, "provider", "", "provider to use: a name defined under providers: in a config.yml, or openai / anthropic (default LLM_PROVIDER)")
	fs.StringVar(&o.user, "user", "local", "user login")
	fs.StringVar(&o.prompt, "p", "", "print mode: send this prompt (and stdin, if piped), print the answer, exit")
	fs.BoolVar(&o.json, "json", false, "print mode: emit the AG-UI events as JSON lines on stdout")
	fs.BoolVar(&o.auto, "auto", false, "interactive UI: do not ask for approval of risky tool calls")
	fs.BoolVar(&o.ephemeral, "ephemeral", false, "keep nothing: do not save the conversation (default: saved under LOCAL_STATE_DIR)")
	fs.BoolVar(&o.cont, "continue", false, "continue the most recent conversation")
	fs.StringVar(&o.resume, "resume", "", "continue the conversation with this id (see -sessions)")
	fs.BoolVar(&o.undo, "undo", false, "restore the files of the workspace (a git repository) to the last snapshot taken before a run of the conversation (-resume <id>, default the most recent), then exit")
	fs.BoolVar(&o.list, "sessions", false, "list the saved conversations and exit")
	fs.StringVar(&o.remote, "remote", "", "attach to the harness-web server at this URL instead of running an engine in process (bearer token: HARNESS_REMOTE_TOKEN)")
	fs.BoolVar(&o.configDump, "config-dump", false, "print the effective configuration with the source of each value (flag, env, project file, user file; secrets masked) and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	if o.configDump {
		cfg, err := config.LoadWith(config.LoadOptions{Provider: o.provider})
		if err != nil {
			return fail(stderr, err)
		}
		flags := map[string]string{}
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "model" || f.Name == "user" || f.Name == "provider" {
				flags["-"+f.Name] = f.Value.String()
			}
		})
		cfg.Dump(stdout, flags)
		dumpPermissionFiles(stdout, cfg)
		return 0
	}
	prompt, err := resolvePrompt(o.prompt, stdin, isTTY)
	if err != nil {
		return fail(stderr, err)
	}
	printMode := prompt != ""
	if !printMode && !isTTY && !o.list && !o.undo {
		return fail(stderr, errors.New("no prompt: pass -p, pipe one on stdin, or run in a terminal"))
	}

	if o.remote != "" {
		return runRemote(ctx, o, prompt, printMode, stdout, stderr)
	}
	cfg, err := config.LoadWith(config.LoadOptions{Provider: o.provider})
	if err != nil {
		return fail(stderr, err)
	}
	if o.model == "" {
		o.model = cfg.LLMDefaultModel
	}
	if o.model == "" {
		return fail(stderr, errors.New("no model: pass -model or set LLM_DEFAULT_MODEL"))
	}
	var st harness.Store
	var sessions filestore.Sessions
	if !o.ephemeral {
		fsStore, err := harness.OpenLocalStore(cfg.LocalStateDir)
		if err != nil {
			return fail(stderr, fmt.Errorf("open the session store (use -ephemeral to skip it): %w", err))
		}
		defer fsStore.Close()
		st, sessions = fsStore, fsStore.Sessions()
	}
	if o.list {
		if st == nil {
			return fail(stderr, errors.New("-sessions needs the session store (drop -ephemeral)"))
		}
		return listSessions(ctx, sessions, o.user, stdout, stderr)
	}
	if o.undo {
		return undoRun(ctx, cfg, sessions, o, stdout, stderr)
	}
	h, err := mk(cfg, st)
	if err != nil {
		return fail(stderr, err)
	}
	hctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = h.Run(hctx) }()

	o.discover = discoverer(cfg)
	o.connector = newAuthConnector(cfg)
	o.status = statusFunc(cfg)
	lookup := modelLookup(hctx, cfg)
	o.info = lookup
	c := inproc.New(h, o.user, inproc.WithModels(cfg.LLMModels, o.model), inproc.WithModelInfo(lookup))
	if !cfg.SandboxEnabled() && !o.json {
		fmt.Fprintln(stderr, "(no sandbox: set SANDBOX_BACKEND=local or OPENSANDBOX_URL; the agent has no tools)")
	}
	return attach(hctx, c, o, prompt, printMode, st != nil, stdout, stderr)
}

// modelLookup loads the model price table in the background (never blocking the
// start) and returns its lookup for the configured provider; nil when disabled
// (HARNESS_MODEL_PRICES=off).
func modelLookup(ctx context.Context, cfg *config.Config) func(string) (client.ModelInfo, bool) {
	opts, ok := config.ModelPriceOptions()
	if !ok {
		return nil
	}
	return client.TableLookup(modelinfo.LoadAsync(ctx, opts), cfg.LLMProviderName, cfg.LLMProvider)
}

// runRemote attaches the terminal UI to the harness-web server at o.remote.
func runRemote(ctx context.Context, o options, prompt string, printMode bool, stdout, stderr io.Writer) int {
	if o.undo {
		return fail(stderr, errors.New("-undo restores a local workspace: it does not work with -remote"))
	}
	c := client.HTTP(o.remote, os.Getenv("HARNESS_REMOTE_TOKEN"))
	if o.list {
		list, err := c.Sessions(ctx, 50)
		if err != nil {
			return fail(stderr, err)
		}
		printSessions(list, stdout)
		return 0
	}
	if o.model == "" {
		m, err := c.Models(ctx)
		if err != nil {
			return fail(stderr, fmt.Errorf("%s: %w", o.remote, err))
		}
		o.model = m.Default
	}
	if o.model == "" {
		return fail(stderr, errors.New("no model: pass -model (the server has no default)"))
	}
	return attach(ctx, c, o, prompt, printMode, true, stdout, stderr)
}

// attach picks the conversation and runs print mode or the terminal UI on it.
func attach(ctx context.Context, c client.Client, o options, prompt string, printMode, saved bool, stdout, stderr io.Writer) int {
	th, err := pickThread(ctx, c, saved, o)
	if err != nil {
		return fail(stderr, err)
	}
	be := &tui.ClientBackend{C: c, ThreadID: th.ID, ModelID: o.model, Interactive: !printMode && !o.auto, Discover: o.discover, RemoteURL: o.remote, StatusFunc: o.status, InfoFunc: o.info}
	if printMode {
		return tui.RunPrint(ctx, be, prompt, tui.PrintOptions{Stdout: stdout, Stderr: stderr, JSON: o.json})
	}
	to := tuiOptions()
	to.Connector = o.connector
	if o.remote == "" {
		to.Workspace, _ = os.Getwd() // @file and !command act on the local project; a remote session has none
	}
	if err := tui.Run(ctx, be, to); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// pickThread starts a new conversation, or continues the one asked for.
func pickThread(ctx context.Context, c client.Client, saved bool, o options) (*client.Thread, error) {
	if (o.cont || o.resume != "") && !saved {
		return nil, errors.New("-continue and -resume need the session store (drop -ephemeral)")
	}
	switch {
	case o.resume != "":
		return c.GetThread(ctx, o.resume)
	case o.cont:
		list, err := c.Sessions(ctx, 1)
		if err != nil {
			return nil, err
		}
		if len(list) > 0 {
			return c.GetThread(ctx, list[0].ID)
		}
	}
	return c.CreateThread(ctx, "")
}

// undoRun restores the workspace to the snapshot taken before the conversation's last run. The
// snapshot lives in refs/harness/snapshots/<thread>/ of the repository; the state it replaces is
// kept under refs/harness/backups/, so the undo can itself be undone.
func undoRun(ctx context.Context, cfg *config.Config, sessions filestore.Sessions, o options, stdout, stderr io.Writer) int {
	thread := o.resume
	if thread == "" {
		list, err := sessions.List(ctx, o.user, 1)
		if err != nil || len(list) == 0 {
			return fail(stderr, errors.New("no saved conversation to undo; pass -resume <id>"))
		}
		thread = list[0].ID
	}
	dir := cfg.LocalWorkspace
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return fail(stderr, err)
		}
	}
	backup, restored, err := gitsafe.Undo(ctx, dir, thread)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "restored %s to the snapshot %s; the replaced state is kept as %s\n", dir, restored.Ref, backup.Ref)
	return 0
}

func listSessions(ctx context.Context, sessions filestore.Sessions, user string, stdout, stderr io.Writer) int {
	list, err := sessions.List(ctx, user, 50)
	if err != nil {
		return fail(stderr, err)
	}
	out := make([]client.Session, 0, len(list))
	for _, s := range list {
		out = append(out, client.Session{ID: s.ID, Title: s.Title, Preview: s.Preview, Messages: s.Messages, Pinned: s.Pinned, UpdatedAt: s.UpdatedAt})
	}
	printSessions(out, stdout)
	return 0
}

func printSessions(list []client.Session, stdout io.Writer) {
	for _, s := range list {
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(stdout, "%s  %s  %3d msgs  %s\n      %s\n", s.ID, s.UpdatedAt.Format("2006-01-02 15:04"), s.Messages, title, s.Preview)
	}
	if len(list) == 0 {
		fmt.Fprintln(stdout, "no saved conversations")
	}
}

// resolvePrompt combines the -p flag with piped stdin ("" when neither).
func resolvePrompt(flagPrompt string, stdin io.Reader, isTTY bool) (string, error) {
	var piped string
	if !isTTY && stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", err
		}
		piped = strings.TrimSpace(string(b))
	}
	switch {
	case flagPrompt != "" && piped != "":
		return flagPrompt + "\n\n" + piped, nil
	case flagPrompt != "":
		return flagPrompt, nil
	}
	return piped, nil
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "harness:", err)
	return 1
}

// dumpPermissionFiles lists what the project's permissions.yml asked for and was not honoured (-config-dump).
func dumpPermissionFiles(w io.Writer, cfg *config.Config) {
	ws := cfg.LocalWorkspace
	if ws == "" {
		ws, _ = os.Getwd()
	}
	p, err := permissions.Load(permissions.Options{
		Workspace: ws, Mode: cfg.AgentPermissions, Trusted: cfg.Layers != nil && cfg.Layers.Trusted,
	})
	if err != nil || len(p.Ignored) == 0 {
		return
	}
	fmt.Fprintln(w, "\nignored (permissions file):")
	for _, s := range p.Ignored {
		fmt.Fprintf(w, "  %s\n", s)
	}
}
