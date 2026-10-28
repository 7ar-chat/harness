package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestConfigDumpShowsTheSourceOfTheValuesAndMasksSecrets(t *testing.T) {
	// Arrange
	t.Setenv("LLM_API_KEY", "sk-secret-value")
	t.Setenv("LLM_SESSION_HEADER", "x-session")
	var out, errOut bytes.Buffer

	// Act
	code := run(context.Background(), []string{"-config-dump", "-model", "m1"}, strings.NewReader(""), &out, &errOut, false, scripted())

	// Assert
	got := out.String()
	if code != 0 || !strings.Contains(got, "LLM_SESSION_HEADER") || !strings.Contains(got, "(env)") || !strings.Contains(got, "-model") || !strings.Contains(got, "(flag)") {
		t.Fatalf("code %d err %q out:\n%s", code, errOut.String(), got)
	}
	if strings.Contains(got, "sk-secret-value") || !strings.Contains(got, "****") {
		t.Fatalf("secret not masked:\n%s", got)
	}
}
