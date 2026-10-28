package config_test

import (
	"testing"

	"github.com/GhaziBenDahmane/harness/config"
)

func TestLoadRefusesAShutdownGraceThatOverrunsThePodTerminationGrace(t *testing.T) {
	// RUNTIME_SHUTDOWN_GRACE_SECONDS plus the 12 s interrupt/close budget must fit RUNTIME_TERMINATION_GRACE_SECONDS (default 30).
	// Arrange
	t.Setenv("RUNTIME_SHUTDOWN_GRACE_SECONDS", "25")
	_, tooLong := config.Load()
	t.Setenv("RUNTIME_TERMINATION_GRACE_SECONDS", "60")

	// Act
	cfg, err := config.Load()

	// Assert
	if tooLong == nil || err != nil || cfg.RuntimeTerminationGrace != 60 {
		t.Fatalf("default grace error %v, 60 s grace error %v", tooLong, err)
	}
}
