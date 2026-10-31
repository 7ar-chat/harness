// Command website shows how a website embeds the harness as a Go library:
// one process serving its own pages and endpoints next to the agent.
//
//	WEBSITE_ADDR=:8080 LLM_API_KEY=... go run ./examples/website
//
// What it demonstrates (docs/embedding.md has the step-by-step):
//
//   - an Authenticator that maps the site's session to a user id (here a
//     header set by the site's own auth layer; never trust one from a browser);
//   - per-user thread listing, with ownership enforced by the harness (a
//     thread of another user is "not found");
//   - a host-defined tool (lookup_order) registered with a Feature, scoped to
//     the user the run belongs to;
//   - a per-request choice of Sandbox: off for questions and API calls, auto
//     (sandbox created lazily on the first command) when the user asks for code;
//   - hosts/web's JSON+SSE handler mounted under /agent/;
//   - graceful shutdown: stop accepting requests, let in-flight runs finish.
//
// One process (this file) uses the in-memory queue and event log. To scale out,
// keep the same code but give harness.Options a Postgres store
// (store/pgstore) and Redis (redisinfra.Infra as Options.Infra); the web processes then never call
// h.Run (they only Send and stream events) and the agent processes run
// hosts/worker; see docs/embedding.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/filestore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

// Orders is the site's own data the agent may read: in a real site this is
// your database or an internal API client.
type Orders interface {
	Lookup(ctx context.Context, user, orderID string) (status string, ok bool)
}

// site holds what the HTTP handlers need.
type site struct {
	h     *harness.Harness
	model string
}

// authenticate is the site's Authenticator. The harness only needs a stable,
// unique user id: it namespaces every thread, run and sandbox by it. Replace the
// header with your session cookie / JWT verification.
func authenticate(r *http.Request) (string, error) {
	if u := strings.TrimSpace(r.Header.Get("X-Site-User")); u != "" {
		return u, nil
	}
	return "", errors.New("not signed in")
}

// ordersFeature is a Feature: the unit hosts use to add capabilities (tools,
// prompt sections, hooks). It adds lookup_order, which only ever sees the
// orders of the user the run belongs to.
func ordersFeature(orders Orders) harness.Feature {
	return harness.FeatureFunc("orders", func(b *harness.Builder) error {
		b.AddTools(ordersTools{orders})
		return nil
	})
}

type ordersTools struct{ orders Orders }

func (ordersTools) Name() string { return "orders" }

// Tools is called once per run: rc.Login is the run's user, so the closure
// below cannot be tricked (by a prompt injection or the model) into reading
// another customer's order.
func (p ordersTools) Tools(_ context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	type in struct {
		OrderID string `json:"orderId" jsonschema_description:"The order number, e.g. A-1042"`
	}
	t, err := ai.InferTool("lookup_order", "Look up the status of one of the signed-in customer's orders.",
		func(ctx context.Context, a in) (string, error) {
			status, ok := p.orders.Lookup(ctx, rc.Login, a.OrderID)
			if !ok {
				return "no such order for this customer", nil // same answer for "not yours": no oracle
			}
			return status, nil
		})
	return []ai.BaseTool{t}, func() {}, err
}

// newHarness builds the harness. Backend nil means "no sandbox at all": the
// site then only offers the tools it registers. A backend (OpenSandbox, or your
// own: docs/sandbox-adapters.md) makes runs with Sandbox auto able to run code.
func newHarness(cfg *config.Config, models llm.Factory, backend sandbox.Backend, st harness.Store, orders Orders) (*harness.Harness, error) {
	return harness.New(harness.Options{
		Config: cfg, Models: models, Backend: backend, Store: st,
		Features: append(harness.ProfileWork(), ordersFeature(orders)),
		Version:  "website-example",
	})
}

// handler is the site: its own endpoints plus the agent API under /agent/.
func (s *site) handler() http.Handler {
	mux := http.NewServeMux()
	// Everything hosts/web serves (threads, runs, SSE events, files...) under
	// /agent/api/...; the site's session is the only way in. NoUI because the
	// site has its own front end.
	mux.Handle("/agent/", http.StripPrefix("/agent", httpapi.New(s.h, httpapi.Options{
		Authenticate: authenticate, DefaultModel: s.model, NoUI: true,
	})))
	mux.HandleFunc("GET /my/threads", s.requireUser(s.myThreads))
	mux.HandleFunc("POST /ask", s.requireUser(s.ask))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	return mux
}

func (s *site) requireUser(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, err := authenticate(r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r, user)
	}
}

// myThreads lists the signed-in user's conversations, newest first.
func (s *site) myThreads(w http.ResponseWriter, r *http.Request, user string) {
	threads, err := s.h.Store.ListThreads(r.Context(), user, 50, nil)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	type row struct{ ID, Title string }
	out := []row{}
	for _, t := range threads {
		out = append(out, row{t.ID, t.Title})
	}
	writeJSON(w, http.StatusOK, out)
}

// ask is the site's own "send a message" endpoint: it decides, per request,
// whether the run may use a sandbox. The browser then streams
// /agent/api/runs/{runId}/events (SSE) and may cancel with
// POST /agent/api/runs/{runId}/cancel.
//
//	POST /ask {"threadId"?, "message", "code": bool}
func (s *site) ask(w http.ResponseWriter, r *http.Request, user string) {
	var body struct {
		ThreadID string `json:"threadId"`
		Message  string `json:"message"`
		Code     bool   `json:"code"` // the user asked for something that needs to run code or create files
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || strings.TrimSpace(body.Message) == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if body.ThreadID == "" {
		th, err := s.h.NewThread(ctx, user, "")
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		body.ThreadID = th.ID
	}
	mode := harness.SandboxOff // questions, order lookups, searches: no sandbox, no cost
	if body.Code {
		mode = harness.SandboxAuto // created on the first command, not before
	}
	run, err := s.h.Send(ctx, user, body.ThreadID, body.Message, harness.RunOptions{Model: s.model, Sandbox: mode})
	switch {
	case errors.Is(err, harness.ErrInvalidRequest):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, store.ErrNotFound):
		// A thread of another user is store.ErrNotFound: answer like a missing one.
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, store.ErrRunActive):
		http.Error(w, "a run is already in progress on this thread", http.StatusConflict)
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{
			"threadId": run.ThreadID, "runId": run.ID, "events": "/agent/api/runs/" + run.ID + "/events"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type demoOrders map[string]string

func (d demoOrders) Lookup(_ context.Context, user, id string) (string, bool) {
	s, ok := d[user+"/"+id]
	return s, ok
}

func main() {
	cfg, err := config.Load() // LLM_*, SANDBOX_BACKEND, OPENSANDBOX_URL, MAX_SANDBOXES_PER_USER...
	if err != nil {
		log.Fatal(err)
	}
	// Durable single-node storage: threads and runs survive restarts. For several
	// processes use store/pgstore + redisinfra.Infra (Options.Infra) instead (docs/embedding.md).
	var st harness.Store
	if dir := os.Getenv("WEBSITE_DATA_DIR"); dir != "" {
		fs, err := filestore.Open(dir)
		if err != nil {
			log.Fatal(err)
		}
		defer fs.Close()
		st = fs
	}
	// Backend nil: harness.New selects it from SANDBOX_BACKEND / OPENSANDBOX_URL.
	h, err := newHarness(cfg, nil, nil, st, demoOrders{"alice/A-1042": "shipped 2026-10-20"})
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runDone := make(chan error, 1)
	go func() { runDone <- h.Run(ctx) }() // executes the queued runs in this process

	addr := os.Getenv("WEBSITE_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{Addr: addr, Handler: (&site{h: h, model: cfg.LLMDefaultModel}).handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	log.Printf("website listening on %s", addr)

	<-ctx.Done() // SIGINT / SIGTERM
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx) // stop taking requests; open SSE streams end with the context
	<-runDone              // h.Run returns once in-flight runs finished (or after the shutdown grace)
}
