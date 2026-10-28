package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
	"gopkg.in/yaml.v3"

	"github.com/GhaziBenDahmane/harness/store"
)

// Skills marketplace sync, a port of AstraForge's skills/marketplace_sync.py:
// every skills/<namespace>/<path>/SKILL.md of a Git repository (GitLab
// vibe-toolbox-market) becomes a published global skill named like
// AstraForge's slug, tagged with its frontmatter tags; a changed package adds
// a version (frontmatter version, else sync-<commit>); a path removed from the
// repository is unpublished (AstraForge: deprecated). Only the Cowork policy
// (policy.go) decides what users may attach: like in AstraForge, the
// studio-work* tagged skills.

// Marketplace sync triggers (AstraForge's SkillCatalogSyncTrigger).
const (
	MarketplaceTriggerScheduled = "scheduled"
	MarketplaceTriggerCommand   = "management_command"
)

// MarketplaceOwner is the created_by of synced skills and versions (AstraForge's service user).
const MarketplaceOwner = "marketplace-sync"

const (
	marketplaceSkillsDir = "skills"
	// SKILL.md files are discovered from skills/<namespace>/<skill>/SKILL.md
	// (depth 3) down to MAX_SKILL_ARCHIVE_PATH_DEPTH, outside skills/wip/.
	marketplaceMinDepth = 3
	marketplaceMaxDepth = 32
	// maxMarketplaceEntries bounds the discovery walk.
	maxMarketplaceEntries = 100_000
	// maxMarketplaceIssues bounds the warnings and errors stored per run (AstraForge: 200).
	maxMarketplaceIssues = 200
)

var (
	marketplaceExcludedNames = map[string]bool{".git": true, ".hg": true, ".svn": true, ".DS_Store": true,
		"__pycache__": true, "node_modules": true, ".pytest_cache": true, ".mypy_cache": true, ".ruff_cache": true}
	marketplaceExcludedSuffixes = map[string]bool{".pyc": true, ".pyo": true, ".env": true, ".log": true}
	marketplaceExcludedFirst    = map[string]bool{"wip": true}
)

// MarketplaceSource yields a repository checkout: its files (rooted at the
// repository root) and the commit they come from.
type MarketplaceSource interface {
	Checkout(ctx context.Context) (fsys fs.FS, commitSHA string, err error)
}

// MarketplaceSyncer synchronises a marketplace repository into the global skills.
type MarketplaceSyncer struct {
	Registry *Registry // reserved names (built-in and Anthropic skills)
	Catalog  store.SkillCatalogStore
	Market   store.SkillMarketplaceStore
	Source   MarketplaceSource
	// RepositoryURL (without credentials) and Branch identify the synced skills.
	RepositoryURL string
	Branch        string
	// Lock, when set, guards a sync across processes (false: one is already running).
	Lock func(ctx context.Context) (release func(), ok bool, err error)

	mu sync.Mutex
}

// ErrMarketplaceSyncRunning means another sync holds the lock.
var ErrMarketplaceSyncRunning = errors.New("a marketplace sync is already running")

type marketplaceCandidate struct {
	namespace       string
	skillPath       []string
	name            string
	displayName     string
	tags            []string
	declaredVersion string
	sourcePath      string // skills/<namespace>/<path>
	dir             string // in the checkout
}

// RedactURLCredentials drops the userinfo of a URL (AstraForge's redact_url_credentials).
func RedactURLCredentials(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return raw
	}
	host, tail, _ := strings.Cut(rest, "/")
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if tail == "" && !strings.HasSuffix(rest, "/") {
		return scheme + "://" + host
	}
	return scheme + "://" + host + "/" + tail
}

// Sync runs one sync and returns its run (also stored). A failure before the
// skills are processed (checkout) marks the run failed and is returned.
func (m *MarketplaceSyncer) Sync(ctx context.Context, trigger string) (*store.MarketplaceRun, error) {
	if !m.mu.TryLock() {
		return nil, ErrMarketplaceSyncRunning
	}
	defer m.mu.Unlock()
	if m.Lock != nil {
		release, ok, err := m.Lock(ctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrMarketplaceSyncRunning
		}
		defer release()
	}
	repo := RedactURLCredentials(m.RepositoryURL)
	run := &store.MarketplaceRun{Trigger: trigger, Status: store.MarketplaceRunRunning, RepositoryURL: repo, Branch: m.Branch}
	if err := m.Market.CreateMarketplaceRun(ctx, run); err != nil {
		return nil, err
	}
	fail := func(err error) (*store.MarketplaceRun, error) {
		run.Status = store.MarketplaceRunFailed
		run.Errors = append(run.Errors, store.MarketplaceIssue{Path: "", Message: err.Error()})
		if ferr := m.Market.FinishMarketplaceRun(context.WithoutCancel(ctx), run); ferr != nil {
			slog.Warn("skills marketplace: recording the failed run failed", "error", ferr)
		}
		return run, err
	}
	fsys, commit, err := m.Source.Checkout(ctx)
	if err != nil {
		return fail(err)
	}
	run.CommitSHA = commit

	candidates, warnings, discovered := discoverMarketplace(fsys)
	run.Discovered, run.Warnings = len(candidates), warnings
	state, err := m.loadState(ctx)
	if err != nil {
		return fail(err)
	}
	// Paths present in the repository (also the failing ones): never deprecated.
	present := map[string]bool{}
	for _, w := range warnings {
		present[w.Path] = true
	}
	for _, c := range candidates {
		present[c.sourcePath] = true
		result, err := m.upsert(ctx, state, fsys, c, commit)
		if err != nil {
			run.Failed++
			run.Errors = append(run.Errors, store.MarketplaceIssue{Path: c.sourcePath, Message: err.Error()})
			continue
		}
		switch result {
		case "created":
			run.Created++
		case "updated":
			run.Updated++
		default:
			run.Unchanged++
		}
	}
	run.Skipped = len(warnings)
	// Unlike AstraForge (no deprecation after any warning or error), a failing
	// path still present is only protected itself: Work Chat refuses packages
	// AstraForge accepts (e.g. a skill embedding another one's SKILL.md), which
	// would otherwise block every deprecation. A failed discovery deprecates nothing.
	if discovered {
		n, err := m.deprecateMissing(ctx, state, present, commit)
		run.Deprecated = n
		if err != nil {
			run.Errors = append(run.Errors, store.MarketplaceIssue{Path: "", Message: "deprecation: " + err.Error()})
		}
	}
	run.Status = store.MarketplaceRunSucceeded
	if len(run.Warnings) > 0 || len(run.Errors) > 0 {
		run.Status = store.MarketplaceRunPartial
	}
	if len(run.Warnings) > maxMarketplaceIssues {
		run.Warnings = run.Warnings[:maxMarketplaceIssues]
	}
	if len(run.Errors) > maxMarketplaceIssues {
		run.Errors = run.Errors[:maxMarketplaceIssues]
	}
	if err := m.Market.FinishMarketplaceRun(context.WithoutCancel(ctx), run); err != nil {
		return run, err
	}
	return run, nil
}

// marketplaceState is the catalog as the sync sees it.
type marketplaceState struct {
	byName  map[string]*store.GlobalSkill
	sources map[string]*store.MarketplaceSource // by global skill id
}

func (m *MarketplaceSyncer) loadState(ctx context.Context) (*marketplaceState, error) {
	globals, err := m.Catalog.ListGlobalSkills(ctx)
	if err != nil {
		return nil, err
	}
	sources, err := m.Market.MarketplaceSources(ctx)
	if err != nil {
		return nil, err
	}
	st := &marketplaceState{byName: map[string]*store.GlobalSkill{}, sources: map[string]*store.MarketplaceSource{}}
	for _, g := range globals {
		st.byName[g.Name] = g
	}
	for _, s := range sources {
		st.sources[s.GlobalSkillID] = s
	}
	return st, nil
}

func (m *MarketplaceSyncer) sameSource(src *store.MarketplaceSource, sourcePath string) bool {
	return src != nil && RedactURLCredentials(src.RepositoryURL) == RedactURLCredentials(m.RepositoryURL) &&
		src.Branch == m.Branch && src.SourcePath == sourcePath
}

// upsert is AstraForge's _upsert_candidate: created, updated or unchanged.
func (m *MarketplaceSyncer) upsert(ctx context.Context, st *marketplaceState, fsys fs.FS, c marketplaceCandidate, commit string) (string, error) {
	if m.Registry != nil && m.Registry.Reserved(c.name) {
		return "", fmt.Errorf("name_conflict: %q is the name of a Studio skill", c.name)
	}
	existing := st.byName[c.name]
	if existing != nil && !m.sameSource(st.sources[existing.ID], c.sourcePath) {
		return "", fmt.Errorf("marketplace skill name collision for %s; existing skill %q is not from the same marketplace source", c.sourcePath, c.name)
	}
	pkg, err := packageMarketplaceDir(fsys, c.dir)
	if err != nil {
		return "", err
	}
	tags, err := CleanTags(c.tags)
	if err != nil {
		return "", err
	}
	description := pkg.Description
	source := store.MarketplaceSource{RepositoryURL: RedactURLCredentials(m.RepositoryURL), Branch: m.Branch,
		SourcePath: c.sourcePath, Namespace: c.namespace, CommitSHA: commit, DeclaredVersion: c.declaredVersion}

	if existing == nil {
		label, _ := marketplaceLabel(nil, c.declaredVersion, pkg.Revision, commit)
		v, err := versionOf(pkg, label, store.SkillSourceUpload, MarketplaceOwner)
		if err != nil {
			return "", err
		}
		g, _, err := m.Catalog.CreateGlobalSkill(ctx, store.GlobalSkill{Name: c.name, DisplayName: c.displayName,
			Description: description, Tags: tags, CreatedBy: MarketplaceOwner}, v, true)
		if err != nil {
			return "", err
		}
		source.GlobalSkillID = g.ID
		st.byName[g.Name] = g
		st.sources[g.ID] = &source
		return "created", m.Market.UpsertMarketplaceSource(ctx, source)
	}

	versions, err := m.Catalog.ListSkillVersions(ctx, []string{existing.ID})
	if err != nil {
		return "", err
	}
	label, reuse := marketplaceLabel(versions[existing.ID], c.declaredVersion, pkg.Revision, commit)
	result := "unchanged"
	if reuse != nil {
		// Same content: it becomes (again) the published version.
		if existing.PublishedVersionID != reuse.ID || existing.LatestVersionID != reuse.ID {
			if err := m.Catalog.PublishSkillVersion(ctx, MarketplaceOwner, existing.ID, reuse.ID, true); err != nil {
				return "", err
			}
		}
	} else {
		v, err := versionOf(pkg, label, store.SkillSourceUpload, MarketplaceOwner)
		if err != nil {
			return "", err
		}
		v.SkillID, v.Global = existing.ID, true
		if _, err := m.Catalog.AddSkillVersion(ctx, MarketplaceOwner, v, true); err != nil {
			return "", err
		}
		result = "updated"
	}
	if existing.DisplayName != c.displayName || existing.Description != description {
		if err := m.Market.UpdateGlobalSkillInfo(ctx, existing.ID, c.displayName, description); err != nil {
			return "", err
		}
	}
	if !equalStrings(existing.Tags, tags) {
		if err := m.Catalog.SetGlobalSkillTags(ctx, existing.ID, tags); err != nil {
			return "", err
		}
	}
	source.GlobalSkillID = existing.ID
	return result, m.Market.UpsertMarketplaceSource(ctx, source)
}

// marketplaceLabel is AstraForge's _version_for_candidate: the version with the
// same content (reused), else the declared version when free, else
// sync-<short commit> (with -2, -3 … when taken).
func marketplaceLabel(versions []*store.SkillVersion, declared, revision, commit string) (string, *store.SkillVersion) {
	byLabel := map[string]*store.SkillVersion{}
	for _, v := range versions {
		if v.SHA256 == revision {
			return v.Version, v
		}
		byLabel[v.Version] = v
	}
	if declared != "" {
		if _, taken := byLabel[declared]; !taken {
			if l, err := cleanLabel(declared); err == nil {
				return l, nil
			}
		}
	}
	synthetic := "sync-" + shortCommit(commit)
	label := synthetic
	for n := 2; byLabel[label] != nil; n++ {
		label = synthetic + "-" + strconv.Itoa(n)
	}
	return label, nil
}

func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return "local"
	}
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}

// deprecateMissing unpublishes the skills of this repository and branch whose path is gone.
func (m *MarketplaceSyncer) deprecateMissing(ctx context.Context, st *marketplaceState, present map[string]bool, commit string) (int, error) {
	n := 0
	var errs []error
	for _, src := range st.sources {
		if src.Deprecated || present[src.SourcePath] || src.Branch != m.Branch ||
			RedactURLCredentials(src.RepositoryURL) != RedactURLCredentials(m.RepositoryURL) {
			continue
		}
		if err := m.Catalog.UnpublishGlobalSkill(ctx, src.GlobalSkillID); err != nil && !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, fmt.Errorf("%s: %w", src.SourcePath, err))
			continue
		}
		dep := *src
		dep.Deprecated, dep.CommitSHA = true, commit
		if err := m.Market.UpsertMarketplaceSource(ctx, dep); err != nil && !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, fmt.Errorf("%s: %w", src.SourcePath, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// discoverMarketplace is AstraForge's discover_marketplace_skill_candidates
// (sorted by source path); discovered is false when skills/ cannot be read.
func discoverMarketplace(fsys fs.FS) (candidates []marketplaceCandidate, warnings []store.MarketplaceIssue, discovered bool) {
	info, err := fs.Stat(fsys, marketplaceSkillsDir)
	if err != nil || !info.IsDir() {
		return nil, []store.MarketplaceIssue{{Path: marketplaceSkillsDir, Message: "skills directory not found."}}, false
	}
	var files []string
	entries := 0
	err = fs.WalkDir(fsys, marketplaceSkillsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == marketplaceSkillsDir {
			return nil
		}
		if entries++; entries > maxMarketplaceEntries {
			return errors.New("discovery contains too many entries")
		}
		rel := strings.TrimPrefix(p, marketplaceSkillsDir+"/")
		depth := strings.Count(rel, "/") + 1
		if d.IsDir() {
			if depth == 1 && marketplaceExcludedFirst[d.Name()] || depth >= marketplaceMaxDepth {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() == "SKILL.md" && d.Type().IsRegular() && depth >= marketplaceMinDepth {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return nil, []store.MarketplaceIssue{{Path: marketplaceSkillsDir, Message: err.Error()}}, false
	}
	sort.Strings(files)
	for _, rel := range files {
		relative := marketplaceSkillsDir + "/" + rel
		parts := strings.Split(rel, "/")
		namespace, skillPath := parts[0], parts[1:len(parts)-1]
		data, err := fs.ReadFile(fsys, relative)
		if err == nil && len(data) > MaxSkillMDBytes {
			err = errors.New("SKILL.md is too large.")
		}
		var meta map[string]any
		if err == nil {
			meta, err = marketplaceFrontmatter(data)
		}
		if err != nil {
			warnings = append(warnings, store.MarketplaceIssue{Path: path.Dir(relative), Message: err.Error()})
			continue
		}
		dirName := skillPath[len(skillPath)-1]
		candidates = append(candidates, marketplaceCandidate{
			namespace: namespace, skillPath: skillPath,
			name:            marketplaceName(namespace, skillPath),
			displayName:     marketplaceDisplayName(meta, dirName),
			tags:            marketplaceTags(meta),
			declaredVersion: marketplaceDeclaredVersion(meta),
			sourcePath:      marketplaceSkillsDir + "/" + namespace + "/" + strings.Join(skillPath, "/"),
			dir:             path.Dir(relative),
		})
	}
	return candidates, warnings, true
}

func marketplaceIncluded(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if marketplaceExcludedNames[part] {
			return false
		}
	}
	base := path.Base(rel)
	return !strings.HasPrefix(base, ".env") && !marketplaceExcludedSuffixes[path.Ext(base)]
}

// packageMarketplaceDir collects a skill directory of fsys (AstraForge's
// package_marketplace_skill: no VCS / cache directories, .env*, *.pyc, *.log …)
// and validates it as a Work Chat package.
func packageMarketplaceDir(fsys fs.FS, dir string) (*Package, error) {
	var files []File
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel := strings.TrimPrefix(p, dir+"/")
		if !marketplaceIncluded(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: only regular files are allowed (symlink or special file)", rel)
		}
		if len(files) >= MaxEntries {
			return errors.New("the package contains too many entries")
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		files = append(files, File{Path: rel, Data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	pkg, issues := build(files)
	if len(issues) > 0 {
		return nil, invalidIssues(issues)
	}
	return pkg, nil
}

// marketplaceFrontmatter parses SKILL.md's YAML frontmatter (AstraForge's _parse_skill_markdown).
func marketplaceFrontmatter(data []byte) (map[string]any, error) {
	text := strings.ReplaceAll(strings.TrimPrefix(string(data), "\ufeff"), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, errors.New("SKILL.md must begin with YAML frontmatter.")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			var meta map[string]any
			if err := yaml.Unmarshal([]byte(quotePlainScalars(lines[1:i])), &meta); err != nil {
				return nil, fmt.Errorf("Invalid SKILL.md frontmatter: %v", err)
			}
			if meta == nil {
				return nil, errors.New("SKILL.md frontmatter must be a YAML mapping.")
			}
			return meta, nil
		}
	}
	return nil, errors.New("SKILL.md frontmatter is not closed.")
}

func metadataBlock(meta map[string]any) map[string]any {
	if nested, ok := meta["metadata"].(map[string]any); ok {
		return nested
	}
	return map[string]any{}
}

// marketplaceTags: frontmatter tags, else metadata.tags (strings, trimmed, non-empty).
func marketplaceTags(meta map[string]any) []string {
	raw, ok := meta["tags"]
	if !ok || raw == nil {
		raw = metadataBlock(meta)["tags"]
	}
	list, ok := raw.([]any)
	if !ok {
		return []string{}
	}
	out := []string{}
	for _, t := range list {
		if s := strings.TrimSpace(pyStr(t)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// marketplaceDeclaredVersion: metadata.version, else version.
func marketplaceDeclaredVersion(meta map[string]any) string {
	v := metadataBlock(meta)["version"]
	if v == nil || pyStr(v) == "" {
		v = meta["version"]
	}
	return strings.TrimSpace(pyStr(v))
}

// marketplaceDisplayName: the frontmatter name, title-cased when it is the directory name.
func marketplaceDisplayName(meta map[string]any, dirName string) string {
	raw := strings.TrimSpace(pyStr(meta["name"]))
	if raw == "" {
		raw = dirName
	}
	if raw == dirName {
		return pyTitle(strings.ReplaceAll(raw, "-", " "))
	}
	return raw
}

// marketplaceName is AstraForge's _marketplace_skill_slug (as a Work Chat name):
// <namespace>-<slugified path>, plus -<8 hex of sha256(namespace/path)> for nested skills.
func marketplaceName(namespace string, skillPath []string) string {
	pathText := strings.Join(skillPath, "/")
	slug := djangoSlugify(strings.ReplaceAll(pathText, "/", "-"))
	if slug == "" {
		slug = skillPath[len(skillPath)-1]
	}
	name := namespace + "-" + slug
	if len(skillPath) > 1 {
		sum := sha256.Sum256([]byte(namespace + "/" + pathText))
		name += "-" + hex.EncodeToString(sum[:])[:8]
	}
	if s := Slug(name); s != name { // Work Chat names are [a-z0-9] runs joined by "-"
		return s
	}
	return name
}

var (
	slugifyStrip = regexp.MustCompile(`[^\w\s-]`)
	slugifyDash  = regexp.MustCompile(`[-\s]+`)
)

// djangoSlugify is django.utils.text.slugify (allow_unicode=False).
func djangoSlugify(value string) string {
	var b strings.Builder
	for _, r := range norm.NFKD.String(value) {
		if r < 128 {
			b.WriteRune(r)
		}
	}
	value = slugifyStrip.ReplaceAllString(strings.ToLower(b.String()), "")
	return strings.Trim(slugifyDash.ReplaceAllString(value, "-"), "-_")
}

// pyTitle is Python's str.title(): a letter after a non-letter is uppercased, the others lowercased.
func pyTitle(s string) string {
	var b strings.Builder
	prevCased := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			if prevCased {
				b.WriteRune(unicode.ToLower(r))
			} else {
				b.WriteRune(unicode.ToUpper(r))
			}
			prevCased = true
			continue
		}
		prevCased = false
		b.WriteRune(r)
	}
	return b.String()
}

// pyStr renders a YAML scalar like Python's str() of PyYAML's value ("" for nil).
func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(t)
	case float64:
		s := strconv.FormatFloat(t, 'f', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		return s
	case time.Time:
		return t.Format("2006-01-02")
	default:
		return fmt.Sprint(t)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
