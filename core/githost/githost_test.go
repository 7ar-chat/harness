package githost

import "testing"

func TestParseRepoURL(t *testing.T) {
	cases := []struct {
		in   string
		repo string
		ssh  bool
		ok   bool
	}{
		{"https://github.com/o/r.git", "o/r", false, true},
		{"git@github.com:o/r.git", "o/r", true, true},
		{"https://gitlab.example.com/grp/sub/proj", "gitlab.example.com/grp/sub/proj", false, true},
		{"git@gitlab.example.com:grp/proj.git", "gitlab.example.com/grp/proj", true, true},
		{"ssh://git@git.example.com:2222/a/b.git", "git.example.com:2222/a/b", true, true},
		{"https://Gitea.Example.com:443/a/b/", "gitea.example.com/a/b", false, true},
		{"https://user:pw@gitlab.example.com/a/b", "", false, false},
		{"https://gitlab.example.com/a/../b", "", false, false},
		{"https://github.com/o/r/extra", "", false, false},
		{"ftp://x.example.com/a/b", "", false, false},
		{"", "", false, false},
		{"not a url", "", false, false},
	}
	for _, c := range cases {
		r, ok := ParseRepoURL(c.in)
		if ok != c.ok || (ok && (r.Repo() != c.repo || r.SSH != c.ssh)) {
			t.Errorf("%q: got %q ssh=%v ok=%v, want %q ssh=%v ok=%v", c.in, r.Repo(), r.SSH, ok, c.repo, c.ssh, c.ok)
		}
	}
}

func TestSplitRepo(t *testing.T) {
	if h, p, ok := SplitRepo("o/r"); !ok || h != "" || p != "o/r" {
		t.Fatal(h, p, ok)
	}
	if h, p, ok := SplitRepo("Git.Example.com:8443/a/b/c"); !ok || h != "git.example.com:8443" || p != "a/b/c" {
		t.Fatal(h, p, ok)
	}
	if _, _, ok := SplitRepo("single"); ok {
		t.Fatal("single segment")
	}
}

func TestHostValidate(t *testing.T) {
	h := Host{Name: "GitLab.Example.com", Kind: KindGitLab, KnownHosts: []string{"# c", "gitlab.example.com ssh-ed25519 AAAA"}}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	if h.BaseURL != "https://gitlab.example.com" || h.APIBase() != "https://gitlab.example.com/api/v4" || len(h.KnownHosts) != 1 {
		t.Fatalf("%+v", h)
	}
	if got := h.SSHURL("a/b"); got != "ssh://git@gitlab.example.com:22/a/b.git" {
		t.Fatal(got)
	}
	for _, bad := range []Host{
		{Name: "github.com", Kind: KindGitLab}, {Name: "nodot", Kind: KindGitea}, {Name: "a.example.com", Kind: "x"},
		{Name: "a.example.com", Kind: KindGitea, BaseURL: "https://b.example.com"},
		{Name: "a.example.com", Kind: KindGitea, BaseURL: "https://u:p@a.example.com"},
		{Name: "a.example.com", Kind: KindGitea, KnownHosts: []string{"bad"}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
