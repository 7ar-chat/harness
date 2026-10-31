package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/config"
)

func writeTrustFile(t *testing.T, dir, body string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := config.TrustPath(dir)
	if err := os.WriteFile(f, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f, mode); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestCanonicalProject_ResolvesSymlinksAndRelativePaths(t *testing.T) {
	// Arrange
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	wantReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}

	// Act
	viaLink, err1 := config.CanonicalProject(link + "/")
	dotted, err2 := config.CanonicalProject(filepath.Join(real, "sub", ".."))

	// Assert
	if err1 != nil || err2 != nil {
		t.Fatalf("%v %v", err1, err2)
	}
	if viaLink != wantReal || dotted != wantReal {
		t.Fatalf("link %q dotted %q want %q", viaLink, dotted, wantReal)
	}
}

func TestCanonicalProject_NonexistentPathIsKeptAbsoluteAndClean(t *testing.T) {
	// Arrange
	missing := filepath.Join(t.TempDir(), "nope", "..", "gone")

	// Act
	got, err := config.CanonicalProject(missing)

	// Assert
	if err != nil || !filepath.IsAbs(got) || filepath.Base(got) != "gone" || strings.Contains(got, "..") {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestTrust_ReadWithoutDirOrFileIsEmpty(t *testing.T) {
	for name, dir := range map[string]string{"no dir": "", "no file": t.TempDir()} {
		t.Run(name, func(t *testing.T) {
			list, err := config.ReadTrust(dir)
			ok, err2 := config.IsTrusted(dir, t.TempDir())
			if err != nil || err2 != nil || len(list) != 0 || ok {
				t.Fatalf("list %v ok %v errs %v %v", list, ok, err, err2)
			}
		})
	}
}

func TestTrust_InvalidFileIsAnErrorThatNamesTheFile(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	file := writeTrustFile(t, dir, `{"projects": ["/a",`, 0o600)
	proj := t.TempDir()

	// Act
	_, errRead := config.ReadTrust(dir)
	_, errIs := config.IsTrusted(dir, proj)
	_, errAdd := config.AddTrust(dir, proj)
	_, errRm := config.RemoveTrust(dir, proj)
	_, errMCP := config.MCPApproved(dir, proj, config.MCPServer{Name: "x", Command: "c"})
	_, errApprove := config.ApproveMCP(dir, proj, config.MCPServer{Name: "x", Command: "c"})
	_, errRevoke := config.RevokeMCP(dir, proj, "x")

	// Assert
	for i, err := range []error{errRead, errIs, errAdd, errRm, errMCP, errApprove, errRevoke} {
		if err == nil || !strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), "not valid JSON") {
			t.Errorf("call %d: err %v", i, err)
		}
	}
	if got, _ := os.ReadFile(file); string(got) != `{"projects": ["/a",` {
		t.Error("a failed mutation must not overwrite the file")
	}
}

func TestTrust_LooseModeIsRefusedByEveryEntryPoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	// Arrange
	dir := t.TempDir()
	writeTrustFile(t, dir, `{"projects": []}`, 0o664)
	proj := t.TempDir()
	srv := config.MCPServer{Name: "x", Command: "c"}

	// Act
	_, e1 := config.IsTrusted(dir, proj)
	_, e2 := config.AddTrust(dir, proj)
	_, e3 := config.RemoveTrust(dir, proj)
	_, e4 := config.MCPApproved(dir, proj, srv)
	_, e5 := config.ApproveMCP(dir, proj, srv)
	_, e6 := config.RevokeMCP(dir, proj, "x")

	// Assert
	for i, err := range []error{e1, e2, e3, e4, e5, e6} {
		if err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("call %d: err %v", i, err)
		}
	}
}

func TestTrust_EmptyFileIsAnEmptyList(t *testing.T) {
	dir := t.TempDir()
	writeTrustFile(t, dir, "  \n", 0o600)

	list, err := config.ReadTrust(dir)

	if err != nil || len(list) != 0 {
		t.Fatalf("list %v err %v", list, err)
	}
}

func TestTrust_FileThatIsADirectoryIsReportedNotIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(config.TrustPath(dir), 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := config.ReadTrust(dir)

	if err == nil || !strings.Contains(err.Error(), config.TrustFile) {
		t.Fatalf("err %v", err)
	}
}

func TestTrust_StatErrorOtherThanMissingIsReported(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := config.ReadTrust(blocker)

	if err == nil || !strings.Contains(err.Error(), config.TrustFile) {
		t.Fatalf("err %v", err)
	}
}

func TestIsTrusted_EmptyRootIsNeverTrusted(t *testing.T) {
	dir := t.TempDir()
	if _, err := config.AddTrust(dir, t.TempDir()); err != nil {
		t.Fatal(err)
	}

	ok, err := config.IsTrusted(dir, "")

	if ok || err != nil {
		t.Fatalf("ok %v err %v", ok, err)
	}
}

func TestIsTrusted_MatchesThroughSymlinkAndHandEditedEntries(t *testing.T) {
	// Arrange
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	other := t.TempDir()
	dir := t.TempDir()
	// A hand-written entry that is the symlink, not the canonical path.
	writeTrustFile(t, dir, `{"projects": ["`+link+`"]}`, 0o600)

	// Act
	viaReal, err1 := config.IsTrusted(dir, real)
	viaOther, err2 := config.IsTrusted(dir, other)

	// Assert
	if err1 != nil || err2 != nil {
		t.Fatalf("%v %v", err1, err2)
	}
	if !viaReal {
		t.Error("a symlinked entry must trust the real path")
	}
	if viaOther {
		t.Error("an unrelated project must stay untrusted")
	}
}

func TestAddTrust_StoresCanonicalSortedRootsWithoutDuplicates(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	a, b := t.TempDir(), t.TempDir()
	ca, _ := config.CanonicalProject(a)
	cb, _ := config.CanonicalProject(b)

	// Act
	for _, p := range []string{b, a, filepath.Join(a, "x", "..")} {
		if _, err := config.AddTrust(dir, p); err != nil {
			t.Fatal(err)
		}
	}
	list, err := config.ReadTrust(dir)

	// Assert
	if err != nil || len(list) != 2 {
		t.Fatalf("list %v err %v", list, err)
	}
	if !(list[0] < list[1]) {
		t.Errorf("roots must be sorted: %v", list)
	}
	if !(list[0] == ca && list[1] == cb) && !(list[0] == cb && list[1] == ca) {
		t.Errorf("roots must be canonical: %v", list)
	}
}

func TestAddTrust_WithoutUserDirFailsInsteadOfPretendingToTrust(t *testing.T) {
	added, err := config.AddTrust("", t.TempDir())

	if err == nil || added && err == nil {
		t.Fatalf("added %v err %v", added, err)
	}
	if !strings.Contains(err.Error(), "no user directory") {
		t.Errorf("err %v", err)
	}
}

func TestRemoveTrust_AlsoDropsMCPApprovalsOfThatProjectOnly(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	p1, p2 := t.TempDir(), t.TempDir()
	srv := config.MCPServer{Name: "fs", Command: "npx", Args: []string{"x"}}
	for _, p := range []string{p1, p2} {
		if _, err := config.AddTrust(dir, p); err != nil {
			t.Fatal(err)
		}
		if _, err := config.ApproveMCP(dir, p, srv); err != nil {
			t.Fatal(err)
		}
	}

	// Act
	removed, err := config.RemoveTrust(dir, p1)
	ok1, _ := config.MCPApproved(dir, p1, srv)
	ok2, _ := config.MCPApproved(dir, p2, srv)

	// Assert
	if !removed || err != nil {
		t.Fatalf("removed %v err %v", removed, err)
	}
	if ok1 {
		t.Error("approvals must not outlive the trust of their project")
	}
	if !ok2 {
		t.Error("the other project's approval must survive")
	}
}

func TestRemoveTrust_RemovesAHandEditedNonCanonicalEntry(t *testing.T) {
	// Arrange
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	dir := t.TempDir()
	writeTrustFile(t, dir, `{"projects": ["`+link+`", "/does/not/exist/keep"]}`, 0o600)

	// Act
	removed, err := config.RemoveTrust(dir, real)
	list, _ := config.ReadTrust(dir)

	// Assert
	if !removed || err != nil {
		t.Fatalf("removed %v err %v", removed, err)
	}
	if len(list) != 1 || list[0] != "/does/not/exist/keep" {
		t.Fatalf("list %v", list)
	}
}

func TestRemoveTrust_UnlistedProjectDoesNotRewriteTheFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := config.AddTrust(dir, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(config.TrustPath(dir))

	removed, err := config.RemoveTrust(dir, t.TempDir())

	after, _ := os.ReadFile(config.TrustPath(dir))
	if removed || err != nil || string(before) != string(after) {
		t.Fatalf("removed %v err %v", removed, err)
	}
}

func TestRemoveTrust_LastEntryLeavesAnEmptyArrayNotNull(t *testing.T) {
	dir := t.TempDir()
	p := t.TempDir()
	if _, err := config.AddTrust(dir, p); err != nil {
		t.Fatal(err)
	}

	if removed, err := config.RemoveTrust(dir, p); !removed || err != nil {
		t.Fatalf("removed %v err %v", removed, err)
	}

	data, _ := os.ReadFile(config.TrustPath(dir))
	if !strings.Contains(string(data), `"projects": []`) {
		t.Errorf("file should keep a well-formed empty list: %s", data)
	}
}

func TestMCPFingerprint_DependsOnEveryPartOfWhatRuns(t *testing.T) {
	base := config.MCPServer{Name: "fs", Command: "npx", Args: []string{"a", "b"}, Env: map[string]string{"K": "v"}}
	same := config.MCPServer{Name: "different-name", Command: "npx", Args: []string{"a", "b"}, Env: map[string]string{"K": "v"}, URL: "ignored"}
	variants := map[string]config.MCPServer{
		"command":       {Command: "node", Args: []string{"a", "b"}, Env: map[string]string{"K": "v"}},
		"arg value":     {Command: "npx", Args: []string{"a", "c"}, Env: map[string]string{"K": "v"}},
		"arg order":     {Command: "npx", Args: []string{"b", "a"}, Env: map[string]string{"K": "v"}},
		"arg split":     {Command: "npx", Args: []string{"ab"}, Env: map[string]string{"K": "v"}},
		"env value":     {Command: "npx", Args: []string{"a", "b"}, Env: map[string]string{"K": "w"}},
		"env key":       {Command: "npx", Args: []string{"a", "b"}, Env: map[string]string{"J": "v"}},
		"env added":     {Command: "npx", Args: []string{"a", "b"}, Env: map[string]string{"K": "v", "L": "1"}},
		"env removed":   {Command: "npx", Args: []string{"a", "b"}},
		"arg to env":    {Command: "npx", Args: []string{"a", "b", "env"}, Env: map[string]string{"K": "v"}},
		"boundary glue": {Command: "npxa", Args: []string{"b"}, Env: map[string]string{"K": "v"}},
	}

	want := config.MCPFingerprint(base)
	if got := config.MCPFingerprint(same); got != want {
		t.Error("name and URL are not part of what runs")
	}
	for name, v := range variants {
		if config.MCPFingerprint(v) == want {
			t.Errorf("changing the %s must change the fingerprint", name)
		}
	}
	// Map iteration order must not matter.
	multi := config.MCPServer{Command: "c", Env: map[string]string{"A": "1", "B": "2", "C": "3", "D": "4"}}
	first := config.MCPFingerprint(multi)
	for range 20 {
		if config.MCPFingerprint(multi) != first {
			t.Fatal("fingerprint is not deterministic")
		}
	}
}

func TestMCPApproval_IsBoundToProjectNameAndExactCommand(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	proj, other := t.TempDir(), t.TempDir()
	srv := config.MCPServer{Name: "fs", Command: "npx", Args: []string{"-y", "pkg"}}

	// Act
	before, _ := config.MCPApproved(dir, proj, srv)
	added, err := config.ApproveMCP(dir, proj, srv)
	again, err2 := config.ApproveMCP(dir, proj, srv)
	same, _ := config.MCPApproved(dir, proj, srv)
	otherProj, _ := config.MCPApproved(dir, other, srv)
	renamed, _ := config.MCPApproved(dir, proj, config.MCPServer{Name: "other", Command: "npx", Args: []string{"-y", "pkg"}})
	tampered, _ := config.MCPApproved(dir, proj, config.MCPServer{Name: "fs", Command: "npx", Args: []string{"-y", "evil"}})
	noRoot, errNoRoot := config.MCPApproved(dir, "", srv)

	// Assert
	if before || !added || err != nil || again || err2 != nil {
		t.Fatalf("before %v added %v/%v again %v/%v", before, added, err, again, err2)
	}
	if !same {
		t.Error("the approved server must be approved")
	}
	if otherProj || renamed || tampered || noRoot || errNoRoot != nil {
		t.Errorf("approval leaked: otherProj %v renamed %v tampered %v noRoot %v", otherProj, renamed, tampered, noRoot)
	}
}

func TestApproveMCP_ChangedCommandReplacesTheEarlierApproval(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	proj := t.TempDir()
	v1 := config.MCPServer{Name: "fs", Command: "npx", Args: []string{"v1"}}
	v2 := config.MCPServer{Name: "fs", Command: "npx", Args: []string{"v2"}}
	if _, err := config.ApproveMCP(dir, proj, v1); err != nil {
		t.Fatal(err)
	}

	// Act
	changed, err := config.ApproveMCP(dir, proj, v2)
	ok1, _ := config.MCPApproved(dir, proj, v1)
	ok2, _ := config.MCPApproved(dir, proj, v2)

	// Assert
	if !changed || err != nil {
		t.Fatalf("changed %v err %v", changed, err)
	}
	if ok1 || !ok2 {
		t.Errorf("v1 approved %v, v2 approved %v: only the latest approval may stand", ok1, ok2)
	}
	data, _ := os.ReadFile(config.TrustPath(dir))
	if n := strings.Count(string(data), `"server"`); n != 1 {
		t.Errorf("one entry expected, file has %d: %s", n, data)
	}
}

func TestApproveMCP_WithoutUserDirFailsAndWritesNothing(t *testing.T) {
	changed, err := config.ApproveMCP("", t.TempDir(), config.MCPServer{Name: "a", Command: "c"})

	if err == nil || !strings.Contains(err.Error(), "no user directory") {
		t.Fatalf("changed %v err %v", changed, err)
	}
}

func TestRevokeMCP(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	proj, other := t.TempDir(), t.TempDir()
	a := config.MCPServer{Name: "a", Command: "ca"}
	b := config.MCPServer{Name: "b", Command: "cb"}
	for _, s := range []config.MCPServer{a, b} {
		if _, err := config.ApproveMCP(dir, proj, s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := config.ApproveMCP(dir, other, a); err != nil {
		t.Fatal(err)
	}

	// Act
	unknown, errUnknown := config.RevokeMCP(dir, proj, "zzz")
	revoked, err := config.RevokeMCP(dir, proj, "a")
	again, errAgain := config.RevokeMCP(dir, proj, "a")

	// Assert
	if unknown || errUnknown != nil || !revoked || err != nil || again || errAgain != nil {
		t.Fatalf("unknown %v/%v revoked %v/%v again %v/%v", unknown, errUnknown, revoked, err, again, errAgain)
	}
	okA, _ := config.MCPApproved(dir, proj, a)
	okB, _ := config.MCPApproved(dir, proj, b)
	okOtherA, _ := config.MCPApproved(dir, other, a)
	if okA || !okB || !okOtherA {
		t.Errorf("revoke must be scoped to one project and one server: a %v b %v otherProject.a %v", okA, okB, okOtherA)
	}
}

func TestWriteTrust_KeepsApprovalsSortedByProjectThenServer(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	proj := t.TempDir()
	for _, n := range []string{"zeta", "alpha", "mid"} {
		if _, err := config.ApproveMCP(dir, proj, config.MCPServer{Name: n, Command: "c"}); err != nil {
			t.Fatal(err)
		}
	}

	// Act
	data, _ := os.ReadFile(config.TrustPath(dir))

	// Assert
	s := string(data)
	a, m, z := strings.Index(s, `"alpha"`), strings.Index(s, `"mid"`), strings.Index(s, `"zeta"`)
	if !(a >= 0 && a < m && m < z) {
		t.Errorf("approvals must be stored in a stable order (diff-friendly): %s", s)
	}
	if !strings.Contains(s, `"projects": []`) {
		t.Errorf("projects must serialise as [] when only MCP approvals exist: %s", s)
	}
}

func TestTrustEnv_ValueSemantics(t *testing.T) {
	dir, user := repo(t, "permissions:\n  allow: [\"Bash(make*)\"]\n", "")
	_ = dir
	cases := map[string]bool{
		"1": true, "true": true, "TRUE": true, " yes ": true, "on": true, "anything": true,
		"": false, "0": false, "false": false, "No": false, "OFF": false, "  ": false,
	}
	for val, want := range cases {
		t.Run("["+val+"]", func(t *testing.T) {
			cfg, err := load(t, dir, user, config.TrustEnv+"="+val)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Layers.Trusted != want {
				t.Errorf("%s=%q: trusted=%v, want %v", config.TrustEnv, val, cfg.Layers.Trusted, want)
			}
		})
	}
}
