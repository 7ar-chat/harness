package skills

import "strings"

// Skill attachment policy, ported from Work (ai/cowork/skill_policy.py):
//   - allowed: Anthropic skills (slug "anthropic-*" or source path
//     "skills/anthropic/*"), global skills tagged "studio-work*" and the user's
//     private skills; anything else is rejected with DeniedMessage;
//   - included by default: global skills tagged "studio-work-default*" that
//     have a published version (attached to every run, merged first with the
//     user's selection).
//
// Work Chat's built-in skills are global skills tagged "studio-work-default"
// with one published version (they were always on by default): see README.
const (
	CoworkTagPrefix        = "studio-work"
	DefaultCoworkTagPrefix = "studio-work-default"
	// DeniedMessage is Work's SKILL_ATTACHMENT_DENIED_MESSAGE.
	DeniedMessage = "Only approved Cowork skills can be attached in Studio."
)

var (
	anthropicSlugPrefixes       = []string{"anthropic-"}
	anthropicSourcePathPrefixes = []string{"skills/anthropic/"}
)

// Scopes.
const (
	ScopeGlobal  = "global"
	ScopePrivate = "private"
)

// PolicyView is what the policy looks at (Work's skill mapping).
type PolicyView struct {
	Scope      string
	Slug       string
	SourcePath string
	Tags       []string
	// PublishedVersionID, LatestVersionID and VersionIDs are "" / empty when absent.
	PublishedVersionID string
	LatestVersionID    string
	VersionIDs         []string
}

func globalWithTagPrefix(s PolicyView, prefix string) bool {
	if strings.ToLower(strings.TrimSpace(s.Scope)) != ScopeGlobal {
		return false
	}
	for _, t := range s.Tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" && strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}

// IsCatalogSkill: a global skill tagged studio-work*.
func IsCatalogSkill(s PolicyView) bool { return globalWithTagPrefix(s, CoworkTagPrefix) }

// IsAnthropicSkill: slug anthropic-* or source path skills/anthropic/*.
func IsAnthropicSkill(s PolicyView) bool {
	slug := strings.ToLower(strings.TrimSpace(s.Slug))
	src := strings.ToLower(strings.TrimSpace(s.SourcePath))
	for _, p := range anthropicSlugPrefixes {
		if strings.HasPrefix(slug, p) {
			return true
		}
	}
	for _, p := range anthropicSourcePathPrefixes {
		if strings.HasPrefix(src, p) {
			return true
		}
	}
	return false
}

// IsDefaultSkill: a global skill tagged studio-work-default*.
func IsDefaultSkill(s PolicyView) bool { return globalWithTagPrefix(s, DefaultCoworkTagPrefix) }

// IsPersonalSkill: a private skill.
func IsPersonalSkill(s PolicyView) bool {
	return strings.ToLower(strings.TrimSpace(s.Scope)) == ScopePrivate
}

// IsAllowed is is_allowed_cowork_skill.
func IsAllowed(s PolicyView) bool {
	return IsAnthropicSkill(s) || IsCatalogSkill(s) || IsPersonalSkill(s)
}

// IncludedByDefault: a default skill with a published version.
func IncludedByDefault(s PolicyView) bool {
	return IsDefaultSkill(s) && strings.TrimSpace(s.PublishedVersionID) != ""
}

// DefaultVersionIDs returns the published versions of the default skills
// (default_cowork_skill_version_ids).
func DefaultVersionIDs(views []PolicyView) []string {
	var ids []string
	for _, s := range views {
		if IsDefaultSkill(s) {
			ids = append(ids, s.PublishedVersionID)
		}
	}
	return MergeVersionIDs(nil, ids)
}

// MergeVersionIDs places the defaults first and deduplicates stably, dropping
// blank ids (merge_cowork_skill_version_ids).
func MergeVersionIDs(defaults, selected []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range append(append([]string(nil), defaults...), selected...) {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// AllowedVersionIDs returns every version id of the allowed skills (published,
// latest and historical: allowed_cowork_skill_version_ids).
func AllowedVersionIDs(views []PolicyView) map[string]bool {
	out := map[string]bool{}
	for _, s := range views {
		if !IsAllowed(s) {
			continue
		}
		for _, id := range append([]string{s.PublishedVersionID, s.LatestVersionID}, s.VersionIDs...) {
			if id = strings.TrimSpace(id); id != "" {
				out[id] = true
			}
		}
	}
	return out
}
