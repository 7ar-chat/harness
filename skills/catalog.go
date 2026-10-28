package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/GhaziBenDahmane/harness/store"
)

var (
	// ErrForbidden means the user may not change the skill (not its owner, not an admin).
	ErrForbidden = errors.New("not allowed to change this skill")
	// ErrVersionExists means the version label is already used by the skill.
	ErrVersionExists = errors.New("this version already exists")
	// ErrNotAllowed means a skill version outside the policy was requested (DeniedMessage).
	ErrNotAllowed = errors.New(DeniedMessage)
	// ErrCatalogUnavailable means the store has no catalog (versions, global skills).
	ErrCatalogUnavailable = errors.New("the skill catalog is unavailable")
)

// Limits of a draft batch (Work: 100 generated roots, 64 MiB in total).
const (
	MaxDraftBatchPackages = 100
	MaxDraftBatchBytes    = 64 << 20
	MaxVersionLabelChars  = 64
	MaxTags               = 20
	MaxTagChars           = 64
	// DraftVersion is the version of new private drafts (Work: 0.1.0, publish=false).
	DraftVersion = "0.1.0"
)

// UnknownSkillError is a run request naming a skill the user does not have.
type UnknownSkillError struct{ Name string }

func (e *UnknownSkillError) Error() string { return "Unknown skill " + strconv.Quote(e.Name) + "." }

func (r *Registry) catalog() (store.SkillCatalogStore, error) {
	if r.cat == nil {
		return nil, ErrCatalogUnavailable
	}
	return r.cat, nil
}

// Versions returns the versions of a skill (newest first).
func (r *Registry) Versions(ctx context.Context, login, id string, admin bool) ([]VersionInfo, error) {
	s, _, err := r.find(ctx, login, id, admin)
	if err != nil {
		return nil, err
	}
	return s.Versions, nil
}

// NextVersion proposes the label after the latest one: the minor of a
// "major.minor.patch" label bumped (0.1.0 → 0.2.0), else DraftVersion.
func NextVersion(s Skill) string {
	if s.Latest == nil {
		return DraftVersion
	}
	parts := strings.Split(strings.SplitN(s.Latest.Version, "-", 2)[0], ".")
	if len(parts) == 3 {
		major, e1 := strconv.Atoi(parts[0])
		minor, e2 := strconv.Atoi(parts[1])
		if e1 == nil && e2 == nil {
			return fmt.Sprintf("%d.%d.0", major, minor+1)
		}
	}
	return DraftVersion
}

func cleanLabel(label string) (string, error) {
	label = strings.TrimSpace(label)
	if label == "" || len([]rune(label)) > MaxVersionLabelChars || strings.ContainsAny(label, "\x00\n\r") {
		return "", invalid("invalid_version: The version must be 1 to 64 characters.")
	}
	return label, nil
}

// editable returns the skill when the user may add versions to it.
func (r *Registry) editable(ctx context.Context, login, id string, admin bool) (Skill, error) {
	s, _, err := r.find(ctx, login, id, admin)
	if err != nil {
		return Skill{}, err
	}
	switch {
	case s.Source == SourceUser:
		return s, nil
	case s.Source == SourceGlobal && admin:
		return s, nil
	default:
		return Skill{}, ErrForbidden
	}
}

func versionOf(pkg *Package, label, sourceType, login string) (store.SkillVersion, error) {
	canonical, err := pkg.Zip()
	if err != nil {
		return store.SkillVersion{}, err
	}
	return store.SkillVersion{Version: label, SourceType: sourceType, Name: pkg.Name, Description: pkg.Description,
		SHA256: pkg.Revision, Package: canonical, Files: len(pkg.Files), SizeBytes: pkg.Size, CreatedBy: login}, nil
}

// PackageVersion is the store version of a validated package (imports).
func PackageVersion(pkg *Package, label, sourceType, login string) (store.SkillVersion, error) {
	label, err := cleanLabel(label)
	if err != nil {
		return store.SkillVersion{}, err
	}
	return versionOf(pkg, label, sourceType, login)
}

// Reserved reports a name taken by a built-in or an Anthropic skill.
func (r *Registry) Reserved(name string) bool { return r.byName[name] != nil || r.byAnth[name] != nil }

// AddEditorVersion creates a version from the SKILL.md editor (Work's
// SkillVersionCreate: source editor, publish defaults to true). The other files
// of the current version are kept (Work's editor versions only hold SKILL.md).
func (r *Registry) AddEditorVersion(ctx context.Context, login, id, label, skillMD string, publish, admin bool) (Skill, error) {
	cat, err := r.catalog()
	if err != nil {
		return Skill{}, err
	}
	s, err := r.editable(ctx, login, id, admin)
	if err != nil {
		return Skill{}, err
	}
	if strings.TrimSpace(label) == "" {
		label = NextVersion(s)
	}
	if label, err = cleanLabel(label); err != nil {
		return Skill{}, err
	}
	base, err := r.packageOf(ctx, login, s, "")
	if err != nil {
		return Skill{}, err
	}
	files := []File{{Path: "SKILL.md", Data: []byte(skillMD)}}
	for _, f := range base.Files {
		if f.Path != "SKILL.md" {
			files = append(files, f)
		}
	}
	return r.addVersion(ctx, cat, login, s, files, label, store.SkillSourceEditor, publish)
}

// AddPackageVersion creates a version of a global skill from a zip (admin).
func (r *Registry) AddPackageVersion(ctx context.Context, login, id, label string, data []byte, publish bool) (Skill, error) {
	cat, err := r.catalog()
	if err != nil {
		return Skill{}, err
	}
	s, err := r.editable(ctx, login, id, true)
	if err != nil {
		return Skill{}, err
	}
	if strings.TrimSpace(label) == "" {
		label = NextVersion(s)
	}
	if label, err = cleanLabel(label); err != nil {
		return Skill{}, err
	}
	files, iss := readZip(data)
	if iss != nil {
		return Skill{}, invalidIssues([]Issue{*iss})
	}
	return r.addVersion(ctx, cat, login, s, reroot(files), label, store.SkillSourceUpload, publish)
}

func (r *Registry) addVersion(ctx context.Context, cat store.SkillCatalogStore, login string, s Skill, files []File, label, sourceType string, publish bool) (Skill, error) {
	pkg, issues := build(files)
	if len(issues) > 0 {
		return Skill{}, invalidIssues(issues)
	}
	v, err := versionOf(pkg, label, sourceType, login)
	if err != nil {
		return Skill{}, err
	}
	v.SkillID, v.Global = s.ID, s.Source == SourceGlobal
	if _, err := cat.AddSkillVersion(ctx, login, v, publish); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return Skill{}, ErrVersionExists
		}
		return Skill{}, err
	}
	out, _, err := r.find(ctx, login, s.ID, v.Global)
	return out, err
}

// PublishVersion makes a version the published one (owner of a private skill, admin for a global one).
func (r *Registry) PublishVersion(ctx context.Context, login, id, versionID string, admin bool) (Skill, error) {
	cat, err := r.catalog()
	if err != nil {
		return Skill{}, err
	}
	s, err := r.editable(ctx, login, id, admin)
	if err != nil {
		return Skill{}, err
	}
	global := s.Source == SourceGlobal
	if versionID == "" && s.Latest != nil {
		versionID = s.Latest.ID
	}
	if err := cat.PublishSkillVersion(ctx, login, s.ID, versionID, global); err != nil {
		return Skill{}, err
	}
	out, _, err := r.find(ctx, login, s.ID, global)
	return out, err
}

// DraftPackage is one validated package of a draft batch.
type DraftPackage struct {
	Package    *Package
	SourceType string
	// Root is the generated folder (informational, part of the request hash).
	Root string
}

type draftResponse struct {
	Items           []draftItem `json:"items"`
	SkillCount      int         `json:"skillCount"`
	SkillID         string      `json:"skillId,omitempty"`
	VersionID       string      `json:"versionId,omitempty"`
	Scope           string      `json:"scope"`
	Status          string      `json:"status"`
	PackageRevision string      `json:"packageRevision"`
}

type draftItem struct {
	SkillID   string `json:"skillId"`
	VersionID string `json:"versionId"`
	Name      string `json:"name"`
}

// CreateDrafts atomically saves every package as a new private draft skill at
// version 0.1.0 (publish=false), like Work's drafts batch: the response is
// stored under the idempotency key (command) and replayed for the same request
// (another request with that key is store.ErrIdempotencyConflict). Names get a
// "-2", "-3"… suffix when already used. It returns the JSON response.
func (r *Registry) CreateDrafts(ctx context.Context, login, command, key, revision string, pkgs []DraftPackage) ([]byte, error) {
	cat, err := r.catalog()
	if err != nil {
		return nil, err
	}
	if len(pkgs) == 0 || len(pkgs) > MaxDraftBatchPackages {
		return nil, invalid(fmt.Sprintf("invalid_batch: A draft batch holds 1 to %d packages.", MaxDraftBatchPackages))
	}
	var total int64
	hash := sha256.New()
	hash.Write([]byte(revision + "\x00"))
	for _, p := range pkgs {
		total += p.Package.Size
		hash.Write([]byte(p.Root + "\x00" + p.SourceType + "\x00" + p.Package.Revision + "\x00"))
	}
	if total > MaxDraftBatchBytes {
		return nil, invalid("package_too_large: The draft batch exceeds the 64 MiB limit.")
	}
	idem := store.Idempotency{Login: login, Command: command, Key: key, RequestHash: hex.EncodeToString(hash.Sum(nil))}
	if r, err := cat.CommandResult(ctx, login, command, key); err == nil {
		if r.RequestHash != idem.RequestHash {
			return nil, store.ErrIdempotencyConflict
		}
		return r.Response, nil
	}
	respond := func(items []store.DraftItem) []byte {
		resp := draftResponse{Items: make([]draftItem, 0, len(items)), SkillCount: len(items), Scope: ScopePrivate,
			Status: store.SkillDraft, PackageRevision: revision}
		for _, it := range items {
			resp.Items = append(resp.Items, draftItem{SkillID: it.SkillID, VersionID: it.VersionID, Name: it.Name})
		}
		if len(items) == 1 {
			resp.SkillID, resp.VersionID = items[0].SkillID, items[0].VersionID
		}
		b, _ := json.Marshal(resp)
		return b
	}
	for attempt := 0; ; attempt++ {
		drafts, err := r.draftsOf(ctx, login, pkgs)
		if err != nil {
			return nil, err
		}
		body, _, err := cat.CreateSkillDrafts(ctx, idem, drafts, respond)
		if errors.Is(err, store.ErrConflict) && attempt < 2 {
			continue // a name was taken concurrently: pick new suffixes
		}
		return body, err
	}
}

func (r *Registry) draftsOf(ctx context.Context, login string, pkgs []DraftPackage) ([]store.NewSkillDraft, error) {
	v, err := r.view(ctx, login, false)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, u := range v.users {
		used[u.Name] = true
	}
	for _, g := range v.globals {
		used[g.Name] = true // drafts get a fresh name rather than shadowing a global skill
	}
	drafts := make([]store.NewSkillDraft, 0, len(pkgs))
	for _, p := range pkgs {
		base := Slug(p.Package.Name)
		name := base
		for n := 2; used[name] || r.reserved(name); n++ {
			suffix := "-" + strconv.Itoa(n)
			name = strings.TrimRight(base[:min(len(base), MaxSlugChars-len(suffix))], "-") + suffix
		}
		used[name] = true
		ver, err := versionOf(p.Package, DraftVersion, p.SourceType, login)
		if err != nil {
			return nil, err
		}
		drafts = append(drafts, store.NewSkillDraft{Name: name, DisplayName: p.Package.Name, Description: p.Package.Description, Version: ver})
	}
	return drafts, nil
}

// CleanTags trims, deduplicates and bounds tags.
func CleanTags(tags []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		if len([]rune(t)) > MaxTagChars || strings.ContainsAny(t, "\x00\n\r,") {
			return nil, invalid(fmt.Sprintf("invalid_tags: A tag is 1 to %d characters without commas.", MaxTagChars))
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	if len(out) > MaxTags {
		return nil, invalid(fmt.Sprintf("invalid_tags: At most %d tags.", MaxTags))
	}
	return out, nil
}

// CreateGlobal publishes a zip package as a global skill (admin).
func (r *Registry) CreateGlobal(ctx context.Context, login string, data []byte, tags []string, label string, publish bool) (Skill, error) {
	pkg, err := ParseZip(data)
	if err != nil {
		return Skill{}, err
	}
	return r.createGlobal(ctx, login, pkg, tags, label, store.SkillSourceUpload, publish)
}

// PromoteToGlobal copies a version of one of the admin's private skills into a new global skill.
func (r *Registry) PromoteToGlobal(ctx context.Context, login, skillID, versionID string, tags []string, publish bool) (Skill, error) {
	s, _, err := r.find(ctx, login, skillID, false)
	if err != nil {
		return Skill{}, err
	}
	if s.Source != SourceUser {
		return Skill{}, ErrForbidden
	}
	pkg, err := r.packageOf(ctx, login, s, versionID)
	if err != nil {
		return Skill{}, err
	}
	label := s.VersionLabel
	for _, v := range s.Versions {
		if v.ID == versionID {
			label = v.Version
		}
	}
	return r.createGlobal(ctx, login, pkg, tags, label, store.SkillSourceUpload, publish)
}

func (r *Registry) createGlobal(ctx context.Context, login string, pkg *Package, tags []string, label, sourceType string, publish bool) (Skill, error) {
	cat, err := r.catalog()
	if err != nil {
		return Skill{}, err
	}
	if strings.TrimSpace(label) == "" {
		label = "1.0.0"
	}
	if label, err = cleanLabel(label); err != nil {
		return Skill{}, err
	}
	if tags, err = CleanTags(tags); err != nil {
		return Skill{}, err
	}
	name := Slug(pkg.Name)
	if r.byName[name] != nil || r.byAnth[name] != nil {
		return Skill{}, invalid(fmt.Sprintf("name_conflict: %q is the name of a Studio skill; choose another name.", name))
	}
	v, err := versionOf(pkg, label, sourceType, login)
	if err != nil {
		return Skill{}, err
	}
	g, _, err := cat.CreateGlobalSkill(ctx, store.GlobalSkill{Name: name, DisplayName: pkg.Name, Description: pkg.Description,
		Tags: tags, CreatedBy: login}, v, publish)
	if errors.Is(err, store.ErrConflict) {
		return Skill{}, invalid(fmt.Sprintf("name_conflict: a global skill named %q already exists; add a version to it instead.", name))
	}
	if err != nil {
		return Skill{}, err
	}
	out, _, err := r.find(ctx, login, g.ID, true)
	return out, err
}

func (r *Registry) globalOp(ctx context.Context, login, id string, op func(store.SkillCatalogStore) error) (Skill, error) {
	cat, err := r.catalog()
	if err != nil {
		return Skill{}, err
	}
	s, _, err := r.find(ctx, login, id, true)
	if err != nil {
		return Skill{}, err
	}
	if s.Source != SourceGlobal {
		return Skill{}, ErrForbidden
	}
	if err := op(cat); err != nil {
		return Skill{}, err
	}
	out, _, err := r.find(ctx, login, id, true)
	return out, err
}

// SetGlobalTags replaces a global skill's tags (admin).
func (r *Registry) SetGlobalTags(ctx context.Context, login, id string, tags []string) (Skill, error) {
	tags, err := CleanTags(tags)
	if err != nil {
		return Skill{}, err
	}
	return r.globalOp(ctx, login, id, func(cat store.SkillCatalogStore) error { return cat.SetGlobalSkillTags(ctx, id, tags) })
}

// UnpublishGlobal removes the published version of a global skill (admin).
func (r *Registry) UnpublishGlobal(ctx context.Context, login, id string) (Skill, error) {
	return r.globalOp(ctx, login, id, func(cat store.SkillCatalogStore) error { return cat.UnpublishGlobalSkill(ctx, id) })
}

// DeleteGlobal deletes a global skill and its versions (admin).
func (r *Registry) DeleteGlobal(ctx context.Context, login, id string) error {
	_, err := r.globalOp(ctx, login, id, func(cat store.SkillCatalogStore) error { return cat.DeleteGlobalSkill(ctx, id) })
	if errors.Is(err, ErrNotFound) {
		// globalOp re-reads the skill after the operation: gone is success.
		if _, _, ferr := r.find(ctx, login, id, true); errors.Is(ferr, ErrNotFound) {
			return nil
		}
	}
	return err
}

// ResolveRunSkills validates the skills requested for a run: names of allowed
// skills, and version ids that Work's policy allows (ErrNotAllowed otherwise).
// It returns the entries stored in work_runs.skills: "name" (effective
// version at run time) or "name@versionId".
func (r *Registry) ResolveRunSkills(ctx context.Context, login string, names, versionIDs []string) ([]string, error) {
	v, err := r.view(ctx, login, false)
	if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	add := func(e string) {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if _, ok := lookupName(v, n); !ok {
			return nil, &UnknownSkillError{Name: n}
		}
		add(n)
	}
	if len(versionIDs) == 0 {
		return out, nil
	}
	all := v.all()
	views := make([]PolicyView, 0, len(all))
	for _, s := range all {
		views = append(views, s.Policy())
	}
	allowed := AllowedVersionIDs(views)
	for _, id := range versionIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !allowed[id] {
			return nil, ErrNotAllowed
		}
		for _, s := range all {
			if s.Allowed && hasVersion(s, id) {
				add(s.Name + "@" + id)
				break
			}
		}
	}
	return out, nil
}

func hasVersion(s Skill, id string) bool {
	for _, v := range s.Versions {
		if v.ID == id {
			return true
		}
	}
	return false
}
