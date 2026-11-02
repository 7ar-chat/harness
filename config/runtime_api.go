package config

import (
	"fmt"
	"regexp"
	"strings"
)

// ShutdownInterruptBudget is what a runtime needs after RUNTIME_SHUTDOWN_GRACE_SECONDS
// to interrupt its runs (8 s), let background archives end and flush Sentry.
const ShutdownInterruptBudget = 12

// RuntimeAPI holds the harness v3 settings of the runtime & API stream
// (docs/harness-v3.md §7–§9, README "Harness v3: runtime & API").
type RuntimeAPI struct {
	// RuntimeTerminationGrace is the pod's terminationGracePeriodSeconds: the
	// whole shutdown (HTTP drain, runs, archives, Sentry) must fit it.
	RuntimeTerminationGrace int `env:"RUNTIME_TERMINATION_GRACE_SECONDS" envDefault:"30"`
	// MaxStreamsPerUser caps concurrent SSE streams of one user per api process
	// (429 too_many_streams beyond).
	MaxStreamsPerUser int `env:"MAX_STREAMS_PER_USER" envDefault:"8"`
	// ApprovalTTLHours: a pending approval older than this expires.
	ApprovalTTLHours int `env:"APPROVAL_TTL_HOURS" envDefault:"72"`
	// ApprovalRiskyToolPatterns: MCP tools with a name word starting with one of
	// these (case-insensitive regexp alternatives) need approval in interactive mode.
	ApprovalRiskyToolPatterns string `env:"APPROVAL_RISKY_TOOL_PATTERNS" envDefault:"create|send|update|delete|post|publish|write"`
	// RiskyToolPattern is ApprovalRiskyToolPatterns compiled (nil when empty).
	RiskyToolPattern *regexp.Regexp `env:"-"`
}

func (c *Config) normalizeRuntimeAPI() error {
	if c.RuntimeShutdownGrace+ShutdownInterruptBudget > c.RuntimeTerminationGrace {
		return fmt.Errorf("RUNTIME_SHUTDOWN_GRACE_SECONDS (%d) + %d s must fit RUNTIME_TERMINATION_GRACE_SECONDS (%d), the pod's termination grace",
			c.RuntimeShutdownGrace, ShutdownInterruptBudget, c.RuntimeTerminationGrace)
	}
	if c.MaxStreamsPerUser < 1 {
		return fmt.Errorf("MAX_STREAMS_PER_USER must be at least 1, got %d", c.MaxStreamsPerUser)
	}
	if c.ApprovalTTLHours < 1 {
		return fmt.Errorf("APPROVAL_TTL_HOURS must be at least 1, got %d", c.ApprovalTTLHours)
	}
	if p := strings.TrimSpace(c.ApprovalRiskyToolPatterns); p != "" {
		re, err := regexp.Compile("(?i)^(?:" + p + ")") // matched against each word of the tool name
		if err != nil {
			return fmt.Errorf("APPROVAL_RISKY_TOOL_PATTERNS: %w", err)
		}
		c.RiskyToolPattern = re
	}
	return nil
}
