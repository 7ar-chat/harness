package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/product"
)

// TestMain keeps the tests from downloading the model price table.
func TestMain(m *testing.M) {
	os.Setenv("HARNESS_MODEL_PRICES", "off")
	os.Exit(m.Run())
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestNonLoopbackBindWithoutATokenIsRefused(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "192.168.1.5:8080", "example.com:80"} {
		var out, errb bytes.Buffer

		code := run(context.Background(), []string{"-addr", addr}, env(nil), &out, &errb)

		if code != 1 || !strings.Contains(errb.String(), "HARNESS_WEB_TOKEN") {
			t.Errorf("%s: code %d stderr %q", addr, code, errb.String())
		}
	}
}

func TestLoopbackDetection(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080": true, "[::1]:8080": true, "localhost:9": true,
		"0.0.0.0:80": false, ":80": false, "[::]:80": false, "10.0.0.1:80": false, "garbage": false,
	} {
		if got := isLoopback(addr); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestServesTheUIUntilTheContextEnds(t *testing.T) {
	// Arrange: a loopback server on a free port with the local sandbox and the in-process store.
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("LLM_DEFAULT_MODEL", "gpt-test")
	t.Setenv("LOCAL_WORKSPACE", t.TempDir())
	t.Setenv("LOCAL_STATE_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	done := make(chan int, 1)

	// Act
	go func() { done <- run(ctx, []string{"-local", "-addr", "127.0.0.1:0"}, env(nil), pw, io.Discard) }()
	line := make([]byte, 200)
	n, _ := pr.Read(line)
	url := strings.Fields(strings.TrimPrefix(string(line[:n]), "harness-web: "))[0]
	resp, err := http.Get(url + "healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	models, err := http.Get(url + "api/models")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(models.Body)
	models.Body.Close()
	cancel()

	// Assert
	if resp.StatusCode != 200 || models.StatusCode != 200 || !strings.Contains(string(body), "gpt-test") {
		t.Fatalf("healthz %d, models %d %s", resp.StatusCode, models.StatusCode, body)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not stop")
	}
}

func TestTheHostGuardOnlyProtectsTheServerWithoutACredential(t *testing.T) {
	oidc := &product.OIDCConfig{Issuer: "https://id.example.com"}
	for _, c := range []struct {
		name  string
		token string
		oidc  *product.OIDCConfig
		guard bool
	}{
		{"nothing configured", "", nil, true},
		{"token", "secret", nil, false},
		{"login (OIDC)", "", oidc, false},
		{"both", "secret", oidc, false},
	} {
		if got := allowedHosts(c.token, c.oidc); (len(got) > 0) != c.guard {
			t.Errorf("%s: allowedHosts = %v, want guard=%v", c.name, got, c.guard)
		}
	}
}

func TestTheLocalSandboxIsRefusedWhereTheAgentIsShared(t *testing.T) {
	cases := []struct {
		name    string
		backend string
		team    bool
		addr    string
		allow   string
		refused bool
	}{
		{"single user on loopback", config.BackendLocal, false, "127.0.0.1:8080", "", false},
		{"team mode", config.BackendLocal, true, "127.0.0.1:8080", "", true},
		{"reachable address", config.BackendLocal, false, "0.0.0.0:8080", "", true},
		{"accepted risk", config.BackendLocal, true, "0.0.0.0:8080", "1", false},
		{"real sandbox in team mode", config.BackendOpenSandbox, true, "0.0.0.0:8080", "", false},
	}
	for _, c := range cases {
		cfg := &config.Config{SandboxBackend: c.backend}
		err := checkLocalSandbox(cfg, c.team, c.addr, func(k string) string {
			if k == "HARNESS_ALLOW_LOCAL_SANDBOX" {
				return c.allow
			}
			return ""
		})
		if (err != nil) != c.refused {
			t.Errorf("%s: err = %v, want refused=%v", c.name, err, c.refused)
		}
	}
}
