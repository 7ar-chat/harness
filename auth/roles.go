package auth

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
)

// Roles returns the roles and groups carried by the raw X-Userinfo value:
// the "roles" and "groups" claims (string or list) and "realm_access.roles"
// as is, and every "resource_access.<client>.roles" (Keycloak) qualified as
// "<client>:<role>" (a client role never passes for a realm role; B-03),
// deduplicated.
func (u *User) Roles() []string {
	if u == nil {
		return nil
	}
	return Roles(u.Raw)
}

// Roles decodes the roles of a raw X-Userinfo value (nil when unreadable).
func Roles(header string) []string {
	header = strings.TrimSpace(header)
	var decoded []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawURLEncoding, base64.RawStdEncoding} {
		if decoded, err = enc.DecodeString(header); err == nil {
			break
		}
	}
	if err != nil {
		return nil
	}
	var raw map[string]any
	if json.Unmarshal(decoded, &raw) != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(v any) {
		switch x := v.(type) {
		case string:
			for _, s := range strings.FieldsFunc(x, func(r rune) bool { return r == ',' || r == ' ' }) {
				if !seen[s] {
					seen[s] = true
					out = append(out, s)
				}
			}
		case []any:
			for _, e := range x {
				if s, ok := e.(string); ok && strings.TrimSpace(s) != "" && !seen[strings.TrimSpace(s)] {
					seen[strings.TrimSpace(s)] = true
					out = append(out, strings.TrimSpace(s))
				}
			}
		}
	}
	add(raw["roles"])
	add(raw["groups"])
	if ra, ok := raw["realm_access"].(map[string]any); ok {
		add(ra["roles"])
	}
	if rs, ok := raw["resource_access"].(map[string]any); ok {
		clients := make([]string, 0, len(rs))
		for name := range rs {
			clients = append(clients, name)
		}
		sort.Strings(clients) // deterministic order
		for _, name := range clients {
			m, ok := rs[name].(map[string]any)
			if !ok || strings.TrimSpace(name) == "" {
				continue
			}
			var qualified []any
			switch x := m["roles"].(type) {
			case []any:
				for _, e := range x {
					if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
						qualified = append(qualified, strings.TrimSpace(name)+":"+strings.TrimSpace(s))
					}
				}
			case string:
				for _, s := range strings.FieldsFunc(x, func(r rune) bool { return r == ',' || r == ' ' }) {
					qualified = append(qualified, strings.TrimSpace(name)+":"+s)
				}
			}
			add(qualified)
		}
	}
	return out
}
