package config_test

import (
	"testing"

	"github.com/GhaziBenDahmane/harness/config"
)

func TestLoadAppliesTheMemoryDefaults(t *testing.T) {
	// The memory settings default to Work's (warn, soft, simple backend, recall on, weekly consolidation, 30-day retention).
	// Act
	cfg, err := config.Load()

	// Assert
	if err != nil || cfg.MemoryWriteMode != "warn" || cfg.MemoryDeleteMode != "soft" || cfg.MemoryDefaultScope != "personal" ||
		cfg.MemoryEmbeddingBackend != "simple" || !cfg.MemoryRecallInjection || cfg.MemoryMaxEntries != 1000 ||
		cfg.MemoryMaxContentBytes != 200000 || cfg.MemoryConsolidationHours != 168 || cfg.MemoryDeletedRetentionDays != 30 {
		t.Fatalf("err %v cfg %+v", err, cfg)
	}
}

func TestLoadRejectsUnknownMemoryModes(t *testing.T) {
	// Unknown write / delete modes or scopes stop the service, like Work's memory proxy; case and spaces are tolerated.
	for _, c := range []struct{ name, value string }{
		{"WORK_MEMORY_WRITE_MODE", "lenient"}, {"WORK_MEMORY_DELETE_MODE", "archive"}, {"WORK_MEMORY_DEFAULT_SCOPE", "global"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			t.Setenv(c.name, c.value)

			// Act
			_, err := config.Load()

			// Assert
			if err == nil {
				t.Fatalf("%s=%s accepted", c.name, c.value)
			}
		})
	}
	t.Setenv("WORK_MEMORY_WRITE_MODE", " Strict ")
	if cfg, err := config.Load(); err != nil || cfg.MemoryWriteMode != "strict" {
		t.Fatalf("strict: %v %+v", err, cfg)
	}
}
