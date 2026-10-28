package config_test

import (
	"testing"

	"github.com/GhaziBenDahmane/harness/config"
)

func TestDjangoProductionIsOnWhenSetToAnyValue(t *testing.T) {
	// DJANGO_PRODUCTION follows the house convention: any non-empty value means production, unset or empty means debug.
	// Arrange
	t.Setenv("DJANGO_PRODUCTION", "yes")
	yes, yesErr := config.Load()
	t.Setenv("DJANGO_PRODUCTION", "")

	// Act
	empty, emptyErr := config.Load()

	// Assert
	if yesErr != nil || yes.IsDebug() || emptyErr != nil || !empty.IsDebug() {
		t.Fatalf("yes: %v debug=%v; empty: %v debug=%v", yesErr, yes != nil && yes.IsDebug(), emptyErr, empty != nil && empty.IsDebug())
	}
}
