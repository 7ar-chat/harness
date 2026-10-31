package opensandbox_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/adapters/sandbox/opensandbox"
	"github.com/7ar-chat/harness/core/sandbox"
)

// fakeVault emulates the lifecycle API and the egress sidecar's Credential Vault for one sandbox.
type fakeVault struct {
	mu         sync.Mutex
	created    map[string]any
	exists     bool
	revision   int
	creds      map[string]bool
	binds      map[string]bool
	conflicts  int // patches to refuse with 409 before accepting one
	patches    []map[string]any
	creates    int
	srv        *httptest.Server
	cleartexts []string
}

func newFakeVault(t *testing.T) *fakeVault {
	t.Helper()
	f := &fakeVault{creds: map[string]bool{}, binds: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVault) state() map[string]any {
	var cs, bs []map[string]any
	for n := range f.creds {
		cs = append(cs, map[string]any{"name": n, "sourceType": "inline", "revision": f.revision})
	}
	for n := range f.binds {
		bs = append(bs, map[string]any{"name": n, "revision": f.revision})
	}
	return map[string]any{"revision": f.revision, "credentials": cs, "bindings": bs}
}

func (f *fakeVault) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
		_ = json.Unmarshal(body, &f.created)
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"id":"sbx-1","status":{"state":"Pending"},"entrypoint":[],"createdAt":"2026-10-06T00:00:00Z"}`))
	case strings.HasPrefix(r.URL.Path, "/v1/sandboxes/sbx-1/endpoints/"):
		// every endpoint (execd, egress) is this server; the egress paths are served below
		_, _ = w.Write([]byte(`{"endpoint":"` + strings.TrimPrefix(f.srv.URL, "http://") + `"}`))
	case r.URL.Path == "/v1/sandboxes/sbx-1":
		_, _ = w.Write([]byte(`{"id":"sbx-1","status":{"state":"Running"},"entrypoint":[],"createdAt":"2026-10-06T00:00:00Z"}`))
	case r.URL.Path == "/ping":
		w.WriteHeader(200)
	case r.URL.Path == "/credential-vault" && r.Method == http.MethodGet:
		if !f.exists {
			http.Error(w, "credential vault not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(f.state())
	case r.URL.Path == "/credential-vault" && r.Method == http.MethodPost:
		if f.exists {
			http.Error(w, "credential vault already exists", http.StatusConflict)
			return
		}
		var req struct {
			Credentials []struct {
				Name   string
				Source struct{ Value string }
			}
			Bindings []struct{ Name string }
		}
		_ = json.Unmarshal(body, &req)
		for _, c := range req.Credentials {
			f.creds[c.Name] = true
			f.cleartexts = append(f.cleartexts, c.Source.Value)
		}
		for _, b := range req.Bindings {
			f.binds[b.Name] = true
		}
		f.exists, f.revision = true, 1
		f.creates++
		_ = json.NewEncoder(w).Encode(f.state())
	case r.URL.Path == "/credential-vault" && r.Method == http.MethodPatch:
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if f.conflicts > 0 {
			f.conflicts--
			http.Error(w, "revision conflict", http.StatusConflict)
			return
		}
		if exp, ok := req["expectedRevision"].(float64); !ok || int(exp) != f.revision {
			http.Error(w, "revision conflict", http.StatusConflict)
			return
		}
		f.patches = append(f.patches, req)
		apply := func(group string, set map[string]bool) {
			g, _ := req[group].(map[string]any)
			for _, k := range []string{"add", "replace"} {
				items, _ := g[k].([]any)
				for _, it := range items {
					m := it.(map[string]any)
					set[m["name"].(string)] = true
				}
			}
			dels, _ := g["delete"].([]any)
			for _, d := range dels {
				delete(set, d.(string))
			}
		}
		apply("credentials", f.creds)
		apply("bindings", f.binds)
		f.revision++
		_ = json.NewEncoder(w).Encode(f.state())
	default:
		w.WriteHeader(500)
	}
}

func brokerBackend(t *testing.T, f *fakeVault, credentialProxy bool) sandbox.Instance {
	t.Helper()
	b, err := opensandbox.NewOpenSandbox(opensandbox.OpenSandboxOptions{
		URL: f.srv.URL, Image: "img", CPU: "1", Memory: "1Gi", TimeoutSeconds: 60,
		EgressAllowlist: []string{"github.com", "api.github.com"}, CredentialProxy: credentialProxy,
	})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := b.Create(context.Background(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func githubSet(token string) sandbox.CredentialSet {
	return sandbox.CredentialSet{
		Credentials: []sandbox.Credential{{Name: "github-git", Value: "basic-" + token}, {Name: "github-api", Value: token}},
		Bindings: []sandbox.CredentialBinding{
			{Name: "github-git", Host: "github.com", Paths: []string{"/acme/api.git*"}, Auth: sandbox.AuthBasic, Credential: "github-git"},
			{Name: "github-api", Host: "api.github.com", Paths: []string{"/repos/acme/api/*"}, Auth: sandbox.AuthBearer, Credential: "github-api"},
		},
	}
}

func TestOnlyCredentialProxySandboxesExposeTheBrokerAndAskForIt(t *testing.T) {
	// A sandbox created without the credential proxy must never look like one that keeps secrets out of the workload.
	off, on := newFakeVault(t), newFakeVault(t)

	plain := brokerBackend(t, off, false)
	proxied := brokerBackend(t, on, true)

	if sandbox.BrokerOf(plain) != nil {
		t.Error("a sandbox created without the credential proxy exposes a broker")
	}
	if sandbox.BrokerOf(proxied) == nil {
		t.Fatal("a sandbox created with the credential proxy exposes no broker")
	}
	if p, _ := off.created["credentialProxy"].(map[string]any); p != nil {
		t.Errorf("credentialProxy sent although disabled: %v", off.created["credentialProxy"])
	}
	if p, _ := on.created["credentialProxy"].(map[string]any); p == nil || p["enabled"] != true {
		t.Errorf("credentialProxy = %v, want enabled", on.created["credentialProxy"])
	}
	// Deny by default stays in force next to the vault: the binding hosts must be reachable, nothing else.
	if pol, _ := on.created["networkPolicy"].(map[string]any); pol["defaultAction"] != "deny" {
		t.Errorf("networkPolicy = %v", on.created["networkPolicy"])
	}
}

func TestSetCredentialsCreatesThenReplacesInOneGuardedPatch(t *testing.T) {
	f := newFakeVault(t)
	broker := sandbox.BrokerOf(brokerBackend(t, f, true))
	ctx := context.Background()

	if err := broker.SetCredentials(ctx, githubSet("one")); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || f.revision != 1 || len(f.cleartexts) != 2 {
		t.Fatalf("creates %d revision %d", f.creates, f.revision)
	}

	// A renewed token replaces the credentials of the same names in a single patch against the revision just read.
	if err := broker.SetCredentials(ctx, githubSet("two")); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || len(f.patches) != 1 || f.patches[0]["expectedRevision"] != float64(1) {
		t.Fatalf("creates %d patches %v", f.creates, f.patches)
	}
	creds, _ := f.patches[0]["credentials"].(map[string]any)
	if rep, _ := creds["replace"].([]any); len(rep) != 2 || creds["add"] != nil || creds["delete"] != nil {
		t.Fatalf("credential mutation = %v", creds)
	}

	// Names that are no longer wanted are removed, so a token cannot outlive its binding.
	if err := broker.SetCredentials(ctx, sandbox.CredentialSet{Credentials: githubSet("three").Credentials[:1], Bindings: githubSet("three").Bindings[:1]}); err != nil {
		t.Fatal(err)
	}
	if f.creds["github-api"] || f.binds["github-api"] || !f.creds["github-git"] {
		t.Fatalf("vault after shrinking: creds %v binds %v", f.creds, f.binds)
	}
}

func TestSetCredentialsRetriesALostRevisionRace(t *testing.T) {
	// Two runs of one thread renew at the same time: the loser re-reads the vault and applies again.
	f := newFakeVault(t)
	broker := sandbox.BrokerOf(brokerBackend(t, f, true))
	ctx := context.Background()
	if err := broker.SetCredentials(ctx, githubSet("one")); err != nil {
		t.Fatal(err)
	}

	f.conflicts = 2
	if err := broker.SetCredentials(ctx, githubSet("two")); err != nil {
		t.Fatalf("a transient conflict must be retried: %v", err)
	}
	if len(f.patches) != 1 {
		t.Fatalf("patches = %d, want the one that finally went through", len(f.patches))
	}

	f.conflicts = 100
	if err := broker.SetCredentials(ctx, githubSet("three")); err == nil {
		t.Fatal("a conflict that never clears must be reported, not swallowed")
	}
}

func TestSetCredentialsErrorsNeverCarryTheSecret(t *testing.T) {
	f := newFakeVault(t)
	broker := sandbox.BrokerOf(brokerBackend(t, f, true))
	f.srv.Close() // the sidecar is unreachable

	err := broker.SetCredentials(context.Background(), githubSet("ghs_topsecret"))

	if err == nil || strings.Contains(err.Error(), "ghs_topsecret") {
		t.Fatalf("err = %v", err)
	}
}
