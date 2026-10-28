package bench

import (
	"regexp"
	"sort"
	"strings"
)

// Builtin returns the built-in tasks, by name.
func Builtin() []Task {
	ts := append([]Task{goFixBugs, goAddFeature, pyFixBugs, goRename, codeQuestion, agentsMDCheck}, hardTasks()...)
	ts = append(ts, LongTasks()...)
	for i := range ts {
		ts[i] = distinctHiddenTests(ts[i])
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name })
	return ts
}

// Select returns the named tasks (all when names is empty); unknown names are returned in missing.
func Select(names []string) (tasks []Task, missing []string) {
	all := Builtin()
	if len(names) == 0 {
		return all, nil
	}
	by := map[string]Task{}
	for _, t := range all {
		by[t.Name] = t
	}
	for _, n := range names {
		if t, ok := by[n]; ok {
			tasks = append(tasks, t)
		} else {
			missing = append(missing, n)
		}
	}
	return tasks, missing
}

// FilterTier keeps the tasks of one tier ("easy", "hard" or "long"). An empty tier or "all" keeps the
// easy and hard tasks: the long ones need 50+ steps and are only run when asked for.
func FilterTier(ts []Task, tier string) []Task {
	if tier == "" || tier == "all" {
		var out []Task
		for _, t := range ts {
			if t.TierOf() != TierLong {
				out = append(out, t)
			}
		}
		return out
	}
	var out []Task
	for _, t := range ts {
		if t.TierOf() == tier {
			out = append(out, t)
		}
	}
	return out
}

const noAsking = " Work on your own: do not ask questions."

// ---- go-fix-bugs ----

const statsTest = `package stats

import "testing"

func TestMean(t *testing.T) {
	if got := Mean([]float64{2, 4, 6}); got != 4 {
		t.Fatalf("Mean = %v, want 4", got)
	}
	if got := Mean(nil); got != 0 {
		t.Fatalf("Mean(nil) = %v, want 0", got)
	}
}

func TestMax(t *testing.T) {
	if got := Max([]float64{-5, -2, -9}); got != -2 {
		t.Fatalf("Max = %v, want -2", got)
	}
}

func TestMedian(t *testing.T) {
	in := []float64{9, 1, 5}
	if got := Median(in); got != 5 {
		t.Fatalf("Median = %v, want 5", got)
	}
	if in[0] != 9 || in[1] != 1 || in[2] != 5 {
		t.Fatalf("Median changed its input: %v", in)
	}
	if got := Median([]float64{4, 1, 3, 2}); got != 2.5 {
		t.Fatalf("Median(even) = %v, want 2.5", got)
	}
}
`

var goFixBugs = Task{
	Name:   "go-fix-bugs",
	Prompt: "The tests in this Go module fail. Find the bugs in the code and fix them. Do not change the tests." + noAsking,
	Needs:  []string{"go"},
	Files: map[string]string{
		"go.mod": "module fixbugs\n\ngo 1.21\n",
		"stats/stats.go": `// Package stats has small statistics helpers.
package stats

import "sort"

// Mean is the average of xs (0 for an empty slice).
func Mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0.0
	for i := 1; i < len(xs); i++ {
		sum += xs[i]
	}
	return sum / float64(len(xs))
}

// Max is the largest of xs (0 for an empty slice).
func Max(xs []float64) float64 {
	m := 0.0
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}

// Median is the middle value of xs; xs is left as it was.
func Median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	n := len(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}
`,
		"stats/stats_test.go": statsTest,
	},
	Hidden: map[string]string{"stats/stats_test.go": statsTest},
	Verify: "go test ./...",
	Solution: map[string]string{"stats/stats.go": `// Package stats has small statistics helpers.
package stats

import "sort"

// Mean is the average of xs (0 for an empty slice).
func Mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// Max is the largest of xs (0 for an empty slice).
func Max(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	m := xs[0]
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}

// Median is the middle value of xs; xs is left as it was.
func Median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
`},
}

// ---- go-add-feature ----

const slugTest = `package text

import "testing"

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Hello, World!":        "hello-world",
		"  multiple   spaces ": "multiple-spaces",
		"Already-slug":         "already-slug",
		"---":                  "",
		"":                     "",
		"Go 1.22 rocks":        "go-1-22-rocks",
		"under_score":          "under-score",
	} {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
`

var goAddFeature = Task{
	Name:   "go-add-feature",
	Prompt: "Implement the Slugify function of package text as described in README.md so that the tests pass. Do not change the tests." + noAsking,
	Needs:  []string{"go"},
	Files: map[string]string{
		"go.mod": "module addfeature\n\ngo 1.21\n",
		"README.md": `# text

## Slugify(s string) string

Turns a title into a URL slug: lower-case ASCII letters and digits, with every run of any other
characters replaced by a single "-", and no leading or trailing "-". An input without letters or
digits gives "".
`,
		"text/text.go":      "// Package text has string helpers.\npackage text\n",
		"text/text_test.go": slugTest,
	},
	Hidden: map[string]string{"text/text_test.go": slugTest},
	Verify: "go vet ./... && go test ./...",
	Solution: map[string]string{"text/text.go": `// Package text has string helpers.
package text

import "strings"

// Slugify turns a title into a URL slug.
func Slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		} else {
			dash = true
		}
	}
	return b.String()
}
`},
}

// ---- py-fix-bugs ----

const invTest = `import unittest

from inventory import Inventory


class InventoryTest(unittest.TestCase):
    def test_add_accumulates(self):
        inv = Inventory()
        inv.add("bolt", 5)
        inv.add("bolt", 3)
        self.assertEqual(inv.count("bolt"), 8)

    def test_remove_more_than_stock_fails_and_changes_nothing(self):
        inv = Inventory()
        inv.add("nut", 2)
        with self.assertRaises(ValueError):
            inv.remove("nut", 3)
        self.assertEqual(inv.count("nut"), 2)

    def test_total_value_uses_unit_prices(self):
        inv = Inventory({"bolt": 0.5, "nut": 0.25})
        inv.add("bolt", 4)
        inv.add("nut", 4)
        self.assertAlmostEqual(inv.total_value(), 3.0)

    def test_unknown_item_counts_zero(self):
        self.assertEqual(Inventory().count("x"), 0)


if __name__ == "__main__":
    unittest.main()
`

var pyFixBugs = Task{
	Name:   "py-fix-bugs",
	Prompt: "The unit tests of this Python project fail. Fix the bugs in inventory.py. Do not change the tests." + noAsking,
	Needs:  []string{"python3"},
	Files: map[string]string{
		"inventory.py": `"""A tiny stock keeper."""


class Inventory:
    def __init__(self, prices=None):
        self.stock = {}
        self.prices = prices or {}

    def add(self, item, qty):
        self.stock[item] = qty

    def remove(self, item, qty):
        self.stock[item] = self.stock.get(item, 0) - qty
        if self.stock[item] < 0:
            raise ValueError("not enough " + item)

    def count(self, item):
        return self.stock.get(item, 0)

    def total_value(self):
        return sum(self.stock.values()) * sum(self.prices.values())
`,
		"test_inventory.py": invTest,
	},
	Hidden: map[string]string{"test_inventory.py": invTest},
	Verify: "python3 -m unittest -q",
	Solution: map[string]string{"inventory.py": `"""A tiny stock keeper."""


class Inventory:
    def __init__(self, prices=None):
        self.stock = {}
        self.prices = prices or {}

    def add(self, item, qty):
        self.stock[item] = self.stock.get(item, 0) + qty

    def remove(self, item, qty):
        if self.stock.get(item, 0) < qty:
            raise ValueError("not enough " + item)
        self.stock[item] -= qty

    def count(self, item):
        return self.stock.get(item, 0)

    def total_value(self):
        return sum(qty * self.prices.get(item, 0) for item, qty in self.stock.items())
`},
}

// ---- go-rename ----

var goRename = Task{
	Name: "go-rename",
	Prompt: "Rename the function FetchUserData to LoadUser everywhere in this module: its definition, every call, " +
		"the tests, comments and docs. Behaviour must not change." + noAsking,
	Needs: []string{"go"},
	Files: map[string]string{
		"go.mod":    "module rename\n\ngo 1.21\n",
		"README.md": "# rename\n\nCall FetchUserData(id) to get a user record; the HTTP handler and the CLI both use FetchUserData.\n",
		"user/user.go": `// Package user loads user records.
package user

import "fmt"

// User is a user record.
type User struct {
	ID   int
	Name string
}

// FetchUserData returns the record of id, or an error for an unknown id.
func FetchUserData(id int) (User, error) {
	if id <= 0 {
		return User{}, fmt.Errorf("unknown user %d", id)
	}
	return User{ID: id, Name: fmt.Sprintf("user-%d", id)}, nil
}
`,
		"user/user_test.go": `package user

import "testing"

func TestFetchUserData(t *testing.T) {
	u, err := FetchUserData(7)
	if err != nil || u.Name != "user-7" {
		t.Fatalf("FetchUserData(7) = %+v, %v", u, err)
	}
	if _, err := FetchUserData(0); err == nil {
		t.Fatal("FetchUserData(0) succeeded")
	}
}
`,
		"api/handler.go": `// Package api is the HTTP side.
package api

import (
	"fmt"

	"rename/user"
)

// Greeting is what the handler answers for id. It relies on user.FetchUserData.
func Greeting(id int) string {
	u, err := user.FetchUserData(id)
	if err != nil {
		return "error: " + err.Error()
	}
	return fmt.Sprintf("hello %s", u.Name)
}
`,
		"cmd/cli/main.go": `// Command cli prints a greeting.
package main

import (
	"fmt"

	"rename/user"
)

func main() {
	// FetchUserData never fails for positive ids.
	u, _ := user.FetchUserData(1)
	fmt.Println(u.Name)
}
`,
	},
	Verify: "go vet ./... && go test ./... && ! grep -rn FetchUserData . && grep -rq LoadUser user/user.go",
	Solution: map[string]string{
		"README.md":         "# rename\n\nCall LoadUser(id) to get a user record; the HTTP handler and the CLI both use LoadUser.\n",
		"user/user.go":      "// Package user loads user records.\npackage user\n\nimport \"fmt\"\n\n// User is a user record.\ntype User struct {\n\tID   int\n\tName string\n}\n\n// LoadUser returns the record of id, or an error for an unknown id.\nfunc LoadUser(id int) (User, error) {\n\tif id <= 0 {\n\t\treturn User{}, fmt.Errorf(\"unknown user %d\", id)\n\t}\n\treturn User{ID: id, Name: fmt.Sprintf(\"user-%d\", id)}, nil\n}\n",
		"user/user_test.go": "package user\n\nimport \"testing\"\n\nfunc TestLoadUser(t *testing.T) {\n\tu, err := LoadUser(7)\n\tif err != nil || u.Name != \"user-7\" {\n\t\tt.Fatalf(\"LoadUser(7) = %+v, %v\", u, err)\n\t}\n\tif _, err := LoadUser(0); err == nil {\n\t\tt.Fatal(\"LoadUser(0) succeeded\")\n\t}\n}\n",
		"api/handler.go":    "// Package api is the HTTP side.\npackage api\n\nimport (\n\t\"fmt\"\n\n\t\"rename/user\"\n)\n\n// Greeting is what the handler answers for id. It relies on user.LoadUser.\nfunc Greeting(id int) string {\n\tu, err := user.LoadUser(id)\n\tif err != nil {\n\t\treturn \"error: \" + err.Error()\n\t}\n\treturn fmt.Sprintf(\"hello %s\", u.Name)\n}\n",
		"cmd/cli/main.go":   "// Command cli prints a greeting.\npackage main\n\nimport (\n\t\"fmt\"\n\n\t\"rename/user\"\n)\n\nfunc main() {\n\t// LoadUser never fails for positive ids.\n\tu, _ := user.LoadUser(1)\n\tfmt.Println(u.Name)\n}\n",
	},
}

// ---- code-question ----

var codeQuestion = Task{
	Name: "code-question",
	Prompt: "In this repository, which function enforces the allowlist of email domains, in which file, and what exact error message " +
		"does it return for a domain that is not allowed? Answer with the file path, the function name and the message. Do not change any file." + noAsking,
	Files: map[string]string{
		"go.mod": "module question\n\ngo 1.21\n",
		"signup/signup.go": `// Package signup registers accounts.
package signup

import "question/validate"

// Register creates an account after validating the address.
func Register(email string) error {
	if err := validate.Email(email); err != nil {
		return err
	}
	return nil
}
`,
		"validate/email.go": `// Package validate checks user input.
package validate

import (
	"errors"
	"strings"
)

var errSyntax = errors.New("email address is malformed")

// Email checks the syntax of an address and then its domain.
func Email(s string) error {
	at := strings.LastIndex(s, "@")
	if at < 1 || at == len(s)-1 {
		return errSyntax
	}
	return checkDomain(s[at+1:])
}
`,
		"validate/domains.go": `package validate

import "errors"

var allowed = map[string]bool{"example.com": true, "example.org": true}

// ErrDomain is returned for a domain outside the allowlist.
var ErrDomain = errors.New("email domain is not on the allowlist")

// checkDomain enforces the allowlist of email domains.
func checkDomain(d string) error {
	if !allowed[d] {
		return ErrDomain
	}
	return nil
}
`,
		"validate/phone.go": `package validate

import "errors"

// Phone checks a phone number.
func Phone(s string) error {
	if len(s) < 6 {
		return errors.New("phone number is too short")
	}
	return nil
}
`,
	},
	Answer:         []string{`(?i)domains\.go`, `checkDomain`, `(?i)email domain is not on the allowlist`},
	SolutionAnswer: "validate/domains.go: checkDomain returns ErrDomain, \"email domain is not on the allowlist\".",
}

// ---- agents-md-check ----

const clampTest = `package mathx

import "testing"

func TestClampReferenceCases(t *testing.T) {
	for _, c := range []struct{ v, lo, hi, want int }{
		{5, 0, 10, 5}, {-3, 0, 10, 0}, {42, 0, 10, 10}, {0, 0, 0, 0}, {7, 7, 9, 7}, {9, 7, 9, 9},
	} {
		if got := Clamp(c.v, c.lo, c.hi); got != c.want {
			t.Errorf("Clamp(%d, %d, %d) = %d, want %d", c.v, c.lo, c.hi, got, c.want)
		}
	}
}
`

var agentsMDCheck = Task{
	Name:   "agents-md-check",
	Prompt: "Add a function Clamp(v, lo, hi int) int to package mathx that limits v to the range lo..hi, with tests for it." + noAsking,
	Needs:  []string{"go", "make"},
	Files: map[string]string{
		"go.mod": "module agentsmd\n\ngo 1.21\n",
		"AGENTS.md": `# Notes for agents

- Build and test with: make check  (gofmt, go vet and go test). Run it before you finish.
- Put tests next to the code, in <name>_test.go, as table tests.
- Keep functions small; exported identifiers need a doc comment starting with their name.
`,
		"Makefile": "check:\n\t@test -z \"$$(gofmt -l .)\" || (echo 'gofmt needed:'; gofmt -l .; exit 1)\n\tgo vet ./...\n\tgo test ./...\n",
		"mathx/mathx.go": `// Package mathx has integer helpers.
package mathx

// Abs returns the absolute value of v.
func Abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
`,
		"mathx/mathx_test.go": "package mathx\n\nimport \"testing\"\n\nfunc TestAbs(t *testing.T) {\n\tif Abs(-3) != 3 || Abs(3) != 3 {\n\t\tt.Fatal(\"Abs\")\n\t}\n}\n",
	},
	// The agent's own tests may be named differently; the hidden file adds the reference test.
	Hidden: map[string]string{"mathx/clamp_hidden_test.go": clampTest},
	Verify: "make check && grep -rq 'func Clamp' mathx/mathx.go && grep -l Clamp mathx/*_test.go | grep -qv clamp_hidden",
	Solution: map[string]string{
		"mathx/mathx.go": `// Package mathx has integer helpers.
package mathx

// Abs returns the absolute value of v.
func Abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// Clamp limits v to the range lo..hi.
func Clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
`,
		"mathx/mathx_test.go": "package mathx\n\nimport \"testing\"\n\nfunc TestAbs(t *testing.T) {\n\tif Abs(-3) != 3 || Abs(3) != 3 {\n\t\tt.Fatal(\"Abs\")\n\t}\n}\n\nfunc TestClamp(t *testing.T) {\n\tfor _, c := range []struct{ v, lo, hi, want int }{{5, 0, 10, 5}, {-3, 0, 10, 0}, {42, 0, 10, 10}} {\n\t\tif got := Clamp(c.v, c.lo, c.hi); got != c.want {\n\t\t\tt.Errorf(\"Clamp(%d, %d, %d) = %d, want %d\", c.v, c.lo, c.hi, got, c.want)\n\t\t}\n\t}\n}\n",
	},
}

var goTestFunc = regexp.MustCompile(`(?m)^func (Test\w+)\(`)

// distinctHiddenTests renames the test functions that only the hidden test files have (their name does
// not occur in the visible file of the same path) with a "Hidden" suffix, so a test the agent writes
// under the same name does not break the build with "redeclared in this block".
func distinctHiddenTests(t Task) Task {
	if len(t.Hidden) == 0 {
		return t
	}
	hidden := make(map[string]string, len(t.Hidden))
	for path, content := range t.Hidden {
		if strings.HasSuffix(path, "_test.go") {
			visible := t.Files[path]
			content = goTestFunc.ReplaceAllStringFunc(content, func(m string) string {
				name := goTestFunc.FindStringSubmatch(m)[1]
				if strings.Contains(visible, "func "+name+"(") {
					return m
				}
				return "func " + name + "Hidden("
			})
		}
		hidden[path] = content
	}
	t.Hidden = hidden
	return t
}
