package harness_test

import (
	"strings"
	"testing"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

// This test binary does not import package mcpfeature, which is how a program without MCP support looks: the harness
// package must not link the MCP SDK, and must say so instead of ignoring servers the configuration names.

func TestMCPServersWithoutTheMCPPackageFailNewAndNameThePackage(t *testing.T) {
	// Arrange
	cfg := featureConfig(t, map[string]string{"MCP_SERVERS": `[{"name":"docs","url":"http://127.0.0.1:1/mcp"}]`})

	// Act
	_, err := harness.New(harness.Options{
		Config: cfg, Models: llmtest.Factory{M: llmtest.New()}, Backend: sandboxtest.NewBackend(),
		Features: []harness.Feature{harness.Sandbox(), harness.MCP()},
	})

	// Assert
	if err == nil || !strings.Contains(err.Error(), "mcpfeature") {
		t.Fatalf("err %v, want an error naming the mcpfeature package", err)
	}
}

func TestWithoutMCPServersTheMCPFeatureIsANoOp(t *testing.T) {
	// A program that configures no MCP servers needs nothing imported.
	cfg := featureConfig(t, nil)

	h, err := harness.New(harness.Options{
		Config: cfg, Models: llmtest.Factory{M: llmtest.New()}, Backend: sandboxtest.NewBackend(),
		Features: []harness.Feature{harness.Sandbox(), harness.MCP()},
	})

	if err != nil || h == nil {
		t.Fatalf("New: %v", err)
	}
}
