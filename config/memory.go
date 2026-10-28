package config

import (
	"fmt"
	"strings"
)

// validateMemory rejects unknown HARNESS_MEMORY_* modes (Work's proxy refuses them too).
func (c *Config) validateMemory() error {
	c.MemoryWriteMode = strings.ToLower(strings.TrimSpace(c.MemoryWriteMode))
	c.MemoryDeleteMode = strings.ToLower(strings.TrimSpace(c.MemoryDeleteMode))
	c.MemoryDefaultScope = strings.ToLower(strings.TrimSpace(c.MemoryDefaultScope))
	switch {
	case c.MemoryWriteMode != "warn" && c.MemoryWriteMode != "strict":
		return fmt.Errorf("HARNESS_MEMORY_WRITE_MODE must be warn or strict, got %q", c.MemoryWriteMode)
	case c.MemoryDeleteMode != "soft" && c.MemoryDeleteMode != "hard":
		return fmt.Errorf("HARNESS_MEMORY_DELETE_MODE must be soft or hard, got %q", c.MemoryDeleteMode)
	case c.MemoryDefaultScope != "personal" && c.MemoryDefaultScope != "repository" && c.MemoryDefaultScope != "workspace":
		return fmt.Errorf("HARNESS_MEMORY_DEFAULT_SCOPE must be personal, repository or workspace, got %q", c.MemoryDefaultScope)
	}
	return nil
}
