package product

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/features/jobhunt"
)

func jobsRig(t *testing.T) *rig {
	t.Helper()
	board := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jobs":[{"id":1,"title":"Go Engineer","absolute_url":"https://boards.greenhouse.io/acme/jobs/1","location":{"name":"Paris"}}]}`))
	}))
	t.Cleanup(board.Close)
	r := newRig(t, rigOpts{team: true, env: map[string]string{"HARNESS_JOBS_DIR": t.TempDir()}})
	r.h.Jobs.Bases = map[string]string{jobhunt.Greenhouse: board.URL}
	return r
}

func body(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	return out
}

func TestJobSearchProfileScanAndTheApprovalOnlyThePersonGives(t *testing.T) {
	r := jobsRig(t)
	prof := `{"enabled":true,"titles":["go"],"companies":[{"name":"Acme","ats":"greenhouse","slug":"acme"}]}`
	if got := r.do("PUT", "/api/me/jobs/profile", "alice", prof).StatusCode; got != 200 {
		t.Fatalf("profile = %d", got)
	}
	if got := r.do("PUT", "/api/me/jobs/profile", "alice", `{"mode":"yolo"}`).StatusCode; got != 400 {
		t.Fatalf("a bad mode = %d, want 400", got)
	}
	scan := body(t, r.do("POST", "/api/me/jobs/scan", "alice", ""))
	if scan["added"] != float64(1) {
		t.Fatalf("scan %v", scan)
	}
	list := body(t, r.do("GET", "/api/me/jobs", "alice", ""))
	jobs := list["jobs"].([]any)
	if len(jobs) != 1 || list["counts"].(map[string]any)["new"] != float64(1) {
		t.Fatalf("list %v", list)
	}
	id := jobs[0].(map[string]any)["id"].(string)

	// the person's approval lets the search claim it; bob cannot touch alice's job
	if got := r.do("PATCH", "/api/me/jobs/"+id, "bob", `{"status":"approved"}`).StatusCode; got != 404 {
		t.Fatalf("bob = %d, want 404", got)
	}
	if got := r.do("PATCH", "/api/me/jobs/"+id, "alice", `{"status":"approved"}`).StatusCode; got != 200 {
		t.Fatalf("approve = %d", got)
	}
	if got := r.do("PATCH", "/api/me/jobs/"+id, "alice", `{"status":"applied"}`).StatusCode; got != 400 {
		t.Fatalf("applied by hand = %d, want 400 (only a claim leads to applied)", got)
	}
	if _, d, err := r.h.Jobs.Claim("alice", id); err != nil || !d.Allowed {
		t.Fatalf("claim %+v %v", d, err)
	}
	if n := len(body(t, r.do("GET", "/api/me/jobs", "bob", ""))["jobs"].([]any)); n != 0 {
		t.Fatalf("bob sees %d of alice's jobs", n)
	}
}

func TestJobSearchBoardsMustBeSupportedBoardAddresses(t *testing.T) {
	r := jobsRig(t)
	if got := r.do("POST", "/api/me/jobs/boards", "alice", `{"url":"https://example.com/careers"}`).StatusCode; got != 400 {
		t.Fatalf("a careers page = %d, want 400", got)
	}
	out := body(t, r.do("POST", "/api/me/jobs/boards", "alice", `{"url":"https://job-boards.greenhouse.io/acme/jobs/9"}`))
	if out["openJobs"] != float64(1) || !strings.Contains(toJSON(out["profile"]), `"slug":"acme"`) {
		t.Fatalf("%v", out)
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestJobSearchIsAbsentWithoutADirectory(t *testing.T) {
	r := newRig(t, rigOpts{team: true})
	if got := r.do("GET", "/api/me/jobs", "alice", "").StatusCode; got != 404 {
		t.Fatalf("status = %d, want 404", got)
	}
}

func TestJobSearchHasOneConversationPerUser(t *testing.T) {
	r := jobsRig(t)
	a := body(t, r.do("POST", "/api/me/jobs/thread", "alice", ""))["threadId"]
	again := body(t, r.do("POST", "/api/me/jobs/thread", "alice", ""))["threadId"]
	b := body(t, r.do("POST", "/api/me/jobs/thread", "bob", ""))["threadId"]
	if a == nil || a != again || a == b {
		t.Fatalf("alice %v then %v, bob %v", a, again, b)
	}
}
