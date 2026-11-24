package executors

import (
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/executors"
)

// The failure messages below were printed by the real CLIs (see features/executors/clitest): no credentials, and a
// key the provider rejected. Diagnose must recognise each as an authentication problem.
func TestDiagnoseRealAuthFailures(t *testing.T) {
	cases := map[string]string{
		"claude-code":  "Not logged in · Please run /login",
		"claude-code2": "Invalid API key · Fix external API key",
		"codex":        "unexpected status 401 Unauthorized: Missing bearer or basic authentication in header, url: https://api.openai.com/v1/responses, cf-ray: a5f547151a452e53-CDG, request id: req_50ed3634ea1d48e9a270326db708352d",
		"codex2":       "unexpected status 401 Unauthorized: Incorrect API key provided: test. You can find your API key at https://platform.openai.com/account/api-keys., url: http://127.0.0.1:64994/v1/responses",
		"omp":          "401 Incorrect API key provided: test-key. You can find your API key at https://platform.openai.com/account/api-keys. (type=invalid_request_error param=invalid_api_key)",
	}
	for name, out := range cases {
		ex, ok := executors.BuiltinNamed(strings.TrimSuffix(name, "2"))
		if !ok {
			t.Fatal(name)
		}
		if got := Diagnose(ex, out); !strings.Contains(got, "could not authenticate") {
			t.Errorf("%s: %q not diagnosed (%q)", name, out, got)
		}
	}
	// A failing command of the task is not an auth problem.
	ex, _ := executors.BuiltinNamed(executors.Codex)
	if got := Diagnose(ex, "[run failed, exit 1] FAIL: TestX"); got != "" {
		t.Fatalf("%q", got)
	}
}
