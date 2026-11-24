package product

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/adapters/store/recipestore"
	"github.com/7ar-chat/harness/adapters/store/schedstore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/core/store"
)

// recipeRig is a team server with schedules, the factory and recipes: alice owns "api" and is an administrator, bob is
// a member of it and owns "web", vic is a viewer of "api", eve is outside both.
type recipeRig struct {
	*rig
	api, web string
}

func newRecipeRig(t *testing.T) *recipeRig {
	t.Helper()
	tm := teamstore.NewMem()
	for _, l := range []string{"alice", "bob", "vic", "eve"} {
		if _, err := tm.UpsertUser(context.Background(), store.User{Login: l, Name: l, Email: l + "@x.io"}); err != nil {
			t.Fatal(err)
		}
	}
	fo := harness.FactoryOptions{Store: factorystore.NewMem(nil), PollEvery: 10 * time.Millisecond}
	r := newRig(t, rigOpts{
		team: true, env: map[string]string{"LLM_DEFAULT_MODEL": "gpt-test"},
		prod: Options{Team: tm, Admins: []string{"alice"}, Recipes: recipestore.NewMem(nil),
			Schedules: &SchedulesOptions{Store: schedstore.NewMem(), NoRun: true}},
		mod: func(ho *harness.Options) { ho.Features = append(harness.ProfileMinimal(), harness.FactoryFeature(fo)) },
	})
	rr := &recipeRig{rig: r}
	rr.api = r.json("POST", "/api/projects", "alice", `{"name":"api","repoUrl":"https://github.com/x/api"}`, 201)["id"].(string)
	r.json("PUT", "/api/projects/"+rr.api+"/members/bob", "alice", `{"role":"member"}`, 204)
	r.json("PUT", "/api/projects/"+rr.api+"/members/vic", "alice", `{"role":"viewer"}`, 204)
	rr.web = r.json("POST", "/api/projects", "bob", `{"name":"web"}`, 201)["id"].(string)
	return rr
}

func (r *recipeRig) list(user, query string) []map[string]any {
	r.t.Helper()
	resp := r.do("GET", "/api/recipes"+query, user, "")
	b, _ := io.ReadAll(resp.Body)
	var out []map[string]any
	if resp.StatusCode != 200 || json.Unmarshal(b, &out) != nil {
		r.t.Fatalf("list %s: %d %s", query, resp.StatusCode, b)
	}
	return out
}

func recipeIDs(list []map[string]any) []string {
	out := []string{}
	for _, m := range list {
		out = append(out, m["id"].(string))
	}
	return out
}

func hasID(list []map[string]any, id string) bool {
	for _, i := range recipeIDs(list) {
		if i == id {
			return true
		}
	}
	return false
}

func (r *recipeRig) apply(project, id, user, body string, want int) map[string]any {
	r.t.Helper()
	return r.json("POST", "/api/projects/"+project+"/recipes/"+id+"/apply", user, body, want)
}

const mine = `{"name":"Release check","summary":"Look at {{thing}}","category":"quality","kind":"schedule",
 "body":{"name":"Check {{thing}}","prompt":"Check {{thing}} and report.","cron":"0 9 * * 1"},
 "params":[{"key":"thing","label":"What","type":"text","default":"the build","required":true}],"suggest":{"policy":"readonly"}`

func TestTheGalleryListsReadyMadeRecipesAndFiltersThem(t *testing.T) {
	r := newRecipeRig(t)

	all := r.list("eve", "") // someone in no project still sees the gallery
	if len(all) < 12 {
		t.Fatalf("want a gallery of at least 12, got %d", len(all))
	}
	var digest map[string]any
	for _, m := range all {
		if m["id"] == "weekly-status-digest" {
			digest = m
		}
	}
	if digest == nil || digest["creates"] != "A schedule: every Friday at 17:00 (UTC)" || digest["scope"] != "builtin" || digest["canEdit"] != false {
		t.Fatalf("digest = %v", digest)
	}
	if !strings.Contains(digest["cost"].(string), "$1.00") {
		t.Fatalf("cost = %v", digest["cost"])
	}
	if got := r.list("eve", "?category=security"); len(got) < 2 || hasID(got, "weekly-status-digest") {
		t.Fatalf("category filter: %v", recipeIDs(got))
	}
	if got := r.list("eve", "?kind=trigger"); !hasID(got, "issue-triage") || hasID(got, "weekly-status-digest") {
		t.Fatalf("kind filter: %v", recipeIDs(got))
	}
	if got := r.list("eve", "?q=CHANGELOG"); !hasID(got, "weekly-changelog") || hasID(got, "weekly-status-digest") {
		t.Fatalf("search: %v", recipeIDs(got))
	}
	r.json("GET", "/api/recipes/nope", "eve", "", 404)
}

func TestApplyingARecipeCreatesAScheduleAsTheApplyingMember(t *testing.T) {
	r := newRecipeRig(t)
	pre := r.apply(r.api, "weekly-status-digest", "bob", `{"params":{"audience":"managers and stakeholders","timezone":"Europe/Paris"},"dryRun":true}`, 200)
	if pre["dryRun"] != true || !strings.Contains(pre["summary"].(string), "every Friday at 17:00 (Europe/Paris)") {
		t.Fatalf("dry run = %v", pre)
	}
	rendered := pre["rendered"].(map[string]any)["body"].(map[string]any)
	if !strings.Contains(rendered["prompt"].(string), "managers and stakeholders") || strings.Contains(rendered["prompt"].(string), "{{") {
		t.Fatalf("rendered prompt = %v", rendered["prompt"])
	}
	if got := r.json("GET", "/api/projects/"+r.api+"/schedules", "bob", "", 200); got != nil {
		_ = got
	}
	resp := r.do("GET", "/api/projects/"+r.api+"/schedules", "bob", "")
	var before []map[string]any
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &before)
	if len(before) != 0 {
		t.Fatalf("a dry run created a schedule: %s", b)
	}

	out := r.apply(r.api, "weekly-status-digest", "bob", `{"params":{"audience":"the whole team","length":"10"}}`, 201)
	sc := out["created"].(map[string]any)
	if out["kind"] != "schedule" || sc["owner"] != "bob" || sc["runAs"] != "bob" || sc["policy"] != "readonly" || sc["cron"] != "0 17 * * 5" || sc["enabled"] != true {
		t.Fatalf("created = %v", out)
	}
	resp = r.do("GET", "/api/projects/"+r.api+"/schedules", "alice", "")
	b, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(b), sc["id"].(string)) {
		t.Fatalf("the schedule is not listed: %s", b)
	}
	if uses := r.json("GET", "/api/recipes/weekly-status-digest", "bob", "", 200)["uses"]; uses != float64(1) {
		t.Fatalf("uses = %v", uses)
	}
}

func TestApplyKeepsEveryExistingPermissionCheck(t *testing.T) {
	r := newRecipeRig(t)
	r.apply(r.api, "weekly-status-digest", "vic", `{}`, 403) // a viewer cannot create schedules
	r.apply(r.api, "weekly-status-digest", "eve", `{}`, 404) // an outsider does not see the project
	r.apply(r.api, "issue-triage", "bob", `{}`, 403)         // creating a trigger takes a project owner
	r.apply(r.api, "weekly-status-digest", "bob", `{"params":{"bogus":"x"}}`, 400)
	r.apply(r.api, "weekly-status-digest", "bob", `{"params":{"audience":"aliens"}}`, 400)
	r.apply(r.api, "weekly-status-digest", "bob", `{"advanced":{"policy":"autonomous"}}`, 400) // not a schedule policy
	r.apply(r.api, "nope", "bob", `{}`, 404)
	r.apply(r.api, "write-missing-tests", "bob", `{}`, 400) // the required "what to test" is missing
	if uses := r.json("GET", "/api/recipes/weekly-status-digest", "bob", "", 200)["uses"]; uses != float64(0) {
		t.Fatalf("a refused apply counted as a use: %v", uses)
	}
}

func TestApplyingATaskAndATrigger(t *testing.T) {
	r := newRecipeRig(t)
	task := r.apply(r.api, "write-missing-tests", "bob", `{"params":{"target":"internal/billing"},"advanced":{"maxUsd":1.5}}`, 201)["created"].(map[string]any)
	if task["owner"] != "bob" || task["title"] != "Add tests for internal/billing" || task["policy"] != "sandboxed" || task["budget"].(map[string]any)["maxUsd"] != 1.5 {
		t.Fatalf("task = %v", task)
	}

	trig := r.apply(r.api, "issue-triage", "alice", `{"params":{"labels":"{{.secret}}, bug"}}`, 201)["created"].(map[string]any)
	if trig["kind"] != "webhook" || trig["event"] != "issues.opened" || trig["owner"] != "alice" {
		t.Fatalf("trigger = %v", trig)
	}
	tpl := trig["template"].(map[string]any)
	if tpl["policy"] != "sandboxed" || tpl["budget"].(map[string]any)["maxUsd"] != 0.5 || tpl["title"] != "Triage: {{.title}}" {
		t.Fatalf("template = %v", tpl)
	}
	// the value could not smuggle a template action into the trigger's text
	if spec := tpl["spec"].(string); strings.Contains(spec, "{{.secret}}") || !strings.Contains(spec, `{{"{{"}}.secret}}`) {
		t.Fatalf("spec = %q", spec)
	}
	cron := r.apply(r.api, "weekly-dependency-update", "alice", `{}`, 201)["created"].(map[string]any)
	if cron["kind"] != "cron" || cron["schedule"] != "0 6 * * 1" {
		t.Fatalf("cron trigger = %v", cron)
	}
}

func TestWhoMayWriteWhichScope(t *testing.T) {
	r := newRecipeRig(t)
	proj := func(scope, pid string) string {
		return strings.TrimSuffix(mine, "") + `,"scope":"` + scope + `","projectId":"` + pid + `"}`
	}
	// personal: anyone, visible to the author only
	p := r.json("POST", "/api/recipes", "bob", proj("personal", ""), 201)
	if p["scope"] != "personal" || p["author"] != "bob" || p["version"] != float64(1) || p["canEdit"] != true {
		t.Fatalf("personal = %v", p)
	}
	if hasID(r.list("alice", ""), p["id"].(string)) || !hasID(r.list("bob", ""), p["id"].(string)) {
		t.Fatal("a personal recipe is visible to its author only")
	}
	r.json("GET", "/api/recipes/"+p["id"].(string), "alice", "", 404)

	// project: owners write, members read
	r.json("POST", "/api/recipes", "bob", proj("project", r.api), 403)
	r.json("POST", "/api/recipes", "eve", proj("project", r.api), 404)
	pr := r.json("POST", "/api/recipes", "alice", proj("project", r.api), 201)
	id := pr["id"].(string)
	if pr["projectName"] != "api" || !hasID(r.list("bob", ""), id) || !hasID(r.list("vic", ""), id) || hasID(r.list("eve", ""), id) {
		t.Fatalf("project recipe visibility: %v", pr)
	}
	if b := r.json("GET", "/api/recipes/"+id, "bob", "", 200); b["canEdit"] != false {
		t.Fatalf("a member cannot edit: %v", b)
	}
	if got := r.list("bob", "?project="+r.web); hasID(got, id) {
		t.Fatal("another project's recipe leaked into the filtered list")
	}

	// instance: administrators only, visible to all
	r.json("POST", "/api/recipes", "bob", proj("instance", ""), 403)
	inst := r.json("POST", "/api/recipes", "alice", proj("instance", ""), 201)
	if !hasID(r.list("eve", ""), inst["id"].(string)) {
		t.Fatal("an instance recipe is visible to everyone")
	}
	r.json("POST", "/api/recipes", "alice", proj("galaxy", ""), 400)

	// a project recipe can be applied in another project of a member who sees it; an outsider cannot see it at all
	r.apply(r.web, id, "bob", `{}`, 201)
	r.apply(r.web, id, "eve", `{}`, 404)
}

func TestUpdatingKeepsVersionsAndChecksTheOwner(t *testing.T) {
	r := newRecipeRig(t)
	body := func(summary string, version int) string {
		return strings.Replace(mine, `"summary":"Look at {{thing}}"`, `"summary":"`+summary+`"`, 1) + `,"version":` + jsonInt(version) + `}`
	}
	c := r.json("POST", "/api/recipes", "alice", body("one", 0)[:len(body("one", 0))-len(`,"version":0}`)]+`,"scope":"project","projectId":"`+r.api+`"}`, 201)
	id := c["id"].(string)

	r.json("PUT", "/api/recipes/"+id, "bob", body("hijack", 1), 403)
	u := r.json("PUT", "/api/recipes/"+id, "alice", body("two", 1), 200)
	if u["version"] != float64(2) || u["summary"] != "two" || u["scope"] != "project" {
		t.Fatalf("updated = %v", u)
	}
	r.json("PUT", "/api/recipes/"+id, "alice", body("three", 1), 409) // stale
	v := r.json("GET", "/api/recipes/"+id+"/versions", "bob", "", 200)
	hist := v["history"].([]any)
	if v["current"] != float64(2) || len(hist) != 1 || hist[0].(map[string]any)["recipe"].(map[string]any)["summary"] != "one" {
		t.Fatalf("versions = %v", v)
	}
	r.json("PUT", "/api/recipes/weekly-changelog", "alice", body("x", 0), 403) // builtins are read-only
	bad := strings.Replace(body("x", 0), `"Check {{thing}} and report."`, `"Check {{missing}}"`, 1)
	r.json("PUT", "/api/recipes/"+id, "alice", bad, 400)

	r.json("DELETE", "/api/recipes/"+id, "bob", "", 403)
	r.do("DELETE", "/api/recipes/"+id, "alice", "")
	r.json("GET", "/api/recipes/"+id, "alice", "", 404)
	r.json("DELETE", "/api/recipes/weekly-changelog", "alice", "", 403)
}

func jsonInt(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestSharingAScheduleAsARecipeAndUsingItInAnotherProject(t *testing.T) {
	r := newRecipeRig(t)
	sc := r.json("POST", "/api/projects/"+r.api+"/schedules", "bob",
		`{"name":"Merged PRs of api","prompt":"Summarise merged pull requests of api for the team.","cron":"0 7 * * 1","policy":"readonly"}`, 201)

	from := `{"from":"schedule","id":"` + sc["id"].(string) + `","name":"PR summary","summary":"Weekly merged PRs","category":"reporting","scope":"personal",
	 "params":[{"key":"repo","label":"Project name","type":"text","text":"api"}]}`
	rec := r.json("POST", "/api/projects/"+r.api+"/recipes/from", "bob", from, 201)
	body := rec["body"].(map[string]any)
	if rec["kind"] != "schedule" || body["prompt"] != "Summarise merged pull requests of {{repo}} for the team." || body["name"] != "Merged PRs of {{repo}}" {
		t.Fatalf("recipe = %v", rec)
	}
	if p := rec["params"].([]any)[0].(map[string]any); p["default"] != "api" {
		t.Fatalf("the marked text must be the default: %v", p)
	}

	out := r.apply(r.web, rec["id"].(string), "bob", `{"params":{"repo":"web"}}`, 201)["created"].(map[string]any)
	if out["prompt"] != "Summarise merged pull requests of web for the team." || out["projectId"] != r.web {
		t.Fatalf("applied = %v", out)
	}

	// refusals: text not in the schedule, a viewer sharing to the project, someone else's project, a bad source
	r.json("POST", "/api/projects/"+r.api+"/recipes/from", "bob", strings.Replace(from, `"text":"api"`, `"text":"nowhere"`, 1), 400)
	r.json("POST", "/api/projects/"+r.api+"/recipes/from", "bob", strings.Replace(from, `"personal"`, `"project"`, 1), 403)
	r.json("POST", "/api/projects/"+r.api+"/recipes/from", "eve", from, 404)
	r.json("POST", "/api/projects/"+r.web+"/recipes/from", "bob", from, 404) // the schedule is not in that project
	shared := r.json("POST", "/api/projects/"+r.api+"/recipes/from", "alice", strings.Replace(from, `"personal"`, `"project"`, 1), 201)
	if shared["scope"] != "project" || shared["scopeId"] != r.api {
		t.Fatalf("shared = %v", shared)
	}
}

func TestExportAndImportAreStrict(t *testing.T) {
	r := newRecipeRig(t)
	resp := r.do("GET", "/api/recipes/daily-ci-triage/export", "eve", "")
	file, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), "daily-ci-failure-triage.recipe.json") {
		t.Fatalf("export: %d %s", resp.StatusCode, resp.Header)
	}
	var f map[string]any
	if err := json.Unmarshal(file, &f); err != nil || f["format"] != "7ar-recipe" {
		t.Fatalf("file = %s", file)
	}
	if _, leaks := f["recipe"].(map[string]any)["id"]; leaks || strings.Contains(string(file), "scope") {
		t.Fatalf("the file carries where the recipe came from: %s", file)
	}

	got := r.doRaw("POST", "/api/recipes/import", "bob", string(file), 201)
	if got["scope"] != "personal" || got["author"] != "bob" || got["name"] != "Daily CI failure triage" {
		t.Fatalf("imported = %v", got)
	}
	// round trip: the imported one exports to the same document
	resp = r.do("GET", "/api/recipes/"+got["id"].(string)+"/export", "bob", "")
	again, _ := io.ReadAll(resp.Body)
	if string(again) != string(file) {
		t.Fatalf("round trip differs:\n%s\n%s", file, again)
	}
	r.doRaw("POST", "/api/recipes/import?scope=project&project="+r.api, "bob", string(file), 403)
	r.doRaw("POST", "/api/recipes/import?scope=project&project="+r.api, "alice", string(file), 201)

	bad := map[string]string{
		"unknown field":      strings.Replace(string(file), `"summary"`, `"admin": true, "summary"`, 1),
		"unknown nested":     strings.Replace(string(file), `"prompt"`, `"command": "rm -rf /", "prompt"`, 1),
		"wrong format":       strings.Replace(string(file), `7ar-recipe`, `something-else`, 1),
		"wrong version":      strings.Replace(string(file), `"version": 1`, `"version": 7`, 1),
		"trailing data":      string(file) + `{"x":1}`,
		"not json":           `hello`,
		"undeclared param":   strings.Replace(string(file), `"prompt": "Look`, `"prompt": "{{ghost}} Look`, 1),
		"bad category":       strings.Replace(string(file), `"quality"`, `"gossip"`, 1),
		"param without type": strings.Replace(string(file), `"type": "cron"`, `"type": "script"`, 1),
	}
	for name, body := range bad {
		if body == string(file) {
			t.Fatalf("%s: the test did not change the file", name)
		}
		r.doRaw("POST", "/api/recipes/import", "bob", body, 400)
	}
	r.doRaw("POST", "/api/recipes/import", "bob", strings.Repeat(" ", 130<<10), 413)
}

// doRaw sends body as is (not necessarily JSON) and decodes an object answer.
func (r *recipeRig) doRaw(method, path, user, body string, want int) map[string]any {
	r.t.Helper()
	resp := r.do(method, path, user, body)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		r.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, b)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func TestDeletingAProjectDeletesItsRecipes(t *testing.T) {
	r := newRecipeRig(t)
	rec := r.json("POST", "/api/recipes", "bob", mine+`,"scope":"project","projectId":"`+r.web+`"}`, 201)
	r.do("DELETE", "/api/projects/"+r.web, "bob", "")
	r.json("GET", "/api/recipes/"+rec["id"].(string), "bob", "", 404)
}
