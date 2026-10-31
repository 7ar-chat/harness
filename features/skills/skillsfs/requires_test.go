package skillsfs_test

import (
	"context"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/features/skills"
	skillsfs "github.com/7ar-chat/harness/features/skills/skillsfs"
)

// Lazy skills: a sandbox gets only the skills a run needs, each with the
// skills its frontmatter "requires". A built-in whose scripts (or documented
// imports) use another skill's files must declare it, so installing it alone
// never breaks. (None of the built-ins depends on another today.)

var (
	// An absolute skill path: /skills/<name>/... (also a sys.path entry).
	absSkillRef = regexp.MustCompile(`/skills/([a-z0-9]+(?:-[a-z0-9]+)*)/`)
	// A sibling skill folder next to the script: os.path.join(HERE, "..", "..", "<name>", "scripts").
	siblingRef = regexp.MustCompile(`"\.\.",\s*"\.\.",\s*"([a-z0-9]+(?:-[a-z0-9]+)*)"`)
	// A documented import of another skill's helpers: sys.path.insert(0, "/skills/<name>/scripts").
	importRef = regexp.MustCompile(`sys\.path\.insert\(0,\s*"/skills/([a-z0-9]+(?:-[a-z0-9]+)*)/scripts"\)`)
)

func TestEveryBuiltinDeclaresTheSkillsItsScriptsUse(t *testing.T) {
	// Each built-in's scripts and documented imports use only that skill and the skills its frontmatter "requires" (which exist).
	// Arrange
	reg := skills.NewRegistry(skillsfs.Builtin(), memstore.New())
	builtin := skillsfs.Builtin()
	names, _ := fs.Glob(builtin, "*/SKILL.md")

	// Act
	used := map[string][]string{}
	requires := map[string][]string{}
	for _, md := range names {
		name := strings.TrimSuffix(md, "/SKILL.md")
		_, pkg, err := reg.ByName(context.Background(), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		requires[name] = pkg.Requires
		for _, u := range pkg.Uses { // implicit dependencies (installed anyway) are declared too
			if !slices.Contains(used[name], u) {
				used[name] = append(used[name], u)
			}
		}
		for _, f := range pkg.Files {
			text := string(f.Data)
			var refs [][]string
			switch {
			case strings.HasPrefix(f.Path, "scripts/"):
				refs = append(absSkillRef.FindAllStringSubmatch(text, -1), siblingRef.FindAllStringSubmatch(text, -1)...)
			default:
				refs = importRef.FindAllStringSubmatch(text, -1)
			}
			for _, m := range refs {
				if m[1] != name && !slices.Contains(used[name], m[1]) {
					used[name] = append(used[name], m[1])
				}
			}
		}
	}

	// Assert
	for name, deps := range used {
		for _, d := range deps {
			if !slices.Contains(requires[name], d) {
				t.Errorf("%s uses /skills/%s/ but does not declare it (requires: %v)", name, d, requires[name])
			}
		}
	}
	for name, deps := range requires {
		for _, d := range deps {
			if _, ok := requires[d]; !ok {
				t.Errorf("%s requires %q, which is not a built-in skill", name, d)
			}
		}
	}
}
