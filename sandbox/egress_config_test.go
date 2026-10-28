package sandbox_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/sandbox"
)

// fakeCreateServer emulates the OpenSandbox lifecycle API and execd for one
// creation and records the create request body.
func fakeCreateServer(t *testing.T) (*httptest.Server, func() map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var created map[string]any
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			_ = json.Unmarshal(body, &created)
			mu.Unlock()
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"sbx-1","status":{"state":"Pending"},"entrypoint":[],"createdAt":"2026-10-06T00:00:00Z"}`))
		case strings.HasPrefix(r.URL.Path, "/v1/sandboxes/sbx-1/endpoints/"):
			_, _ = w.Write([]byte(`{"endpoint":"` + strings.TrimPrefix(srv.URL, "http://") + `/execd"}`))
		case r.URL.Path == "/v1/sandboxes/sbx-1":
			_, _ = w.Write([]byte(`{"id":"sbx-1","status":{"state":"Running"},"entrypoint":[],"createdAt":"2026-10-06T00:00:00Z"}`))
		case strings.HasPrefix(r.URL.Path, "/execd/"):
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return created
	}
}

func TestEveryProcessRefusesUnrestrictedEgressInProduction(t *testing.T) {
	// The backend every process (api included) builds from the configuration refuses production without an allowlist (C-02).
	// Arrange
	t.Setenv("DJANGO_PRODUCTION", "1")
	t.Setenv("OPENSANDBOX_URL", "http://opensandbox:8090")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Act
	backend, err := sandbox.BackendFromConfig(cfg)

	// Assert
	if err == nil || backend != nil || !strings.Contains(err.Error(), "SANDBOX_EGRESS_ALLOWLIST") {
		t.Fatalf("backend %v err %v", backend, err)
	}
}

func TestSandboxCreatedFromConfigCarriesTheAllowlist(t *testing.T) {
	// A sandbox created by any process (e.g. the api for an Office preview) is created with the configured deny-by-default allowlist.
	// Arrange
	srv, created := fakeCreateServer(t)
	t.Setenv("DJANGO_PRODUCTION", "1")
	t.Setenv("OPENSANDBOX_URL", srv.URL)
	t.Setenv("OPENSANDBOX_USE_SERVER_PROXY", "true")
	t.Setenv("SANDBOX_EGRESS_ALLOWLIST", "pypi.org,mirror.example.com")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.BackendFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Act
	_, err = backend.Create(context.Background(), sandbox.CreateOptions{Metadata: map[string]string{"work-thread": "t1"}})

	// Assert
	policy, _ := created()["networkPolicy"].(map[string]any)
	rules, _ := policy["egress"].([]any)
	if err != nil || policy["defaultAction"] != "deny" || len(rules) != 2 {
		t.Fatalf("err %v policy %v", err, created()["networkPolicy"])
	}
	if r, _ := rules[1].(map[string]any); r["action"] != "allow" || r["target"] != "mirror.example.com" {
		t.Fatalf("rules %v", rules)
	}
}

func TestDefaultDenyListCoversIPv6CGNATAndThisNetwork(t *testing.T) {
	// Unrestricted egress denies by default the private and link-local IPv4 ranges, CGNAT 100.64/10, 0/8 and the IPv6 ULA/link-local ranges (C-19).
	// Arrange
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Act
	policy := sandbox.EgressPolicy(sandbox.OptionsFromConfig(cfg))
	fallback := sandbox.EgressPolicy(sandbox.OpenSandboxOptions{})

	// Assert
	denied := map[string]bool{}
	for _, r := range policy.Egress {
		if r.Action == "deny" {
			denied[r.Target] = true
		}
	}
	for _, want := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10", "0.0.0.0/8", "fc00::/7", "fe80::/10"} {
		if !denied[want] {
			t.Fatalf("%s not denied: %+v", want, policy.Egress)
		}
	}
	if policy.DefaultAction != "allow" || len(fallback.Egress) != len(policy.Egress) ||
		strings.Join(cfg.SandboxEgressDenyCIDRs, ",") != strings.Join(sandbox.DefaultEgressDenyCIDRs, ",") {
		t.Fatalf("config default %v, package default %v", cfg.SandboxEgressDenyCIDRs, sandbox.DefaultEgressDenyCIDRs)
	}
}

func TestDefaultDenyListCoversIPv4MappedIPv6(t *testing.T) {
	// Unrestricted egress also denies IPv4-mapped IPv6 addresses (::ffff:10.0.0.1 reaches 10.0.0.1), in the config and package defaults (C-19, review round 1).
	// Arrange
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Act
	policy := sandbox.EgressPolicy(sandbox.OptionsFromConfig(cfg))
	fallback := sandbox.EgressPolicy(sandbox.OpenSandboxOptions{})

	// Assert
	var configured, packaged []string
	for _, r := range policy.Egress {
		configured = append(configured, string(r.Action)+" "+r.Target)
	}
	for _, r := range fallback.Egress {
		packaged = append(packaged, string(r.Action)+" "+r.Target)
	}
	if !slices.Contains(configured, "deny ::ffff:0:0/96") || !slices.Contains(packaged, "deny ::ffff:0:0/96") {
		t.Fatalf("::ffff:0:0/96 not denied: config %v, package %v", configured, packaged)
	}
}
