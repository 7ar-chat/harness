package memory

import (
	"time"

	"github.com/GhaziBenDahmane/harness/config"
)

// OptionsFromConfig maps the WORK_MEMORY_* settings.
func OptionsFromConfig(cfg *config.Config) Options {
	return Options{
		WriteMode: cfg.MemoryWriteMode, DeleteMode: cfg.MemoryDeleteMode, DefaultScope: cfg.MemoryDefaultScope,
		EmbeddingBackend: cfg.MemoryEmbeddingBackend, RecallInjection: cfg.MemoryRecallInjection,
		MaxEntries: entriesLimit(cfg.MemoryMaxEntries), MaxContentBytes: cfg.MemoryMaxContentBytes,
	}
}

// entriesLimit maps "≤ 0: no limit" to the service's negative value (0 is the default there).
func entriesLimit(n int) int {
	if n <= 0 {
		return -1
	}
	return n
}

// ConsolidationEvery is the period of the consolidation job (0: disabled).
func ConsolidationEvery(cfg *config.Config) time.Duration {
	if cfg.MemoryConsolidationHours <= 0 {
		return 0
	}
	return time.Duration(cfg.MemoryConsolidationHours) * time.Hour
}

// DeletedRetention is how long soft-deleted entries are kept (0: forever).
func DeletedRetention(cfg *config.Config) time.Duration {
	if cfg.MemoryDeletedRetentionDays <= 0 {
		return 0
	}
	return time.Duration(cfg.MemoryDeletedRetentionDays) * 24 * time.Hour
}
