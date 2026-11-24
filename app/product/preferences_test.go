package product

import (
	"strings"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
)

func prefRig(t *testing.T) *rig {
	t.Helper()
	tm := teamstore.NewMem()
	cnUsers(t, tm, "alice", "bob")
	return newRig(t, rigOpts{team: true, prod: Options{Team: tm}})
}

func TestPreferencesAreTheCallersOwnAndMerge(t *testing.T) {
	r := prefRig(t)
	empty := r.json("GET", "/api/me/preferences", "alice", "", 200)
	if len(empty["values"].(map[string]any)) != 0 || empty["updatedAt"] != nil {
		t.Fatalf("new person = %v", empty)
	}
	got := r.json("PATCH", "/api/me/preferences", "alice", `{"values":{"onboarding":{"dismissed":true},"ui.panel":{"h":220}}}`, 200)
	if got["updatedAt"] == nil || got["values"].(map[string]any)["onboarding"].(map[string]any)["dismissed"] != true {
		t.Fatalf("patched = %v", got)
	}
	again := r.json("PATCH", "/api/me/preferences", "alice", `{"values":{"ui.panel":null}}`, 200)
	vals := again["values"].(map[string]any)
	if _, ok := vals["ui.panel"]; ok || vals["onboarding"] == nil {
		t.Fatalf("merge = %v", vals)
	}
	// Another person sees none of it, and no login in the path means no way to reach alice's.
	if other := r.json("GET", "/api/me/preferences", "bob", "", 200); len(other["values"].(map[string]any)) != 0 {
		t.Fatalf("bob sees %v", other)
	}
	r.json("GET", "/api/me/preferences", "", "", 401)
	r.json("PATCH", "/api/me/preferences", "", `{"values":{}}`, 401)
}

func TestPreferencesRefuseTooMuchAndBadKeys(t *testing.T) {
	r := prefRig(t)
	big := `{"values":{"big":"` + strings.Repeat("x", 9000) + `"}}`
	if got := cnErrCode(r.json("PATCH", "/api/me/preferences", "alice", big, 413)); got != "too_large" {
		t.Fatalf("code %q", got)
	}
	r.json("PATCH", "/api/me/preferences", "alice", `{"values":{"Bad Key":1}}`, 400)
	r.json("PATCH", "/api/me/preferences", "alice", `{"values":`, 400)
	if left := r.json("GET", "/api/me/preferences", "alice", "", 200); len(left["values"].(map[string]any)) != 0 {
		t.Fatalf("a refused patch left %v", left)
	}
}
