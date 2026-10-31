package sandboxtools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/coding/sandboxtools"
)

func TestAForegroundCommandWhoseProcessVanishedReportsAnUnknownExit(t *testing.T) {
	for _, status := range []string{"missing", "lost"} {
		t.Run(status, func(t *testing.T) {
			// Arrange: the job record says the process ended without an exit status.
			ts := toolsOn(t, newScripted(statusScript(status)), sandboxtools.Options{})

			// Act
			out, err := ts["execute_command"].InvokableRun(context.Background(), `{"command":"x"}`)

			// Assert
			r := decodeExec(t, out)
			if err != nil || r.ExitCode == nil || *r.ExitCode != -1 || !strings.Contains(r.Stderr, "ended without an exit status") {
				t.Fatalf("err %v out %s", err, out)
			}
		})
	}
}

func TestAForegroundCommandWhoseFilesWereRemovedWithoutAnExitCodeReportsMinusOne(t *testing.T) {
	// Arrange
	ts := toolsOn(t, newScripted(statusScript("removed")), sandboxtools.Options{})

	// Act
	out, err := ts["execute_command"].InvokableRun(context.Background(), `{"command":"x"}`)

	// Assert
	r := decodeExec(t, out)
	if err != nil || r.ExitCode == nil || *r.ExitCode != -1 || !strings.Contains(r.Stderr, "deleted its own output files") || !strings.Contains(r.Stderr, "exited with code -1") {
		t.Fatalf("err %v out %s", err, out)
	}
}

func TestJobOutputOfAJobWhoseProcessWasLostReportsItFinishedWithoutAStatus(t *testing.T) {
	// Arrange
	ts := toolsOn(t, newScripted(statusScript("lost")), sandboxtools.Options{})

	// Act
	out, err := ts["job_output"].InvokableRun(context.Background(), `{"jobId":"job_0123456789ab"}`)
	missing := toolsOn(t, newScripted(statusScript("missing")), sandboxtools.Options{})
	_, missErr := missing["job_output"].InvokableRun(context.Background(), `{"jobId":"job_0123456789ab"}`)

	// Assert
	m := decodeMap(t, out)
	if err != nil || m["status"] != "finished" || m["exitCode"] != float64(-1) {
		t.Fatalf("err %v out %s", err, out)
	}
	if missErr == nil || !strings.Contains(missErr.Error(), "the sandbox may have been recreated") {
		t.Fatalf("missing: %v", missErr)
	}
}
