package sandbox_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

// memThreads is the thread store the manager needs, in memory, with failure injection.
type memThreads struct {
	mu      sync.Mutex
	owner   map[string]string // threadID -> login
	sandbox map[string]string // threadID -> sandbox id
	deleted map[string]bool
	fail    map[string]error // method name -> error
	swapErr error
}

func newThreads() *memThreads {
	return &memThreads{owner: map[string]string{}, sandbox: map[string]string{}, deleted: map[string]bool{}, fail: map[string]error{}}
}

func (s *memThreads) add(threadID, login string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owner[threadID] = login
}

func (s *memThreads) recorded(threadID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sandbox[threadID]
}

func (s *memThreads) remove(threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted[threadID] = true
}

func (s *memThreads) failWith(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[method] = err
}

func (s *memThreads) ThreadSandbox(_ context.Context, threadID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail["ThreadSandbox"]; err != nil {
		return "", err
	}
	if _, ok := s.owner[threadID]; !ok || s.deleted[threadID] {
		return "", sandbox.ErrThreadNotFound
	}
	return s.sandbox[threadID], nil
}

func (s *memThreads) SetThreadSandbox(_ context.Context, threadID, expected, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail["SetThreadSandbox"]; err != nil {
		return err
	}
	if s.sandbox[threadID] == expected {
		s.sandbox[threadID] = id
	}
	return nil
}

func (s *memThreads) SwapThreadSandbox(_ context.Context, threadID, expected, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.swapErr != nil {
		return false, s.swapErr
	}
	if s.deleted[threadID] || s.sandbox[threadID] != expected {
		return false, nil
	}
	s.sandbox[threadID] = id
	return true, nil
}

func (s *memThreads) CountUserSandboxes(_ context.Context, login string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail["CountUserSandboxes"]; err != nil {
		return 0, err
	}
	n := 0
	for t, l := range s.owner {
		if l == login && !s.deleted[t] && s.sandbox[t] != "" {
			n++
		}
	}
	return n, nil
}

func (s *memThreads) UserSandboxes(_ context.Context, login string) ([]sandbox.UserSandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail["UserSandboxes"]; err != nil {
		return nil, err
	}
	var out []sandbox.UserSandbox
	for t, l := range s.owner {
		if l == login && !s.deleted[t] && s.sandbox[t] != "" {
			out = append(out, sandbox.UserSandbox{ThreadID: t, SandboxID: s.sandbox[t]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ThreadID < out[j].ThreadID })
	return out, nil
}

type rig struct {
	t       *testing.T
	threads *memThreads
	backend *sandboxtest.Backend
	coord   *sandbox.LocalCoordinator
	mgr     *sandbox.DefaultManager
}

func newRig(t *testing.T, opts sandbox.ManagerOptions) *rig {
	t.Helper()
	r := &rig{t: t, threads: newThreads(), backend: sandboxtest.NewBackend(), coord: sandbox.NewLocalCoordinator()}
	if opts.Timeout == 0 {
		opts.Timeout = time.Hour
	}
	if opts.RenewEvery == 0 {
		opts.RenewEvery = time.Hour
	}
	r.mgr = sandbox.NewManager(r.backend, r.threads, r.coord, opts)
	return r
}

// process is another manager over the same backend, store and coordinator (no cached handles).
func (r *rig) process(opts sandbox.ManagerOptions) *sandbox.DefaultManager {
	if opts.Timeout == 0 {
		opts.Timeout = time.Hour
	}
	return sandbox.NewManager(r.backend, r.threads, r.coord, opts)
}

func (r *rig) thread(id, login string) string {
	r.threads.add(id, login)
	return id
}

var bg = context.Background()

func TestADisabledManagerHandsOutNothingAndKillsNothing(t *testing.T) {
	// Arrange
	var nilMgr *sandbox.DefaultManager
	off := sandbox.NewManager(nil, newThreads(), nil, sandbox.ManagerOptions{})

	// Act
	_, acqErr := off.Acquire(bg, "t", "alice")
	_, getErr := off.Get(bg, "t")

	// Assert
	if off.Enabled() || nilMgr.Enabled() {
		t.Error("a manager without a backend reports itself enabled")
	}
	if !errors.Is(acqErr, sandbox.ErrDisabled) || !errors.Is(getErr, sandbox.ErrNoSandbox) {
		t.Errorf("acquire %v get %v", acqErr, getErr)
	}
	if err := off.Kill(bg, "sbx-1"); err != nil {
		t.Errorf("Kill on a disabled manager: %v", err)
	}
	off.Invalidate(bg, "t", "sbx-1") // must not panic or touch anything
}

func TestGetNeverCreatesAndSeparatesUnknownThreadsFromStoreFailures(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{})
	r.thread("t1", "alice")
	boom := errors.New("db down")

	// Act
	_, noSandbox := r.mgr.Get(bg, "t1")
	_, unknown := r.mgr.Get(bg, "nope")
	r.threads.failWith("ThreadSandbox", boom)
	_, storeErr := r.mgr.Get(bg, "t1")

	// Assert
	if !errors.Is(noSandbox, sandbox.ErrNoSandbox) || !errors.Is(unknown, sandbox.ErrNoSandbox) {
		t.Errorf("no sandbox %v unknown %v", noSandbox, unknown)
	}
	if !errors.Is(storeErr, boom) || errors.Is(storeErr, sandbox.ErrNoSandbox) {
		t.Errorf("a store failure must not read as 'no sandbox': %v", storeErr)
	}
	if len(r.backend.Created()) != 0 {
		t.Error("Get created a sandbox")
	}
}

func TestGetForgetsASandboxTheProviderNoLongerHas(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{})
	r.thread("t1", "alice")
	sb, _ := r.mgr.Acquire(bg, "t1", "alice")
	r.backend.Forget(sb.ID())

	// Act
	_, err := r.process(sandbox.ManagerOptions{}).Get(bg, "t1")

	// Assert
	if !errors.Is(err, sandbox.ErrNoSandbox) {
		t.Fatalf("err = %v", err)
	}
	if got := r.threads.recorded("t1"); got != "" {
		t.Errorf("the dead sandbox %q is still recorded", got)
	}
}

func TestAStoreFailureWhileForgettingIsReportedNotSwallowed(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{})
	r.thread("t1", "alice")
	sb, _ := r.mgr.Acquire(bg, "t1", "alice")
	r.backend.Forget(sb.ID())
	boom := errors.New("db down")
	r.threads.failWith("SetThreadSandbox", boom)

	// Act
	_, err := r.process(sandbox.ManagerOptions{}).Get(bg, "t1")

	// Assert
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestAcquireReportsStoreFailuresAtEveryStep(t *testing.T) {
	boom := errors.New("db down")
	for _, method := range []string{"ThreadSandbox", "CountUserSandboxes"} {
		t.Run(method, func(t *testing.T) {
			// Arrange
			r := newRig(t, sandbox.ManagerOptions{})
			r.thread("t1", "alice")
			r.threads.failWith(method, boom)

			// Act
			_, err := r.mgr.Acquire(bg, "t1", "alice")

			// Assert
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v", err)
			}
			if len(r.backend.Created()) != 0 {
				t.Error("a sandbox was created although the store failed")
			}
		})
	}
}

func TestAcquireOfAnUnknownThreadFails(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{})

	// Act
	_, err := r.mgr.Acquire(bg, "ghost", "alice")

	// Assert
	if !errors.Is(err, sandbox.ErrThreadNotFound) || len(r.backend.Created()) != 0 {
		t.Fatalf("err = %v created %d", err, len(r.backend.Created()))
	}
}

func TestAcquireRecordsTheSandboxAndTheNoticeOfOnCreate(t *testing.T) {
	// Arrange
	var seen struct {
		thread    string
		recreated bool
		recorded  string
	}
	var r *rig
	r = newRig(t, sandbox.ManagerOptions{OnCreate: func(_ context.Context, threadID string, sb sandbox.Sandbox, recreated bool) string {
		seen.thread, seen.recreated = threadID, recreated
		seen.recorded = r.threads.recorded(threadID) // the restore runs before the sandbox is recorded (nobody else can use it yet)
		return "workspace restored"
	}})
	r.thread("t1", "alice")

	// Act
	sb, err := r.mgr.Acquire(bg, "t1", "alice")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.NoticeOf(sb) != "workspace restored" || sandbox.WasRecreated(sb) {
		t.Errorf("notice %q recreated %v", sandbox.NoticeOf(sb), sandbox.WasRecreated(sb))
	}
	if seen.thread != "t1" || seen.recreated || seen.recorded != "" {
		t.Errorf("OnCreate saw %+v; the sandbox must not be recorded before the restore finished", seen)
	}
	if r.threads.recorded("t1") != sb.ID() {
		t.Errorf("recorded %q, want %q", r.threads.recorded("t1"), sb.ID())
	}
	again, _ := r.mgr.Acquire(bg, "t1", "alice")
	if sandbox.NoticeOf(again) != "" {
		t.Error("the creation notice was delivered twice")
	}
}

func TestACreatedSandboxGetsTheWorkspaceLayoutAndIsMetadataTagged(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{})
	r.thread("t1", "alice")

	// Act
	_, err := r.mgr.Acquire(bg, "t1", "alice")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	created := r.backend.Created()[0]
	for _, d := range sandbox.Layout {
		if !created.HasDir(d) {
			t.Errorf("%s missing", d)
		}
	}
	if md := r.backend.CreateOptions()[0].Metadata; md["work-thread"] != "t1" || md["work-user"] != sandbox.UserTag("alice") {
		t.Errorf("metadata %v", md)
	}
}

func TestAnUnhealthySandboxIsReplacedAfterTheStreakAndTheModelIsTold(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{UnhealthyRecreate: 40 * time.Millisecond})
	r.thread("t1", "alice")
	first, _ := r.mgr.Acquire(bg, "t1", "alice")
	r.backend.Created()[0].SetHealthy(false)
	fresh := func() *sandbox.DefaultManager {
		return r.process(sandbox.ManagerOptions{UnhealthyRecreate: 40 * time.Millisecond})
	}

	// Act: three failed checks over more than the recreate window
	var errs []error
	for range 3 {
		_, err := fresh().Get(bg, "t1")
		errs = append(errs, err)
		time.Sleep(25 * time.Millisecond)
	}
	second, err := fresh().Acquire(bg, "t1", "alice")

	// Assert
	for i, err := range errs[:2] {
		if !errors.Is(err, sandbox.ErrUnavailable) {
			t.Errorf("check %d: %v, want ErrUnavailable while the streak is short", i, err)
		}
	}
	if !errors.Is(errs[2], sandbox.ErrNoSandbox) {
		t.Errorf("third check: %v, want the sandbox forgotten", errs[2])
	}
	if err != nil || second.ID() == first.ID() || !sandbox.WasRecreated(second) {
		t.Fatalf("replacement %v err %v recreated %v", second, err, sandbox.WasRecreated(second))
	}
}

func TestADeadHealthCheckThatTheProviderConfirmsGoneIsForgottenAtOnce(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{})
	r.thread("t1", "alice")
	_, _ = r.mgr.Acquire(bg, "t1", "alice")
	inner := r.backend.Created()[0]
	inner.SetHealthy(false)
	inner.SetGone(true)

	// Act
	_, err := r.process(sandbox.ManagerOptions{}).Get(bg, "t1")

	// Assert
	if !errors.Is(err, sandbox.ErrNoSandbox) || r.threads.recorded("t1") != "" {
		t.Fatalf("err %v recorded %q", err, r.threads.recorded("t1"))
	}
}

func TestACachedHandleIsRecheckedAfterTheRecheckInterval(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{HealthRecheck: time.Millisecond})
	r.thread("t1", "alice")
	_, _ = r.mgr.Acquire(bg, "t1", "alice")
	r.backend.Created()[0].SetHealthy(false)
	time.Sleep(5 * time.Millisecond)

	// Act: the same process, its cached handle is stale and now unhealthy
	_, err := r.mgr.Get(bg, "t1")

	// Assert
	if !errors.Is(err, sandbox.ErrUnavailable) {
		t.Fatalf("err = %v; an unhealthy cached handle must not be served", err)
	}
}

func TestACachedHandleWithinTheRecheckWindowIsServedWithoutAHealthCall(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{HealthRecheck: time.Hour})
	r.thread("t1", "alice")
	first, _ := r.mgr.Acquire(bg, "t1", "alice")
	r.backend.Created()[0].SetHealthy(false)

	// Act
	second, err := r.mgr.Get(bg, "t1")

	// Assert
	if err != nil || second.ID() != first.ID() {
		t.Fatalf("err %v id %v", err, second)
	}
}

func TestTheCacheKeepsAtMostCacheSizeHandles(t *testing.T) {
	// Arrange: two threads, room for one handle; the least recently used is evicted, so reconnecting costs a Connect.
	r := newRig(t, sandbox.ManagerOptions{CacheSize: 1, HealthRecheck: time.Hour})
	r.thread("a", "alice")
	r.thread("b", "alice")
	a, _ := r.mgr.Acquire(bg, "a", "alice")
	b, _ := r.mgr.Acquire(bg, "b", "alice")

	// Act
	gotA, errA := r.mgr.Get(bg, "a")
	gotB, errB := r.mgr.Get(bg, "b")

	// Assert
	if errA != nil || errB != nil || gotA.ID() != a.ID() || gotB.ID() != b.ID() {
		t.Fatalf("a %v %v b %v %v", gotA, errA, gotB, errB)
	}
}

func TestAcquireWaitsForAnotherCreatorAndReusesItsSandbox(t *testing.T) {
	// Arrange: a slow creation in one process; a second process asks meanwhile
	r := newRig(t, sandbox.ManagerOptions{})
	r.backend.CreateDelay = 150 * time.Millisecond
	r.thread("t1", "alice")
	done := make(chan sandbox.Sandbox, 1)
	go func() {
		sb, _ := r.mgr.Acquire(bg, "t1", "alice")
		done <- sb
	}()
	deadline := time.Now().Add(5 * time.Second)
	for r.coordHeld("work:sbx:lock:t1") == false && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	// Act
	waiter, err := r.process(sandbox.ManagerOptions{}).Acquire(bg, "t1", "alice")
	creator := <-done

	// Assert
	if err != nil || creator == nil || waiter.ID() != creator.ID() || len(r.backend.Created()) != 1 {
		t.Fatalf("waiter %v err %v creator %v created %d", waiter, err, creator, len(r.backend.Created()))
	}
}

// coordHeld reports whether the coordinator key currently exists.
func (r *rig) coordHeld(key string) bool {
	ok, _ := r.coord.Exists(bg, key)
	return ok
}

func TestAcquireTimesOutWaitingForAStuckLock(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{LockWait: 150 * time.Millisecond})
	r.thread("t1", "alice")
	ok, _ := r.coord.SetNX(bg, "work:sbx:lock:t1", "someone-else", time.Hour)
	if !ok {
		t.Fatal("could not hold the lock")
	}

	// Act
	_, err := r.mgr.Acquire(bg, "t1", "alice")

	// Assert
	if !errors.Is(err, sandbox.ErrUnavailable) || len(r.backend.Created()) != 0 {
		t.Fatalf("err %v created %d", err, len(r.backend.Created()))
	}
}

func TestAcquireStopsWaitingForTheLockWhenTheCallerCancels(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{})
	r.thread("t1", "alice")
	_, _ = r.coord.SetNX(bg, "work:sbx:lock:t1", "someone-else", time.Hour)
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()

	// Act
	_, err := r.mgr.Acquire(ctx, "t1", "alice")

	// Assert
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

// brokenCoordinator fails SetNX, so no lock can be taken.
type brokenCoordinator struct {
	*sandbox.LocalCoordinator
	err error
}

func (b brokenCoordinator) SetNX(context.Context, string, string, time.Duration) (bool, error) {
	return false, b.err
}

func TestACoordinatorFailureIsAnErrorNotACreationWithoutALock(t *testing.T) {
	// Arrange
	boom := errors.New("redis down")
	threads := newThreads()
	threads.add("t1", "alice")
	backend := sandboxtest.NewBackend()
	m := sandbox.NewManager(backend, threads, brokenCoordinator{sandbox.NewLocalCoordinator(), boom}, sandbox.ManagerOptions{})

	// Act
	_, err := m.Acquire(bg, "t1", "alice")

	// Assert
	if !errors.Is(err, boom) || len(backend.Created()) != 0 {
		t.Fatalf("err %v created %d", err, len(backend.Created()))
	}
}

func TestAcquireKillsAndReportsTheSandboxWhenRecordingFails(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{})
	r.thread("t1", "alice")
	boom := errors.New("db down")
	r.threads.swapErr = boom

	// Act
	_, err := r.mgr.Acquire(bg, "t1", "alice")

	// Assert
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	waitFor(t, func() bool { return len(r.backend.Killed()) == 1 })
}

func TestAcquireOfAThreadDeletedDuringCreationKillsTheSandbox(t *testing.T) {
	// Arrange
	var r *rig
	r = newRig(t, sandbox.ManagerOptions{OnCreate: func(context.Context, string, sandbox.Sandbox, bool) string {
		r.threads.remove("t1") // the conversation is deleted while the sandbox is being prepared
		return ""
	}})
	r.thread("t1", "alice")

	// Act
	_, err := r.mgr.Acquire(bg, "t1", "alice")

	// Assert
	if !errors.Is(err, sandbox.ErrNoSandbox) {
		t.Fatalf("err = %v", err)
	}
	waitFor(t, func() bool { return len(r.backend.Killed()) == 1 })
}

func TestACreationThatLostTheRecordRaceKeepsTheRecordedSandbox(t *testing.T) {
	// Arrange: another process records its sandbox while ours is being prepared
	var r *rig
	r = newRig(t, sandbox.ManagerOptions{OnCreate: func(context.Context, string, sandbox.Sandbox, bool) string {
		other := sandboxtest.NewSandbox("other")
		r.backend.Add(other)
		r.threads.mu.Lock()
		r.threads.sandbox["t1"] = "other"
		r.threads.mu.Unlock()
		return ""
	}})
	r.thread("t1", "alice")

	// Act
	sb, err := r.mgr.Acquire(bg, "t1", "alice")

	// Assert
	if err != nil || sb.ID() != "other" {
		t.Fatalf("got %v, %v; want the sandbox recorded meanwhile", sb, err)
	}
	waitFor(t, func() bool { return len(r.backend.Killed()) == 1 && r.backend.Killed()[0] == "sbx-1" })
}

func TestACreationThatLostTheRaceToAVanishedRecordAsksForARetry(t *testing.T) {
	// Arrange: the record changed and then cleared meanwhile
	var r *rig
	r = newRig(t, sandbox.ManagerOptions{OnCreate: func(context.Context, string, sandbox.Sandbox, bool) string {
		r.threads.mu.Lock()
		r.threads.sandbox["t1"] = "x"
		r.threads.mu.Unlock()
		return ""
	}})
	r.thread("t1", "alice")
	r.threads.failWith("SetThreadSandbox", nil)
	// the competing record points to a sandbox that does not exist at the provider

	// Act
	_, err := r.mgr.Acquire(bg, "t1", "alice")

	// Assert
	if !errors.Is(err, sandbox.ErrUnavailable) {
		t.Fatalf("err = %v, want a retryable error", err)
	}
}

func TestQuotaReclaimsSandboxesThatAreGoneAndKeepsUnknownOnes(t *testing.T) {
	// Arrange: quota of 2; thread a's sandbox expired, thread b's provider is unreachable
	r := newRig(t, sandbox.ManagerOptions{MaxPerUser: 2})
	for _, id := range []string{"a", "b", "c"} {
		r.thread(id, "alice")
	}
	a, _ := r.mgr.Acquire(bg, "a", "alice")
	_, _ = r.mgr.Acquire(bg, "b", "alice")
	r.backend.Forget(a.ID())

	// Act
	c, err := r.mgr.Acquire(bg, "c", "alice")

	// Assert
	if err != nil || c == nil {
		t.Fatalf("err = %v; the expired sandbox must free its quota", err)
	}
	if r.threads.recorded("a") != "" {
		t.Errorf("thread a still records %q", r.threads.recorded("a"))
	}
	r.backend.SetConnectErr(errors.New("provider timeout"))
	d := r.thread("d", "alice")
	_, errD := r.process(sandbox.ManagerOptions{MaxPerUser: 2}).Acquire(bg, d, "alice")
	if !errors.Is(errD, sandbox.ErrQuota) {
		t.Errorf("with the provider unreachable the live sandboxes still count: %v", errD)
	}
}

func TestQuotaReclaimReportsStoreFailures(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{MaxPerUser: 1})
	r.thread("a", "alice")
	r.thread("b", "alice")
	_, _ = r.mgr.Acquire(bg, "a", "alice")
	boom := errors.New("db down")
	r.threads.failWith("UserSandboxes", boom)

	// Act
	_, err := r.mgr.Acquire(bg, "b", "alice")

	// Assert
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestQuietCreationKeepsTheNoticeForTheAgentsNextAcquire(t *testing.T) {
	// Arrange: an Office preview creates the sandbox; the model must still hear that the workspace was restored
	r := newRig(t, sandbox.ManagerOptions{OnCreate: func(context.Context, string, sandbox.Sandbox, bool) string { return "restored 3 files" }})
	r.thread("t1", "alice")

	// Act
	quiet, err := r.mgr.Acquire(sandbox.Quiet(bg), "t1", "alice")
	api, err2 := r.mgr.Acquire(sandbox.Quiet(bg), "t1", "alice")
	agent, err3 := r.mgr.Acquire(bg, "t1", "alice")
	later, err4 := r.mgr.Acquire(bg, "t1", "alice")

	// Assert
	if err != nil || err2 != nil || err3 != nil || err4 != nil {
		t.Fatal(err, err2, err3, err4)
	}
	if sandbox.NoticeOf(quiet) != "" || sandbox.NoticeOf(api) != "" {
		t.Error("a quiet acquire consumed the notice")
	}
	if sandbox.NoticeOf(agent) != "restored 3 files" {
		t.Errorf("agent notice = %q", sandbox.NoticeOf(agent))
	}
	if sandbox.NoticeOf(later) != "" {
		t.Errorf("the notice was delivered again: %q", sandbox.NoticeOf(later))
	}
	if !sandbox.Quieted(sandbox.Quiet(bg)) || sandbox.Quieted(bg) {
		t.Error("Quieted does not follow Quiet")
	}
}

func TestQuietRecreationIsAnnouncedToTheAgentWithoutAText(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{UnhealthyRecreate: time.Nanosecond})
	r.thread("t1", "alice")
	_, _ = r.mgr.Acquire(bg, "t1", "alice")
	r.backend.Created()[0].SetHealthy(false)
	for range 3 {
		_, _ = r.process(sandbox.ManagerOptions{UnhealthyRecreate: time.Nanosecond}).Get(bg, "t1")
	}

	// Act
	_, err := r.process(sandbox.ManagerOptions{}).Acquire(sandbox.Quiet(bg), "t1", "alice")
	agent, err2 := r.process(sandbox.ManagerOptions{}).Acquire(bg, "t1", "alice")

	// Assert
	if err != nil || err2 != nil {
		t.Fatal(err, err2)
	}
	if !sandbox.WasRecreated(agent) || sandbox.NoticeOf(agent) != "" {
		t.Errorf("recreated %v notice %q", sandbox.WasRecreated(agent), sandbox.NoticeOf(agent))
	}
}

func TestInvalidateForgetsOnlyWhatTheProviderConfirmsGone(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(r *rig, id string)
		wantForget bool
	}{
		{"removed at the provider", func(r *rig, id string) { r.backend.Forget(id) }, true},
		{"terminated but still listed", func(r *rig, id string) {
			r.backend.Created()[0].SetHealthy(false)
			r.backend.Created()[0].SetGone(true)
		}, true},
		{"still healthy", func(*rig, string) {}, false},
		{"unhealthy but not gone", func(r *rig, id string) { r.backend.Created()[0].SetHealthy(false) }, false},
		{"provider unreachable", func(r *rig, id string) { r.backend.SetConnectErr(errors.New("timeout")) }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			r := newRig(t, sandbox.ManagerOptions{})
			r.thread("t1", "alice")
			sb, _ := r.mgr.Acquire(bg, "t1", "alice")
			c.setup(r, sb.ID())

			// Act
			r.mgr.Invalidate(bg, "t1", sb.ID())

			// Assert
			forgotten := r.threads.recorded("t1") == ""
			if forgotten != c.wantForget {
				t.Fatalf("forgotten = %v, want %v", forgotten, c.wantForget)
			}
			if c.wantForget {
				next, err := r.process(sandbox.ManagerOptions{}).Acquire(bg, "t1", "alice")
				if err != nil || !sandbox.WasRecreated(next) {
					t.Errorf("replacement %v err %v recreated %v: the model must be told /workspace was reset", next, err, next != nil && sandbox.WasRecreated(next))
				}
			}
		})
	}
}

func TestInvalidateIgnoresAnAlreadyReplacedSandboxAndEmptyIDs(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{})
	r.thread("t1", "alice")
	sb, _ := r.mgr.Acquire(bg, "t1", "alice")
	r.backend.Forget(sb.ID())

	// Act
	r.mgr.Invalidate(bg, "t1", "")          // nothing to invalidate
	r.mgr.Invalidate(bg, "t1", "sbx-other") // not the recorded one: already replaced
	r.mgr.Invalidate(bg, "gone-thread", sb.ID())

	// Assert
	if r.threads.recorded("t1") != sb.ID() {
		t.Fatalf("recorded %q; a stale invalidation must not clear the current record", r.threads.recorded("t1"))
	}
}

func TestKillDropsTheCachedHandleAndTheSandbox(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{HealthRecheck: time.Hour})
	r.thread("t1", "alice")
	sb, _ := r.mgr.Acquire(bg, "t1", "alice")

	// Act
	err := r.mgr.Kill(bg, sb.ID())
	emptyErr := r.mgr.Kill(bg, "")
	_, getErr := r.mgr.Get(bg, "t1")

	// Assert
	if err != nil || emptyErr != nil {
		t.Fatal(err, emptyErr)
	}
	if k := r.backend.Killed(); len(k) != 1 || k[0] != sb.ID() {
		t.Errorf("killed %v", k)
	}
	if !errors.Is(getErr, sandbox.ErrNoSandbox) {
		t.Errorf("a killed sandbox was served from the cache: %v", getErr)
	}
}

func TestRunningCommandsAndKeepAliveRenewTheSandbox(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{Timeout: 30 * time.Millisecond, RenewEvery: time.Hour})
	r.thread("t1", "alice")
	sb, _ := r.mgr.Acquire(bg, "t1", "alice")
	inner := r.backend.Created()[0]
	gate := make(chan struct{})
	inner.SetExec(func(ctx context.Context, _ sandbox.ExecRequest) (sandbox.ExecResult, error) {
		<-gate
		return sandbox.ExecResult{}, nil
	})
	before := inner.Renewals()

	// Act: a long command keeps the sandbox alive while it runs, and only then
	done := make(chan error, 1)
	go func() { _, err := sb.Exec(bg, sandbox.ExecRequest{Command: "sleep"}); done <- err }()
	waitFor(t, func() bool { return inner.Renewals() >= before+3 })
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after := inner.Renewals()
	time.Sleep(80 * time.Millisecond)

	// Assert
	if inner.Renewals() != after {
		t.Errorf("renewals continued after the command ended: %d -> %d", after, inner.Renewals())
	}
}

func TestKeepAliveLooksThroughTheNoticeAndRecreatedWrappers(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{Timeout: 30 * time.Millisecond, OnCreate: func(context.Context, string, sandbox.Sandbox, bool) string { return "note" }})
	r.thread("t1", "alice")
	sb, _ := r.mgr.Acquire(bg, "t1", "alice") // a noticed wrapper
	inner := r.backend.Created()[0]
	wrapped := sandbox.MarkRecreated(sb)
	before := inner.Renewals()

	// Act
	stop := sandbox.KeepAlive(bg, wrapped)
	waitFor(t, func() bool { return inner.Renewals() >= before+2 })
	stop()
	stop() // idempotent
	plainStop := sandbox.KeepAlive(bg, sandboxtest.NewSandbox("plain"))
	plainStop()

	// Assert
	after := inner.Renewals()
	time.Sleep(60 * time.Millisecond)
	if inner.Renewals() != after {
		t.Errorf("renewals continued after stop: %d -> %d", after, inner.Renewals())
	}
}

func TestKeepAliveIsDisabledWithoutATimeout(t *testing.T) {
	// A manager with no sandbox lifetime has nothing to renew.
	// Arrange
	r := &rig{threads: newThreads(), backend: sandboxtest.NewBackend()}
	r.mgr = sandbox.NewManager(r.backend, r.threads, nil, sandbox.ManagerOptions{})
	r.thread("t1", "alice")
	sb, _ := r.mgr.Acquire(bg, "t1", "alice")
	inner := r.backend.Created()[0]
	before := inner.Renewals()

	// Act
	stop := sandbox.KeepAlive(bg, sb)
	time.Sleep(30 * time.Millisecond)
	stop()

	// Assert
	if inner.Renewals() != before {
		t.Errorf("renewed %d times without a timeout", inner.Renewals()-before)
	}
}

func TestStreamedWritesSurviveTheManagersWrappers(t *testing.T) {
	// Arrange
	r := newRig(t, sandbox.ManagerOptions{OnCreate: func(context.Context, string, sandbox.Sandbox, bool) string { return "note" }})
	r.thread("t1", "alice")
	noticed, _ := r.mgr.Acquire(bg, "t1", "alice")
	managed, _ := r.mgr.Get(bg, "t1")
	recreated := sandbox.MarkRecreated(managed)

	// Act
	var errs []error
	for i, sb := range []sandbox.Sandbox{noticed, managed, recreated} {
		errs = append(errs, sandbox.WriteFrom(bg, sb, "/workspace/f"+string(rune('a'+i)), strings.NewReader("body")))
	}

	// Assert
	for i, err := range errs {
		if err != nil {
			t.Errorf("wrapper %d: %v", i, err)
		}
	}
	inner := r.backend.Created()[0]
	for _, p := range []string{"/workspace/fa", "/workspace/fb", "/workspace/fc"} {
		if got, _ := inner.File(p); got != "body" {
			t.Errorf("%s = %q", p, got)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

// brokerBackend creates sandboxes that can hold credentials outside the workload (or not).
type brokerBackend struct {
	*sandboxtest.Backend
	broker bool
}

func (b brokerBackend) Create(ctx context.Context, o sandbox.CreateOptions) (sandbox.Instance, error) {
	inst, err := b.Backend.Create(ctx, o)
	if err != nil || !b.broker {
		return inst, err
	}
	return brokerInstance{inst}, nil
}

type brokerInstance struct{ sandbox.Instance }

func (brokerInstance) SetCredentials(context.Context, sandbox.CredentialSet) error { return nil }

func TestBrokerOfFindsTheBrokerOfAManagedSandboxOnlyWhenTheInstanceHasOne(t *testing.T) {
	for _, withBroker := range []bool{true, false} {
		// Arrange
		threads := newThreads()
		threads.add("t1", "alice")
		m := sandbox.NewManager(brokerBackend{sandboxtest.NewBackend(), withBroker}, threads, nil, sandbox.ManagerOptions{})
		created, err := m.Acquire(bg, "t1", "alice")
		if err != nil {
			t.Fatal(err)
		}
		got, err := m.Get(bg, "t1")
		if err != nil {
			t.Fatal(err)
		}

		// Act
		fromAcquire, fromGet := sandbox.BrokerOf(created), sandbox.BrokerOf(sandbox.MarkRecreated(got))

		// Assert
		if (fromAcquire != nil) != withBroker || (fromGet != nil) != withBroker {
			t.Errorf("instance with broker=%v: BrokerOf(acquired) = %v, BrokerOf(got, recreated) = %v", withBroker, fromAcquire, fromGet)
		}
	}
}
