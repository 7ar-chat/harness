//go:build unix

package local_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/sandbox"
)

// result is the captured outcome of one Exec.
type result struct {
	stdout, stderr string
	code           int
}

// execIn runs req and captures both streams (callbacks may run on another goroutine).
func execIn(t *testing.T, sb sandbox.Sandbox, req sandbox.ExecRequest) result {
	t.Helper()
	var mu sync.Mutex
	var out, errs strings.Builder
	req.OnStdout = func(s string) { mu.Lock(); out.WriteString(s); mu.Unlock() }
	req.OnStderr = func(s string) { mu.Lock(); errs.WriteString(s); mu.Unlock() }
	res, err := sb.Exec(context.Background(), req)
	if err != nil {
		t.Fatalf("Exec(%q): %v", req.Command, err)
	}
	mu.Lock()
	defer mu.Unlock()
	return result{stdout: out.String(), stderr: errs.String(), code: res.ExitCode}
}

func TestExecKeepsStdoutAndStderrApartAndReportsTheExitCode(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)

	// Act
	r := execIn(t, sb, sandbox.ExecRequest{Command: "echo out; echo err >&2; exit 7"})

	// Assert
	if r.stdout != "out\n" || r.stderr != "err\n" || r.code != 7 {
		t.Fatalf("got %+v", r)
	}
}

func TestExecShowsHostPathsAsSandboxPathsInTheOutput(t *testing.T) {
	// The agent must never learn (or have to use) the host location of /workspace.
	// Arrange
	sb, _ := newSandbox(t)

	// Act
	r := execIn(t, sb, sandbox.ExecRequest{Command: "pwd; echo $HOME; ls -d /workspace/. >&2; cd /workspace && pwd -P"})

	// Assert
	if r.stdout != "/workspace\n/workspace\n/workspace\n" {
		t.Fatalf("stdout = %q", r.stdout)
	}
}

func TestExecCwdIsMappedAndRefusedOutsideTheSandbox(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)
	ctx := context.Background()
	_ = sb.MkdirAll(ctx, "/workspace/sub")

	// Act
	inside := execIn(t, sb, sandbox.ExecRequest{Command: "pwd", Cwd: "/workspace/sub"})
	relative := execIn(t, sb, sandbox.ExecRequest{Command: "pwd", Cwd: "sub"})
	_, outsideErr := sb.Exec(ctx, sandbox.ExecRequest{Command: "pwd", Cwd: "/etc"})

	// Assert
	if inside.stdout != "/workspace/sub\n" || relative.stdout != "/workspace/sub\n" {
		t.Errorf("inside %q relative %q", inside.stdout, relative.stdout)
	}
	if outsideErr == nil || !strings.Contains(outsideErr.Error(), "outside the local sandbox") {
		t.Errorf("cwd /etc: %v", outsideErr)
	}
}

func TestExecPassesEnvironmentAndNeedsNoCallbacks(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)

	// Act
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: `printf %s "$GREETING" > /workspace/env.txt`, Env: map[string]string{"GREETING": "hello"}})

	// Assert
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("Exec = %+v, %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "env.txt")); string(b) != "hello" {
		t.Fatalf("env.txt = %q", b)
	}
}

func TestExecMapsHarnessPathVariablesButNotFreeText(t *testing.T) {
	// HARNESS_* paths are mapped to the host; the pattern, the base64 payloads and ordinary variables are not.
	// Arrange
	sb, ws := newSandbox(t)

	// Act
	r := execIn(t, sb, sandbox.ExecRequest{
		Command: `[ -d "$HARNESS_ROOT" ] && echo dir-ok; echo "$HARNESS_PATTERN|$HARNESS_X_B64|$PLAIN"`,
		Env:     map[string]string{"HARNESS_ROOT": "/workspace", "HARNESS_PATTERN": "/workspace/*.go", "HARNESS_X_B64": "/workspace/a", "PLAIN": "/workspace"},
	})

	// Assert
	want := "dir-ok\n/workspace/*.go|/workspace/a|/workspace\n"
	if r.stdout != want {
		t.Fatalf("stdout = %q, want %q", r.stdout, want)
	}
	if strings.Contains(r.stdout, ws) {
		t.Fatal("the host path leaked into the output")
	}
}

func TestExecCancellationKillsTheProcessGroupAndReturnsTheContextError(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	marker := filepath.Join(ws, "survivor")

	// Act: the child outlives the shell unless the whole group is killed
	start := time.Now()
	_, err := sb.Exec(ctx, sandbox.ExecRequest{
		Command:  "(sleep 2; touch /workspace/survivor) & echo started; sleep 30",
		OnStdout: func(string) { cancel() },
	})
	elapsed := time.Since(start)
	time.Sleep(2500 * time.Millisecond) // the orphan would have fired by now

	// Assert
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("cancellation took %v", elapsed)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a background child survived the cancellation")
	}
}

func TestExecDeadlineOfTheCallerIsNotTheCommandsOwnTimeout(t *testing.T) {
	// The request's own Timeout is exit code 124; the caller's deadline is an error.
	// Arrange
	sb, _ := newSandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Act
	_, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "sleep 30"})

	// Assert
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestExecMapsSandboxRootsInCommandsOnlyAtPathStarts(t *testing.T) {
	// The mapped command is what the background-job script records (cmd.sh).
	// Arrange
	sb, ws := newSandbox(t)
	hostWs, _ := filepath.EvalSymlinks(ws)
	dir := "/workspace/.work/jobs/map"
	cmd := "cat /workspace/a /workspaces/b /workspace-old/c x/workspace/d \"/workspace\"; ls /skills/x"

	// Act
	execIn(t, sb, sandbox.ExecRequest{Command: "# work:job-start\n", Env: map[string]string{
		"HARNESS_JOB_DIR": dir, "HARNESS_JOB_CMD_B64": b64(cmd), "HARNESS_JOB_WAIT": "0"}})
	script, err := os.ReadFile(filepath.Join(ws, ".work", "jobs", "map", "cmd.sh"))

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	got := string(script)
	for _, want := range []string{"cat " + hostWs + "/a ", " /workspaces/b ", " /workspace-old/c ", " x/workspace/d ", ` "` + hostWs + `";`, "ls "} {
		if !strings.Contains(got, want) {
			t.Errorf("mapped script %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, " /skills/x") {
		t.Errorf("/skills was not mapped: %q", got)
	}
}

// --- native protocol scripts ---------------------------------------------------------------------------

func TestPlaceRefusesATargetThatIsASymlinkAndReportsTheRealDirectory(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	ctx := context.Background()
	_ = sb.WriteFile(ctx, "/workspace/real/keep.txt", []byte("k"))
	if err := os.Symlink(filepath.Join(ws, "real"), filepath.Join(ws, "linkdir")); err != nil {
		t.Fatal(err)
	}
	_ = sb.WriteFile(ctx, "/workspace/real/target.txt", []byte("t"))
	if err := os.Symlink(filepath.Join(ws, "real", "target.txt"), filepath.Join(ws, "linkfile")); err != nil {
		t.Fatal(err)
	}

	// Act
	viaDir := sandbox.WriteFileNoFollow(ctx, sb, "/workspace/linkdir/new.txt", []byte("x"))
	viaFile := sandbox.WriteFileNoFollow(ctx, sb, "/workspace/linkfile", []byte("x"))

	// Assert
	var symErr *sandbox.SymlinkError
	if !errors.As(viaDir, &symErr) || symErr.RealDir != "/workspace/real" {
		t.Errorf("write through a symlinked directory: %v", viaDir)
	}
	if !errors.Is(viaFile, sandbox.ErrSymlinkTarget) {
		t.Errorf("write onto a symlink: %v", viaFile)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "real", "target.txt")); string(b) != "t" {
		t.Errorf("the symlink target was overwritten: %q", b)
	}
	if _, err := os.Stat(filepath.Join(ws, "real", "new.txt")); err == nil {
		t.Error("the file landed behind the symlinked directory")
	}
	if entries, _ := os.ReadDir(filepath.Join(ws, ".work", ".staging")); len(entries) != 0 {
		t.Errorf("staged uploads left behind: %d", len(entries))
	}
}

func TestPlaceReportsExit3ForPathsItCannotUse(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	ctx := context.Background()
	staged := "/workspace/.work/.staging/s1"
	_ = sb.WriteFile(ctx, staged, []byte("x"))
	place := func(target, staged string) int {
		return execIn(t, sb, sandbox.ExecRequest{Command: sandbox.PlaceMarker + "\n", Env: map[string]string{"HARNESS_TARGET": target, "HARNESS_STAGED": staged}}).code
	}
	_ = os.WriteFile(filepath.Join(ws, "blocker"), nil, 0o644)

	// Act
	outsideTarget := place("/etc/evil", staged)
	outsideStaged := place("/workspace/ok.txt", "/etc/passwd")
	parentIsFile := place("/workspace/blocker/x.txt", staged)

	// Assert
	if outsideTarget != 3 || outsideStaged != 3 || parentIsFile != 3 {
		t.Fatalf("exit codes %d %d %d, want 3 3 3", outsideTarget, outsideStaged, parentIsFile)
	}
	if _, err := os.Stat("/etc/evil"); err == nil {
		t.Fatal("placed a file outside the sandbox")
	}
}

func TestUnstageRemovesTheStagedFileAndIgnoresPathsOutsideTheSandbox(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	ctx := context.Background()
	_ = sb.WriteFile(ctx, "/workspace/.work/.staging/s2", []byte("x"))
	unstage := func(p string) int {
		return execIn(t, sb, sandbox.ExecRequest{Command: sandbox.UnstageMarker + "\n", Env: map[string]string{"HARNESS_STAGED": p}}).code
	}

	// Act
	ok := unstage("/workspace/.work/.staging/s2")
	outside := unstage("/etc/hosts")

	// Assert
	if ok != 0 || outside != 0 {
		t.Fatalf("exit codes %d %d", ok, outside)
	}
	if _, err := os.Stat(filepath.Join(ws, ".work", ".staging", "s2")); err == nil {
		t.Error("staged file survived")
	}
	if _, err := os.Stat("/etc/hosts"); err != nil {
		t.Errorf("/etc/hosts was touched: %v", err)
	}
}

func TestRealPathsResolveSymlinksAndFailClosedOutsideTheSandbox(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	ctx := context.Background()
	_ = sb.WriteFile(ctx, "/workspace/real/f.txt", []byte("x"))
	if err := os.Symlink(filepath.Join(ws, "real"), filepath.Join(ws, "ln")); err != nil {
		t.Fatal(err)
	}

	// Act
	got, err := sandbox.RealPaths(ctx, sb, []string{"/workspace/ln/f.txt", "/workspace/ln/new/er.txt", "/workspace"})
	_, outsideErr := sandbox.RealPaths(ctx, sb, []string{"/workspace/a", "/etc/passwd"})

	// Assert
	if err != nil || strings.Join(got, ",") != "/workspace/real/f.txt,/workspace/real/new/er.txt,/workspace" {
		t.Fatalf("RealPaths = %v, %v", got, err)
	}
	if !errors.Is(outsideErr, sandbox.ErrRealPath) {
		t.Fatalf("a path outside the sandbox: %v", outsideErr)
	}
}

func TestReadRegularFilesHonoursKnownStampsAndTheBudget(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	ctx := context.Background()
	_ = sb.WriteFile(ctx, "/workspace/a.txt", []byte("aaaaaaaa"))
	_ = sb.WriteFile(ctx, "/workspace/b.txt", []byte("bbbbbbbb"))
	_ = sb.MkdirAll(ctx, "/workspace/dir")
	if err := os.Symlink(filepath.Join(ws, "a.txt"), filepath.Join(ws, "ln")); err != nil {
		t.Fatal(err)
	}
	paths := []string{"/workspace/a.txt", "/workspace/b.txt", "/workspace/dir", "/workspace/ln", "/workspace/missing", "/etc/hosts"}
	first, err := sandbox.ReadRegularFiles(ctx, sb, paths, nil, 1<<20)
	if err != nil || len(first) != 2 {
		t.Fatalf("first read = %+v, %v", first, err)
	}

	// Act
	again, err1 := sandbox.ReadRegularFiles(ctx, sb, paths[:2], []string{first[0].Stamp, "stale"}, 1<<20)
	sizeOnly, err2 := sandbox.ReadRegularFiles(ctx, sb, paths[:1], []string{"8 -"}, 1<<20)
	tiny, err3 := sandbox.ReadRegularFiles(ctx, sb, paths[:2], nil, 5)
	none, err4 := sandbox.ReadRegularFiles(ctx, sb, paths[:2], nil, 0)

	// Assert
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		t.Fatal(err1, err2, err3, err4)
	}
	if len(again) != 2 || !again[0].Unchanged || again[0].Data != nil || again[1].Unchanged || string(again[1].Data) != "bbbbbbbb" {
		t.Errorf("known stamp: %+v", again)
	}
	if len(sizeOnly) != 1 || !sizeOnly[0].Unchanged {
		t.Errorf("size-only stamp: %+v", sizeOnly)
	}
	if len(tiny) != 1 || string(tiny[0].Data) != "aaaaa" {
		t.Errorf("budget 5 = %+v, want only the first file cut to the budget", tiny)
	}
	if len(none) != 0 {
		t.Errorf("budget 0 returned content: %+v", none)
	}
}

func TestSweepRemovesOnlyOldStagedUploads(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	ctx := context.Background()
	staging := filepath.Join(ws, ".work", ".staging")
	_ = sb.WriteFile(ctx, sandbox.StagingDir+"/old", []byte("x"))
	_ = sb.WriteFile(ctx, sandbox.StagingDir+"/fresh", []byte("x"))
	if err := os.MkdirAll(filepath.Join(staging, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(staging, "old"), past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(staging, "subdir"), past, past); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HARNESS_STAGING_DIR": sandbox.StagingDir, "HARNESS_STAGING_MINUTES": "60"}

	// Act
	r := execIn(t, sb, sandbox.ExecRequest{Command: "# work:sweep\n", Env: env})
	outside := execIn(t, sb, sandbox.ExecRequest{Command: "# work:sweep\n", Env: map[string]string{"HARNESS_STAGING_DIR": "/etc", "HARNESS_STAGING_MINUTES": "0"}})

	// Assert
	if r.code != 0 || outside.code != 0 {
		t.Fatalf("exit codes %d %d", r.code, outside.code)
	}
	if _, err := os.Stat(filepath.Join(staging, "old")); err == nil {
		t.Error("an old staged upload survived")
	}
	for _, keep := range []string{"fresh", "subdir"} {
		if _, err := os.Stat(filepath.Join(staging, keep)); err != nil {
			t.Errorf("%s was removed: %v", keep, err)
		}
	}
}

// --- background jobs -----------------------------------------------------------------------------------

const jobsDir = "/workspace/.work/jobs/"

func startJob(t *testing.T, sb sandbox.Sandbox, id, cmd string, wait int) result {
	t.Helper()
	return execIn(t, sb, sandbox.ExecRequest{Command: "# work:job-start\n", Env: map[string]string{
		"HARNESS_JOB_DIR": jobsDir + id, "HARNESS_JOB_CMD_B64": b64(cmd), "HARNESS_JOB_WAIT": fmt.Sprint(wait)}})
}

func jobStatus(t *testing.T, sb sandbox.Sandbox, id string, extra map[string]string) result {
	t.Helper()
	env := map[string]string{"HARNESS_JOB_DIR": jobsDir + id, "HARNESS_HEAD": "1000", "HARNESS_TAIL": "1000"}
	for k, v := range extra {
		env[k] = v
	}
	return execIn(t, sb, sandbox.ExecRequest{Command: "# work:job-status\n", Env: env})
}

func TestJobStartStreamsLiveOutputUntilTheJobEnds(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)

	// Act
	r := startJob(t, sb, "live", "echo one; echo two >&2; exit 5", 30)
	st := jobStatus(t, sb, "live", nil)

	// Assert
	if r.code != 0 || r.stdout != "one\n" || r.stderr != "two\n" {
		t.Fatalf("live output = %+v", r)
	}
	if !strings.HasPrefix(st.stdout, "status finished\ncode 5\nstdout 4 ") || !strings.Contains(st.stdout, "\nstderr 4 ") {
		t.Fatalf("status = %q", st.stdout)
	}
}

func TestJobStartRejectsAnInvalidRequest(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)
	start := func(env map[string]string) int {
		return execIn(t, sb, sandbox.ExecRequest{Command: "# work:job-start\n", Env: env}).code
	}

	// Act
	noDir := start(map[string]string{"HARNESS_JOB_CMD_B64": b64("true")})
	badDir := start(map[string]string{"HARNESS_JOB_DIR": "/etc/job", "HARNESS_JOB_CMD_B64": b64("true")})
	badCmd := start(map[string]string{"HARNESS_JOB_DIR": jobsDir + "bad", "HARNESS_JOB_CMD_B64": "!!!not base64!!!"})

	// Assert
	if noDir != 2 || badDir != 2 || badCmd != 2 {
		t.Fatalf("exit codes %d %d %d, want 2 for each", noDir, badDir, badCmd)
	}
}

func TestJobWaitFollowsARunningJobAndIgnoresUnknownOnes(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)
	startJob(t, sb, "w1", "sleep 0.3; echo finished-late", 0)

	// Act
	waited := execIn(t, sb, sandbox.ExecRequest{Command: "# work:job-wait\n", Env: map[string]string{"HARNESS_JOB_DIR": jobsDir + "w1", "HARNESS_JOB_WAIT": "30"}})
	unknown := execIn(t, sb, sandbox.ExecRequest{Command: "# work:job-wait\n", Env: map[string]string{"HARNESS_JOB_DIR": jobsDir + "none", "HARNESS_JOB_WAIT": "30"}})
	noDir := execIn(t, sb, sandbox.ExecRequest{Command: "# work:job-wait\n"})

	// Assert
	if waited.stdout != "finished-late\n" || waited.code != 0 {
		t.Errorf("wait = %+v", waited)
	}
	if unknown.code != 0 || unknown.stdout != "" || noDir.code != 0 {
		t.Errorf("unknown %+v noDir %+v", unknown, noDir)
	}
}

func TestJobWaitStopsWhenTheCallerCancels(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)
	startJob(t, sb, "w2", "echo hello; sleep 30", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Act
	start := time.Now()
	_, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "# work:job-wait\n",
		Env:      map[string]string{"HARNESS_JOB_DIR": jobsDir + "w2", "HARNESS_JOB_WAIT": "60"},
		OnStdout: func(string) { cancel() }})

	// Assert
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the wait outlived its context")
	}
	// the job itself keeps running
	if st := jobStatus(t, sb, "w2", nil); !strings.HasPrefix(st.stdout, "status running\n") {
		t.Errorf("status after the caller left = %q", st.stdout)
	}
	execIn(t, sb, sandbox.ExecRequest{Command: "# work:job-kill\n", Env: map[string]string{"HARNESS_JOB_DIR": jobsDir + "w2"}})
}

func TestJobLiveOutputIsBoundedPerPassButNothingIsLost(t *testing.T) {
	// A runaway job must not flood the live stream in one call; the full output stays available through status.
	// Arrange
	sb, _ := newSandbox(t)
	const total = 1 << 20

	// Act
	var biggest int
	var mu sync.Mutex
	_, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: "# work:job-start\n",
		Env: map[string]string{"HARNESS_JOB_DIR": jobsDir + "flood", "HARNESS_JOB_CMD_B64": b64(fmt.Sprintf("head -c %d /dev/zero | tr '\\0' x", total)), "HARNESS_JOB_WAIT": "30"},
		OnStdout: func(s string) {
			mu.Lock()
			biggest = max(biggest, len(s))
			mu.Unlock()
		}})
	st := jobStatus(t, sb, "flood", map[string]string{"HARNESS_HEAD": "10", "HARNESS_TAIL": "10"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if biggest > 256<<10 {
		t.Errorf("one live chunk was %d bytes, want at most 256 KiB", biggest)
	}
	if !strings.Contains(st.stdout, fmt.Sprintf("\nstdout %d ", total)) {
		t.Errorf("status lost output: %q", st.stdout)
	}
}

func TestJobStatusCoversEveryState(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	jobDir := func(id string) string { return filepath.Join(ws, ".work", "jobs", id) }
	startJob(t, sb, "running", "sleep 30", 0)
	startJob(t, sb, "done", "exit 0", 5)
	lost := jobDir("lost")
	if err := os.MkdirAll(lost, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lost, "pid"), []byte("99999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		execIn(t, sb, sandbox.ExecRequest{Command: "# work:job-kill\n", Env: map[string]string{"HARNESS_JOB_DIR": jobsDir + "running"}})
	})

	// Act
	running := jobStatus(t, sb, "running", nil)
	done := jobStatus(t, sb, "done", nil)
	lostSt := jobStatus(t, sb, "lost", nil)
	missing := jobStatus(t, sb, "never-started", nil)
	noDir := execIn(t, sb, sandbox.ExecRequest{Command: "# work:job-status\n"})

	// Assert
	for name, c := range map[string]struct{ got, want string }{
		"running": {running.stdout, "status running\ncode \nstdout 0 - -\nstderr 0 - -\n"},
		"done":    {done.stdout, "status finished\ncode 0\nstdout 0 - -\nstderr 0 - -\n"},
		"lost":    {lostSt.stdout, "status lost\ncode \nstdout 0 - -\nstderr 0 - -\n"},
		"missing": {missing.stdout, "status missing\n"},
		"no dir":  {noDir.stdout, "status missing\n"},
	} {
		if c.got != c.want {
			t.Errorf("%s: %q, want %q", name, c.got, c.want)
		}
	}
}

func TestJobStatusReturnsHeadAndTailOfLargeOutput(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)
	startJob(t, sb, "ht", "printf 'AAAA'; printf 'mmmmmmmmmm'; printf 'ZZZZ'", 30)

	// Act
	small := jobStatus(t, sb, "ht", map[string]string{"HARNESS_HEAD": "100", "HARNESS_TAIL": "100"})
	big := jobStatus(t, sb, "ht", map[string]string{"HARNESS_HEAD": "4", "HARNESS_TAIL": "4"})

	// Assert
	if want := "stdout 18 " + b64("AAAAmmmmmmmmmmZZZZ") + " -\n"; !strings.Contains(small.stdout, want) {
		t.Errorf("whole output as head: %q", small.stdout)
	}
	if want := "stdout 18 " + b64("AAAA") + " " + b64("ZZZZ") + "\n"; !strings.Contains(big.stdout, want) {
		t.Errorf("head+tail: %q", big.stdout)
	}
}

func TestJobStatusCleanRemovesOnlyFinishedJobsWithSmallOutput(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	startJob(t, sb, "small", "echo hi", 30)
	startJob(t, sb, "large", "head -c 5000 /dev/zero | tr '\\0' x", 30)
	startJob(t, sb, "live", "sleep 30", 0)
	clean := map[string]string{"HARNESS_JOB_CLEAN": "1", "HARNESS_HEAD": "10", "HARNESS_TAIL": "10"}
	t.Cleanup(func() {
		execIn(t, sb, sandbox.ExecRequest{Command: "# work:job-kill\n", Env: map[string]string{"HARNESS_JOB_DIR": jobsDir + "live"}})
	})

	// Act
	jobStatus(t, sb, "small", clean)
	jobStatus(t, sb, "large", clean)
	jobStatus(t, sb, "live", clean)

	// Assert
	exists := func(id string) bool { _, err := os.Stat(filepath.Join(ws, ".work", "jobs", id)); return err == nil }
	if exists("small") {
		t.Error("a finished job with a small output was not cleaned")
	}
	if !exists("large") {
		t.Error("a job whose output was truncated was cleaned: the rest could never be read")
	}
	if !exists("live") {
		t.Error("a running job was cleaned")
	}
}

func TestJobKillStopsTheWholeJobAndMarksItKilled(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	startJob(t, sb, "k", "(sleep 3; touch /workspace/orphan) & sleep 30", 0)
	kill := func(id string) int {
		return execIn(t, sb, sandbox.ExecRequest{Command: "# work:job-kill\n", Env: map[string]string{"HARNESS_JOB_DIR": jobsDir + id}}).code
	}

	// Act
	code := kill("k")
	st := jobStatus(t, sb, "k", nil)
	again := kill("k")
	unknown := kill("nope")
	time.Sleep(3500 * time.Millisecond)

	// Assert
	if code != 0 || again != 0 {
		t.Errorf("kill exit codes %d, %d", code, again)
	}
	if !strings.HasPrefix(st.stdout, "status killed\n") {
		t.Errorf("status = %q", st.stdout)
	}
	if unknown != 1 {
		t.Errorf("killing an unknown job = %d, want 1", unknown)
	}
	if _, err := os.Stat(filepath.Join(ws, "orphan")); err == nil {
		t.Error("a child of the job survived the kill")
	}
}

func TestJobKillOfAFinishedJobChangesNothing(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	startJob(t, sb, "fin", "echo bye", 30)

	// Act
	code := execIn(t, sb, sandbox.ExecRequest{Command: "# work:job-kill\n", Env: map[string]string{"HARNESS_JOB_DIR": jobsDir + "fin"}}).code
	st := jobStatus(t, sb, "fin", nil)

	// Assert
	if code != 0 || !strings.HasPrefix(st.stdout, "status finished\n") {
		t.Fatalf("kill %d status %q", code, st.stdout)
	}
	if _, err := os.Stat(filepath.Join(ws, ".work", "jobs", "fin", "killed")); err == nil {
		t.Error("a finished job was marked killed")
	}
}
