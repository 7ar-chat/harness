package config

import "strings"

// SkillsMarketplaceConfigured reports whether the marketplace sync can run
// (AstraForge's SKILLS_MARKETPLACE_SYNC_CONFIGURED).
func (c *Config) SkillsMarketplaceConfigured() bool {
	return strings.TrimSpace(c.SkillsMarketplaceSourcePath) != "" || strings.TrimSpace(c.SkillsMarketplaceToken) != "" ||
		strings.TrimSpace(c.SkillsMarketplaceUsername) != "" && strings.TrimSpace(c.SkillsMarketplacePassword) != ""
}
