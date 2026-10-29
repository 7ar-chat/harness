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
//	POST   /api/threads/{id}/runs  {"message","model","reasoningEffort","skills","interactive","sandbox":"auto|off|on"} → 202 Run
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
	"github.com/GhaziBenDahmane/harness/hosts/client"
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
	s.routeHistory()
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

// ---- handlers ----
//
// The handlers are a thin HTTP skin over client.InProcess: the DTOs, the
// validation and the errors are those of package client, which is also what
// client.HTTP decodes.

// api is the engine as seen by the caller.
func (s *server) api(c Caller) client.Client {
	return client.InProcess(s.h, c.Login, client.WithModels(s.o.Models, s.o.DefaultModel))
}

func (s *server) models(w http.ResponseWriter, r *http.Request, c Caller) {
	m, err := s.api(c).Models(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *server) sessions(w http.ResponseWriter, r *http.Request, c Caller) {
	out, err := s.api(c).Sessions(r.Context(), limitParam(r, 50))
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

func (s *server) createThread(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Title string `json:"title"`
	}
	if !decode(w, r, &body) {
		return
	}
	th, err := s.api(c).CreateThread(r.Context(), body.Title)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, th)
}

func (s *server) listThreads(w http.ResponseWriter, r *http.Request, c Caller) {
	ths, err := s.api(c).ListThreads(r.Context(), limitParam(r, 50))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ths)
}

func (s *server) getThread(w http.ResponseWriter, r *http.Request, c Caller) {
	th, err := s.api(c).GetThread(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, th)
}

func (s *server) renameThread(w http.ResponseWriter, r *http.Request, c Caller) {
	var body client.ThreadPatch
	if !decode(w, r, &body) {
		return
	}
	th, err := s.api(c).RenameThread(r.Context(), r.PathValue("id"), body)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, th)
}

func (s *server) deleteThread(w http.ResponseWriter, r *http.Request, c Caller) {
	if err := s.api(c).DeleteThread(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) createRun(w http.ResponseWriter, r *http.Request, c Caller) {
	var body client.RunRequest
	if !decode(w, r, &body) {
		return
	}
	run, err := s.api(c).Send(r.Context(), r.PathValue("id"), body.Message, body.SendOptions)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *server) resume(w http.ResponseWriter, r *http.Request, c Caller) {
	var body client.ResumeRequest
	if !decode(w, r, &body) {
		return
	}
	run, err := s.api(c).Resume(r.Context(), r.PathValue("id"), body)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *server) getRun(w http.ResponseWriter, r *http.Request, c Caller) {
	run, err := s.api(c).GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *server) events(w http.ResponseWriter, r *http.Request, c Caller) {
	after := int64(0)
	for _, v := range []string{r.URL.Query().Get("after"), r.Header.Get("Last-Event-ID")} {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n >= 0 {
			after = n
		}
	}
	events, err := s.api(c).Events(r.Context(), r.PathValue("id"), after)
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
	if err := s.api(c).Cancel(r.Context(), r.PathValue("id")); err != nil {
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
	if err := s.api(c).Steer(r.Context(), r.PathValue("id"), body.Text); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
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
