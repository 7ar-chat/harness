// Package conformance checks that a sandbox adapter runs the harness' sandbox
// tools (execute_command with background jobs, read/write/edit/list files,
// search_files) end to end: the scripts those tools send through
// sandbox.Sandbox.Exec need bash, GNU coreutils, python3 and ripgrep in the
// sandbox image (docs/sandbox-adapters.md). Run it after
// sandboxtest.RunConformance, which checks the adapter's own contract:
//
//	func TestToolsConformance(t *testing.T) {
//		conformance.Run(t, func(t *testing.T) sandbox.Backend { return myadapter.New(...) })
//	}
package conformance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"

	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/engine/tools/sandboxtools"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/store/memstore"
)

// Run drives the sandbox tools on a backend through a real sandbox.Manager.
func Run(t *testing.T, newBackend func(t *testing.T) sandbox.Backend) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	st := memstore.New()
	th, err := st.CreateThread(ctx, "conformance", "t")
	if err != nil {
		t.Fatal(err)
	}
	mgr := sandbox.NewManager(newBackend(t), st, nil, sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour})
	p := sandboxtools.New(mgr, sandboxtools.Options{CommandTimeout: 30 * time.Second, MaxSandboxesPerUser: 5})
	list, cleanup, err := p.Tools(ctx, tools.RunContext{ThreadID: th.ID, RunID: "conformance", Login: "conformance"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	byName := map[string]tool.InvokableTool{}
	for _, x := range list {
		info, _ := x.Info(ctx)
		byName[info.Name] = x.(tool.InvokableTool)
	}
	t.Cleanup(func() {
		if sb, err := mgr.Get(context.Background(), th.ID); err == nil {
			_ = mgr.Kill(context.Background(), sb.ID())
		}
	})
	raw := func(t *testing.T, name, args string) string {
		t.Helper()
		tl := byName[name]
		if tl == nil {
			t.Fatalf("no tool %s", name)
		}
		out, err := tl.InvokableRun(ctx, args)
		if err != nil {
			t.Fatalf("%s(%s): %v", name, args, err)
		}
		return out
	}
	call := func(t *testing.T, name, args string) map[string]any {
		t.Helper()
		out := raw(t, name, args)
		var m map[string]any
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("%s returned %q", name, out)
		}
		return m
	}

	t.Run("ExecuteCommand", func(t *testing.T) {
		r := call(t, "execute_command", `{"command":"echo hello; echo oops >&2; exit 3"}`)
		if r["exitCode"] != float64(3) || !strings.Contains(str(r["stdout"]), "hello") || !strings.Contains(str(r["stderr"]), "oops") {
			t.Fatalf("result %v", r)
		}
	})
	t.Run("WriteReadEditList", func(t *testing.T) {
		call(t, "write_file", `{"path":"/workspace/conf/tools/a.txt","content":"alpha\nbeta\n"}`)
		raw(t, "read_file", `{"path":"/workspace/conf/tools/a.txt"}`) // edit_file refuses unread files
		e := call(t, "edit_file", `{"path":"/workspace/conf/tools/a.txt","oldText":"beta","newText":"gamma"}`)
		if e["replacements"] != float64(1) {
			t.Fatalf("edit_file %v", e)
		}
		if r := raw(t, "read_file", `{"path":"/workspace/conf/tools/a.txt"}`); !strings.Contains(r, "gamma") {
			t.Fatalf("read_file %q", r)
		}
		l := call(t, "list_files", `{"path":"/workspace/conf/tools","depth":1}`)
		if !strings.Contains(toJSON(l), "a.txt") {
			t.Fatalf("list_files %v", l)
		}
	})
	t.Run("SearchFiles", func(t *testing.T) {
		call(t, "write_file", `{"path":"/workspace/conf/search/x.py","content":"def conformance_needle():\n    return 1\n"}`)
		r := call(t, "search_files", `{"pattern":"conformance_needle","path":"/workspace/conf/search"}`)
		if !strings.Contains(toJSON(r), "x.py") {
			t.Fatalf("search_files found nothing: %v (needs ripgrep or python3 in the image)", r)
		}
	})
	t.Run("BackgroundJobRunsToCompletion", func(t *testing.T) {
		r := call(t, "execute_command", `{"command":"echo started; sleep 2; echo finished","background":true}`)
		id, _ := r["jobId"].(string)
		if id == "" {
			t.Fatalf("no jobId: %v", r)
		}
		o := call(t, "job_output", `{"jobId":"`+id+`","waitSeconds":30}`)
		if o["status"] != "finished" || o["exitCode"] != float64(0) || !strings.Contains(str(o["stdoutTail"]), "finished") {
			t.Fatalf("job_output %v (needs setsid, bash, stat -c, base64 -w0, tail -F)", o)
		}
	})
	t.Run("JobKillStopsTheProcessTree", func(t *testing.T) {
		r := call(t, "execute_command", `{"command":"sleep 600 & echo $! > /workspace/conf/child.pid; wait","background":true}`)
		id, _ := r["jobId"].(string)
		if id == "" {
			t.Fatalf("no jobId: %v", r)
		}
		time.Sleep(time.Second)
		k := call(t, "job_kill", `{"jobId":"`+id+`"}`)
		if k["status"] != "killed" {
			t.Fatalf("job_kill %v", k)
		}
		deadline := time.Now().Add(15 * time.Second)
		for {
			out := call(t, "execute_command", `{"command":"kill -0 $(cat /workspace/conf/child.pid) 2>/dev/null && echo alive || echo dead"}`)
			if strings.Contains(str(out["stdout"]), "dead") {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the job's child survived job_kill: the job must run in its own session/process group (setsid, kill -- -PID)")
			}
			time.Sleep(500 * time.Millisecond)
		}
	})
}

func str(v any) string { s, _ := v.(string); return s }

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
