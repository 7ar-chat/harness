package memory_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/features/memory"
)

func TestRedactTextReplacesWorkSecretPatterns(t *testing.T) {
	// The redactor of Work's memory proxy: inline key=value / key: value, OpenAI, GitHub and AWS keys, private key blocks.
	cases := []struct{ name, in, want string }{
		{"inline equals", "use api_key=abc123 here", "use api_key= [REDACTED] here"},
		{"inline colon", "Password: hunter2, then", "Password: [REDACTED], then"},
		{"openai", "key sk-" + strings.Repeat("a", 24) + " end", "key [REDACTED] end"},
		{"github", "ghp_" + strings.Repeat("b", 30), "[REDACTED]"},
		{"aws", "AKIAABCDEFGHIJKLMNOP is the id", "[REDACTED] is the id"},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIB\n-----END RSA PRIVATE KEY-----", "[REDACTED]"},
		{"clean", "Prefers French answers", "Prefers French answers"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			got, hits := memory.RedactText(c.in)

			// Assert
			if got != c.want || (hits > 0) != (c.in != c.want) {
				t.Fatalf("got %q (%d hits), want %q", got, hits, c.want)
			}
		})
	}
}

func TestRedactValueRedactsSensitiveMetadataKeys(t *testing.T) {
	// Sensitive keys (password, token, camelCase apiKey, *_secret suffixes) are replaced whole; strings elsewhere are redacted; nil values are kept.
	// Arrange
	in := map[string]any{"apiKey": "x", "db_password": "y", "Client-Secret": "z", "token": nil, "note": "token=abc",
		"nested": map[string]any{"github_token": "t", "list": []any{"sk-" + strings.Repeat("c", 22)}}, "trigger": "explicit_remember"}

	// Act
	out, hits := memory.RedactValue(in)

	// Assert
	want := map[string]any{"apiKey": "[REDACTED]", "db_password": "[REDACTED]", "Client-Secret": "[REDACTED]", "token": nil, "note": "token= [REDACTED]",
		"nested": map[string]any{"github_token": "[REDACTED]", "list": []any{"[REDACTED]"}}, "trigger": "explicit_remember"}
	if !reflect.DeepEqual(out, want) || hits != 6 {
		t.Fatalf("got %v (%d hits)", out, hits)
	}
}

func TestEvaluateResolvesTriggersLikeWork(t *testing.T) {
	// An explicit metadata.trigger wins; otherwise reason, user_message then content are matched in Work's pattern order; a delete with any reason is a revocation.
	cases := []struct {
		name, action, content string
		meta                  map[string]any
		want                  string
	}{
		{"explicit", "create", "anything", map[string]any{"trigger": "Accepted_Decision"}, "accepted_decision"},
		{"unknown explicit falls back", "create", "We decided to use Go", map[string]any{"trigger": "whim"}, "accepted_decision"},
		{"remember this", "create", "remember this: standup at 9", nil, "explicit_remember"},
		{"preference wins over decision", "create", "We always decided in the morning", nil, "durable_preference"},
		{"user message", "create", "Go 1.26", map[string]any{"user_message": "I discovered the build needs Go 1.26"}, "discovered_fact"},
		{"investigation", "create", "Root cause: the proxy strips headers", nil, "investigation"},
		{"none", "create", "Lunch was nice", nil, ""},
		{"delete reason", "delete", "", map[string]any{"reason": "batch_delete"}, "explicit_revocation"},
		{"delete no reason", "delete", "", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			d := memory.Evaluate(memory.WriteWarn, c.action, c.content, c.meta)

			// Assert
			if d.Trigger != c.want || !d.Allowed {
				t.Fatalf("trigger %q allowed %v, want %q", d.Trigger, d.Allowed, c.want)
			}
		})
	}
}

func TestEvaluateWriteModes(t *testing.T) {
	// warn records violations but allows the write; strict blocks it; a redaction adds the secret_redacted warning.
	// Act
	warn := memory.Evaluate(memory.WriteWarn, "create", "Lunch was nice", nil)
	strict := memory.Evaluate(memory.WriteStrict, "create", "Lunch was nice", nil)
	strictDelete := memory.Evaluate(memory.WriteStrict, "delete", "", map[string]any{"trigger": "discovered_fact"})
	secret := memory.Evaluate(memory.WriteStrict, "create", "remember this: password=abc", nil)

	// Assert
	if !warn.Allowed || !reflect.DeepEqual(warn.Violations, []string{"write_trigger_missing"}) {
		t.Fatalf("warn %+v", warn)
	}
	if strict.Allowed || strictDelete.Allowed || !reflect.DeepEqual(strictDelete.Violations, []string{"delete_trigger_missing"}) {
		t.Fatalf("strict %+v delete %+v", strict, strictDelete)
	}
	if !secret.Allowed || !reflect.DeepEqual(secret.Warnings, []string{"secret_redacted"}) || secret.RedactedContent != "remember this: password= [REDACTED]" {
		t.Fatalf("secret %+v", secret)
	}
	p := secret.Payload()
	if p["write_mode"] != "strict" || p["trigger"] != "explicit_remember" || !reflect.DeepEqual(p["precedence"], memory.Precedence) {
		t.Fatalf("payload %v", p)
	}
}
