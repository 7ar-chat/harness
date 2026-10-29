package httpapi_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GhaziBenDahmane/harness/config"
)

func TestCommandsAreListedAndExpandedOverHTTP(t *testing.T) {
	// Arrange
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "commands"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "commands", "hello.md"), []byte("---\ndescription: Greet\nargument-hint: <who>\n---\nSay hello to $1."), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, rigOpts{env: map[string]string{config.HomeEnv: home, config.NoUserDirEnv: ""}})

	// Act
	resp := r.do("GET", "/api/commands", "alice", "")
	var list []map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	expanded := r.json("POST", "/api/commands/hello/expand", "alice", `{"args":"Ada"}`, 200)

	// Assert
	found := map[string]string{}
	for _, c := range list {
		found[c["name"]] = c["source"]
	}
	if found["hello"] != "user" || found["init"] != "builtin" {
		t.Errorf("commands = %v", list)
	}
	if expanded["prompt"] != "Say hello to Ada." {
		t.Errorf("prompt = %v", expanded["prompt"])
	}
}

func TestExpandingAnUnknownCommandIsNotFound(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{})

	// Act
	resp := r.do("POST", "/api/commands/nope/expand", "alice", `{}`)

	// Assert
	if resp.StatusCode != 404 {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
}

func TestRunModeAndAttachmentsAreValidatedOverHTTP(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{})
	id := r.newThread("alice")
	for body, want := range map[string]int{
		`{"message":"hi","mode":"yolo"}`: 400,
		`{"message":"hi","attachments":[{"name":"a.pdf","mime":"application/pdf","data":"JVBERg=="}]}`: 400,
		`{"message":"hi","mode":"plan"}`: 202,
	} {
		// Act
		resp := r.do("POST", "/api/threads/"+id+"/runs", "alice", body)

		// Assert
		if resp.StatusCode != want {
			t.Errorf("%s: status %d, want %d", body, resp.StatusCode, want)
		}
	}
}
