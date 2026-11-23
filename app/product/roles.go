package product

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/core/roles"
	"github.com/7ar-chat/harness/core/store"
)

// Roles: named agent configurations (a prompt, a model, tool limits, skills) of a project or of a user. The five
// builtin roles of the delegate catalogue are always there, read-only; a project (or a user) overrides one by
// creating a role of the same name, and removing that role brings the builtin back. The delegate tools, factory
// tasks and a chat run (the composer's @role) resolve a name personal > project > builtin.
//
//	GET    /api/projects/{pid}/roles                    → [RoleView]    (any member: builtin, project and the caller's own, merged)
//	POST   /api/projects/{pid}/roles {name,prompt,…}    → 201 RoleView  (member)
//	GET    /api/projects/{pid}/roles/{name}             → RoleView
//	PUT    /api/projects/{pid}/roles/{name} {…,version?} → RoleView    (member; a stale version is 409)
//	DELETE /api/projects/{pid}/roles/{name}             → 204           (member)
//	POST   /api/projects/{pid}/roles/{name}/duplicate {name?} → 201 RoleView   (member; copies any role, a builtin included, into the project)
//	GET    /api/projects/{pid}/roles/{name}/history     → [RoleVersion]  (the last 10 versions, newest first)
//	POST   /api/projects/{pid}/roles/{name}/restore {version} → RoleView   (member)
//	POST   /api/projects/{pid}/roles/preview {…}        → RolePreview   (what a run in that role would get; nothing is stored)
//
// The same routes exist under /api/me/roles for a user's personal roles (no project: the caller alone).

// RoleView is a role as the web app lists it.
type RoleView struct {
	roles.Role
	// Overrides says the role replaces a role of a lower layer with the same name (a builtin one).
	Overrides bool `json:"overrides,omitempty"`
	// Editable is whether the caller may change or delete it.
	Editable bool `json:"editable"`
}

// RoleVersion is a past state of a role.
type RoleVersion struct {
	Version   int        `json:"version"`
	Role      roles.Role `json:"role"`
	ChangedBy string     `json:"changedBy,omitempty"`
	At        string     `json:"at"`
}

// RolePreview is what a run in a role gets: the prompt section and the settings it applies.
type RolePreview struct {
	Prompt    string   `json:"prompt"`
	Mode      string   `json:"mode"` // "build" or "plan"
	Model     string   `json:"model,omitempty"`
	Effort    string   `json:"reasoningEffort,omitempty"`
	Skills    []string `json:"skills,omitempty"`
	AskFirst  bool     `json:"askFirst"`
	Notes     []string `json:"notes,omitempty"`
	AllowTool []string `json:"allowTools,omitempty"`
	DenyTool  []string `json:"denyTools,omitempty"`
}

// roleBody is the body of a POST and of a PUT.
type roleBody struct {
	Name            string   `json:"name"`
	Title           string   `json:"title"`
	Description     string   `json:"description"`
	Prompt          string   `json:"prompt"`
	Model           string   `json:"model"`
	ReasoningEffort string   `json:"reasoningEffort"`
	ToolProfile     string   `json:"toolProfile"`
	AllowTools      []string `json:"allowTools"`
	DenyTools       []string `json:"denyTools"`
	Skills          []string `json:"skills"`
	MCP             []string `json:"mcp"`
	PermissionMode  string   `json:"permissionMode"`
	Enabled         *bool    `json:"enabled"`
	// Version is the version the editor started from; 0 skips the check.
	Version int `json:"version"`
}

func (b roleBody) role(scope roles.Scope, scopeID, name string) roles.Role {
	enabled := true
	if b.Enabled != nil {
		enabled = *b.Enabled
	}
	return roles.Role{Scope: scope, ScopeID: scopeID, Name: name, Title: b.Title, Description: b.Description, Prompt: b.Prompt,
		Model: b.Model, ReasoningEffort: b.ReasoningEffort, ToolProfile: b.ToolProfile, AllowTools: b.AllowTools, DenyTools: b.DenyTools,
		Skills: b.Skills, MCP: b.MCP, PermissionMode: b.PermissionMode, Enabled: enabled, Version: b.Version}
}

// ---- the resolver ----

// RoleRegistry resolves role names for the harness (roles.Resolver) and the routes: the roles of a project and of a
// user over the builtin ones. Create it with NewRoleRegistry and give it to harness.Options.Roles.
type RoleRegistry struct {
	team    store.TeamStore
	store   roles.Store
	builtin map[string]roles.Role
}

var _ roles.Resolver = (*RoleRegistry)(nil)

// NewRoleRegistry returns the registry over st. team (nil: none) maps a thread to its project.
func NewRoleRegistry(team store.TeamStore, st roles.Store) *RoleRegistry {
	return &RoleRegistry{team: team, store: st, builtin: builtinRoles()}
}

// builtinRoles is the delegate catalogue as roles.
func builtinRoles() map[string]roles.Role {
	out := map[string]roles.Role{}
	for _, r := range harness.BuiltinRoles() {
		out[r.Name] = r
	}
	return out
}

func (g *RoleRegistry) projectOf(ctx context.Context, q roles.Query) string {
	if q.ProjectID != "" || g.team == nil || q.ThreadID == "" {
		return q.ProjectID
	}
	meta, err := g.team.GetThreadMeta(ctx, q.ThreadID)
	if err != nil {
		return ""
	}
	return meta.ProjectID
}

// layers are the places a name is looked up, most specific first.
func (g *RoleRegistry) layers(ctx context.Context, q roles.Query) [][2]string {
	var out [][2]string
	if q.Login != "" {
		out = append(out, [2]string{string(roles.ScopeUser), q.Login})
	}
	if pid := g.projectOf(ctx, q); pid != "" {
		out = append(out, [2]string{string(roles.ScopeProject), pid})
	}
	return out
}

// Resolve implements roles.Resolver. A role that is turned off ends the search: it does not fall back to a
// lower layer, so disabling the project's "coder" really removes it.
func (g *RoleRegistry) Resolve(ctx context.Context, q roles.Query, name string) (*roles.Role, error) {
	for _, l := range g.layers(ctx, q) {
		r, err := g.store.Get(ctx, roles.Scope(l[0]), l[1], name)
		if errors.Is(err, roles.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !r.Enabled {
			return nil, roles.ErrNotFound
		}
		return r, nil
	}
	if b, ok := g.builtin[name]; ok {
		return &b, nil
	}
	return nil, roles.ErrNotFound
}

// Get implements roles.Resolver.
func (g *RoleRegistry) Get(ctx context.Context, ref roles.Ref) (*roles.Role, error) {
	if ref.Scope == roles.ScopeBuiltin {
		if b, ok := g.builtin[ref.Name]; ok {
			return &b, nil
		}
		return nil, roles.ErrNotFound
	}
	return g.store.Get(ctx, ref.Scope, ref.ScopeID, ref.Name)
}

// Available implements roles.Resolver.
func (g *RoleRegistry) Available(ctx context.Context, q roles.Query) ([]roles.Role, error) {
	all, err := g.merged(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]roles.Role, 0, len(all))
	for _, v := range all {
		if v.Enabled {
			out = append(out, v.Role)
		}
	}
	return out, nil
}

// merged lists every role in reach, one per name, the most specific layer winning; a disabled role still hides the
// lower one (it is listed, flagged disabled).
func (g *RoleRegistry) merged(ctx context.Context, q roles.Query) ([]RoleView, error) {
	byName := map[string]RoleView{}
	for name, b := range g.builtin {
		byName[name] = RoleView{Role: b}
	}
	layers := g.layers(ctx, q)
	for i := len(layers) - 1; i >= 0; i-- { // project first, then personal over it
		list, err := g.store.List(ctx, roles.Scope(layers[i][0]), layers[i][1])
		if err != nil {
			return nil, err
		}
		for _, r := range list {
			_, over := byName[r.Name]
			byName[r.Name] = RoleView{Role: r, Overrides: over}
		}
	}
	out := make([]RoleView, 0, len(byName))
	for _, v := range byName {
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b RoleView) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// ---- routes ----

// roleScope is whose roles a request acts on.
type roleScope struct {
	scope   roles.Scope
	id      string
	caller  Caller
	project bool
	query   roles.Query
	write   bool // the caller may change this scope's roles
}

func (s *server) routeRoles() {
	if s.opt.Roles == nil {
		return
	}
	s.roles = NewRoleRegistry(s.team, s.opt.Roles)
	for _, p := range []struct {
		prefix  string
		project bool
	}{{"/projects/{pid}", true}, {"/me", false}} {
		s.Route("GET "+p.prefix+"/roles", s.roleRoute(p.project, false, s.listRoles))
		s.Route("POST "+p.prefix+"/roles", s.roleRoute(p.project, true, s.createRole))
		s.Route("POST "+p.prefix+"/roles/preview", s.roleRoute(p.project, false, s.previewRole))
		s.Route("GET "+p.prefix+"/roles/{name}", s.roleRoute(p.project, false, s.getRole))
		s.Route("PUT "+p.prefix+"/roles/{name}", s.roleRoute(p.project, true, s.putRole))
		s.Route("DELETE "+p.prefix+"/roles/{name}", s.roleRoute(p.project, true, s.deleteRole))
		s.Route("POST "+p.prefix+"/roles/{name}/duplicate", s.roleRoute(p.project, true, s.duplicateRole))
		s.Route("GET "+p.prefix+"/roles/{name}/history", s.roleRoute(p.project, false, s.roleHistory))
		s.Route("POST "+p.prefix+"/roles/{name}/restore", s.roleRoute(p.project, true, s.restoreRole))
	}
}

// roleRoute resolves the scope of the route: the project of the path (a member to write, any member to read) or
// the caller's own roles.
func (s *server) roleRoute(project, write bool, h func(http.ResponseWriter, *http.Request, roleScope)) handler {
	return func(w http.ResponseWriter, r *http.Request, c Caller) {
		if !project {
			h(w, r, roleScope{scope: roles.ScopeUser, id: c.Actor, caller: c, query: roles.Query{Login: c.Actor}, write: true})
			return
		}
		need := store.RoleViewer
		if write {
			need = store.RoleMember
		}
		p, role, ok := s.project(w, r, c, need)
		if !ok {
			return
		}
		h(w, r, roleScope{scope: roles.ScopeProject, id: p.ID, caller: inProject(c, p.ID), project: true,
			query: roles.Query{Login: c.Actor, ProjectID: p.ID}, write: store.RoleAtLeast(role, store.RoleMember)})
	}
}

func (s *server) roleAudit(sc roleScope, action, name string, detail map[string]any) {
	if !sc.project {
		return
	}
	d := map[string]any{"role": name}
	for k, v := range detail {
		d[k] = v
	}
	s.note(sc.caller, "", action, d, FeedEvent{Type: "project.changed"})
}

func roleFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, roles.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "No such role.")
	case errors.Is(err, roles.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "A role with that name already exists.")
	case errors.Is(err, roles.ErrStale):
		writeError(w, http.StatusConflict, "stale", "The role was changed by someone else: reload it and apply your change again.")
	case errors.Is(err, roles.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", strings.TrimPrefix(err.Error(), roles.ErrInvalid.Error()+": "))
	default:
		fail(w, err)
	}
}

// view marks what the caller may do with r.
func (sc roleScope) view(v RoleView) RoleView {
	v.Editable = sc.write && v.Scope == sc.scope && v.ScopeID == sc.id
	return v
}

func (s *server) listRoles(w http.ResponseWriter, r *http.Request, sc roleScope) {
	all, err := s.roles.merged(r.Context(), sc.query)
	if err != nil {
		roleFail(w, err)
		return
	}
	for i := range all {
		all[i] = sc.view(all[i])
	}
	writeJSON(w, http.StatusOK, all)
}

func (s *server) getRole(w http.ResponseWriter, r *http.Request, sc roleScope) {
	all, err := s.roles.merged(r.Context(), sc.query)
	if err != nil {
		roleFail(w, err)
		return
	}
	for _, v := range all {
		if v.Name == r.PathValue("name") {
			writeJSON(w, http.StatusOK, sc.view(v))
			return
		}
	}
	roleFail(w, roles.ErrNotFound)
}

func (s *server) createRole(w http.ResponseWriter, r *http.Request, sc roleScope) {
	var body roleBody
	if !decode(w, r, &body) {
		return
	}
	name := strings.ToLower(strings.TrimSpace(body.Name))
	_, builtin := s.roles.builtin[name]
	created, err := s.opt.Roles.Create(r.Context(), body.role(sc.scope, sc.id, name), sc.caller.Actor)
	if err != nil {
		roleFail(w, err)
		return
	}
	s.roleAudit(sc, "role.create", name, nil)
	writeJSON(w, http.StatusCreated, sc.view(RoleView{Role: *created, Overrides: builtin}))
}

func (s *server) putRole(w http.ResponseWriter, r *http.Request, sc roleScope) {
	name := r.PathValue("name")
	var body roleBody
	if !decode(w, r, &body) {
		return
	}
	cur, err := s.opt.Roles.Get(r.Context(), sc.scope, sc.id, name)
	if err != nil {
		roleFail(w, err)
		return
	}
	if body.Enabled == nil {
		body.Enabled = &cur.Enabled
	}
	updated, err := s.opt.Roles.Update(r.Context(), body.role(sc.scope, sc.id, name), sc.caller.Actor)
	if err != nil {
		roleFail(w, err)
		return
	}
	s.roleAudit(sc, "role.update", name, map[string]any{"version": updated.Version})
	_, builtin := s.roles.builtin[name]
	writeJSON(w, http.StatusOK, sc.view(RoleView{Role: *updated, Overrides: builtin}))
}

func (s *server) deleteRole(w http.ResponseWriter, r *http.Request, sc roleScope) {
	name := r.PathValue("name")
	if err := s.opt.Roles.Delete(r.Context(), sc.scope, sc.id, name); err != nil {
		roleFail(w, err)
		return
	}
	s.roleAudit(sc, "role.delete", name, nil)
	w.WriteHeader(http.StatusNoContent)
}

// duplicateRole copies a role in reach into the scope. A builtin keeps its name (the copy overrides it); another
// role gets "-copy" appended unless the body names the copy.
func (s *server) duplicateRole(w http.ResponseWriter, r *http.Request, sc roleScope) {
	var body struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	src, err := s.roles.Resolve(r.Context(), sc.query, r.PathValue("name"))
	if errors.Is(err, roles.ErrNotFound) { // a disabled role can still be copied
		var all []RoleView
		if all, err = s.roles.merged(r.Context(), sc.query); err == nil {
			err = roles.ErrNotFound
			for _, v := range all {
				if v.Name == r.PathValue("name") {
					c := v.Role
					src, err = &c, nil
				}
			}
		}
	}
	if err != nil {
		roleFail(w, err)
		return
	}
	name := strings.ToLower(strings.TrimSpace(body.Name))
	if name == "" {
		name = src.Name
		if !src.Builtin {
			name = strings.TrimRight(src.Name[:min(len(src.Name), 26)], "-") + "-copy"
		}
	}
	cp := *src
	cp.Scope, cp.ScopeID, cp.Name, cp.Version, cp.Builtin, cp.Enabled = sc.scope, sc.id, name, 0, false, true
	if cp.Title == "" || src.Name == name {
		// keep the title
	} else {
		cp.Title += " (copy)"
	}
	created, err := s.opt.Roles.Create(r.Context(), cp, sc.caller.Actor)
	if err != nil {
		roleFail(w, err)
		return
	}
	s.roleAudit(sc, "role.duplicate", name, map[string]any{"from": src.Name})
	_, builtin := s.roles.builtin[name]
	writeJSON(w, http.StatusCreated, sc.view(RoleView{Role: *created, Overrides: builtin}))
}

func (s *server) roleHistory(w http.ResponseWriter, r *http.Request, sc roleScope) {
	h, err := s.opt.Roles.History(r.Context(), sc.scope, sc.id, r.PathValue("name"))
	if err != nil {
		roleFail(w, err)
		return
	}
	out := make([]RoleVersion, 0, len(h))
	for _, v := range h {
		out = append(out, RoleVersion{Version: v.Version, Role: v.Role, ChangedBy: v.ChangedBy, At: v.At.Format("2006-01-02T15:04:05Z07:00")})
	}
	writeJSON(w, http.StatusOK, out)
}

// restoreRole writes the content of an older version as a new version.
func (s *server) restoreRole(w http.ResponseWriter, r *http.Request, sc roleScope) {
	var body struct {
		Version int `json:"version"`
	}
	if !decode(w, r, &body) {
		return
	}
	name := r.PathValue("name")
	h, err := s.opt.Roles.History(r.Context(), sc.scope, sc.id, name)
	if err != nil {
		roleFail(w, err)
		return
	}
	i := slices.IndexFunc(h, func(v roles.Version) bool { return v.Version == body.Version })
	if i < 0 {
		roleFail(w, roles.ErrNotFound)
		return
	}
	old := h[i].Role
	old.Version = 0
	updated, err := s.opt.Roles.Update(r.Context(), old, sc.caller.Actor)
	if err != nil {
		roleFail(w, err)
		return
	}
	s.roleAudit(sc, "role.restore", name, map[string]any{"from": body.Version, "version": updated.Version})
	_, builtin := s.roles.builtin[name]
	writeJSON(w, http.StatusOK, sc.view(RoleView{Role: *updated, Overrides: builtin}))
}

// previewRole renders what a run in the posted role would get, without storing anything.
func (s *server) previewRole(w http.ResponseWriter, r *http.Request, sc roleScope) {
	var body roleBody
	if !decode(w, r, &body) {
		return
	}
	name := strings.ToLower(strings.TrimSpace(body.Name))
	if name == "" {
		name = "preview"
	}
	role := body.role(sc.scope, sc.id, name)
	if err := role.Validate(); err != nil {
		roleFail(w, err)
		return
	}
	p := RolePreview{Prompt: harness.RolePrompt(role), Mode: "build", Model: role.Model, Effort: role.ReasoningEffort, Skills: role.Skills,
		AskFirst: role.PermissionMode == roles.PermAsk, AllowTool: role.AllowTools, DenyTool: role.DenyTools}
	if role.ReadOnly() {
		p.Mode = "plan"
		p.Notes = append(p.Notes, "Runs in plan mode: every tool that could change something is removed.")
	}
	if len(role.AllowTools) > 0 {
		p.Notes = append(p.Notes, "Only the allowed tools are offered (plus ask_user, update_plan and load_skill).")
	}
	if len(role.MCP) > 0 {
		p.Notes = append(p.Notes, "Only the tools of these MCP servers are offered: "+strings.Join(role.MCP, ", ")+".")
	}
	if p.AskFirst {
		p.Notes = append(p.Notes, "Risky tool calls wait for a person's approval.")
	}
	writeJSON(w, http.StatusOK, p)
}
