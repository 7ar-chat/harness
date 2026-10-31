package projectdocs

import "testing"

func TestCursorGlobsMatchLikeGitignoreStyleGlobs(t *testing.T) {
	cases := []struct {
		glob string
		hit  []string
		miss []string
	}{
		{"src/**/*.go", []string{"src/a.go", "src/x/y/b.go"}, []string{"lib/a.go", "src/a.ts", "other/src/a.go"}},
		{"*.md", []string{"README.md", "docs/deep/guide.md"}, []string{"README.mdx", "md"}},
		{"./cmd/*", []string{"cmd/main.go"}, []string{"cmd/sub/main.go", "pkg/cmd/main.go"}},
		{"file?.txt", []string{"file1.txt", "docs/fileA.txt"}, []string{"file12.txt", "file.txt"}},
		{"web/**", []string{"web/a.ts", "web/x/y.ts"}, []string{"webx/a.ts"}},
		{"**/*.{ts,tsx}", []string{"a.ts", "ui/b.tsx"}, []string{"a.js", "ui/b.tsxx"}},
		{"docs/{api,guide}/*.md", []string{"docs/api/a.md", "docs/guide/b.md"}, []string{"docs/other/c.md"}},
		{"a.b+c", []string{"a.b+c", "x/a.b+c"}, []string{"aXb+c", "a.bbc"}},
		{"unclosed.{go,rs", []string{"unclosed.go", "unclosed.rs"}, []string{"unclosed.py"}},
		{"stray}.txt", []string{"stray}.txt"}, nil},
		{"a,b", []string{"a,b"}, nil}, // a bare comma inside a single glob is literal
	}
	for _, tc := range cases {
		t.Run(tc.glob, func(t *testing.T) {
			// Arrange
			r := parseMDC("---\nglobs: [\""+tc.glob+"\"]\n---\nbody", false)

			// Act / Assert
			for _, p := range tc.hit {
				if !r.matches(p) {
					t.Errorf("%q must cover %s", tc.glob, p)
				}
			}
			for _, p := range tc.miss {
				if r.matches(p) {
					t.Errorf("%q must not cover %s", tc.glob, p)
				}
			}
		})
	}
}

func TestCursorGlobHeadersInAllTheirYamlForms(t *testing.T) {
	cases := []struct {
		name, header string
		globs        []string
		match, miss  string
	}{
		{"comma separated string", `globs: "*.go, {a,b}/*.rs"`, []string{"*.go", "{a,b}/*.rs"}, "b/x.rs", "c/x.rs"},
		{"comma inside braces is not a separator", `globs: "src/{x,y}/*.go"`, []string{"src/{x,y}/*.go"}, "src/y/a.go", "src/z/a.go"},
		{"unquoted string", `globs: *.go`, nil, "", ""},
		{"quoted pieces", `globs: "'*.py', \"*.rb\""`, []string{"*.py", "*.rb"}, "deep/x.rb", "x.js"},
		{"list with blanks and non-strings", "globs:\n  - '*.py'\n  - \" *.rb \"\n  - 7\n  - \"\"", []string{"*.py", "*.rb"}, "x.py", "x.go"},
		{"a number is no glob", `globs: 42`, nil, "", "42"},
		{"empty string", `globs: ""`, nil, "", "x.go"},
		{"no globs key", `description: manual`, nil, "", "x.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			r := parseMDC("---\n"+tc.header+"\n---\nbody", false)

			// Assert
			if tc.globs != nil && (len(r.globs) != len(tc.globs) || r.globs[0] != tc.globs[0] || r.globs[len(r.globs)-1] != tc.globs[len(tc.globs)-1]) {
				t.Errorf("globs = %q, want %q", r.globs, tc.globs)
			}
			if tc.match != "" && !r.matches(tc.match) {
				t.Errorf("must cover %s", tc.match)
			}
			if tc.miss != "" && r.matches(tc.miss) {
				t.Errorf("must not cover %s", tc.miss)
			}
		})
	}
}

func TestParseMDCReadsTheHeaderAndKeepsTheBody(t *testing.T) {
	cases := []struct {
		name, text string
		cut        bool
		always     bool
		body       string
	}{
		{"always apply", "---\nalwaysApply: true\n---\nBody here\n", false, true, "Body here"},
		{"no header", "just a body", false, false, "just a body"},
		{"unclosed header is body", "---\nglobs: x\nno end", false, false, "---\nglobs: x\nno end"},
		{"unreadable header keeps the body, no flags", "---\nalwaysApply: [oops\n---\nthe body", false, false, "the body"},
		{"cut file", "---\nalwaysApply: true\n---\nlong", true, true, "long\n[…cut: the rule file is longer]"},
		{"invalid utf-8 dropped", "---\n\n---\nab\xffcd", false, false, "abcd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			r := parseMDC(tc.text, tc.cut)

			// Assert
			if r.always != tc.always || r.body != tc.body {
				t.Errorf("always=%v body=%q, want always=%v body=%q", r.always, r.body, tc.always, tc.body)
			}
		})
	}
}
