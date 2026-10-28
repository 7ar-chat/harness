package skills

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
)

// SkillCreatorSlug is the Anthropic skill attached to guided skill creation sessions.
const SkillCreatorSlug = "anthropic-skill-creator"

// anthropicManifest is manifest.json of Work's Anthropic fixtures.
type anthropicManifest struct {
	Version string `json:"version"`
	Skills  []struct {
		Slug        string `json:"slug"`
		DisplayName string `json:"display_name"`
		Summary     string `json:"summary"`
		SourcePath  string `json:"source_path"`
		Package     string `json:"package"`
		Version     string `json:"version"`
		VersionID   string `json:"version_id"`
		Checksum    string `json:"checksum"`
	} `json:"skills"`
}

// LoadAnthropic adds the Anthropic skills of fsys (manifest.json +
// packages/<slug>.zip, see work/skills/anthropic): same slugs, names, summaries
// and version ids as Work. An invalid package is logged and skipped; it
// returns how many skills were loaded.
func (r *Registry) LoadAnthropic(fsys fs.FS) (int, error) {
	if fsys == nil {
		return 0, nil
	}
	raw, err := fs.ReadFile(fsys, "manifest.json")
	if err != nil {
		return 0, fmt.Errorf("anthropic skills manifest: %w", err)
	}
	var m anthropicManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, fmt.Errorf("anthropic skills manifest: %w", err)
	}
	for _, e := range m.Skills {
		slug := strings.TrimSpace(e.Slug)
		if !strings.HasPrefix(slug, "anthropic-") || Slug(slug) != slug || r.byName[slug] != nil || r.byAnth[slug] != nil {
			slog.Error("skills: invalid Anthropic skill entry skipped", "slug", slug)
			continue
		}
		data, err := fs.ReadFile(fsys, e.Package)
		if err != nil {
			slog.Error("skills: Anthropic skill package missing", "slug", slug, "error", err)
			continue
		}
		pkg, err := ParseZip(data)
		if err != nil {
			slog.Error("skills: invalid Anthropic skill package skipped", "slug", slug, "error", err)
			continue
		}
		version := e.Version
		if version == "" {
			version = m.Version
		}
		a := &staticSkill{pkg: pkg, slug: slug, displayName: e.DisplayName, summary: strings.TrimSpace(e.Summary),
			sourcePath: e.SourcePath, version: version, versionID: e.VersionID}
		if a.summary == "" {
			a.summary = pkg.Description
		}
		if a.displayName == "" {
			a.displayName = slug
		}
		if a.versionID == "" {
			a.versionID = AnthropicID(slug)
		}
		r.anthropic = append(r.anthropic, a)
		r.byAnth[slug] = a
	}
	sort.Slice(r.anthropic, func(i, j int) bool { return r.anthropic[i].slug < r.anthropic[j].slug })
	return len(r.anthropic), nil
}
