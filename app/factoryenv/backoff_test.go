package factoryenv

import (
	"testing"
	"time"
)

func TestFactoryRetryBackoffCanBeShortenedForTests(t *testing.T) {
	// Arrange
	fxEnv(t)

	// Act
	w, _, err := FromEnv(env(map[string]string{"FACTORY": "1", "FACTORY_RETRY_BACKOFF_SECONDS": "0.5"}), fxConfig(t), nil, nil, nil)
	_, _, bad := FromEnv(env(map[string]string{"FACTORY": "1", "FACTORY_RETRY_BACKOFF_SECONDS": "soon"}), fxConfig(t), nil, nil, nil)
	def, _, _ := FromEnv(env(map[string]string{"FACTORY": "1"}), fxConfig(t), nil, nil, nil)

	// Assert
	if err != nil || w.Options.Backoff == nil || w.Options.Backoff(1) != 500*time.Millisecond || w.Options.Backoff(5) != 500*time.Millisecond {
		t.Fatalf("backoff: err %v, set %v", err, w.Options.Backoff != nil)
	}
	if bad == nil {
		t.Error("a non-number must be refused")
	}
	if def.Options.Backoff != nil {
		t.Error("without the variable the default 30s doubling backoff applies")
	}
}
