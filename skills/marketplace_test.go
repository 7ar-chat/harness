package skills_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/GhaziBenDahmane/harness/skills"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/memstore"
)

const marketRepo = "https://git.example.com/ai-marketplace/vibe-toolbox-market.git"

func skillMD(name, description, extra string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n" + extra + "---\n# " + name + "\n"
}

type marketHarness struct {
	st     *memstore.Store
	reg    *skills.Registry
	src    *dirSource
	syncer *skills.MarketplaceSyncer
}

// dirSource adapts a MapFS to skills.MarketplaceSource.
type dirSource struct {
	files  fstest.MapFS
	commit string
}

func newMarketHarness(t *testing.T, files fstest.MapFS) *marketHarness {
	t.Helper()
	st := memstore.New()
	reg := skills.NewRegistry(os.DirFS(filepath.Join("testdata", "builtin")), st)
	src := &dirSource{files: files, commit: "1fdc3f24652e4f092fb3cf055b8f625f50451b55"}
	return &marketHarness{st: st, reg: reg, src: src, syncer: &skills.MarketplaceSyncer{Registry: reg, Catalog: st, Market: st,
		Source: skills.FSMarketplaceSource{FS: src.files, CommitSHA: src.commit}, RepositoryURL: marketRepo, Branch: "main"}}
}

func (h *marketHarness) setCommit(commit string) {
	h.src.commit = commit
	h.syncer.Source = skills.FSMarketplaceSource{FS: h.src.files, CommitSHA: commit}
}

func (h *marketHarness) sync(t *testing.T) *store.MarketplaceRun {
	t.Helper()
	run, err := h.syncer.Sync(context.Background(), skills.MarketplaceTriggerCommand)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return run
}

func (h *marketHarness) global(t *testing.T, name string) skills.Skill {
	t.Helper()
	all, err := h.reg.ListGlobal(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no global skill %q", name)
	return skills.Skill{}
}

func baseRepo() fstest.MapFS {
	return fstest.MapFS{
		"skills/studio/studio-docx/SKILL.md": {Data: []byte(skillMD("studio-docx", "Word documents in the Studio style.",
			"metadata:\n  version: \"1.0\"\n  tags: [studio-work-default]\n"))},
		"skills/studio/studio-docx/scripts/build.py":                     {Data: []byte("print('docx')\n")},
		"skills/studio/studio-docx/__pycache__/build.cpython-313.pyc":    {Data: []byte("bytecode")},
		"skills/studio/studio-docx/.env.local":                           {Data: []byte("SECRET=1")},
		"skills/studio/studio-amundi-pptx/SKILL.md":                      {Data: []byte(skillMD("Amundi PowerPoint", "Amundi branded decks.", "tags: [studio, studio-work-default, pptx]\n"))},
		"skills/tls/jira/SKILL.md":                                       {Data: []byte(skillMD("jira", "Jira helper.", "metadata:\n  version: 1.0\n"))},
		"skills/rsi/secdev/secdev-design/SKILL.md":                       {Data: []byte(skillMD("secdev-design", "Secure design review.", ""))},
		"skills/wip/draft-skill/SKILL.md":                                {Data: []byte(skillMD("draft-skill", "Not ready.", ""))},
		"skills/studio/README.md":                                        {Data: []byte("not a skill")},
		"skills/studio/broken/SKILL.md":                                  {Data: []byte("no frontmatter\n")},
		"agents/whatever.md":                                             {Data: []byte("outside skills/")},
		"skills/studio/studio-docx/references/style.md":                  {Data: []byte("# Style\n")},
		"skills/studio/studio-amundi-pptx/templates/amundi-template.txt": {Data: []byte("template")},
	}
}

func TestMarketplaceSyncImportsRepositorySkillsLikeAstraForge(t *testing.T) {
	// A first sync creates one published global skill per skills/<namespace>/.../SKILL.md (wip/ and broken frontmatter skipped), named like AstraForge's slug, with the frontmatter tags and AstraForge's version labels.
	// Arrange
	h := newMarketHarness(t, baseRepo())

	// Act
	run := h.sync(t)

	// Assert
	if run.Status != store.MarketplaceRunPartial || run.Created != 4 || run.Skipped != 1 || run.Failed != 0 {
		t.Fatalf("run = %+v, want partial (the broken SKILL.md), 4 created, 1 skipped", run)
	}
	if run.CommitSHA != "1fdc3f24652e4f092fb3cf055b8f625f50451b55" || run.RepositoryURL != marketRepo {
		t.Fatalf("run source = %q %q", run.RepositoryURL, run.CommitSHA)
	}
	docx := h.global(t, "studio-studio-docx")
	if docx.Published == nil || docx.Published.Version != "1.0" || !slices.Equal(docx.Tags, []string{"studio-work-default"}) {
		t.Fatalf("studio-studio-docx = published %+v tags %v", docx.Published, docx.Tags)
	}
	if docx.DisplayName != "Studio Docx" || docx.Description != "Word documents in the Studio style." {
		t.Fatalf("display %q description %q", docx.DisplayName, docx.Description)
	}
	if got := h.global(t, "studio-studio-amundi-pptx").DisplayName; got != "Amundi PowerPoint" {
		t.Fatalf("frontmatter name kept as display name: %q", got)
	}
	if got := h.global(t, "tls-jira").Published.Version; got != "1.0" {
		t.Fatalf("unquoted version 1.0 = %q, want Python's str(1.0)", got)
	}
	if got := h.global(t, "rsi-secdev-secdev-design-82a00bce").Published.Version; got != "sync-1fdc3f24" {
		t.Fatalf("nested skill version = %q", got)
	}
	if docx.Files != 3 {
		t.Fatalf("docx files = %d, want SKILL.md + scripts + references (no __pycache__, .env*)", docx.Files)
	}
}

func TestMarketplaceSyncOnlyStudioWorkTaggedSkillsAreCoworkSkills(t *testing.T) {
	// Users see every published synced skill, but the Cowork policy only allows the studio-work* tagged ones (AstraForge's Cowork rule).
	// Arrange
	h := newMarketHarness(t, baseRepo())
	h.sync(t)

	// Act
	list, err := h.reg.List(context.Background(), "alice")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, s := range list {
		allowed[s.Name] = s.Allowed
	}
	if !allowed["studio-studio-docx"] || !allowed["studio-studio-amundi-pptx"] || allowed["tls-jira"] {
		t.Fatalf("allowed = %v", allowed)
	}
}

func TestMarketplaceSyncIsIdempotentAndVersionsChanges(t *testing.T) {
	// A second sync of the same commit changes nothing; changed content adds a version (declared label when free, else sync-<commit>) and republishes.
	// Arrange
	h := newMarketHarness(t, baseRepo())
	h.sync(t)

	// Act
	again := h.sync(t)
	h.src.files["skills/studio/studio-docx/SKILL.md"] = &fstest.MapFile{Data: []byte(skillMD("studio-docx",
		"Word documents in the Studio style, v1.1.", "metadata:\n  version: \"1.1\"\n  tags: [studio-work-default, docx]\n"))}
	h.src.files["skills/tls/jira/scripts/x.sh"] = &fstest.MapFile{Data: []byte("echo hi\n")}
	h.setCommit("abcdef0123456789")
	changed := h.sync(t)

	// Assert
	if again.Created != 0 || again.Updated != 0 || again.Unchanged != 4 {
		t.Fatalf("second sync = %+v, want 4 unchanged", again)
	}
	if changed.Updated != 2 || changed.Unchanged != 2 {
		t.Fatalf("changed sync = %+v, want 2 updated", changed)
	}
	docx := h.global(t, "studio-studio-docx")
	if docx.Published.Version != "1.1" || len(docx.Versions) != 2 || !slices.Equal(docx.Tags, []string{"studio-work-default", "docx"}) {
		t.Fatalf("docx = %s, %d versions, tags %v", docx.Published.Version, len(docx.Versions), docx.Tags)
	}
	if docx.Description != "Word documents in the Studio style, v1.1." {
		t.Fatalf("description not updated: %q", docx.Description)
	}
	if got := h.global(t, "tls-jira").Published.Version; got != "sync-abcdef01" {
		t.Fatalf("jira (declared 1.0 already taken) = %q, want sync-abcdef01", got)
	}
}

func TestMarketplaceSyncUnpublishesRemovedSkillsAndRepublishesReturningOnes(t *testing.T) {
	// A path removed from the repository is unpublished (hidden from users, AstraForge's deprecation); when it comes back with the same content its version is published again.
	// Arrange
	h := newMarketHarness(t, baseRepo())
	h.sync(t)
	removed := fstest.MapFS{}
	for p, f := range h.src.files {
		removed[p] = f
	}
	for p := range removed {
		if strings.HasPrefix(p, "skills/tls/jira/") {
			delete(removed, p)
		}
	}

	// Act
	h.syncer.Source = skills.FSMarketplaceSource{FS: removed, CommitSHA: "2222222222"}
	gone := h.sync(t)
	afterRemoval := h.global(t, "tls-jira")
	h.syncer.Source = skills.FSMarketplaceSource{FS: h.src.files, CommitSHA: "3333333333"}
	back := h.sync(t)

	// Assert
	if gone.Deprecated != 1 || afterRemoval.Published != nil {
		t.Fatalf("removal: run %+v, published %+v", gone, afterRemoval.Published)
	}
	if back.Deprecated != 0 || h.global(t, "tls-jira").Published == nil || len(h.global(t, "tls-jira").Versions) != 1 {
		t.Fatalf("return: run %+v, jira %+v", back, h.global(t, "tls-jira"))
	}
	srcs, _ := h.st.MarketplaceSources(context.Background())
	for _, s := range srcs {
		if s.Deprecated {
			t.Fatalf("source still deprecated: %+v", s)
		}
	}
}

func TestMarketplaceSyncRefusesNameCollisionsAndUnsupportedPackages(t *testing.T) {
	// A name taken by a global skill from elsewhere and a package Work Chat refuses (a skill embedding another SKILL.md) fail alone: the others sync, the run is partial, and deprecation still runs.
	// Arrange
	files := baseRepo()
	files["skills/fc/fc-cypress/SKILL.md"] = &fstest.MapFile{Data: []byte(skillMD("fc-cypress", "Cypress.", ""))}
	files["skills/fc/fc-cypress/cypress-author/SKILL.md"] = &fstest.MapFile{Data: []byte(skillMD("cypress-author", "Author.", ""))}
	h := newMarketHarness(t, files)
	ctx := context.Background()
	if _, err := h.reg.CreateGlobal(ctx, "admin", zipBytes(t, map[string]string{"SKILL.md": skillMD("tls-jira", "Hand made.", "")}), nil, "", true); err != nil {
		t.Fatal(err)
	}

	// Act
	run := h.sync(t)

	// Assert
	failed := map[string]string{}
	for _, e := range run.Errors {
		failed[e.Path] = e.Message
	}
	if run.Status != store.MarketplaceRunPartial || run.Failed != 2 || !strings.Contains(failed["skills/tls/jira"], "collision") ||
		!strings.Contains(failed["skills/fc/fc-cypress"], "SKILL.md") {
		t.Fatalf("run = %+v", run)
	}
	if h.global(t, "fc-fc-cypress-cypress-author-dce1e254").Name == "" {
		t.Fatal("nested child not imported")
	}
	if got := h.global(t, "tls-jira").Description; got != "Hand made." {
		t.Fatalf("hand-made skill overwritten: %q", got)
	}
}

func TestMarketplaceSyncRejectsAConcurrentSync(t *testing.T) {
	// A second sync while the cross-process lock is held is refused (AstraForge's cache lock).
	// Arrange
	h := newMarketHarness(t, baseRepo())
	h.syncer.Lock = func(context.Context) (func(), bool, error) { return func() {}, false, nil }

	// Act
	_, err := h.syncer.Sync(context.Background(), skills.MarketplaceTriggerScheduled)

	// Assert
	if err != skills.ErrMarketplaceSyncRunning {
		t.Fatalf("err = %v", err)
	}
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestGitLabMarketplaceSourceDownloadsTheBranchCommitArchive(t *testing.T) {
	// The GitLab source exchanges username/password for an OAuth token, resolves the branch commit and downloads skills/ at that commit; secrets never appear in errors.
	// Arrange
	const commit = "1fdc3f24652e4f092fb3cf055b8f625f50451b55"
	archive := zipBytes(t, map[string]string{
		"vibe-toolbox-market-" + commit + "-skills/skills/studio/studio-pdf/SKILL.md": skillMD("studio-pdf", "PDF.", "tags: [studio-work-default]\n"),
	})
	var gotSHA, gotPath, gotAuth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/token":
			_ = r.ParseForm()
			if r.PostForm.Get("username") != "app_alto_studio" || r.PostForm.Get("password") != "s3cret-pw" {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"oauth-tok"}`))
		case r.URL.EscapedPath() == "/api/v4/projects/ai-marketplace%2Fvibe-toolbox-market/repository/branches/main":
			_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"` + commit + `"}}`))
		case r.URL.EscapedPath() == "/api/v4/projects/ai-marketplace%2Fvibe-toolbox-market/repository/archive.zip":
			gotSHA, gotPath, gotAuth = r.URL.Query().Get("sha"), r.URL.Query().Get("path"), r.Header.Get("Authorization")
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	repo := srv.URL + "/ai-marketplace/vibe-toolbox-market.git"
	src := skills.GitLabMarketplaceSource{RepositoryURL: repo, Branch: "main", Username: "app_alto_studio", Password: "s3cret-pw",
		Client: srv.Client()}
	st := memstore.New()
	syncer := &skills.MarketplaceSyncer{Catalog: st, Market: st, Source: src, RepositoryURL: repo, Branch: "main"}

	// Act
	run, err := syncer.Sync(context.Background(), skills.MarketplaceTriggerCommand)
	bad := src
	bad.Password = "wrong-pw"
	_, _, badErr := bad.Checkout(context.Background())

	// Assert
	if err != nil || run.Status != store.MarketplaceRunSucceeded || run.Created != 1 || run.CommitSHA != commit {
		t.Fatalf("run = %+v, err = %v", run, err)
	}
	if gotSHA != commit || gotPath != "skills" || gotAuth != "Bearer oauth-tok" {
		t.Fatalf("archive request sha=%q path=%q auth=%q", gotSHA, gotPath, gotAuth)
	}
	if badErr == nil || strings.Contains(badErr.Error(), "wrong-pw") || !strings.Contains(badErr.Error(), "401") {
		t.Fatalf("bad credentials error = %v", badErr)
	}
	insecure := skills.GitLabMarketplaceSource{RepositoryURL: "http://git.example.com/a/b.git", Token: "glpat-xyz",
		Client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{}}}}
	if _, _, err := insecure.Checkout(context.Background()); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("http repository accepted: %v", err)
	}
}

func TestMarketplaceSyncRealCheckout(t *testing.T) {
	// With WORK_TEST_MARKETPLACE_PATH (a vibe-toolbox-market checkout), the whole repository syncs; failures are listed.
	dir := os.Getenv("WORK_TEST_MARKETPLACE_PATH")
	if dir == "" {
		t.Skip("WORK_TEST_MARKETPLACE_PATH not set")
	}
	st := memstore.New()
	reg := skills.NewRegistry(nil, st)
	syncer := &skills.MarketplaceSyncer{Registry: reg, Catalog: st, Market: st, Source: skills.DirMarketplaceSource{Path: dir},
		RepositoryURL: marketRepo, Branch: "main"}
	run, err := syncer.Sync(context.Background(), skills.MarketplaceTriggerCommand)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("status=%s discovered=%d created=%d skipped=%d failed=%d", run.Status, run.Discovered, run.Created, run.Skipped, run.Failed)
	for _, w := range run.Warnings {
		t.Logf("warning %s: %s", w.Path, w.Message)
	}
	for _, e := range run.Errors {
		t.Logf("error %s: %s", e.Path, e.Message)
	}
	all, _ := reg.ListGlobal(context.Background(), "admin")
	for _, s := range all {
		t.Logf("skill %s (%s) tags=%v", s.Name, s.VersionLabel, s.Tags)
	}
}
