package daytona

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/sandbox"
)

func testBackend(a *fakeAPI, o Options) *Backend {
	b := newBackend(o, a)
	b.drainGrace = 200 * time.Millisecond
	b.pollEvery = time.Millisecond
	return b
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ---- error mapping ----

func TestWrapMapsDaytonaErrorsToSentinels(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		is   error
	}{
		{"quota", quotaErr(), sandbox.ErrQuota},
		{"5xx", serverErr(), sandbox.ErrUnavailable},
		{"no status", plainErr(), sandbox.ErrUnavailable},
		{"timeout", context.DeadlineExceeded, sandbox.ErrUnavailable},
	} {
		// Act
		got := wrap(c.err)
		// Assert
		if !errors.Is(got, c.is) {
			t.Errorf("%s: %v does not wrap %v", c.name, got, c.is)
		}
	}
	if got := wrap(badRequest()); errors.Is(got, sandbox.ErrUnavailable) || errors.Is(got, sandbox.ErrQuota) {
		t.Errorf("a validation error must stay itself, got %v", got)
	}
	if got := wrap(context.Canceled); got != context.Canceled {
		t.Errorf("cancellation changed: %v", got)
	}
	if wrap(nil) != nil {
		t.Error("nil")
	}
}

// ---- Create ----

func TestCreateParamsCarryLabelsLifetimeAndEgressPolicy(t *testing.T) {
	// Arrange
	b := testBackend(newFakeAPI(), Options{TimeoutSeconds: 90, Snapshot: "snap", AutoDeleteMinutes: 30,
		EgressAllowlist: []string{"pypi.org", " ", "github.com"}})
	// Act
	p, err := b.createParams(sandbox.CreateOptions{Metadata: map[string]string{"work-thread": "t1"}})
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if p.Snapshot != "snap" || p.Labels["work-thread"] != "t1" || p.AutoStopMinutes != 2 || *p.AutoDeleteMin != 30 {
		t.Errorf("params %+v", p)
	}
	if p.NetworkAllowList != "" || p.DomainAllowList != "pypi.org,github.com" {
		t.Errorf("allow lists %q / %q", p.NetworkAllowList, p.DomainAllowList)
	}
}

func TestTheSnapshotFollowsTheProfileAndFallsBackToTheDefault(t *testing.T) {
	b := testBackend(newFakeAPI(), Options{Snapshot: "default-snap", Images: map[string]string{"work": "harness-office"}})

	for profile, want := range map[string]string{"work": "harness-office", "coding": "default-snap", "": "default-snap"} {
		p, err := b.createParams(sandbox.CreateOptions{Profile: profile})
		if err != nil || p.Snapshot != want {
			t.Errorf("profile %q: snapshot %q (want %q), err %v", profile, p.Snapshot, want, err)
		}
	}
	// no default snapshot at all: a profile without an entry gets the account default ("")
	if p, _ := testBackend(newFakeAPI(), Options{}).createParams(sandbox.CreateOptions{Profile: "coding"}); p.Snapshot != "" {
		t.Errorf("snapshot %q, want the account default", p.Snapshot)
	}
}

func TestCreateParamsTurnACIDRListIntoANetworkAllowList(t *testing.T) {
	b := testBackend(newFakeAPI(), Options{EgressAllowlist: []string{"10.1.0.0/16", "8.8.8.8"}})

	p, err := b.createParams(sandbox.CreateOptions{})

	if err != nil || p.NetworkAllowList != "10.1.0.0/16,8.8.8.8/32" || p.DomainAllowList != "" {
		t.Errorf("%q / %q, %v", p.NetworkAllowList, p.DomainAllowList, err)
	}
}

func TestCreateParamsRefuseAListMixingCIDRsAndDomains(t *testing.T) {
	// Daytona answers HTTP 400 when both lists are set on one sandbox: say so before calling it
	b := testBackend(newFakeAPI(), Options{EgressAllowlist: []string{"pypi.org", "10.1.0.0/16"}})

	_, err := b.createParams(sandbox.CreateOptions{})

	if err == nil || !strings.Contains(err.Error(), "either") {
		t.Errorf("err = %v", err)
	}
}

func TestACreateRejectedByTheAccountTierRetriesWithoutTheSandboxLevelList(t *testing.T) {
	// Arrange: on Daytona tiers 1 and 2 the organisation's network policy wins and a per-sandbox list is refused
	api := newFakeAPI()
	api.createFn = func(p createParams) error {
		if p.DomainAllowList != "" || p.NetworkAllowList != "" {
			return errors.New("Validation error: Network access is restricted and cannot be overridden at the sandbox level. Remove domainAllowList from the request.")
		}
		return nil
	}
	b := testBackend(api, Options{EgressAllowlist: []string{"pypi.org"}})
	// Act
	inst, err := b.Create(ctxT(t), sandbox.CreateOptions{})
	// Assert
	if err != nil || inst == nil {
		t.Fatalf("Create = %v, %v", inst, err)
	}
	if len(api.created) != 2 || api.created[0].DomainAllowList != "pypi.org" || api.created[1].DomainAllowList != "" || api.created[1].NetworkAllowList != "" {
		t.Fatalf("attempts: %+v", api.created)
	}
}

func TestOtherCreateFailuresAreNotRetriedWithoutTheList(t *testing.T) {
	api := newFakeAPI()
	api.createFn = func(createParams) error { return errors.New("Validation error: snapshot not found") }
	b := testBackend(api, Options{EgressAllowlist: []string{"pypi.org"}})

	_, err := b.Create(ctxT(t), sandbox.CreateOptions{})

	if err == nil || len(api.created) != 1 {
		t.Fatalf("err %v after %d attempts", err, len(api.created))
	}
}

func TestCreateParamsRefuseMoreCIDRsThanDaytonaAllows(t *testing.T) {
	// Arrange
	var l []string
	for i := 0; i < 11; i++ {
		l = append(l, fmt.Sprintf("10.%d.0.0/16", i))
	}
	b := testBackend(newFakeAPI(), Options{EgressAllowlist: l})
	// Act
	_, err := b.createParams(sandbox.CreateOptions{})
	// Assert
	if err == nil {
		t.Fatal("11 CIDRs accepted")
	}
}

func TestCreateParamsDefaultsAutoStopAndAutoDelete(t *testing.T) {
	// Arrange / Act
	p, _ := testBackend(newFakeAPI(), Options{}).createParams(sandbox.CreateOptions{})
	// Assert
	if p.AutoStopMinutes != 30 || p.AutoDeleteMin == nil || *p.AutoDeleteMin != 1440 {
		t.Errorf("%+v", p)
	}
	if p.NetworkAllowList != "" || p.DomainAllowList != "" {
		t.Errorf("no allowlist expected: %+v", p)
	}
}

func TestCreatePreparesTheWorkspace(t *testing.T) {
	// Arrange
	a := newFakeAPI()
	a.next = newFakeBox("sb-1")
	b := testBackend(a, Options{Snapshot: "s"})
	// Act
	inst, err := b.Create(ctxT(t), sandbox.CreateOptions{Metadata: map[string]string{"work-user": "h"}})
	// Assert
	if err != nil || inst.ID() != "sb-1" {
		t.Fatalf("%v %v", inst, err)
	}
	if !a.next.ranContaining("/workspace") || a.created[0].Labels["work-user"] != "h" {
		t.Errorf("runs %v created %+v", a.next.runs, a.created)
	}
}

func TestCreateKillsTheSandboxWhenTheWorkspaceCannotBePrepared(t *testing.T) {
	// Arrange
	a := newFakeAPI()
	a.next = newFakeBox("sb-1")
	a.next.runFunc = func(string) (int, string, error) { return 1, "read-only", nil }
	// Act
	_, err := testBackend(a, Options{}).Create(ctxT(t), sandbox.CreateOptions{})
	// Assert
	if err == nil || !a.next.deleted {
		t.Errorf("err %v deleted %v", err, a.next.deleted)
	}
}

func TestCreateMapsQuotaAndOutage(t *testing.T) {
	for _, c := range []struct {
		err error
		is  error
	}{{quotaErr(), sandbox.ErrQuota}, {serverErr(), sandbox.ErrUnavailable}} {
		// Arrange
		a := newFakeAPI()
		a.createErr = c.err
		// Act
		_, err := testBackend(a, Options{}).Create(ctxT(t), sandbox.CreateOptions{})
		// Assert
		if !errors.Is(err, c.is) {
			t.Errorf("%v: got %v", c.err, err)
		}
	}
}

// ---- Connect / Gone / Kill ----

func TestConnectReturnsNoSandboxOnlyForADefinitiveEnd(t *testing.T) {
	for _, c := range []struct {
		name   string
		setup  func(*fakeAPI)
		noSbx  bool
		wantOK bool
	}{
		{"unknown id", func(a *fakeAPI) {}, true, false},
		{"destroyed", func(a *fakeAPI) { a.add(newFakeBox("x")).info.State = "destroyed" }, true, false},
		{"destroying", func(a *fakeAPI) { a.add(newFakeBox("x")).info.State = "destroying" }, true, false},
		{"unrecoverable error", func(a *fakeAPI) { a.add(newFakeBox("x")).info = boxInfo{State: "error"} }, true, false},
		{"recoverable error", func(a *fakeAPI) { a.add(newFakeBox("x")).info = boxInfo{State: "error", Recoverable: true} }, false, true},
		{"started", func(a *fakeAPI) { a.add(newFakeBox("x")) }, false, true},
		{"server error", func(a *fakeAPI) { a.getErr = serverErr() }, false, false},
		{"network error", func(a *fakeAPI) { a.getErr = plainErr() }, false, false},
		{"unauthorized", func(a *fakeAPI) { a.getErr = badRequest() }, false, false},
	} {
		// Arrange
		a := newFakeAPI()
		c.setup(a)
		// Act
		inst, err := testBackend(a, Options{}).Connect(ctxT(t), "x")
		// Assert
		if errors.Is(err, sandbox.ErrNoSandbox) != c.noSbx || (err == nil) != c.wantOK || (c.wantOK && inst == nil) {
			t.Errorf("%s: inst %v err %v", c.name, inst, err)
		}
	}
}

func TestConnectStartsAStoppedSandbox(t *testing.T) {
	// Arrange
	a := newFakeAPI()
	bx := a.add(newFakeBox("x"))
	bx.info.State = "stopped"
	// Act
	inst, err := testBackend(a, Options{}).Connect(ctxT(t), "x")
	// Assert
	if err != nil || inst == nil || !bx.started {
		t.Errorf("err %v started %v", err, bx.started)
	}
}

func TestConnectStartFailureIsTransientUnlessNotFound(t *testing.T) {
	// Arrange
	a := newFakeAPI()
	bx := a.add(newFakeBox("x"))
	bx.info.State = "stopped"
	bx.startErr = serverErr()
	// Act
	_, err := testBackend(a, Options{}).Connect(ctxT(t), "x")
	// Assert
	if errors.Is(err, sandbox.ErrNoSandbox) || !errors.Is(err, sandbox.ErrUnavailable) {
		t.Errorf("got %v", err)
	}
	bx.startErr = notFound()
	if _, err := testBackend(a, Options{}).Connect(ctxT(t), "x"); !errors.Is(err, sandbox.ErrNoSandbox) {
		t.Errorf("404 on start: %v", err)
	}
}

func TestGoneIsTrueOnlyWhenDaytonaSaysSo(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(*fakeBox)
		gone bool
	}{
		{"started", func(b *fakeBox) {}, false},
		{"stopped", func(b *fakeBox) { b.info.State = "stopped" }, false},
		{"destroyed", func(b *fakeBox) { b.info.State = "destroyed" }, true},
		{"404", func(b *fakeBox) { b.refreshErr = notFound() }, true},
		{"5xx", func(b *fakeBox) { b.refreshErr = serverErr() }, false},
		{"timeout", func(b *fakeBox) { b.refreshErr = context.DeadlineExceeded }, false},
	} {
		// Arrange
		bx := newFakeBox("x")
		c.set(bx)
		inst := testBackend(newFakeAPI(), Options{}).instance(bx)
		// Act / Assert
		if got := inst.Gone(ctxT(t)); got != c.gone {
			t.Errorf("%s: Gone=%v", c.name, got)
		}
	}
}

func TestKillIsIdempotent(t *testing.T) {
	// Arrange
	a := newFakeAPI()
	bx := a.add(newFakeBox("x"))
	b := testBackend(a, Options{})
	// Act / Assert
	if err := b.Kill(ctxT(t), "unknown"); err != nil {
		t.Errorf("unknown: %v", err)
	}
	if err := b.Kill(ctxT(t), "x"); err != nil || !bx.deleted {
		t.Errorf("kill: %v deleted=%v", err, bx.deleted)
	}
	bx.deleteErr = notFound()
	if err := b.Kill(ctxT(t), "x"); err != nil {
		t.Errorf("delete 404: %v", err)
	}
	bx.deleteErr = serverErr()
	if err := b.Kill(ctxT(t), "x"); !errors.Is(err, sandbox.ErrUnavailable) {
		t.Errorf("delete 5xx: %v", err)
	}
}

func TestHealthy(t *testing.T) {
	// Arrange
	bx := newFakeBox("x")
	inst := testBackend(newFakeAPI(), Options{}).instance(bx)
	// Act / Assert
	if !inst.Healthy(ctxT(t)) {
		t.Error("healthy sandbox reported unhealthy")
	}
	bx.runFunc = func(string) (int, string, error) { return 0, "", serverErr() }
	if inst.Healthy(ctxT(t)) {
		t.Error("failing probe reported healthy")
	}
	bx.runFunc = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if inst.Healthy(ctx) {
		t.Error("cancelled context reported healthy")
	}
	bx.info.State = "destroyed"
	if inst.Healthy(ctxT(t)) {
		t.Error("destroyed reported healthy")
	}
	bx.info.State = "stopped"
	if !inst.Healthy(ctxT(t)) || !bx.started {
		t.Error("stopped sandbox is started by the probe")
	}
}

func TestRenewSetsTheAutoStopIntervalInWholeMinutes(t *testing.T) {
	// Arrange
	a := newFakeAPI()
	inst := testBackend(a, Options{}).instance(newFakeBox("x"))
	// Act
	err := inst.Renew(ctxT(t), 90*time.Second)
	// Assert
	if err != nil || a.autoStop["x"] != 2 {
		t.Errorf("err %v interval %v", err, a.autoStop)
	}
	a.stopErr = serverErr()
	if err := inst.Renew(ctxT(t), time.Minute); !errors.Is(err, sandbox.ErrUnavailable) {
		t.Errorf("5xx: %v", err)
	}
	a.stopErr = notFound()
	inst.box.(*fakeBox).refreshErr = notFound()
	if err := inst.Renew(ctxT(t), time.Minute); !sandbox.IsGone(err) {
		t.Errorf("404 on a gone sandbox: %v", err)
	}
}

// ---- Exec script ----

// shimSetsid puts a `setsid` on PATH that only drops -w, so the script can run on any unix.
func shimSetsid(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "setsid"), []byte("#!/bin/sh\nshift\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestExecScriptRunsTheCommandWithCwdEnvAndQuotingIntact(t *testing.T) {
	// Arrange
	if _, err := exec.LookPath("base64"); err != nil {
		t.Skip("no base64")
	}
	work := t.TempDir()
	pid := filepath.Join(work, "pid")
	script, err := execScript(sandbox.ExecRequest{
		Command: "printf '%s|%s|' \"$A\" \"$B\"; pwd; echo \"it's\" >&2\nexit 7",
		Cwd:     work,
		Env:     map[string]string{"A": `quo'te "$HOME" $(x)`, "B": "line1\nline2"},
	}, pid)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+shimSetsid(t)+":"+os.Getenv("PATH"))
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	// Act
	runErr := cmd.Run()
	// Assert
	var ee *exec.ExitError
	if !errors.As(runErr, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("exit: %v", runErr)
	}
	want := "quo'te \"$HOME\" $(x)|line1\nline2|"
	if !strings.HasPrefix(out.String(), want) || !strings.Contains(out.String(), filepath.Base(work)) {
		t.Errorf("stdout %q", out.String())
	}
	if errb.String() != "it's\n" {
		t.Errorf("stderr %q", errb.String())
	}
	if b, _ := os.ReadFile(pid); strings.TrimSpace(string(b)) == "" {
		t.Error("pid file not written")
	}
}

func TestExecScriptRejectsBadEnvNames(t *testing.T) {
	for _, k := range []string{"", "A-B", "1A", "A B", "A;rm"} {
		if _, err := execScript(sandbox.ExecRequest{Command: "true", Env: map[string]string{k: "v"}}, "/tmp/p"); err == nil {
			t.Errorf("%q accepted", k)
		}
	}
}

// ---- Exec ----

func TestExecStreamsOutputAndReturnsTheExitCode(t *testing.T) {
	// Arrange
	bx := newFakeBox("x")
	bx.chunks = []chunk{{false, "a"}, {true, "e1"}, {false, "b"}}
	bx.exitAfter, bx.exitCode = 2, 3
	inst := testBackend(newFakeAPI(), Options{}).instance(bx)
	var mu sync.Mutex
	var so, se strings.Builder
	// Act
	res, err := inst.Exec(ctxT(t), sandbox.ExecRequest{Command: "echo hi", Cwd: "/workspace",
		OnStdout: func(s string) { mu.Lock(); so.WriteString(s); mu.Unlock() },
		OnStderr: func(s string) { mu.Lock(); se.WriteString(s); mu.Unlock() }})
	inst.cleanup.Wait()
	// Assert
	mu.Lock()
	defer mu.Unlock()
	if err != nil || res.ExitCode != 3 || so.String() != "ab" || se.String() != "e1" {
		t.Errorf("res %+v err %v out %q err %q", res, err, so.String(), se.String())
	}
	if len(bx.deleted_) != 1 || !bx.ranContaining("rm -f") {
		t.Errorf("cleanup: sessions %v runs %v", bx.deleted_, bx.runs)
	}
	if !strings.Contains(bx.cmdScript, "setsid -w bash -c") {
		t.Errorf("script %q", bx.cmdScript)
	}
}

func TestExecTimeoutKillsTheProcessGroupAndErrors(t *testing.T) {
	// Arrange
	bx := newFakeBox("x")
	bx.exitAfter = 1 << 30 // never finishes
	bx.streamBlock = true
	inst := testBackend(newFakeAPI(), Options{}).instance(bx)
	start := time.Now()
	// Act
	_, err := inst.Exec(ctxT(t), sandbox.ExecRequest{Command: "sleep 100", Timeout: 100 * time.Millisecond})
	inst.cleanup.Wait()
	// Assert
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("timeout was not prompt")
	}
	if !bx.ranContaining("kill -KILL -- ") {
		t.Errorf("no group kill; runs %v", bx.runs)
	}
}

func TestExecCancelReturnsPromptlyAndKills(t *testing.T) {
	// Arrange
	bx := newFakeBox("x")
	bx.exitAfter = 1 << 30
	bx.streamBlock = true
	inst := testBackend(newFakeAPI(), Options{}).instance(bx)
	ctx, cancel := context.WithCancel(ctxT(t))
	time.AfterFunc(50*time.Millisecond, cancel)
	// Act
	_, err := inst.Exec(ctx, sandbox.ExecRequest{Command: "sleep 100"})
	inst.cleanup.Wait()
	// Assert
	if !errors.Is(err, context.Canceled) || !bx.ranContaining("kill -KILL -- ") {
		t.Errorf("err %v runs %v", err, bx.runs)
	}
}

func TestExecDeliversTheKeptLogWhenTheStreamCannotBeFollowed(t *testing.T) {
	// Arrange
	bx := newFakeBox("x")
	bx.streamErr = plainErr()
	bx.logsOut, bx.logsErr = "out", "err"
	inst := testBackend(newFakeAPI(), Options{}).instance(bx)
	var so, se string
	// Act
	res, err := inst.Exec(ctxT(t), sandbox.ExecRequest{Command: "x", OnStdout: func(s string) { so += s }, OnStderr: func(s string) { se += s }})
	inst.cleanup.Wait()
	// Assert
	if err != nil || res.ExitCode != 0 || so != "out" || se != "err" {
		t.Errorf("res %+v err %v %q %q", res, err, so, se)
	}
}

func TestExecReportsAnInterruptedStreamAfterPartialOutput(t *testing.T) {
	// Arrange
	bx := newFakeBox("x")
	bx.chunks = []chunk{{false, "partial"}}
	bx.streamErr = plainErr()
	inst := testBackend(newFakeAPI(), Options{}).instance(bx)
	// Act
	_, err := inst.Exec(ctxT(t), sandbox.ExecRequest{Command: "x", OnStdout: func(string) {}})
	inst.cleanup.Wait()
	// Assert
	if !errors.Is(err, sandbox.ErrUnavailable) {
		t.Errorf("err %v", err)
	}
}

func TestExecToleratesAFewPollFailuresButNotTheSandboxBeingGone(t *testing.T) {
	// Arrange
	bx := newFakeBox("x")
	bx.pollErr = serverErr()
	inst := testBackend(newFakeAPI(), Options{}).instance(bx)
	// Act
	_, err := inst.Exec(ctxT(t), sandbox.ExecRequest{Command: "x"})
	inst.cleanup.Wait()
	// Assert: persistent failure ends as unavailable, not as a guessed exit code.
	if !errors.Is(err, sandbox.ErrUnavailable) {
		t.Errorf("persistent: %v", err)
	}
	bx2 := newFakeBox("y")
	bx2.sessionErr = notFound()
	bx2.refreshErr = notFound()
	inst2 := testBackend(newFakeAPI(), Options{}).instance(bx2)
	if _, err := inst2.Exec(ctxT(t), sandbox.ExecRequest{Command: "x"}); !sandbox.IsGone(err) {
		t.Errorf("gone: %v", err)
	}
}

// ---- files ----

func TestReadFileCutsAtMaxBytesAndReportsTheFullSize(t *testing.T) {
	// Arrange
	bx := newFakeBox("x")
	bx.files["/workspace/a"] = []byte("0123456789")
	inst := testBackend(newFakeAPI(), Options{}).instance(bx)
	// Act
	cut, size, err := inst.ReadFile(ctxT(t), "/workspace/a", 4)
	all, size2, err2 := inst.ReadFile(ctxT(t), "/workspace/a", 0)
	// Assert
	if err != nil || err2 != nil || string(cut) != "0123" || size != 10 || string(all) != "0123456789" || size2 != 10 {
		t.Errorf("%q %d %v / %q %d %v", cut, size, err, all, size2, err2)
	}
}

func TestMissingFileIsNotFoundButAGoneSandboxIsGone(t *testing.T) {
	// Arrange
	bx := newFakeBox("x")
	inst := testBackend(newFakeAPI(), Options{}).instance(bx)
	// Act / Assert
	if _, _, err := inst.ReadFile(ctxT(t), "/workspace/none", 0); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("read: %v", err)
	}
	if _, err := inst.Stat(ctxT(t), "/workspace/none"); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("stat: %v", err)
	}
	if _, err := inst.List(ctxT(t), "/workspace/none", 1); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("list: %v", err)
	}
	bx.refreshErr = notFound()
	if _, _, err := inst.ReadFile(ctxT(t), "/workspace/none", 0); !errors.Is(err, sandbox.ErrGone) {
		t.Errorf("gone: %v", err)
	}
}

func TestFileErrorsThatAreNotA404AreCheckedWithTestE(t *testing.T) {
	// Arrange
	bx := newFakeBox("x")
	bx.fileErr = badRequest()
	bx.runFunc = func(c string) (int, string, error) {
		if strings.HasPrefix(c, "test -e") {
			return 1, "", nil
		}
		return 0, "", nil
	}
	inst := testBackend(newFakeAPI(), Options{}).instance(bx)
	// Act
	_, err := inst.Stat(ctxT(t), "/workspace/none")
	// Assert
	if !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("got %v", err)
	}
	bx.runFunc = nil // the path exists: the error stays what it is
	if _, err := inst.Stat(ctxT(t), "/workspace/x"); err == nil || errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("existing path: %v", err)
	}
}

func TestWriteFileCreatesParentsThenUploads(t *testing.T) {
	// Arrange
	bx := newFakeBox("x")
	inst := testBackend(newFakeAPI(), Options{}).instance(bx)
	// Act
	err := inst.WriteFile(ctxT(t), "/workspace/a/b/c.txt", []byte{0, 1, 2})
	// Assert
	if err != nil || !bx.ranContaining("mkdir -p -- '/workspace/a/b'") || len(bx.uploads) != 1 || string(bx.files["/workspace/a/b/c.txt"]) != "\x00\x01\x02" {
		t.Errorf("err %v runs %v uploads %v", err, bx.runs, bx.uploads)
	}
	if err := inst.WriteFile(ctxT(t), "relative", nil); err == nil {
		t.Error("relative path accepted")
	}
}

func TestWriteFileOfNothingTruncatesInPlaceInsteadOfUploading(t *testing.T) {
	// Arrange: Daytona rejects an upload without a file part, which is what an empty file is
	bx := newFakeBox("x")
	inst := testBackend(newFakeAPI(), Options{}).instance(bx)
	// Act
	err := inst.WriteFile(ctxT(t), "/workspace/empty.txt", []byte{})
	// Assert
	if err != nil || len(bx.uploads) != 0 || !bx.ranContaining(": > '/workspace/empty.txt'") {
		t.Errorf("err %v runs %v uploads %v", err, bx.runs, bx.uploads)
	}
}

func TestReadingAnEmptyFileReturnsNothingEvenWhenDaytonaFailsTheDownload(t *testing.T) {
	// Arrange: Daytona answers "Failed to read file" for a zero-byte file that does exist
	bx := newFakeBox("x")
	bx.files["/workspace/empty.txt"] = []byte{}
	bx.downloadFn = func(string) ([]byte, error) { return nil, errors.New("Failed to read file") }
	inst := testBackend(newFakeAPI(), Options{}).instance(bx)
	// Act
	data, size, err := inst.ReadFile(ctxT(t), "/workspace/empty.txt", 100)
	// Assert
	if err != nil || len(data) != 0 || size != 0 {
		t.Errorf("data %q size %d err %v", data, size, err)
	}
	// and a file that does not exist is still a failure
	if _, _, err := inst.ReadFile(ctxT(t), "/workspace/missing", 100); err == nil {
		t.Error("a missing file read as empty")
	}
}

func TestMkdirAllReportsAFailedMkdir(t *testing.T) {
	// Arrange
	bx := newFakeBox("x")
	bx.runFunc = func(string) (int, string, error) { return 1, "Permission denied", nil }
	// Act
	err := testBackend(newFakeAPI(), Options{}).instance(bx).MkdirAll(ctxT(t), "/root/x")
	// Assert
	if err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("got %v", err)
	}
}

func TestListWalksToDepthWithAbsolutePaths(t *testing.T) {
	// Arrange
	bx := newFakeBox("x")
	bx.dirs["/w"] = []fileEntry{{Name: "d", IsDir: true}, {Name: "f", Size: 3}}
	bx.dirs["/w/d"] = []fileEntry{{Name: "g", Size: 1}, {Name: "e", IsDir: true}}
	bx.dirs["/w/d/e"] = []fileEntry{{Name: "deep"}}
	inst := testBackend(newFakeAPI(), Options{}).instance(bx)
	paths := func(fs []sandbox.FileInfo) string {
		var p []string
		for _, f := range fs {
			p = append(p, f.Path+":"+f.Type[:1])
		}
		return strings.Join(p, " ")
	}
	// Act
	d1, err1 := inst.List(ctxT(t), "/w", 1)
	d2, err2 := inst.List(ctxT(t), "/w", 2)
	// Assert
	if err1 != nil || err2 != nil || paths(d1) != "/w/d:d /w/f:f" || paths(d2) != "/w/d:d /w/f:f /w/d/g:f /w/d/e:d" {
		t.Errorf("%q / %q (%v %v)", paths(d1), paths(d2), err1, err2)
	}
}

func TestEndpointIsTheSandboxsPreviewLinkWithItsToken(t *testing.T) {
	// Arrange
	ctx := ctxT(t)
	inst, err := testBackend(newFakeAPI(), Options{}).Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	px, ok := inst.(sandbox.PortExposer)
	if !ok {
		t.Fatal("a Daytona sandbox should expose its ports")
	}

	// Act
	ep, err := px.Endpoint(ctx, 5173)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ep.URL, "https://5173-") || strings.HasSuffix(ep.URL, "/") {
		t.Errorf("url = %q", ep.URL)
	}
	if ep.Headers["x-daytona-preview-token"] != "tok" || ep.Headers["X-Daytona-Skip-Preview-Warning"] != "true" {
		t.Errorf("headers = %v", ep.Headers)
	}
}
