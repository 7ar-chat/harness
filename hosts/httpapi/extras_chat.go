package httpapi

import (
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/hosts/client"
)

// routeChat registers the chat workbench routes: the queue of messages sent while a reply runs, the runtime
// environments and the artifacts published into answers.
//
//	POST   /api/threads/{id}/queue            RunRequest → 201 QueueAck  (a message for the reply that is running)
//	GET    /api/threads/{id}/queue            → [QueueItem]
//	DELETE /api/threads/{id}/queue/{qid}      → 204
//	POST   /api/threads/{id}/queue/{qid}/send → 202 QueueSendResult  (steer the running reply, or send now)
//	GET    /api/environments                  → EnvironmentList
//	GET    /api/threads/{id}/environment      → ThreadEnvironment
//	PUT    /api/threads/{id}/environment      {"environment"} → ThreadEnvironment (409 once the thread has a workspace)
//	GET    /api/threads/{id}/published        → [PublishedArtifact]
//	GET    /api/threads/{id}/files/preview?path= → the file with its own type, for a sandboxed iframe or <img>
func (s *Server) routeChat() {
	s.Route("POST /threads/{id}/queue", s.queueAdd)
	s.Route("GET /threads/{id}/queue", s.queueList)
	s.Route("DELETE /threads/{id}/queue/{qid}", s.queueRemove)
	s.Route("POST /threads/{id}/queue/{qid}/send", s.queueSend)
	s.Route("GET /environments", s.environments)
	s.Route("GET /threads/{id}/environment", s.getEnvironment)
	s.Route("PUT /threads/{id}/environment", s.putEnvironment)
	s.Route("GET /threads/{id}/published", s.published)
	s.Route("GET /threads/{id}/files/preview", s.previewFile)
}

// failHarness answers an error of the harness the way the in-process client would.
func failHarness(w http.ResponseWriter, err error) {
	var api *client.Error
	switch {
	case err == nil:
		return
	case errors.As(err, &api):
		fail(w, err)
	case errors.Is(err, store.ErrNotFound):
		fail(w, client.ErrNotFound)
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrRunActive):
		fail(w, client.ErrConflict)
	case errors.Is(err, harness.ErrEnvironmentFrozen):
		fail(w, client.Errorf(client.ErrConflict, "The conversation already has a workspace: open a new session to use another environment."))
	case errors.Is(err, harness.ErrInvalidRequest):
		fail(w, client.Wrap(client.ErrInvalidRequest, err.Error(), err))
	case errors.Is(err, harness.ErrSteersClosed):
		fail(w, client.ErrRunNotActive)
	case errors.Is(err, harness.ErrTooManySteers):
		fail(w, client.ErrTooManySteers)
	default:
		fail(w, err)
	}
}

func toClientRun(r *store.Run) *client.Run {
	if r == nil {
		return nil
	}
	return &client.Run{ID: r.ID, ThreadID: r.ThreadID, Status: r.Status, Model: r.Model, Error: r.Error, ErrorCode: r.ErrorCode,
		LastSeq: r.LastSeq, CreatedAt: r.CreatedAt, Sandbox: r.Sandbox}
}

func toQueueItem(it harness.QueuedMessage) client.QueueItem {
	return client.QueueItem{ID: it.ID, Text: it.Text, Model: it.Options.Model, CreatedAt: it.CreatedAt}
}

func (s *Server) queueAdd(w http.ResponseWriter, r *http.Request, c Caller) {
	var body client.RunRequest
	if !decode(w, r, &body) {
		return
	}
	if len(body.Attachments) > 0 {
		fail(w, client.Errorf(client.ErrInvalidRequest, "Attachments cannot be queued: send them once the reply has ended."))
		return
	}
	id := r.PathValue("id")
	s.policy.BeforeRun(r, id, &body.SendOptions)
	o := body.SendOptions
	if o.Model == "" {
		o.Model = s.O.DefaultModel
	}
	a := s.policy.Author(c)
	it, run, err := s.H.QueueMessage(r.Context(), c.Login, id, body.Message, harness.RunOptions{
		Model: o.Model, ReasoningEffort: o.ReasoningEffort, Skills: o.Skills, Interactive: o.Interactive,
		Sandbox: harness.SandboxMode(o.Sandbox), Mode: harness.RunMode(o.Mode), Role: o.Role,
		Author: a.Login, AuthorKind: a.Kind, OnBehalfOf: a.OnBehalfOf,
	})
	if err != nil {
		failHarness(w, err)
		return
	}
	ack := client.QueueAck{Item: toQueueItem(it), Run: toClientRun(run)}
	if ack.Run != nil {
		s.policy.AfterRun(c, ack.Run, "run.send", nil)
	}
	writeJSON(w, http.StatusCreated, ack)
}

func (s *Server) queueList(w http.ResponseWriter, r *http.Request, c Caller) {
	items, err := s.H.ListQueue(r.Context(), c.Login, r.PathValue("id"))
	if err != nil {
		failHarness(w, err)
		return
	}
	out := make([]client.QueueItem, 0, len(items))
	for _, it := range items {
		out = append(out, toQueueItem(it))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) queueRemove(w http.ResponseWriter, r *http.Request, c Caller) {
	if err := s.H.RemoveQueued(r.Context(), c.Login, r.PathValue("id"), r.PathValue("qid")); err != nil {
		failHarness(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) queueSend(w http.ResponseWriter, r *http.Request, c Caller) {
	steered, run, err := s.H.SendQueuedNow(r.Context(), c.Login, r.PathValue("id"), r.PathValue("qid"))
	if err != nil {
		failHarness(w, err)
		return
	}
	res := client.QueueSendResult{Steered: steered, Run: toClientRun(run)}
	switch {
	case steered:
		s.policy.AfterRunAction(r, c, "run.steer")
	case res.Run != nil:
		s.policy.AfterRun(c, res.Run, "run.send", nil)
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (s *Server) environments(w http.ResponseWriter, _ *http.Request, _ Caller) {
	envs := s.H.Environments()
	list := make([]client.Environment, 0, len(envs))
	for _, e := range envs {
		list = append(list, client.Environment{Name: e.Name, Kind: e.Kind, Description: e.Description, Egress: e.Egress, Default: e.Default})
	}
	writeJSON(w, http.StatusOK, client.EnvironmentList{Environments: list, Default: s.H.DefaultEnvironment(), Selectable: s.H.EnvironmentSelectable()})
}

func (s *Server) threadEnvironment(r *http.Request, c Caller) (client.ThreadEnvironment, error) {
	id := r.PathValue("id")
	if _, err := s.H.Store.GetThread(r.Context(), c.Login, id); err != nil {
		return client.ThreadEnvironment{}, err
	}
	return client.ThreadEnvironment{Environment: s.H.ThreadEnvironment(r.Context(), id), Frozen: s.H.EnvironmentFrozen(r.Context(), id)}, nil
}

func (s *Server) getEnvironment(w http.ResponseWriter, r *http.Request, c Caller) {
	te, err := s.threadEnvironment(r, c)
	if err != nil {
		failHarness(w, err)
		return
	}
	writeJSON(w, http.StatusOK, te)
}

func (s *Server) putEnvironment(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Environment string `json:"environment"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.H.SetThreadEnvironment(r.Context(), c.Login, r.PathValue("id"), body.Environment); err != nil {
		failHarness(w, err)
		return
	}
	te, err := s.threadEnvironment(r, c)
	if err != nil {
		failHarness(w, err)
		return
	}
	writeJSON(w, http.StatusOK, te)
}

func (s *Server) published(w http.ResponseWriter, r *http.Request, c Caller) {
	list, err := s.H.PublishedArtifacts(r.Context(), c.Login, r.PathValue("id"))
	if err != nil {
		failHarness(w, err)
		return
	}
	out := make([]client.PublishedArtifact, 0, len(list))
	for _, p := range list {
		out = append(out, client.PublishedArtifact{ArtifactID: p.ArtifactID, Path: p.Path, Name: p.Name, Title: p.Title, Version: p.Version,
			MimeType: p.MimeType, SizeBytes: p.SizeBytes, RunID: p.RunID, ToolCallID: p.ToolCallID, PublishedAt: p.PublishedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// previewTypes are the files the browser may render inline, by extension. Everything else is not previewable
// (the UI offers the source and the download).
var previewTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".htm": "text/html; charset=utf-8",
	".pdf": "application/pdf",
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".svg": "image/svg+xml",
}

// previewFile serves a workspace file inline for the chat's artifact cards: an HTML page in a sandboxed frame, an
// image, a PDF. The file is agent-written code, so it is served as an opaque origin (CSP sandbox: no cookies, no
// access to this application's API, no network but https subresources) and may only be framed by this application.
func (s *Server) previewFile(w http.ResponseWriter, r *http.Request, c Caller) {
	f, err := s.API(c).DownloadFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		fail(w, err)
		return
	}
	ct, ok := previewTypes[strings.ToLower(path.Ext(f.Name))]
	if !ok {
		fail(w, client.Errorf(client.ErrInvalidRequest, "This file type cannot be previewed: download it instead."))
		return
	}
	csp := "sandbox allow-scripts allow-popups; default-src 'none'; img-src data: blob: https:; media-src data: blob: https:; " +
		"style-src 'unsafe-inline' https:; script-src 'unsafe-inline' https:; font-src data: https:; connect-src 'none'; frame-ancestors 'self'"
	if ct == "application/pdf" {
		// The PDF viewer does not run in a sandboxed frame; a PDF has no scripts of its own to contain.
		csp = "default-src 'none'; frame-ancestors 'self'"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Content-Length", strconv.Itoa(len(f.Data)))
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(f.Data)
}
