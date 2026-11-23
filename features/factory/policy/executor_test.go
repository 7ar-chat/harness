package policy_test

import (
	"testing"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/features/factory/policy"
)

// run_executor starts an external coding agent: an exec, which a supervised task asks a human about and the other
// policies allow.
func TestRunExecutorIsAnExecGatedBySupervision(t *testing.T) {
	a := policy.Classify("run_executor", `{"executor":"claude-code","task":"fix it"}`)
	if a.Kind != policy.KindExec || a.Detail == "" {
		t.Fatalf("action %+v", a)
	}
	for pol, want := range map[string]string{factory.PolicySupervised: ask, factory.PolicySandboxed: allow, factory.PolicyAutonomous: allow} {
		if got := verdict(policy.Decide(a, pol, testCtx)); got != want {
			t.Errorf("%s: %s, want %s", pol, got, want)
		}
	}
	noAsk := testCtx
	noAsk.CanAsk = false
	if got := verdict(policy.Decide(a, factory.PolicySupervised, noAsk)); got != deny {
		t.Errorf("supervised without a gate: %s, want deny", got)
	}
}
