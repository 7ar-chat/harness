package client

import "testing"

func TestConfineKeepsPathsInsideTheWorkspace(t *testing.T) {
	for in, want := range map[string]string{
		"":                      "/workspace",
		"a/b.txt":               "/workspace/a/b.txt",
		"/workspace/a/../b.txt": "/workspace/b.txt",
		"/workspace/":           "/workspace",
		"a/../../etc/passwd":    "",
		"/workspace/../etc":     "",
		"/etc/passwd":           "",
		"/workspace2/x":         "",
		"a\x00b":                "",
	} {
		got, ok := confine(in)
		if (want == "") == ok || got != want {
			t.Errorf("confine(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}
