package config

import "strings"

// IsSkillAdmin reports whether a user may manage global skills: its login is
// in WORK_SKILL_ADMIN_LOGINS or one of its roles in WORK_SKILL_ADMIN_ROLES
// (case-insensitive). roles are auth.Roles: realm roles/groups as is, client
// roles as "client:role". An entry "role" matches a realm role or the role of
// Studio's own client (WORK_SKILL_ADMIN_CLIENT_ID, default OIDC_RP_CLIENT_ID);
// an entry "client:role" matches that client's role only (B-03).
func (c *Config) IsSkillAdmin(login string, roles []string) bool {
	login = strings.TrimSpace(login)
	for _, l := range c.SkillAdminLogins {
		if l = strings.TrimSpace(l); l != "" && strings.EqualFold(l, login) {
			return true
		}
	}
	for _, want := range c.SkillAdminRoles {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		own := ""
		if client := c.skillAdminClient(); client != "" && !strings.Contains(want, ":") {
			own = client + ":" + want
		}
		for _, r := range roles {
			r = strings.TrimSpace(r)
			if strings.EqualFold(r, want) || (own != "" && strings.EqualFold(r, own)) {
				return true
			}
		}
	}
	return false
}

// skillAdminClient is the Keycloak client whose roles count unqualified.
func (c *Config) skillAdminClient() string {
	if v := strings.TrimSpace(c.SkillAdminClientID); v != "" {
		return v
	}
	return strings.TrimSpace(c.OIDCClientID)
}

// SkillsMarketplaceConfigured reports whether the marketplace sync can run
// (AstraForge's SKILLS_MARKETPLACE_SYNC_CONFIGURED).
func (c *Config) SkillsMarketplaceConfigured() bool {
	return strings.TrimSpace(c.SkillsMarketplaceSourcePath) != "" || strings.TrimSpace(c.SkillsMarketplaceToken) != "" ||
		strings.TrimSpace(c.SkillsMarketplaceUsername) != "" && strings.TrimSpace(c.SkillsMarketplacePassword) != ""
}
