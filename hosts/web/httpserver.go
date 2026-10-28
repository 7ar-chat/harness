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
//	POST   /api/threads/{id}/runs  {"message","model","reasoningEffort","skills","interactive"} → 202 Run
//	POST   /api/threads/{id}/resume {"interruptId","reason","decision":"approve|deny","always","text","model","interactive"} → 202 Run
//	GET    /api/threads/{id}/files?path=             → [File]   (read-only, confined to /workspace)
//	GET    /api/threads/{id}/files/download?path=    → file bytes, Content-Disposition: attachment
//	GET    /api/runs/{id}                            → Run
//	GET    /api/runs/{id}/events?after=              → text/event-stream of AG-UI events (or Last-Event-ID)
//	POST   /api/runs/{id}/cancel                     → 202
//	POST   /api/runs/{id}/steer    {"text"}          → 202
//	GET    /, /ui/...                                → the embedded UI
//
// Resuming: a run that ends with RUN_FINISHED outcome "interrupt" waits for an
// answer. Post it to /resume with the interrupt id (the approval id for reason
// "tool_approval", with decision approve|deny and always=true to approve the
// tool for the rest of the run chain; the question's interruptId for "ask_user",
// with text). The response is the run that continues; follow its events.
package httpserver

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
	"github.com/GhaziBenDahmane/harness/engine/queue"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/filestore"
)

// Caller is an authenticated user.
type Caller struct {
	// Login owns threads and runs.
	Login string
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
	// PingEvery keeps idle event streams open through proxies (default 15 s).
	PingEvery time.Duration
	// NoUI leaves out the embedded UI (API only).
	NoUI bool
}

type server struct {
	h    *harness.Harness
	o    Options
	auth Authenticator
	mux  *http.ServeMux
}

// New returns the HTTP handler.
func New(h *harness.Harness, o Options) http.Handler {
	if o.PingEvery <= 0 {
		o.PingEvery = 15 * time.Second
	}
	s := &server{h: h, o: o, mux: http.NewServeMux()}
	switch {
	case o.Authenticate != nil:
		s.auth = o.Authenticate
	case o.User != nil:
		s.auth = func(r *http.Request) (string, error) {
			c, err := o.User(r)
			return c.Login, err
		}
	default:
		s.auth = LocalAuthenticator(o.LocalUser, o.Token)
	}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.route("GET /models", s.models)
	s.route("GET /sessions", s.sessions)
	s.route("POST /threads", s.createThread)
	s.route("GET /threads", s.listThreads)
	s.route("GET /threads/{id}", s.getThread)
	s.route("PATCH /threads/{id}", s.renameThread)
	s.route("DELETE /threads/{id}", s.deleteThread)
	s.route("POST /threads/{id}/runs", s.createRun)
	s.route("POST /threads/{id}/resume", s.resume)
	s.route("GET /threads/{id}/files", s.listFiles)
	s.route("GET /threads/{id}/files/download", s.downloadFile)
	s.route("GET /runs/{id}", s.getRun)
	s.route("GET /runs/{id}/events", s.events)
	s.route("POST /runs/{id}/cancel", s.cancel)
	s.route("POST /runs/{id}/steer", s.steer)
	if !o.NoUI {
		mountUI(s.mux)
	}
	return s.wrap(s.mux)
}

// route registers an authenticated route under /api and without the prefix.
func (s *server) route(pattern string, h handler) {
	method, path, _ := strings.Cut(pattern, " ")
	for _, prefix := range []string{"/api", ""} {
		s.mux.HandleFunc(method+" "+prefix+path, s.authed(h))
	}
}

type handler func(w http.ResponseWriter, r *http.Request, c Caller)

func (s *server) authed(next handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		login, err := s.auth(r)
		if err != nil || login == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "Authentication required.")
			return
		}
		next(w, r, Caller{Login: login})
	}
}

// ---- DTOs ----

type threadDTO struct {
	ID              string                 `json:"id"`
	Title           string                 `json:"title"`
	Pinned          bool                   `json:"pinned"`
	CreatedAt       time.Time              `json:"createdAt"`
	UpdatedAt       time.Time              `json:"updatedAt"`
	ActiveRun       *store.ActiveRun       `json:"activeRun"`
	PendingApproval map[string]any         `json:"pendingApproval"`
	PendingQuestion *store.PendingQuestion `json:"pendingQuestion"`
	Messages        []messageDTO           `json:"messages,omitempty"`
}

type messageDTO struct {
	ID         string           `json:"id"`
	RunID      string           `json:"runId,omitempty"`
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []store.ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string           `json:"toolCallId,omitempty"`
	Error      string           `json:"error,omitempty"`
	Metadata   map[string]any   `json:"metadata,omitempty"`
	CreatedAt  time.Time        `json:"createdAt"`
}

type runDTO struct {
	ID        string    `json:"id"`
	ThreadID  string    `json:"threadId"`
	Status    string    `json:"status"`
	Model     string    `json:"model"`
	Error     string    `json:"error,omitempty"`
	ErrorCode string    `json:"errorCode,omitempty"`
	LastSeq   int64     `json:"lastSeq"`
	CreatedAt time.Time `json:"createdAt"`
	Sandbox   string    `json:"sandbox,omitempty"`
}

func toThread(t *store.Thread) threadDTO {
	d := threadDTO{ID: t.ID, Title: t.Title, Pinned: t.PinnedAt != nil, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		ActiveRun: t.ActiveRun, PendingQuestion: t.PendingQuestion}
	if t.PendingApproval != nil {
		d.PendingApproval = t.PendingApproval.Payload()
	}
	return d
}

func toRun(r *store.Run) runDTO {
	return runDTO{ID: r.ID, ThreadID: r.ThreadID, Status: r.Status, Model: r.Model, Error: r.Error, ErrorCode: r.ErrorCode,
		LastSeq: r.LastSeq, CreatedAt: r.CreatedAt, Sandbox: r.Sandbox}
}

// ---- handlers ----

func (s *server) models(w http.ResponseWriter, _ *http.Request, _ Caller) {
	m := s.o.Models
	if m == nil {
		m = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": m, "default": s.o.DefaultModel})
}

func (s *server) sessions(w http.ResponseWriter, r *http.Request, c Caller) {
	fs, ok := s.h.Store.(interface{ Sessions() filestore.Sessions })
	if !ok {
		writeError(w, http.StatusNotImplemented, "not_supported", "The store has no session listing.")
		return
	}
	list, err := fs.Sessions().List(r.Context(), c.Login, limitParam(r, 50))
	if err != nil {
		fail(w, err)
		return
	}
	type session struct {
		ID         string    `json:"id"`
		Title      string    `json:"title"`
		Preview    string    `json:"preview"`
		Messages   int       `json:"messages"`
		LastStatus string    `json:"lastStatus"`
		CreatedAt  time.Time `json:"createdAt"`
		UpdatedAt  time.Time `json:"updatedAt"`
	}
	out := make([]session, 0, len(list))
	for _, i := range list {
		out = append(out, session{i.ID, i.Title, i.Preview, i.Messages, i.LastStatus, i.CreatedAt, i.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func limitParam(r *http.Request, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		return min(n, 200)
	}
	return def
}

func (s *server) createThread(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Title string `json:"title"`
	}
	if !decode(w, r, &body) {
		return
	}
	th, err := s.h.NewThread(r.Context(), c.Login, body.Title)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toThread(th))
}

func (s *server) listThreads(w http.ResponseWriter, r *http.Request, c Caller) {
	ths, err := s.h.Store.ListThreads(r.Context(), c.Login, limitParam(r, 50), nil)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]threadDTO, 0, len(ths))
	for _, t := range ths {
		out = append(out, toThread(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) getThread(w http.ResponseWriter, r *http.Request, c Caller) {
	th, err := s.h.Store.GetThread(r.Context(), c.Login, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	msgs, err := s.h.Store.ListMessages(r.Context(), th.ID)
	if err != nil {
		fail(w, err)
		return
	}
	d := toThread(th)
	d.Messages = make([]messageDTO, 0, len(msgs))
	for _, m := range msgs {
		d.Messages = append(d.Messages, messageDTO{ID: m.ID, RunID: m.RunID, Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls,
			ToolCallID: m.ToolCallID, Error: m.Error, Metadata: m.Metadata, CreatedAt: m.CreatedAt})
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *server) renameThread(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Title  *string `json:"title"`
		Pinned *bool   `json:"pinned"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Title == nil && body.Pinned == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "title or pinned is required.")
		return
	}
	if body.Title != nil {
		t := strings.TrimSpace(*body.Title)
		if t == "" || len([]rune(t)) > 200 {
			writeError(w, http.StatusBadRequest, "invalid_request", "title must be 1 to 200 characters.")
			return
		}
		body.Title = &t
	}
	th, err := s.h.Store.UpdateThread(r.Context(), c.Login, r.PathValue("id"), store.ThreadPatch{Title: body.Title, Pinned: body.Pinned})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toThread(th))
}

func (s *server) deleteThread(w http.ResponseWriter, r *http.Request, c Caller) {
	if _, err := s.h.Store.DeleteThread(r.Context(), c.Login, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) createRun(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Message         string   `json:"message"`
		Model           string   `json:"model"`
		ReasoningEffort string   `json:"reasoningEffort"`
		Skills          []string `json:"skills"`
		Interactive     bool     `json:"interactive"`
		// Sandbox is auto (default), off (no sandbox tools for this run) or on
		// (fail when the harness has no sandbox).
		Sandbox string `json:"sandbox"`
	}
	if !decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "message is required.")
		return
	}
	if !s.ownsThread(w, r, c) {
		return
	}
	run, err := s.h.Send(r.Context(), c.Login, r.PathValue("id"), body.Message, harness.RunOptions{
		Model: s.model(body.Model), ReasoningEffort: body.ReasoningEffort, Skills: body.Skills, Interactive: body.Interactive,
		Sandbox: harness.SandboxMode(body.Sandbox),
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toRun(run))
}

func (s *server) model(m string) string {
	if m == "" {
		return s.o.DefaultModel
	}
	return m
}

// ownsThread answers 404 (and returns false) unless the path's thread belongs to the caller.
func (s *server) ownsThread(w http.ResponseWriter, r *http.Request, c Caller) bool {
	if _, err := s.h.Store.GetThread(r.Context(), c.Login, r.PathValue("id")); err != nil {
		fail(w, err)
		return false
	}
	return true
}

func (s *server) resume(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		InterruptID     string `json:"interruptId"`
		Reason          string `json:"reason"`
		Decision        string `json:"decision"`
		Always          bool   `json:"always"`
		Text            string `json:"text"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoningEffort"`
		Interactive     bool   `json:"interactive"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.InterruptID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "interruptId is required.")
		return
	}
	if body.Reason == "" {
		body.Reason = "ask_user"
		if body.Decision != "" {
			body.Reason = store.ReasonToolApproval
		}
	}
	if body.Reason == store.ReasonToolApproval && body.Decision != store.DecisionApprove && body.Decision != store.DecisionDeny && body.Decision != store.DecisionRespond {
		writeError(w, http.StatusBadRequest, "invalid_request", "decision must be approve, deny or respond.")
		return
	}
	if !s.ownsThread(w, r, c) {
		return
	}
	run, err := s.h.Resume(r.Context(), c.Login, r.PathValue("id"), harness.ResumeRequest{
		InterruptID: body.InterruptID, Reason: body.Reason, Decision: body.Decision, Always: body.Always, Text: body.Text,
		Model: s.model(body.Model), Interactive: body.Interactive, ReasoningEffort: body.ReasoningEffort,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toRun(run))
}

func (s *server) getRun(w http.ResponseWriter, r *http.Request, c Caller) {
	run, err := s.h.Store.GetUserRun(r.Context(), c.Login, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRun(run))
}

func (s *server) events(w http.ResponseWriter, r *http.Request, c Caller) {
	run, err := s.h.Store.GetUserRun(r.Context(), c.Login, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	after := int64(0)
	for _, v := range []string{r.URL.Query().Get("after"), r.Header.Get("Last-Event-ID")} {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n >= 0 {
			after = n
		}
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

	events := s.h.Events(r.Context(), run.ID, after)
	ping := time.NewTicker(s.o.PingEvery)
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

func (s *server) cancel(w http.ResponseWriter, r *http.Request, c Caller) {
	if err := s.h.Cancel(r.Context(), c.Login, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}

func (s *server) steer(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "text is required.")
		return
	}
	switch err := s.h.Steer(r.Context(), c.Login, r.PathValue("id"), body.Text); {
	case errors.Is(err, queue.ErrSteersClosed):
		writeError(w, http.StatusConflict, "run_not_active", "The run no longer accepts messages.")
	case errors.Is(err, queue.ErrTooManySteers):
		writeError(w, http.StatusTooManyRequests, "too_many_steers", "Too many pending messages.")
	case err != nil:
		fail(w, err)
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	// An empty body is fine: every field is optional.
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body.")
		return false
	}
	return true
}

// fail maps a harness error to the error envelope {"error":{"code","message"}}.
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "Not found.")
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrRunActive):
		writeError(w, http.StatusConflict, "conflict", "Conflict.")
	case errors.Is(err, harness.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		slog.Error("httpserver: request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "An internal error occurred.")
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
