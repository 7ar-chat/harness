package product

import (
	"encoding/json"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/engine/tools"
	"io"
	"strings"
	"testing"
	"time"
)

// tmList GETs a route that answers a JSON array of objects.
func tmList(t *testing.T, r *rig, path, user string) []map[string]any {
	t.Helper()
	resp := r.do("GET", path, user, "")
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s as %s = %d: %s", path, user, resp.StatusCode, b)
	}
	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("GET %s: %v: %s", path, err, b)
	}
	return out
}

// tmWaitLine returns the first line of a feed (see feedOf) that contains sub.
func tmWaitLine(t *testing.T, ch <-chan string, sub string) string {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case l := <-ch:
			if strings.Contains(l, sub) {
				return l
			}
		case <-timeout:
			t.Fatalf("no feed line containing %s", sub)
		}
	}
}

// tmQuiet fails if the feed delivers a line containing sub within a short window.
func tmQuiet(t *testing.T, ch <-chan string, sub string) {
	t.Helper()
	deadline := time.After(250 * time.Millisecond)
	for {
		select {
		case l := <-ch:
			if strings.Contains(l, sub) {
				t.Fatalf("unexpected feed line %s", l)
			}
		case <-deadline:
			return
		}
	}
}

// aiMsg and tmRunContext keep the supervisor tests short.
type aiMsg = ai.Message

func tmRunContext(thread, login string) tools.RunContext {
	return tools.RunContext{ThreadID: thread, Login: login}
}
