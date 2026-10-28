// Package httpserver exposes a harness.Harness over HTTP for web front ends:
// JSON endpoints for threads and runs, and the run's AG-UI events as
// Server-Sent Events (the SSE id is the event seq, so EventSource reconnects
// resume with Last-Event-ID without loss or duplicates).
//
// Authentication is the host's: Options.User maps a request to the caller.
//
//	POST /threads                 {"title"}                                  → Thread
//	GET  /threads                                                            → [Thread]
//	POST /threads/{id}/runs       {"message","model","reasoningEffort",…}    → 202 Run
//	GET  /runs/{id}/events?after= text/event-stream of AG-UI events
//	POST /runs/{id}/cancel                                                   → 202
//	POST /runs/{id}/steer         {"text"}                                   → 202
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
	"github.com/GhaziBenDahmane/harness/queue"
	"github.com/GhaziBenDahmane/harness/store"
)

// Caller is an authenticated user.
type Caller struct {
	// Login owns threads and runs.
	Login string
}

// Options configures the handler.
type Options struct {
	// User authenticates a request; an error answers 401.
	User func(*http.Request) (Caller, error)
	// DefaultModel is used when a run request names none.
	DefaultModel string
	// PingEvery keeps idle event streams open through proxies (default 15 s).
	PingEvery time.Duration
}

type server struct {
	h *harness.Harness
	o Options
}

// New returns the HTTP handler.
func New(h *harness.Harness, o Options) http.Handler {
	if o.PingEvery <= 0 {
		o.PingEvery = 15 * time.Second
	}
	s := &server{h: h, o: o}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /threads", s.auth(s.createThread))
	mux.HandleFunc("GET /threads", s.auth(s.listThreads))
	mux.HandleFunc("POST /threads/{id}/runs", s.auth(s.createRun))
	mux.HandleFunc("GET /runs/{id}/events", s.auth(s.events))
	mux.HandleFunc("POST /runs/{id}/cancel", s.auth(s.cancel))
	mux.HandleFunc("POST /runs/{id}/steer", s.auth(s.steer))
	return mux
}

type handler func(w http.ResponseWriter, r *http.Request, c Caller)

func (s *server) auth(next handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := s.o.User(r)
		if err != nil || c.Login == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "Authentication required.")
			return
		}
		next(w, r, c)
	}
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
	writeJSON(w, http.StatusCreated, th)
}

func (s *server) listThreads(w http.ResponseWriter, r *http.Request, c Caller) {
	limit := 50
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, 200)
	}
	ths, err := s.h.Store.ListThreads(r.Context(), c.Login, limit, nil)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ths)
}

func (s *server) createRun(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Message         string   `json:"message"`
		Model           string   `json:"model"`
		ReasoningEffort string   `json:"reasoningEffort"`
		Skills          []string `json:"skills"`
		Interactive     bool     `json:"interactive"`
	}
	if !decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "message is required.")
		return
	}
	model := body.Model
	if model == "" {
		model = s.o.DefaultModel
	}
	run, err := s.h.Send(r.Context(), c.Login, r.PathValue("id"), body.Message, harness.RunOptions{
		Model: model, ReasoningEffort: body.ReasoningEffort, Skills: body.Skills, Interactive: body.Interactive,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
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
	case errors.Is(err, store.ErrConflict):
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
