package product

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"context"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/recipes"
	"github.com/7ar-chat/harness/core/store"
)

// Recipes: a gallery of ready-made schedules, tasks and triggers with {{placeholders}} that a person fills in and
// starts in two clicks, and that a team can add to. The builtin gallery is code (core/recipes.Builtins); the recipes
// of the instance, of a project and of a person are kept in Options.Recipes.
//
//	GET    /api/recipes?category=&kind=&scope=&q=&project=  → [recipeView]   what the caller may see
//	GET    /api/recipes/{id}                                  → recipeView
//	POST   /api/recipes {name,summary,category,kind,body,params,suggest,scope,projectId} → 201 recipeView
//	PUT    /api/recipes/{id} {the same, without scope; version for a stale check}      → recipeView
//	DELETE /api/recipes/{id}                                  → 204
//	GET    /api/recipes/{id}/versions                         → [Version]   past versions, newest first
//	GET    /api/recipes/{id}/export                           → the portable JSON file
//	POST   /api/recipes/import?scope=&project=  (the file)    → 201 recipeView   strict: unknown fields refused, 128 KiB
//	POST   /api/projects/{pid}/recipes/from {from:"schedule"|"task"|"trigger", id, name, summary, category, scope, params:[{...,text}]}
//	       → 201 recipeView   "Share as recipe": text the author marked becomes a {{parameter}}
//	POST   /api/projects/{pid}/recipes/{id}/apply {params, dryRun, advanced:{role,policy,maxUsd,maxTokens,timeoutMinutes,baseBranch,model}}
//	       → 201 {kind, summary, created}  or, with dryRun, 200 {dryRun, kind, summary, rendered}
//
// Who may write: an administrator (HARNESS_ADMINS) for the instance, a project owner for a project, anyone for their
// own personal recipes. Applying creates the schedule, task or trigger through the very handlers of those features,
// so every one of their checks (project role, repository, budget, model) applies to the person who applies it, and
// the result belongs to that person.

// recipeView is a recipe for the gallery.
type recipeView struct {
	recipes.Recipe
	// Creates says in one line what applying it with its defaults makes ("A schedule: every day at 02:00").
	Creates string `json:"creates"`
	// Cost is the budget hint ("Up to about $1.00 per run").
	Cost string `json:"cost,omitempty"`
	// CanEdit is whether the caller may change or delete it.
	CanEdit bool `json:"canEdit"`
	// ProjectName names the project of a project recipe.
	ProjectName string `json:"projectName,omitempty"`
}

type recipeBody struct {
	Name      string           `json:"name"`
	Summary   string           `json:"summary"`
	Category  recipes.Category `json:"category"`
	Kind      recipes.Kind     `json:"kind"`
	Body      recipes.Body     `json:"body"`
	Params    []recipes.Param  `json:"params"`
	Suggest   recipes.Suggest  `json:"suggest"`
	Scope     recipes.Scope    `json:"scope"`
	ProjectID string           `json:"projectId"`
	Version   int              `json:"version"`
}

// recipeFile is the portable form of a recipe: what is worth sending to someone else, nothing of who or where.
type recipeFile struct {
	Format  string         `json:"format"`
	Version int            `json:"version"`
	Recipe  portableRecipe `json:"recipe"`
}

type portableRecipe struct {
	Name     string           `json:"name"`
	Summary  string           `json:"summary"`
	Category recipes.Category `json:"category"`
	Kind     recipes.Kind     `json:"kind"`
	Body     recipes.Body     `json:"body"`
	Params   []recipes.Param  `json:"params"`
	Suggest  recipes.Suggest  `json:"suggest"`
}

const recipeFileFormat = "7ar-recipe"

func (s *server) routeRecipes() {
	if s.opt.Recipes == nil || s.team == nil {
		return
	}
	s.Route("GET /recipes", s.listRecipes)
	s.Route("POST /recipes", s.createRecipe)
	s.Route("POST /recipes/import", s.importRecipe)
	s.Route("POST /recipes/check", s.checkRecipe)
	s.Route("GET /recipes/{id}", s.getRecipe)
	s.Route("PUT /recipes/{id}", s.updateRecipe)
	s.Route("DELETE /recipes/{id}", s.deleteRecipe)
	s.Route("GET /recipes/{id}/versions", s.recipeVersions)
	s.Route("GET /recipes/{id}/export", s.exportRecipe)
	s.Route("POST /projects/{pid}/recipes/from", s.shareAsRecipe)
	s.Route("POST /projects/{pid}/recipes/{id}/apply", s.applyRecipe)
}

// dropProjectRecipes removes the recipes of a deleted project.
func (s *server) dropProjectRecipes(ctx context.Context, projectID string) {
	if s.opt.Recipes == nil {
		return
	}
	if err := s.opt.Recipes.DeleteScope(ctx, recipes.ScopeProject, projectID); err != nil {
		slog.Warn("recipes: deleting the recipes of a deleted project failed", "project", projectID, "error", err)
	}
}

func recipeFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, recipes.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_recipe", strings.TrimPrefix(err.Error(), "recipes: invalid: "))
	case errors.Is(err, recipes.ErrStale):
		writeError(w, http.StatusConflict, "stale", "Someone changed this recipe meanwhile. Reload it and try again.")
	case errors.Is(err, recipes.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "Not found.")
	default:
		fail(w, err)
	}
}

// who is what the caller is for the recipe routes.
type recipeWho struct {
	actor    string
	admin    bool
	projects map[string]store.ProjectRole
}

func (s *server) recipeWho(r *http.Request, c Caller) (recipeWho, bool, error) {
	u, err := s.currentUser(r, c)
	if err != nil {
		return recipeWho{}, false, err
	}
	list, err := s.team.ListProjects(r.Context(), c.Actor)
	if err != nil {
		return recipeWho{}, false, err
	}
	w := recipeWho{actor: c.Actor, admin: s.isAdmin(u.Login, u.Email), projects: map[string]store.ProjectRole{}}
	for _, p := range list {
		w.projects[p.ID] = p
	}
	return w, true, nil
}

func (w recipeWho) sees(r recipes.Recipe) bool {
	switch r.Scope {
	case recipes.ScopeBuiltin, recipes.ScopeInstance:
		return true
	case recipes.ScopePersonal:
		return r.ScopeID == w.actor
	case recipes.ScopeProject:
		_, ok := w.projects[r.ScopeID]
		return ok
	}
	return false
}

func (w recipeWho) edits(r recipes.Recipe) bool {
	switch r.Scope {
	case recipes.ScopeInstance:
		return w.admin
	case recipes.ScopePersonal:
		return r.ScopeID == w.actor
	case recipes.ScopeProject:
		p, ok := w.projects[r.ScopeID]
		return ok && store.RoleAtLeast(p.Role, store.RoleOwner)
	}
	return false
}

// recipeByID finds a builtin or stored recipe the caller may see (404 otherwise, so ids do not leak).
func (s *server) recipeByID(w http.ResponseWriter, r *http.Request, who recipeWho) (recipes.Recipe, bool) {
	id := r.PathValue("id")
	if b, ok := recipes.BuiltinByID(id); ok {
		return b, true
	}
	rec, err := s.opt.Recipes.Get(r.Context(), id)
	if err != nil || !who.sees(*rec) {
		if err != nil && !errors.Is(err, recipes.ErrNotFound) {
			fail(w, err)
		} else {
			writeError(w, http.StatusNotFound, "not_found", "Not found.")
		}
		return recipes.Recipe{}, false
	}
	return *rec, true
}

func (s *server) viewOfRecipe(r recipes.Recipe, who recipeWho, uses map[string]int) recipeView {
	r.Uses = uses[r.ID]
	v := recipeView{Recipe: r, Creates: recipeCreates(r), Cost: recipeCost(r), CanEdit: who.edits(r)}
	if p, ok := who.projects[r.ScopeID]; ok && r.Scope == recipes.ScopeProject {
		v.ProjectName = p.Name
	}
	if v.Params == nil {
		v.Params = []recipes.Param{}
	}
	return v
}

func (s *server) listRecipes(w http.ResponseWriter, r *http.Request, c Caller) {
	who, ok, err := s.recipeWho(r, c)
	if err != nil || !ok {
		fail(w, err)
		return
	}
	stored, err := s.opt.Recipes.List(r.Context(), "", "")
	if err != nil {
		fail(w, err)
		return
	}
	uses, err := s.opt.Recipes.Uses(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	q := r.URL.Query()
	cat, kind, scope, pid := q.Get("category"), q.Get("kind"), q.Get("scope"), q.Get("project")
	text := strings.ToLower(strings.TrimSpace(q.Get("q")))
	all := recipes.Builtins()
	all = append(all, stored...)
	out := make([]recipeView, 0, len(all))
	for _, rec := range all {
		switch {
		case !who.sees(rec),
			cat != "" && string(rec.Category) != cat,
			kind != "" && string(rec.Kind) != kind,
			scope != "" && string(rec.Scope) != scope,
			pid != "" && rec.Scope == recipes.ScopeProject && rec.ScopeID != pid,
			text != "" && !strings.Contains(strings.ToLower(rec.Name+" "+rec.Summary+" "+string(rec.Category)+" "+string(rec.Kind)), text):
			continue
		}
		out = append(out, s.viewOfRecipe(rec, who, uses))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) getRecipe(w http.ResponseWriter, r *http.Request, c Caller) {
	who, ok, err := s.recipeWho(r, c)
	if err != nil || !ok {
		fail(w, err)
		return
	}
	rec, ok := s.recipeByID(w, r, who)
	if !ok {
		return
	}
	uses, _ := s.opt.Recipes.Uses(r.Context())
	writeJSON(w, http.StatusOK, s.viewOfRecipe(rec, who, uses))
}

// writeRecipeScope resolves where a new recipe goes and checks the caller may write there.
func (s *server) writeRecipeScope(w http.ResponseWriter, who recipeWho, scope recipes.Scope, projectID string) (scopeID string, ok bool) {
	switch scope {
	case recipes.ScopePersonal:
		return who.actor, true
	case recipes.ScopeInstance:
		if !who.admin {
			writeError(w, http.StatusForbidden, "forbidden", "Only administrators can share a recipe with the whole team on this server.")
			return "", false
		}
		return "", true
	case recipes.ScopeProject:
		p, member := who.projects[projectID]
		if !member {
			writeError(w, http.StatusNotFound, "not_found", "Not found.")
			return "", false
		}
		if !store.RoleAtLeast(p.Role, store.RoleOwner) {
			writeError(w, http.StatusForbidden, "forbidden", "Only a project owner can share a recipe with the project.")
			return "", false
		}
		return projectID, true
	}
	writeError(w, http.StatusBadRequest, "invalid_recipe", "scope must be personal, project or instance.")
	return "", false
}

// storeRecipe creates the recipe at the scope after the permission and count checks and writes the answer.
func (s *server) storeRecipe(w http.ResponseWriter, r *http.Request, c Caller, who recipeWho, rec recipes.Recipe) {
	var ok bool
	if rec.ScopeID, ok = s.writeRecipeScope(w, who, rec.Scope, rec.ScopeID); !ok {
		return
	}
	existing, err := s.opt.Recipes.List(r.Context(), rec.Scope, rec.ScopeID)
	if err != nil {
		fail(w, err)
		return
	}
	if len(existing) >= recipes.MaxPerScope {
		writeError(w, http.StatusBadRequest, "invalid_recipe", fmt.Sprintf("At most %d recipes here. Delete one you no longer use.", recipes.MaxPerScope))
		return
	}
	out, err := s.opt.Recipes.Create(r.Context(), rec, c.Actor)
	if err != nil {
		recipeFail(w, err)
		return
	}
	pc := c
	if out.Scope == recipes.ScopeProject {
		pc = inProject(c, out.ScopeID)
	}
	s.note(pc, "", "recipe.create", map[string]any{"recipe": out.ID, "name": out.Name, "scope": out.Scope}, FeedEvent{Type: "project.changed"})
	writeJSON(w, http.StatusCreated, s.viewOfRecipe(*out, who, nil))
}

func (b recipeBody) recipe() recipes.Recipe {
	return recipes.Recipe{Name: b.Name, Summary: b.Summary, Category: b.Category, Kind: b.Kind, Body: b.Body, Params: b.Params,
		Suggest: b.Suggest, Scope: b.Scope, ScopeID: b.ProjectID, Version: b.Version}
}

func (s *server) createRecipe(w http.ResponseWriter, r *http.Request, c Caller) {
	who, ok, err := s.recipeWho(r, c)
	if err != nil || !ok {
		fail(w, err)
		return
	}
	var b recipeBody
	if !decode(w, r, &b) {
		return
	}
	rec := b.recipe()
	if err := rec.Validate(); err != nil {
		recipeFail(w, err)
		return
	}
	s.storeRecipe(w, r, c, who, rec)
}

// storedRecipe loads the recipe of the path for a change: it must be one the caller may edit.
func (s *server) storedRecipe(w http.ResponseWriter, r *http.Request, who recipeWho) (*recipes.Recipe, bool) {
	id := r.PathValue("id")
	if _, builtin := recipes.BuiltinByID(id); builtin {
		writeError(w, http.StatusForbidden, "forbidden", "The ready-made recipes cannot be changed. Copy one to make your own.")
		return nil, false
	}
	rec, err := s.opt.Recipes.Get(r.Context(), id)
	if err != nil || !who.sees(*rec) {
		recipeFail(w, recipes.ErrNotFound)
		return nil, false
	}
	if !who.edits(*rec) {
		writeError(w, http.StatusForbidden, "forbidden", "You cannot change this recipe: ask its owner.")
		return nil, false
	}
	return rec, true
}

func (s *server) updateRecipe(w http.ResponseWriter, r *http.Request, c Caller) {
	who, ok, err := s.recipeWho(r, c)
	if err != nil || !ok {
		fail(w, err)
		return
	}
	cur, ok := s.storedRecipe(w, r, who)
	if !ok {
		return
	}
	var b recipeBody
	if !decode(w, r, &b) {
		return
	}
	next := b.recipe()
	next.ID, next.Scope, next.ScopeID = cur.ID, cur.Scope, cur.ScopeID
	if err := next.Validate(); err != nil {
		recipeFail(w, err)
		return
	}
	out, err := s.opt.Recipes.Update(r.Context(), next, c.Actor)
	if err != nil {
		recipeFail(w, err)
		return
	}
	s.note(recipeCaller(c, out), "", "recipe.update", map[string]any{"recipe": out.ID, "name": out.Name, "version": out.Version}, FeedEvent{Type: "project.changed"})
	uses, _ := s.opt.Recipes.Uses(r.Context())
	writeJSON(w, http.StatusOK, s.viewOfRecipe(*out, who, uses))
}

func recipeCaller(c Caller, r *recipes.Recipe) Caller {
	if r.Scope == recipes.ScopeProject {
		return inProject(c, r.ScopeID)
	}
	return c
}

func (s *server) deleteRecipe(w http.ResponseWriter, r *http.Request, c Caller) {
	who, ok, err := s.recipeWho(r, c)
	if err != nil || !ok {
		fail(w, err)
		return
	}
	cur, ok := s.storedRecipe(w, r, who)
	if !ok {
		return
	}
	if err := s.opt.Recipes.Delete(r.Context(), cur.ID); err != nil {
		recipeFail(w, err)
		return
	}
	s.note(recipeCaller(c, cur), "", "recipe.delete", map[string]any{"recipe": cur.ID, "name": cur.Name}, FeedEvent{Type: "project.changed"})
	w.WriteHeader(http.StatusNoContent)
}

// recipeCheckBody is a draft to check: the recipe as the editor holds it and the values to try it with.
type recipeCheckBody struct {
	Recipe recipeBody        `json:"recipe"`
	Params map[string]string `json:"params"`
}

// recipeCheck answers a check: Error is the server's own words when the draft cannot be saved (or tried), Preview is what
// an apply with these values would make (the same render as apply with dryRun); nothing is stored.
type recipeCheck struct {
	OK      bool          `json:"ok"`
	Error   string        `json:"error,omitempty"`
	Preview *recipeDryRun `json:"preview,omitempty"`
}

// checkRecipe validates a draft recipe and renders it with the given values, so the editor can show messages and a
// preview while the person types. Any member of the team may call it.
func (s *server) checkRecipe(w http.ResponseWriter, r *http.Request, c Caller) {
	var b recipeCheckBody
	if !decode(w, r, &b) {
		return
	}
	rec := b.Recipe.recipe()
	if err := rec.Validate(); err != nil {
		writeJSON(w, http.StatusOK, recipeCheck{Error: strings.TrimPrefix(err.Error(), "recipes: invalid: ")})
		return
	}
	rd, err := rec.Apply(b.Params)
	if err != nil {
		writeJSON(w, http.StatusOK, recipeCheck{OK: true, Error: strings.TrimPrefix(err.Error(), "recipes: invalid: ")})
		return
	}
	writeJSON(w, http.StatusOK, recipeCheck{OK: true, Preview: &recipeDryRun{DryRun: true, Kind: rd.Kind, Summary: renderedCreates(*rd), Rendered: *rd}})
}

func (s *server) recipeVersions(w http.ResponseWriter, r *http.Request, c Caller) {
	who, ok, err := s.recipeWho(r, c)
	if err != nil || !ok {
		fail(w, err)
		return
	}
	rec, ok := s.recipeByID(w, r, who)
	if !ok {
		return
	}
	hist := []recipes.Version{}
	if rec.Scope != recipes.ScopeBuiltin {
		if hist, err = s.opt.Recipes.History(r.Context(), rec.ID); err != nil {
			recipeFail(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, recipeVersions{Current: rec.Version, History: hist})
}

func (s *server) exportRecipe(w http.ResponseWriter, r *http.Request, c Caller) {
	who, ok, err := s.recipeWho(r, c)
	if err != nil || !ok {
		fail(w, err)
		return
	}
	rec, ok := s.recipeByID(w, r, who)
	if !ok {
		return
	}
	if rec.Params == nil {
		rec.Params = []recipes.Param{}
	}
	f := recipeFile{Format: recipeFileFormat, Version: 1, Recipe: portableRecipe{Name: rec.Name, Summary: rec.Summary, Category: rec.Category,
		Kind: rec.Kind, Body: rec.Body, Params: rec.Params, Suggest: rec.Suggest}}
	w.Header().Set("Content-Disposition", `attachment; filename="`+slugify(rec.Name)+`.recipe.json"`)
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(f)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		return "recipe"
	}
	if len(s) > 50 {
		s = strings.Trim(s[:50], "-")
	}
	return s
}

// parseRecipeFile reads a portable file strictly: the format and version are checked, unknown fields are refused,
// nothing may follow the document, and the recipe must be valid.
func parseRecipeFile(b []byte) (recipes.Recipe, error) {
	var f recipeFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return recipes.Recipe{}, fmt.Errorf("this is not a recipe file: %v", err)
	}
	if dec.More() {
		return recipes.Recipe{}, errors.New("this is not a recipe file: there is more after the recipe")
	}
	if f.Format != recipeFileFormat || f.Version != 1 {
		return recipes.Recipe{}, errors.New("this is not a recipe file (expected format 7ar-recipe, version 1)")
	}
	p := f.Recipe
	rec := recipes.Recipe{Name: p.Name, Summary: p.Summary, Category: p.Category, Kind: p.Kind, Body: p.Body, Params: p.Params, Suggest: p.Suggest}
	if err := rec.Validate(); err != nil {
		return recipes.Recipe{}, errors.New(strings.TrimPrefix(err.Error(), "recipes: invalid: "))
	}
	return rec, nil
}

func (s *server) importRecipe(w http.ResponseWriter, r *http.Request, c Caller) {
	who, ok, err := s.recipeWho(r, c)
	if err != nil || !ok {
		fail(w, err)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, recipes.MaxImportSize))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("A recipe file is at most %d KB.", recipes.MaxImportSize>>10))
		return
	}
	rec, err := parseRecipeFile(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_recipe", err.Error())
		return
	}
	rec.Scope, rec.ScopeID = recipes.Scope(r.URL.Query().Get("scope")), r.URL.Query().Get("project")
	if rec.Scope == "" {
		rec.Scope = recipes.ScopePersonal
	}
	s.storeRecipe(w, r, c, who, rec)
}

// shareParam is a parameter of a recipe being made from an existing schedule or task: Text is the part of the
// definition the author marked, which becomes {{key}} (and the parameter's default).
type shareParam struct {
	recipes.Param
	Text string `json:"text"`
}

// shareBody is the request of "Share as recipe".
type shareBody struct {
	From     string           `json:"from"`
	ID       string           `json:"id"`
	Name     string           `json:"name"`
	Summary  string           `json:"summary"`
	Category recipes.Category `json:"category"`
	Scope    recipes.Scope    `json:"scope"`
	Params   []shareParam     `json:"params"`
}

func (s *server) shareAsRecipe(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	who, ok, err := s.recipeWho(r, c)
	if err != nil || !ok {
		fail(w, err)
		return
	}
	var b shareBody
	if !decode(w, r, &b) {
		return
	}
	rec := recipes.Recipe{Name: b.Name, Summary: b.Summary, Category: b.Category, Scope: b.Scope, ScopeID: p.ID}
	if rec.Category == "" {
		rec.Category = recipes.CatMaintenance
	}
	ctx := r.Context()
	switch b.From {
	case "schedule":
		if s.sched == nil {
			writeError(w, http.StatusBadRequest, "not_available", "Schedules are not turned on on this server.")
			return
		}
		sc, err := s.opt.Schedules.Store.Get(ctx, b.ID)
		if err != nil || sc.ProjectID != p.ID {
			writeError(w, http.StatusNotFound, "not_found", "Not found.")
			return
		}
		rec.Kind = recipes.KindSchedule
		rec.Body = recipes.Body{Name: sc.Name, Prompt: sc.Prompt, Cron: sc.Cron, Timezone: sc.Timezone}
		rec.Suggest = recipes.Suggest{Policy: sc.Policy, TimeoutMinutes: sc.TimeoutMinutes}
		if rec.Name == "" {
			rec.Name = sc.Name
		}
	case "task":
		if s.factory() == nil {
			writeError(w, http.StatusBadRequest, "not_available", "Tasks are not turned on on this server.")
			return
		}
		t, err := s.factory().Store.GetTask(ctx, b.ID)
		if err != nil || t.ProjectID != p.ID {
			writeError(w, http.StatusNotFound, "not_found", "Not found.")
			return
		}
		rec.Kind = recipes.KindTask
		rec.Body = recipes.Body{Title: t.Title, Prompt: t.Spec}
		rec.Suggest = recipes.Suggest{Role: t.Role, Policy: t.Policy, MaxUSD: t.Budget.MaxUSD, MaxTokens: t.Budget.MaxTokens}
		if rec.Name == "" {
			rec.Name = t.Title
		}
	case "trigger":
		if s.factory() == nil {
			writeError(w, http.StatusBadRequest, "not_available", "Triggers are not turned on on this server.")
			return
		}
		t, err := s.factory().Store.GetTrigger(ctx, b.ID)
		if err != nil || t.ProjectID != p.ID {
			writeError(w, http.StatusNotFound, "not_found", "Not found.")
			return
		}
		rec.Kind = recipes.KindTrigger
		rec.Body = recipes.Body{Name: t.Name, Title: t.Template.Title, Prompt: t.Template.Spec, Cron: t.Schedule, Event: t.Event}
		for k, v := range t.Match {
			if k != "repo" { // the server sets it from the project
				if rec.Body.Match == nil {
					rec.Body.Match = map[string]string{}
				}
				rec.Body.Match[k] = v
			}
		}
		rec.Suggest = recipes.Suggest{Role: t.Template.Role, Policy: t.Template.Policy, MaxUSD: t.Template.Budget.MaxUSD, MaxTokens: t.Template.Budget.MaxTokens}
		if rec.Name == "" {
			rec.Name = t.Name
		}
	default:
		writeError(w, http.StatusBadRequest, "invalid_recipe", "from must be schedule, task or trigger.")
		return
	}
	// The marked text becomes a parameter whose default is that text: the recipe works as it was.
	for _, sp := range b.Params {
		param := sp.Param
		if sp.Text != "" {
			n := 0
			for _, f := range []*string{&rec.Body.Name, &rec.Body.Title, &rec.Body.Prompt, &rec.Body.Cron, &rec.Body.Timezone} {
				n += strings.Count(*f, sp.Text)
				*f = strings.ReplaceAll(*f, sp.Text, "{{"+param.Key+"}}")
			}
			if n == 0 {
				writeError(w, http.StatusBadRequest, "invalid_recipe", fmt.Sprintf("The text %q was not found in the %s, so it cannot become a parameter.", sp.Text, b.From))
				return
			}
			if param.Default == "" {
				param.Default = sp.Text
			}
		}
		rec.Params = append(rec.Params, param)
	}
	if err := rec.Validate(); err != nil {
		recipeFail(w, err)
		return
	}
	s.storeRecipe(w, r, c, who, rec)
}

// ---- apply ----

type applyBody struct {
	Params   map[string]string `json:"params"`
	DryRun   bool              `json:"dryRun"`
	Advanced struct {
		Role           string  `json:"role"`
		Policy         string  `json:"policy"`
		MaxUSD         float64 `json:"maxUsd"`
		MaxTokens      int64   `json:"maxTokens"`
		TimeoutMinutes int     `json:"timeoutMinutes"`
		BaseBranch     string  `json:"baseBranch"`
		Model          string  `json:"model"`
	} `json:"advanced"`
}

// recipeVersions answers the history route.
type recipeVersions struct {
	Current int               `json:"current"`
	History []recipes.Version `json:"history"`
}

// recipeDryRun answers an apply with dryRun: what would be created, nothing is.
type recipeDryRun struct {
	DryRun   bool             `json:"dryRun"`
	Kind     recipes.Kind     `json:"kind"`
	Summary  string           `json:"summary"`
	Rendered recipes.Rendered `json:"rendered"`
}

// recipeApplied answers an apply: Created is the schedule, task or trigger exactly as its own route returns it.
type recipeApplied struct {
	Kind    recipes.Kind `json:"kind"`
	Summary string       `json:"summary"`
	Recipe  string       `json:"recipe"`
	Created any          `json:"created"`
}

// capture records what a handler writes, so apply can wrap the answer of the feature's own route.
type capture struct {
	h      http.Header
	status int
	buf    bytes.Buffer
}

func (c *capture) Header() http.Header         { return c.h }
func (c *capture) WriteHeader(status int)      { c.status = status }
func (c *capture) Write(p []byte) (int, error) { return c.buf.Write(p) }

// forward runs one of the feature handlers as the caller, as if the person had posted the body to its route.
func (s *server) forward(r *http.Request, c Caller, pid string, h func(http.ResponseWriter, *http.Request, Caller), body any) *capture {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "/", bytes.NewReader(b))
	req.SetPathValue("pid", pid)
	cp := &capture{h: http.Header{}, status: http.StatusOK}
	h(cp, req, c)
	return cp
}

func (s *server) applyRecipe(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	who, ok, err := s.recipeWho(r, c)
	if err != nil || !ok {
		fail(w, err)
		return
	}
	rec, ok := s.recipeByID(w, r, who)
	if !ok {
		return
	}
	var b applyBody
	if !decode(w, r, &b) {
		return
	}
	rd, err := rec.Apply(b.Params)
	if err != nil {
		recipeFail(w, err)
		return
	}
	sg, a := rd.Suggest, b.Advanced
	if a.Role != "" {
		sg.Role = a.Role
	}
	if a.Policy != "" {
		sg.Policy = a.Policy
	}
	if a.MaxUSD > 0 {
		sg.MaxUSD = a.MaxUSD
	}
	if a.MaxTokens > 0 {
		sg.MaxTokens = a.MaxTokens
	}
	if a.TimeoutMinutes > 0 {
		sg.TimeoutMinutes = a.TimeoutMinutes
	}
	if a.BaseBranch != "" {
		sg.BaseBranch = a.BaseBranch
	}
	rd.Suggest = sg
	summary := renderedCreates(*rd)
	if b.DryRun {
		writeJSON(w, http.StatusOK, recipeDryRun{DryRun: true, Kind: rd.Kind, Summary: summary, Rendered: *rd})
		return
	}
	var cp *capture
	bd := rd.Body
	budget := factory.Budget{MaxUSD: sg.MaxUSD, MaxTokens: sg.MaxTokens}
	switch rd.Kind {
	case recipes.KindSchedule:
		if s.sched == nil {
			writeError(w, http.StatusBadRequest, "not_available", "Schedules are not turned on on this server.")
			return
		}
		cp = s.forward(r, c, p.ID, s.createSchedule, map[string]any{"name": bd.Name, "prompt": bd.Prompt, "cron": bd.Cron,
			"timezone": bd.Timezone, "policy": sg.Policy, "timeoutMinutes": sg.TimeoutMinutes, "model": a.Model, "enabled": true})
	case recipes.KindTask:
		if s.factory() == nil {
			writeError(w, http.StatusBadRequest, "not_available", "Tasks are not turned on on this server.")
			return
		}
		cp = s.forward(r, c, p.ID, s.createTask, taskBody{Title: bd.Title, Spec: bd.Prompt, Role: sg.Role, Model: a.Model, Policy: sg.Policy,
			BaseBranch: sg.BaseBranch, Budget: budget})
	case recipes.KindTrigger:
		if s.factory() == nil {
			writeError(w, http.StatusBadRequest, "not_available", "Triggers are not turned on on this server.")
			return
		}
		t := factory.Trigger{Name: bd.Name, Match: bd.Match, Template: factory.Template{Title: bd.Title, Spec: bd.Prompt, Role: sg.Role,
			Policy: sg.Policy, Model: a.Model, BaseBranch: sg.BaseBranch, Budget: budget}}
		if bd.Event != "" {
			t.Kind, t.Event = factory.TriggerWebhook, bd.Event
		} else {
			t.Kind, t.Schedule = factory.TriggerCron, bd.Cron
		}
		cp = s.forward(r, c, p.ID, s.createTrigger, t)
	}
	if cp.status >= 300 {
		// The feature's own refusal, in its own words (a viewer cannot create, a model is unknown, a cron is wrong...).
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(cp.status)
		_, _ = w.Write(cp.buf.Bytes())
		return
	}
	if err := s.opt.Recipes.AddUse(r.Context(), rec.ID); err != nil {
		slog.Warn("recipes: counting a use failed", "recipe", rec.ID, "error", err)
	}
	s.note(inProject(c, p.ID), "", "recipe.apply", map[string]any{"recipe": rec.ID, "name": rec.Name, "version": rec.Version, "kind": rd.Kind}, FeedEvent{Type: "project.changed"})
	writeJSON(w, http.StatusCreated, recipeApplied{Kind: rd.Kind, Summary: summary, Recipe: rec.ID, Created: json.RawMessage(cp.buf.Bytes())})
}

// ---- plain-language descriptions ----

// recipeCreates describes what the recipe makes when started with its defaults.
func recipeCreates(r recipes.Recipe) string {
	vals := map[string]string{}
	for _, p := range r.Params {
		if p.Default != "" {
			vals[p.Key] = p.Default
		} else {
			vals[p.Key] = "..."
		}
	}
	rd := recipes.Rendered{Kind: r.Kind, Suggest: r.Suggest}
	b := r.Body
	gt := r.Kind == recipes.KindTrigger
	rd.Body.Name, _ = recipes.Render(b.Name, vals, gt)
	rd.Body.Title, _ = recipes.Render(b.Title, vals, gt)
	rd.Body.Cron, _ = recipes.Render(b.Cron, vals, gt)
	rd.Body.Timezone, _ = recipes.Render(b.Timezone, vals, gt)
	rd.Body.Event, _ = recipes.Render(b.Event, vals, gt)
	return renderedCreates(rd)
}

func renderedCreates(rd recipes.Rendered) string {
	b := rd.Body
	switch rd.Kind {
	case recipes.KindSchedule:
		return "A schedule: " + describeCron(b.Cron, b.Timezone)
	case recipes.KindTask:
		return "A task: one agent run, started now"
	default:
		if b.Event != "" {
			return "A trigger: starts a task when a GitHub event happens (" + b.Event + ")"
		}
		return "A trigger: starts a task " + describeCron(b.Cron, b.Timezone)
	}
}

func recipeCost(r recipes.Recipe) string {
	if r.Suggest.MaxUSD <= 0 {
		return ""
	}
	switch r.Kind {
	case recipes.KindSchedule:
		return fmt.Sprintf("About $%.2f or less per run", r.Suggest.MaxUSD)
	default:
		return fmt.Sprintf("Capped at $%.2f per task", r.Suggest.MaxUSD)
	}
}

var weekdays = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}

// describeCron says a schedule in words for the common shapes and quotes the expression otherwise.
func describeCron(expr, tz string) string {
	expr = strings.TrimSpace(expr)
	zone := ""
	if tz = strings.TrimSpace(tz); tz != "" && tz != "UTC" {
		zone = " (" + tz + ")"
	} else {
		zone = " (UTC)"
	}
	switch expr {
	case "@hourly":
		return "every hour"
	case "@daily", "@midnight":
		return "every day at 00:00" + zone
	case "@weekly":
		return "every Sunday at 00:00" + zone
	case "@monthly":
		return "on the 1st of every month at 00:00" + zone
	}
	if d, ok := strings.CutPrefix(expr, "@every "); ok {
		return "every " + d
	}
	f := strings.Fields(expr)
	if len(f) == 5 {
		min, e1 := strconv.Atoi(f[0])
		hour, e2 := strconv.Atoi(f[1])
		if e1 == nil && e2 == nil && f[2] == "*" && f[3] == "*" {
			at := fmt.Sprintf("%02d:%02d", hour, min)
			switch {
			case f[4] == "*":
				return "every day at " + at + zone
			case f[4] == "1-5":
				return "every weekday at " + at + zone
			default:
				if d, err := strconv.Atoi(f[4]); err == nil && d >= 0 && d <= 7 {
					return "every " + weekdays[d] + " at " + at + zone
				}
			}
		}
		if f[0] == "0" && f[1] == "*" && f[2] == "*" && f[3] == "*" && f[4] == "*" {
			return "every hour"
		}
	}
	return "on the schedule " + expr + zone
}
