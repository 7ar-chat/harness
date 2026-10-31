// Package httpserver exposes a harness.Harness over HTTP for web front ends:
// a JSON API for threads, runs, approvals, files and models, the run's AG-UI
// events as Server-Sent Events (the SSE id is the event seq, so reconnects
// resume with Last-Event-ID or ?after= without loss or duplicates), and a small
// built-in browser UI (ui/) served at / and /ui/.
//
// Every API route is served under /api and, for compatibility, without the
// prefix. Each route except /healthz needs an authenticated caller (see
// Authenticator) and answers 404 for threads and runs that belong to another
// user. Errors use {"error":{"code","message"}}.
//
//	GET    /healthz                                  → {"status":"ok"}            (no auth)
//	GET    /api/models                               → {"models":[…],"default"}
//	GET    /api/sessions?limit=                      → [Session]  (durable file store only, else 501)
//	POST   /api/threads            {"title"}         → 201 Thread
//	GET    /api/threads?limit=                       → [Thread]
//	GET    /api/threads/{id}                         → Thread + messages (+ pendingApproval, pendingQuestion, activeRun)
//	PATCH  /api/threads/{id}       {"title"}         → Thread
//	DELETE /api/threads/{id}                         → 204
//	POST   /api/threads/{id}/runs  {"message","model","reasoningEffort","skills","interactive","sandbox":"auto|off|on","mode":"build|plan","attachments":[{"name","mime","data":base64}]} → 202 Run
//	POST   /api/threads/{id}/resume {"interruptId","reason","decision":"approve|deny","always","text","model","interactive","mode"} → 202 Run
//	GET    /api/threads/{id}/files?path=             → [File]   (read-only, confined to /workspace)
//	GET    /api/threads/{id}/files/download?path=    → file bytes, Content-Disposition: attachment
//	GET    /api/runs/{id}                            → Run
//	GET    /api/runs/{id}/events?after=              → text/event-stream of AG-UI events (or Last-Event-ID)
//	POST   /api/runs/{id}/cancel                     → 202
//	POST   /api/runs/{id}/steer    {"text"}          → 202
//	GET    /api/commands                             → [Command]  (custom commands, skills, builtins)
//	POST   /api/commands/{name}/expand {"args"}      → {"prompt"}
//	GET    /, /ui/...                              → the embedded UI
//
// Resuming: a run that ends with RUN_FINISHED outcome "interrupt" waits for an
// answer. Post it to /resume with the interrupt id (the approval id for reason
// "tool_approval", with decision approve|deny and always=true to approve the
// tool for the rest of the run chain; the question's interruptId for "ask_user",
// with text). The response is the run that continues; follow its events.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/hosts/client"
	"github.com/GhaziBenDahmane/harness/hosts/inproc"
)

// Caller is an authenticated user.
type Caller struct {
	// Login owns threads and runs: what store calls are scoped by. In team
	// mode it is the owner of the thread being acted on, which is not the
	// actor for a thread shared through a project.
	Login string
	// Actor is the authenticated user (equal to Login outside team mode).
	Actor string
	// ProjectID, Role and CreatedBy describe the project thread being acted on.
	ProjectID string
	Role      string
	CreatedBy string
}

// Options configures the handler.
type Options struct {
	// Authenticate maps a request to the user it acts for; an error answers 401.
	// Takes precedence over User and Token.
	Authenticate Authenticator
	// User is the older form of Authenticate.
	User func(*http.Request) (Caller, error)
	// Token, when set (and no Authenticate/User), is the static bearer token every
	// API request must carry; all requests then act as LocalUser.
	Token string
	// LocalUser is the single user of the default authenticator (default "local").
	LocalUser string
	// AllowedOrigins are the browser origins (scheme://host[:port]) allowed to call the
	// API cross-origin; "*" allows any. Same-origin requests need no entry.
	AllowedOrigins []string
	// AllowedHosts, when set, restricts the Host header (DNS-rebinding guard for
	// token-less loopback servers), e.g. "localhost", "127.0.0.1".
	AllowedHosts []string
	// Models are the model ids offered by GET /api/models.
	Models []string
	// DefaultModel is used when a run request names none.
	DefaultModel string
	// ModelInfo (optional) describes the models for GET /api/models: context window, prices, capabilities.
	ModelInfo func(id string) (client.ModelInfo, bool)
	// PingEvery keeps idle event streams open through proxies (default 15 s).
	PingEvery time.Duration
	// NoUI leaves out the embedded UI (API only).
	NoUI bool
	// Extension plugs a product on top of the API: identity, shared projects,
	// connectors... (package product). Without it the server is the plain
	// single-user API.
	Extension Extension
	// Client is what the browser UI needs to report its own errors and usage (see ClientConfig). The zero value
	// reports nothing and leaves the Content-Security-Policy at 'self'.
	Client ClientConfig
}

// Server is the HTTP API. An Extension receives it in Install to add routes:
// everything it needs (the harness, the options, the mux, the route helpers)
// is exported here.
type Server struct {
	H      *harness.Harness
	O      Options
	Auth   Authenticator
	Mux    *http.ServeMux
	policy Policy
	csp    string
}

// New returns the HTTP handler. It panics when Options are inconsistent (OIDC
// that cannot be reached); use NewServer to get the error.
func New(h *harness.Harness, o Options) http.Handler {
	hd, err := NewServer(h, o)
	if err != nil {
		panic(err)
	}
	return hd
}

// NewServer is New returning the error.
func NewServer(h *harness.Harness, o Options) (http.Handler, error) {
	if o.PingEvery <= 0 {
		o.PingEvery = 15 * time.Second
	}
	client, err := o.Client.normalise()
	if err != nil {
		return nil, err
	}
	o.Client = client
	s := &Server{H: h, O: o, Mux: http.NewServeMux(), policy: soloPolicy{}, csp: contentSecurityPolicy(client)}
	if o.Extension != nil {
		s.policy = o.Extension
		if err := o.Extension.Setup(&s.O, s.Mux); err != nil {
			return nil, err
		}
		o = s.O
	}
	switch {
	case o.Authenticate != nil:
		s.Auth = o.Authenticate
	case o.User != nil:
		s.Auth = func(r *http.Request) (string, error) {
			c, err := o.User(r)
			return c.Login, err
		}
	default:
		s.Auth = LocalAuthenticator(o.LocalUser, o.Token)
	}
	s.Mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Public, like /healthz: the UI reads it before anyone has logged in (the login redirect itself needs no UI,
	// but an error on the first screen should still be reported). It holds a DSN, which is a write-only key.
	s.Mux.HandleFunc("GET /api/client-config", s.clientConfig)
	s.Mux.HandleFunc("GET /client-config", s.clientConfig)
	s.Route("GET /models", s.models)
	s.Route("GET /sessions", s.sessions)
	s.Route("POST /threads", s.createThread)
	s.Route("GET /threads", s.listThreads)
	s.Route("GET /threads/{id}", s.getThread)
	s.Route("PATCH /threads/{id}", s.renameThread)
	s.Route("DELETE /threads/{id}", s.deleteThread)
	s.Route("POST /threads/{id}/runs", s.createRun)
	s.Route("POST /threads/{id}/resume", s.resume)
	s.Route("GET /threads/{id}/files", s.listFiles)
	s.Route("GET /threads/{id}/files/download", s.downloadFile)
	s.Route("GET /runs/{id}", s.getRun)
	s.Route("GET /runs/{id}/events", s.events)
	s.Route("POST /runs/{id}/cancel", s.cancel)
	s.Route("POST /runs/{id}/steer", s.steer)
	s.routeHistory()
	s.routeCommands()
	if o.Extension != nil {
		if err := o.Extension.Install(s); err != nil {
			return nil, err
		}
	}
	if !o.NoUI {
		mountUI(s.Mux)
	}
	return s.wrap(s.Mux), nil
}

// Route registers an authenticated route under /api and without the prefix.
func (s *Server) Route(pattern string, h Handler) {
	method, path, _ := strings.Cut(pattern, " ")
	kind := kindOf(path)
	for _, prefix := range []string{"/api", ""} {
		s.Mux.HandleFunc(method+" "+prefix+path, s.Authed(kind, h))
	}
}

func (s *Server) Authed(kind ResourceKind, next Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		login, err := s.Auth(r)
		if err != nil || login == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "Authentication required.")
			return
		}
		c, err := s.policy.Resolve(r, kind, login)
		if err != nil {
			s.policy.FailAccess(w, err)
			return
		}
		next(w, r, c)
	}
}

// ---- handlers ----
//
// The handlers are a thin HTTP skin over inproc.New: the DTOs, the
// validation and the errors are those of package client, which is also what
// client.HTTP decodes.

// api is the engine as seen by the caller.
func (s *Server) API(c Caller) client.Client {
	return inproc.New(s.H, c.Login, inproc.WithModels(s.O.Models, s.O.DefaultModel), inproc.WithModelInfo(s.O.ModelInfo),
		inproc.WithAuthor(s.policy.Author(c)))
}

func (s *Server) models(w http.ResponseWriter, r *http.Request, c Caller) {
	m, err := s.API(c).Models(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request, c Caller) {
	out, err := s.API(c).Sessions(r.Context(), limitParam(r, 50))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func limitParam(r *http.Request, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		return min(n, 200)
	}
	return def
}

func (s *Server) createThread(w http.ResponseWriter, r *http.Request, c Caller) {
	if s.policy.CreateThread(w, r, c) {
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	if !decode(w, r, &body) {
		return
	}
	th, err := s.API(c).CreateThread(r.Context(), body.Title)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, th)
}

func (s *Server) listThreads(w http.ResponseWriter, r *http.Request, c Caller) {
	if s.policy.ListThreads(w, r, c) {
		return
	}
	ths, err := s.API(c).ListThreads(r.Context(), limitParam(r, 50))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ths)
}

func (s *Server) getThread(w http.ResponseWriter, r *http.Request, c Caller) {
	if s.policy.GetThread(w, r, c) {
		return
	}
	th, err := s.API(c).GetThread(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, th)
}

func (s *Server) renameThread(w http.ResponseWriter, r *http.Request, c Caller) {
	var body client.ThreadPatch
	if !decode(w, r, &body) {
		return
	}
	th, err := s.API(c).RenameThread(r.Context(), r.PathValue("id"), body)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, th)
}

func (s *Server) deleteThread(w http.ResponseWriter, r *http.Request, c Caller) {
	if s.policy.DeleteThread(w, r, c) {
		return
	}
	if err := s.API(c).DeleteThread(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request, c Caller) {
	var body client.RunRequest
	if !decodeLimit(w, r, &body, maxRunBody) {
		return
	}
	s.policy.BeforeRun(r, r.PathValue("id"), &body.SendOptions)
	run, err := s.API(c).Send(r.Context(), r.PathValue("id"), body.Message, body.SendOptions)
	if err != nil {
		fail(w, err)
		return
	}
	s.policy.AfterRun(c, run, "run.send", nil)
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request, c Caller) {
	var body client.ResumeRequest
	if !decode(w, r, &body) {
		return
	}
	tool := s.policy.BeforeResume(r, c, r.PathValue("id"), &body)
	run, err := s.API(c).Resume(r.Context(), r.PathValue("id"), body)
	if err != nil {
		fail(w, err)
		return
	}
	s.policy.AfterRun(c, run, "run.resume", map[string]any{"decision": body.Decision, "reason": body.Reason, "tool": tool, "always": body.Always})
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request, c Caller) {
	run, err := s.API(c).GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, c Caller) {
	after := int64(0)
	for _, v := range []string{r.URL.Query().Get("after"), r.Header.Get("Last-Event-ID")} {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n >= 0 {
			after = n
		}
	}
	events, err := s.API(c).Events(r.Context(), r.PathValue("id"), after)
	if err != nil {
		fail(w, err)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "Streaming is not supported.")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(": connected\n\n"))
	fl.Flush()

	ping := time.NewTicker(s.O.PingEvery)
	defer ping.Stop()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			if _, err := w.Write([]byte("id: " + strconv.FormatInt(ev.Seq, 10) + "\ndata: " + string(ev.Data) + "\n\n")); err != nil {
				return
			}
			fl.Flush()
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request, c Caller) {
	if err := s.API(c).Cancel(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	s.policy.AfterRunAction(r, c, "run.cancel")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}

func (s *Server) steer(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.API(c).Steer(r.Context(), r.PathValue("id"), body.Text); err != nil {
		fail(w, err)
		return
	}
	s.policy.AfterRunAction(r, c, "run.steer")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeLimit(w, r, v, 2<<20)
}

// maxRunBody bounds a run-creation request: the attachments (base64: 4/3 of their size) and the rest.
const maxRunBody = harness.MaxAttachments*harness.MaxAttachmentBytes*4/3 + 2<<20

func decodeLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	// An empty body is fine: every field is optional.
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body.")
		return false
	}
	return true
}

// fail writes a client error with its own status and code, anything else as an internal error.
func fail(w http.ResponseWriter, err error) {
	if e := client.AsError(err); e != nil && e.Status != 500 {
		writeError(w, e.Status, e.Code, e.Message)
		return
	}
	slog.Error("httpserver: request failed", "error", err)
	writeError(w, http.StatusInternalServerError, "internal", "An internal error occurred.")
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
