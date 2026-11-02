package config_test

import (
	"testing"

	"github.com/7ar-chat/harness/config"
)

func TestProductionIsOnWhenSetToAnyValue(t *testing.T) {
	// HARNESS_PRODUCTION works as: any non-empty value means production, unset or empty means debug.
	// Arrange
	t.Setenv("HARNESS_PRODUCTION", "yes")
	yes, yesErr := config.Load()
	t.Setenv("HARNESS_PRODUCTION", "")

	// Act
	empty, emptyErr := config.Load()

	// Assert
	if yesErr != nil || yes.IsDebug() || emptyErr != nil || !empty.IsDebug() {
		t.Fatalf("yes: %v debug=%v; empty: %v debug=%v", yesErr, yes != nil && yes.IsDebug(), emptyErr, empty != nil && empty.IsDebug())
	}
}
