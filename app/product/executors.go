package product

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/core/capabilities"
	"github.com/7ar-chat/harness/core/executors"
	"github.com/7ar-chat/harness/core/roles"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
)

// External coding-agent executors (Claude Code, Codex, OpenCode, or a command a project defined) that agents run in
// the sandbox through the run_executor tool, and that a role can name to do its work. The builtin ones are code; a
// project's owner defines more. A user seals their own API key and base URL for an executor: they are write-only
// (no route returns them), stored in the capabilities installs of the user under "exec_<name>" (the marketplace
// hides them), and handed to the process of a run by the harness: never to the model, never to disk.
//
//	GET    /api/executors?project={pid}                  → [ExecutorView]   (builtin, and the project's when given: any member)
//	GET    /api/projects/{pid}/executors                 → [ExecutorView]
//	POST   /api/projects/{pid}/executors {name,command,binary,…} → 201 ExecutorView   (owner: a custom executor)
//	PUT    /api/projects/{pid}/executors/{name} {…}      → ExecutorView                (owner)
//	DELETE /api/projects/{pid}/executors/{name}          → 204                        (owner)
//	PUT    /api/me/executors/{name}/credentials {env:{NAME:value}} → ExecutorView   (the caller's own; an empty value removes one)
//	DELETE /api/me/executors/{name}/credentials          → 204
//
// A project's executors need the executors store; the credentials need the capabilities store and a secret key.

const executorInstallPrefix = "exec_"

// ExecutorView is an executor as the web app lists it. The credentials are never in it: Configured says whether
// the caller has sealed any for this executor.
type ExecutorView struct {
	executors.Executor
	// Configured: the caller has sealed credentials for it.
	Configured bool `json:"configured"`
	// Editable: the caller may change or delete the definition (the owner of the project, for a custom one).
	Editable bool `json:"editable"`
	// Availability says whether the CLI is in the default sandbox image: "ready", "install" (a run installs it on
	// first use), "missing" or "unknown" (no sandbox backend, or the probe has not answered).
	Availability string `json:"availability"`
}

// ExecutorAPI serves the executors: the routes, the resolver and the credential source of the harness feature.
type ExecutorAPI struct {
	team store.TeamStore
	caps capabilities.Store
	defs executors.Store
	box  *secretBox
	// AllowInstall lets runs install a missing CLI with its install command (harness feature option).
	AllowInstall bool

	cli   cliProbe
	mgrMu sync.Mutex
	mgr   sandbox.Manager
}

var (
	_ executors.Resolver         = (*ExecutorAPI)(nil)
	_ executors.CredentialSource = (*ExecutorAPI)(nil)
)

// NewExecutorAPI returns the service. caps (nil: no credentials can be stored) holds the sealed credentials with
// secretKey; defs (nil: only the builtin executors) holds the executors projects defined.
func NewExecutorAPI(team store.TeamStore, caps capabilities.Store, defs executors.Store, secretKey []byte) (*ExecutorAPI, error) {
	a := &ExecutorAPI{team: team, caps: caps, defs: defs}
	if len(secretKey) > 0 {
		var err error
		if a.box, err = newSecretBoxFor(capabilitiesDomain, secretKey); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// Feature installs the harness's Executors feature over this service. taskStore (nil: none) is the factory's store:
// it gives the runs their task, policy and budget and receives the audit events. Install it before the factory.
func (a *ExecutorAPI) Feature(taskStore harness.ExecutorTaskStore) harness.Feature {
	inner := harness.Executors(harness.ExecutorsOptions{Resolver: a, Credentials: a, Store: taskStore, AllowInstall: a.AllowInstall})
	return harness.FeatureFunc(inner.Name(), func(b *harness.Builder) error {
		a.SetSandboxes(b.Sandboxes)
		return inner.Install(b)
	})
}

func (a *ExecutorAPI) projectOf(ctx context.Context, q executors.Query) string {
	if q.ProjectID != "" || a.team == nil || q.ThreadID == "" {
		return q.ProjectID
	}
	meta, err := a.team.GetThreadMeta(ctx, q.ThreadID)
	if err != nil {
		return ""
	}
	return meta.ProjectID
}

// Resolve implements executors.Resolver.
func (a *ExecutorAPI) Resolve(ctx context.Context, q executors.Query, name string) (*executors.Executor, error) {
	if b, ok := executors.BuiltinNamed(name); ok {
		return &b, nil
	}
	if a.defs != nil {
		if pid := a.projectOf(ctx, q); pid != "" {
			e, err := a.defs.Get(ctx, pid, name)
			switch {
			case err == nil && e.Enabled:
				return e, nil
			case err != nil && !errors.Is(err, executors.ErrNotFound):
				return nil, err
			}
		}
	}
	return nil, executors.ErrNotFound
}

// Available implements executors.Resolver: the builtin executors and the project's enabled ones.
func (a *ExecutorAPI) Available(ctx context.Context, q executors.Query) ([]executors.Executor, error) {
	out := executors.Builtins()
	if a.defs != nil {
		if pid := a.projectOf(ctx, q); pid != "" {
			list, err := a.defs.List(ctx, pid)
			if err != nil {
				return nil, err
			}
			for _, e := range list {
				if e.Enabled {
					out = append(out, e)
				}
			}
		}
	}
	return out, nil
}

// install is the user's install that holds the sealed credentials of an executor (nil: none).
func (a *ExecutorAPI) install(ctx context.Context, login, name string) (*capabilities.Install, error) {
	if a.caps == nil {
		return nil, nil
	}
	installs, err := a.caps.ListInstalls(ctx, capabilities.ScopeUser, login)
	if err != nil {
		return nil, err
	}
	for i := range installs {
		if installs[i].CatalogID == "" && installs[i].Name == executorInstallPrefix+name {
			return &installs[i], nil
		}
	}
	return nil, nil
}

// Credentials implements executors.CredentialSource: what login sealed for the executor, and nothing of a project's.
func (a *ExecutorAPI) Credentials(ctx context.Context, login, name string) (map[string]string, error) {
	in, err := a.install(ctx, login, name)
	if err != nil || in == nil || !in.Enabled {
		return nil, err
	}
	return openSecrets(a.box, in)
}

// ---- routes ----

func (s *server) routeExecutors() {
	a := s.opt.Executors
	if a == nil {
		return
	}
	s.Route("GET /executors", func(w http.ResponseWriter, r *http.Request, c Caller) {
		pid := r.URL.Query().Get("project")
		if pid == "" {
			s.listExecutors(w, r, c, "", false)
			return
		}
		role, err := s.requireProject(r.Context(), pid, c.Actor, store.RoleViewer)
		if err != nil {
			s.FailAccess(w, err)
			return
		}
		s.listExecutors(w, r, c, pid, store.RoleAtLeast(role, store.RoleOwner))
	})
	s.Route("GET /projects/{pid}/executors", func(w http.ResponseWriter, r *http.Request, c Caller) {
		if p, role, ok := s.project(w, r, c, store.RoleViewer); ok {
			s.listExecutors(w, r, c, p.ID, store.RoleAtLeast(role, store.RoleOwner))
		}
	})
	for _, m := range []string{"POST /projects/{pid}/executors", "PUT /projects/{pid}/executors/{name}"} {
		put := strings.HasPrefix(m, "PUT")
		s.Route(m, func(w http.ResponseWriter, r *http.Request, c Caller) {
			if p, _, ok := s.project(w, r, c, store.RoleOwner); ok {
				s.saveExecutor(w, r, inProject(c, p.ID), p.ID, put)
			}
		})
	}
	s.Route("DELETE /projects/{pid}/executors/{name}", func(w http.ResponseWriter, r *http.Request, c Caller) {
		p, _, ok := s.project(w, r, c, store.RoleOwner)
		if !ok {
			return
		}
		if a.defs == nil {
			writeError(w, http.StatusNotImplemented, "not_configured", "This server does not store custom executors.")
			return
		}
		if err := a.defs.Delete(r.Context(), p.ID, r.PathValue("name")); err != nil {
			executorFail(w, err)
			return
		}
		s.note(inProject(c, p.ID), "", "executor.delete", map[string]any{"executor": r.PathValue("name")}, FeedEvent{Type: "project.changed"})
		w.WriteHeader(http.StatusNoContent)
	})
	s.Route("PUT /me/executors/{name}/credentials", s.putExecutorCredentials)
	s.Route("DELETE /me/executors/{name}/credentials", s.deleteExecutorCredentials)
}

func executorFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, executors.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "No such executor.")
	case errors.Is(err, executors.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "An executor with that name already exists.")
	case errors.Is(err, executors.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", strings.TrimPrefix(err.Error(), executors.ErrInvalid.Error()+": "))
	default:
		fail(w, err)
	}
}

func (s *server) listExecutors(w http.ResponseWriter, r *http.Request, c Caller, projectID string, owner bool) {
	a := s.opt.Executors
	list := executors.Builtins()
	if a.defs != nil && projectID != "" {
		custom, err := a.defs.List(r.Context(), projectID)
		if err != nil {
			executorFail(w, err)
			return
		}
		list = append(list, custom...)
	}
	out := make([]ExecutorView, 0, len(list))
	avail := a.availability(r.Context(), list)
	for _, e := range list {
		v := a.view(r.Context(), c.Actor, e, owner)
		v.Availability = avail[e.Name]
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *ExecutorAPI) view(ctx context.Context, login string, e executors.Executor, owner bool) ExecutorView {
	v := ExecutorView{Executor: e, Editable: owner && e.Kind == executors.KindCustom, Availability: AvailUnknown}
	if in, err := a.install(ctx, login, e.Name); err == nil && in != nil && len(in.SecretKeys) > 0 {
		v.Configured = true
	}
	return v
}

// executorBody is the body of a POST and of a PUT of a custom executor.
type executorBody struct {
	Name           string                     `json:"name"`
	Title          string                     `json:"title"`
	Description    string                     `json:"description"`
	Command        string                     `json:"command"`
	ModeFlags      map[executors.Level]string `json:"modeFlags"`
	Binary         string                     `json:"binary"`
	InstallCommand string                     `json:"installCommand"`
	CredentialEnv  []string                   `json:"credentialEnv"`
	Parser         string                     `json:"parser"`
	Enabled        *bool                      `json:"enabled"`
}

func (s *server) saveExecutor(w http.ResponseWriter, r *http.Request, c Caller, projectID string, update bool) {
	a := s.opt.Executors
	if a.defs == nil {
		writeError(w, http.StatusNotImplemented, "not_configured", "This server does not store custom executors.")
		return
	}
	var body executorBody
	if !decode(w, r, &body) {
		return
	}
	name := strings.ToLower(strings.TrimSpace(body.Name))
	if update {
		name = r.PathValue("name")
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	e := executors.Executor{Name: name, ProjectID: projectID, Title: body.Title, Description: body.Description, Command: body.Command,
		ModeFlags: body.ModeFlags, Binary: body.Binary, InstallCommand: body.InstallCommand, CredentialEnv: body.CredentialEnv,
		Parser: body.Parser, Enabled: enabled}
	var saved *executors.Executor
	var err error
	status, action := http.StatusOK, "executor.update"
	if update {
		if body.Enabled == nil {
			if cur, gerr := a.defs.Get(r.Context(), projectID, name); gerr == nil {
				e.Enabled = cur.Enabled
			}
		}
		saved, err = a.defs.Update(r.Context(), e, c.Actor)
	} else {
		status, action = http.StatusCreated, "executor.create"
		saved, err = a.defs.Create(r.Context(), e, c.Actor)
	}
	if err != nil {
		executorFail(w, err)
		return
	}
	s.note(c, "", action, map[string]any{"executor": name}, FeedEvent{Type: "project.changed"})
	writeJSON(w, status, a.view(r.Context(), c.Actor, *saved, true))
}

// credentialsBody is the body of PUT .../credentials: environment variable name to value; an empty value removes it.
type credentialsBody struct {
	Env map[string]string `json:"env"`
}

// declaredFor finds the executor a user seals credentials for: a builtin one, or a custom one of a project the user
// belongs to (the one they name with ?project=).
func (s *server) declaredFor(w http.ResponseWriter, r *http.Request, c Caller, name string) (*executors.Executor, bool) {
	a := s.opt.Executors
	if b, ok := executors.BuiltinNamed(name); ok {
		return &b, true
	}
	if pid := r.URL.Query().Get("project"); pid != "" && a.defs != nil {
		if _, err := s.requireProject(r.Context(), pid, c.Actor, store.RoleViewer); err != nil {
			s.FailAccess(w, err)
			return nil, false
		}
		if e, err := a.defs.Get(r.Context(), pid, name); err == nil {
			return e, true
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "No such executor.")
	return nil, false
}

func (s *server) putExecutorCredentials(w http.ResponseWriter, r *http.Request, c Caller) {
	a := s.opt.Executors
	if a.caps == nil {
		writeError(w, http.StatusNotImplemented, "not_configured", "Storing credentials needs the marketplace store.")
		return
	}
	ex, ok := s.declaredFor(w, r, c, r.PathValue("name"))
	if !ok {
		return
	}
	var body credentialsBody
	if !decode(w, r, &body) {
		return
	}
	in, err := a.install(r.Context(), c.Actor, ex.Name)
	if err != nil {
		fail(w, err)
		return
	}
	if in == nil {
		in = &capabilities.Install{Scope: capabilities.ScopeUser, ScopeID: c.Actor, Name: executorInstallPrefix + ex.Name,
			URL: "exec://" + ex.Name, Transport: capabilities.TransportHTTP, Enabled: true, CreatedBy: c.Actor}
	}
	values, err := openSecrets(a.box, in)
	if err != nil {
		writeError(w, http.StatusConflict, "secrets_unreadable", "The stored credentials cannot be read: remove them and set them up again.")
		return
	}
	if values == nil {
		values = map[string]string{}
	}
	changes := map[string]string{}
	for k, v := range body.Env {
		v = strings.TrimSpace(v)
		if !slices.Contains(ex.CredentialEnv, k) {
			writeError(w, http.StatusBadRequest, "unknown_field", ex.Name+" has no credential "+k+".")
			return
		}
		if v != "" && strings.HasSuffix(k, "_URL") {
			if u, err := url.Parse(v); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				writeError(w, http.StatusBadRequest, "invalid_url", k+" must be an http(s) address.")
				return
			}
		}
		changes[k] = v
	}
	if !s.capMerge(w, changes, values, func(k string) bool { return slices.Contains(ex.CredentialEnv, k) }) {
		return
	}
	if a.box == nil && len(values) > 0 {
		writeError(w, http.StatusNotImplemented, "not_configured", "Storing credentials needs a secret key on the server (HARNESS_SECRET_KEY).")
		return
	}
	in.Enabled = true
	in.Secrets, in.SecretKeys = nil, nil
	if len(values) > 0 {
		in.Secrets, in.SecretKeys = sealValues(a.box, values), keysOf(values)
	}
	saved, err := a.caps.PutInstall(r.Context(), *in)
	if err != nil {
		s.capStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ExecutorView{Executor: *ex, Configured: len(saved.SecretKeys) > 0})
}

func (s *server) deleteExecutorCredentials(w http.ResponseWriter, r *http.Request, c Caller) {
	a := s.opt.Executors
	in, err := a.install(r.Context(), c.Actor, r.PathValue("name"))
	if err != nil {
		fail(w, err)
		return
	}
	if in != nil {
		if err := a.caps.DeleteInstall(r.Context(), in.ID); err != nil && !errors.Is(err, capabilities.ErrNotFound) {
			fail(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// sealValues encrypts the credential values of an install.
func sealValues(box *secretBox, values map[string]string) []byte {
	plain, _ := json.Marshal(values)
	return box.seal(plain)
}

// roleExecutorOK checks that a role's executor exists in reach of the role's scope (nil executors service: any
// name is accepted, nothing would run it anyway).
func (s *server) roleExecutorOK(w http.ResponseWriter, r *http.Request, q roles.Query, name string) bool {
	if name == "" || s.opt.Executors == nil {
		return true
	}
	if _, err := s.opt.Executors.Resolve(r.Context(), executors.Query{Login: q.Login, ProjectID: q.ProjectID}, name); err != nil {
		if errors.Is(err, executors.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "invalid_request", "Unknown executor "+name+": define it in the project's executors or use a builtin one.")
		} else {
			fail(w, err)
		}
		return false
	}
	return true
}
