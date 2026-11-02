package opensandbox_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/sandbox/opensandbox"
	"github.com/7ar-chat/harness/core/sandbox"
)

// TestCredentialVaultLive proves against a real OpenSandbox server that a credential handed to the egress sidecar
// authenticates a request on its way out while the sandbox itself never holds it. Opt in with OPENSANDBOX_URL (and
// OPENSANDBOX_API_KEY, OPENSANDBOX_IMAGE, OPENSANDBOX_USE_SERVER_PROXY=true when the server is not on this host);
// the server needs egress >= 1.1.1 in dns+nft mode and the sandbox needs internet access to httpbin.org.
func TestCredentialVaultLive(t *testing.T) {
	url := os.Getenv("OPENSANDBOX_URL")
	if url == "" {
		t.Skip("set OPENSANDBOX_URL to run the live Credential Vault test")
	}
	b, err := opensandbox.NewOpenSandbox(opensandbox.OpenSandboxOptions{
		URL: url, APIKey: os.Getenv("OPENSANDBOX_API_KEY"), Image: os.Getenv("OPENSANDBOX_IMAGE"),
		UseServerProxy: os.Getenv("OPENSANDBOX_USE_SERVER_PROXY") == "true",
		CPU:            "1", Memory: "1Gi", TimeoutSeconds: 600, CredentialProxy: true,
		EgressAllowlist: []string{"httpbin.org"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	inst, err := b.Create(ctx, sandbox.CreateOptions{Metadata: map[string]string{"work-thread": "live-vault-test"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Kill(context.WithoutCancel(ctx), inst.ID()) })
	broker := sandbox.BrokerOf(inst)
	if broker == nil {
		t.Fatal("no broker on a sandbox created with the credential proxy")
	}
	set := func(secret string) sandbox.CredentialSet {
		return sandbox.CredentialSet{
			Credentials: []sandbox.Credential{{Name: "tok", Value: secret}},
			Bindings: []sandbox.CredentialBinding{{Name: "hb", Host: "httpbin.org", Methods: []string{"GET"},
				Paths: []string{"/headers"}, Auth: sandbox.AuthBearer, Credential: "tok"}},
		}
	}
	run := func(cmd string) string {
		var out strings.Builder
		res, err := inst.Exec(ctx, sandbox.ExecRequest{Command: cmd, Timeout: time.Minute,
			OnStdout: func(s string) { out.WriteString(s) }, OnStderr: func(s string) { out.WriteString(s) }})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("%s: exit %d err %v: %s", cmd, res.ExitCode, err, out.String())
		}
		return out.String()
	}

	// httpbin.org/headers reflects the Authorization header it receives, so a command that printed the response
	// would itself put the secret into the sandbox (the sandbox keeps each command's output in a file). The checks
	// below therefore only count matches, and spell the secret so that its literal text is not in any command line.
	spelled := func(secret string) string { return "'" + secret[:8] + "''" + secret[8:] + "'" }
	reflected := func(secret string) string {
		return strings.TrimSpace(run(`curl -sS -m 30 https://httpbin.org/headers | grep -c "Bearer "` + spelled(secret) + ` || true`))
	}

	if err := broker.SetCredentials(ctx, set("live-secret-one")); err != nil {
		t.Fatal(err)
	}
	if got := reflected("live-secret-one"); got != "1" {
		t.Fatalf("the sidecar did not authenticate the request (matches: %s)", got)
	}

	// Renewing replaces the credential in place: the very next request carries the new one.
	if err := broker.SetCredentials(ctx, set("live-secret-two")); err != nil {
		t.Fatal(err)
	}
	if got := reflected("live-secret-two"); got != "1" {
		t.Fatalf("the renewed credential was not applied (matches: %s)", got)
	}
	if got := reflected("live-secret-one"); got != "0" {
		t.Fatalf("the replaced credential is still being sent (matches: %s)", got)
	}

	// The secret exists nowhere in the sandbox: not in the environment, not in any file.
	if got := strings.TrimSpace(run(`env | grep -c ` + spelled("live-secret") + ` || true`)); got != "0" {
		t.Fatalf("the secret is in the sandbox environment (%s matches)", got)
	}
	if got := strings.TrimSpace(run(`grep -rIl ` + spelled("live-secret") + ` /workspace /tmp /home /root /etc 2>/dev/null | head -5`)); got != "" {
		t.Fatalf("the secret is in sandbox files:\n%s", got)
	}

	// A path outside the binding is not authenticated.
	if got := strings.TrimSpace(run(`curl -sS -m 30 https://httpbin.org/get | grep -c ` + spelled("live-secret") + ` || true`)); got != "0" {
		t.Fatalf("a request outside the binding was authenticated (matches: %s)", got)
	}
}
