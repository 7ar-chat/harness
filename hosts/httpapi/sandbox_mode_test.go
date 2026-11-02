package httpapi_test

import (
	"testing"
)

func TestRunCreationTakesASandboxMode(t *testing.T) {
	r := newRig(t, rigOpts{})
	id := r.newThread("alice")
	for _, c := range []struct{ body, want string }{
		{`{"message":"a","sandbox":"off"}`, "off"},
		{`{"message":"b"}`, "auto"},
		{`{"message":"c","sandbox":"on"}`, "on"},
	} {
		run := r.json("POST", "/api/threads/"+id+"/runs", "alice", c.body, 202)
		if run["sandbox"] != c.want {
			t.Fatalf("%s: sandbox %v, want %s", c.body, run["sandbox"], c.want)
		}
		r.stream(run["id"].(string), "alice") // let the run finish before the next one
	}
}

func TestRunCreationRefusesABadSandboxModeAndOnWithoutABackend(t *testing.T) {
	r := newRig(t, rigOpts{noSandbox: true})
	id := r.newThread("alice")

	for _, body := range []string{`{"message":"x","sandbox":"maybe"}`, `{"message":"x","sandbox":"on"}`} {
		if resp := r.do("POST", "/api/threads/"+id+"/runs", "alice", body); resp.StatusCode != 400 {
			t.Errorf("%s: status %d, want 400", body, resp.StatusCode)
		}
	}
	// off and auto work without a backend.
	for _, body := range []string{`{"message":"x","sandbox":"off"}`, `{"message":"y"}`} {
		run := r.json("POST", "/api/threads/"+id+"/runs", "alice", body, 202)
		r.stream(run["id"].(string), "alice")
	}
}
