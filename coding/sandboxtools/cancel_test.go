package sandboxtools_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
)

// cancellableJob reports when it started and whether it was stopped by job_kill (its context).
func cancellableJob(started chan<- struct{}, killed *atomic.Bool) sandboxtest.JobFunc {
	return func(ctx context.Context, command string, out func(stream, text string)) int {
		out("stdout", "started\n")
		close(started)
		select {
		case <-ctx.Done():
			killed.Store(true)
			return 143
		case <-time.After(10 * time.Second):
			return 0
		}
	}
}

func TestCancellingAForegroundCommandKillsIt(t *testing.T) {
	// Stopping the run while execute_command waits for its command kills the command (its whole job), it does not keep running.
	// Arrange
	started := make(chan struct{})
	var killed atomic.Bool
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.SetJobs(cancellableJob(started, &killed)) })
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()

	// Act
	_, err := f.call(ctx, "execute_command", `{"command":"python3 long_batch.py","timeoutSeconds":60}`)

	// Assert
	if err == nil {
		t.Fatal("a cancelled call returned no error")
	}
	eventually(t, "the cancelled command to be killed", killed.Load)
}

func TestCancellingTheRunKeepsABackgroundJobRunning(t *testing.T) {
	// A background:true job outlives the run that started it: cancelling the run afterwards does not kill it.
	// Arrange
	started := make(chan struct{})
	var killed atomic.Bool
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.SetJobs(cancellableJob(started, &killed)) })
	ctx, cancel := context.WithCancel(context.Background())

	// Act
	out, err := f.call(ctx, "execute_command", `{"command":"python3 server.py","background":true}`)
	<-started
	cancel()
	time.Sleep(100 * time.Millisecond)

	// Assert
	if err != nil || decodeExec(t, out).Status != "running" || killed.Load() {
		t.Fatalf("err %v out %s killed %v", err, out, killed.Load())
	}
}
