package skills_test

import (
	"reflect"
	"testing"

	"github.com/GhaziBenDahmane/harness/skills"
)

// Mirrors Work's ai/cowork/tests/test_skill_policy.py.

func view(slug string, opts ...func(*skills.PolicyView)) skills.PolicyView {
	v := skills.PolicyView{Slug: slug, Scope: "global", PublishedVersionID: "published-version", LatestVersionID: "latest-version"}
	for _, o := range opts {
		o(&v)
	}
	return v
}

func tags(t ...string) func(*skills.PolicyView) { return func(v *skills.PolicyView) { v.Tags = t } }
func scope(s string) func(*skills.PolicyView)   { return func(v *skills.PolicyView) { v.Scope = s } }
func published(id string) func(*skills.PolicyView) {
	return func(v *skills.PolicyView) { v.PublishedVersionID = id }
}
func latest(id string) func(*skills.PolicyView) {
	return func(v *skills.PolicyView) { v.LatestVersionID = id }
}
func source(p string) func(*skills.PolicyView) {
	return func(v *skills.PolicyView) { v.SourcePath = p }
}
func history(ids ...string) func(*skills.PolicyView) {
	return func(v *skills.PolicyView) { v.VersionIDs = ids }
}

func TestPolicyFiltersTaggedGlobalAndPrivateSkills(t *testing.T) {
	// Arrange
	all := []skills.PolicyView{
		view("selectable-exact", tags("studio-work")),
		view("selectable-prefixed", tags(" Studio-Work-Office ")),
		view("default-exact", tags("studio-work-default")),
		view("default-prefixed", tags(" Studio-Work-Default-Office ")),
		view("excluded-global", tags("office")),
		view("personal", scope("private")),
	}

	// Act
	var allowed []string
	var defaults []bool
	for _, s := range all {
		if skills.IsAllowed(s) {
			allowed = append(allowed, s.Slug)
			defaults = append(defaults, skills.IncludedByDefault(s))
		}
	}

	// Assert
	if !reflect.DeepEqual(allowed, []string{"selectable-exact", "selectable-prefixed", "default-exact", "default-prefixed", "personal"}) {
		t.Fatalf("allowed %v", allowed)
	}
	if !reflect.DeepEqual(defaults, []bool{false, false, true, true, false}) {
		t.Fatalf("defaults %v", defaults)
	}
}

func TestPolicyPreservesAnthropicSkillsWithoutDefaultingThem(t *testing.T) {
	// Arrange
	all := []skills.PolicyView{view("anthropic-xlsx"), view("pptx", source("skills/anthropic/pptx"))}

	// Act / Assert
	for _, s := range all {
		if !skills.IsAllowed(s) || skills.IncludedByDefault(s) {
			t.Fatalf("%s allowed=%v default=%v", s.Slug, skills.IsAllowed(s), skills.IncludedByDefault(s))
		}
	}
}

func TestPolicyNeverDefaultsPrivateOrUnpublishedSkills(t *testing.T) {
	// Arrange
	private := view("personal", scope("private"), tags("studio-work-personal"))
	unpublished := view("unpublished", tags("studio-work"), published(""))

	// Act / Assert
	if skills.IncludedByDefault(private) || skills.IncludedByDefault(unpublished) || !skills.IsAllowed(unpublished) {
		t.Fatal("private or unpublished skill included by default")
	}
}

func TestPolicyRejectsMalformedOrNonMatchingDefaultTags(t *testing.T) {
	// Arrange
	cases := [][]string{nil, {}, {"", "office"}, {"studio-workdefault"}}

	// Act / Assert
	for _, c := range cases {
		if skills.IsDefaultSkill(view("malformed", tags(c...))) {
			t.Errorf("tags %q are default", c)
		}
	}
}

func TestPolicyDefaultVersionsRequirePublishedGlobalTaggedSkills(t *testing.T) {
	// Arrange
	all := []skills.PolicyView{
		view("default", tags("studio-work-default"), published("default-version")),
		view("unpublished", tags("studio-work-default-extra"), published("")),
		view("selectable", tags("studio-work-office"), published("selectable-version")),
		view("personal", scope("private"), tags("studio-work-default"), published("private-version")),
	}

	// Act
	got := skills.DefaultVersionIDs(all)

	// Assert
	if !reflect.DeepEqual(got, []string{"default-version"}) {
		t.Fatalf("defaults %v", got)
	}
}

func TestPolicyAllowedVersionsIncludePrivateLatestAndHistoricalVersions(t *testing.T) {
	// Arrange
	all := []skills.PolicyView{view("personal", scope("private"), published(""), latest("private-latest"), history("private-history"))}

	// Act
	got := skills.AllowedVersionIDs(all)

	// Assert
	if !reflect.DeepEqual(got, map[string]bool{"private-latest": true, "private-history": true}) {
		t.Fatalf("allowed %v", got)
	}
}

func TestPolicyMergePlacesDefaultsFirstAndDeduplicatesStably(t *testing.T) {
	// Act
	got := skills.MergeVersionIDs([]string{"default-1", "shared", ""}, []string{"shared", "private-1", "default-1"})

	// Assert
	if !reflect.DeepEqual(got, []string{"default-1", "shared", "private-1"}) {
		t.Fatalf("merged %v", got)
	}
}
