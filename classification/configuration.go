package classification

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Raw is the unvalidated configuration (FILE_CLASSIFICATION_* environment, see
// README). ProviderSettings values are JSON-like (string, bool or nil), as in
// FILE_CLASSIFICATION_PROVIDER_SETTINGS_JSON.
type Raw struct {
	Mode             string
	ProfileName      string
	ProfileVersion   string
	ProfileLevel     string
	Providers        []string
	ProviderSettings map[string]map[string]any
	Actor            string
	MaxInputBytes    int64
	MaxOutputBytes   int64
	MaxConcurrent    int
}

// DefaultProviderSettings are the defaults of ai/api/settings.py; each value
// may be overridden by its FILE_CLASSIFICATION_* variable.
func DefaultProviderSettings(env func(key, def string) string) map[string]map[string]any {
	return map[string]map[string]any{
		"mip": {
			"enabled":    "true",
			"label_id":   env("FILE_CLASSIFICATION_MIP_LABEL_ID", "18dda7a4-b650-4246-b8ce-bc9857d399d0"),
			"label_name": env("FILE_CLASSIFICATION_MIP_LABEL_NAME", "C3-External-Allowed"),
			"site_id":    env("FILE_CLASSIFICATION_MIP_SITE_ID", "a5c34232-eadc-4609-bff3-dd6fcdae3fe2"),
		},
		"gvdata": {
			"enabled":                   "true",
			"classification":            env("FILE_CLASSIFICATION_GVDATA_CLASSIFICATION", "Confidentiel"),
			"classification_tag_set_id": env("FILE_CLASSIFICATION_GVDATA_CLASSIFICATION_TAG_SET_ID", "e16409a7-1700-4153-9090-3955bc2f0ae8"),
			"compliance_tag_set_id":     env("FILE_CLASSIFICATION_GVDATA_COMPLIANCE_TAG_SET_ID", "f14fc1f1-8950-40d5-8a29-45909da947d6"),
			"distribution":              env("FILE_CLASSIFICATION_GVDATA_DISTRIBUTION", "CA-TITRES"),
			"distribution_tag_set_id":   env("FILE_CLASSIFICATION_GVDATA_DISTRIBUTION_TAG_SET_ID", "004dea33-8751-4399-a76e-95f371cb4119"),
		},
	}
}

// ParseProviderSettingsJSON parses FILE_CLASSIFICATION_PROVIDER_SETTINGS_JSON.
func ParseProviderSettingsJSON(s string) (map[string]map[string]any, error) {
	var out map[string]map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, errf(ErrConfiguration, "FILE_CLASSIFICATION_PROVIDER_SETTINGS_JSON must be valid JSON")
	}
	return out, nil
}

var supportedProviders = map[string]bool{"mip": true, "gvdata": true}

// Load validates raw settings like load_classification_settings (configuration.py).
func Load(raw Raw) (Settings, error) {
	mode := raw.Mode
	if mode != "enforce" && mode != "block" {
		return Settings{}, errf(ErrConfiguration, "classification mode must be exactly 'enforce' or 'block'")
	}
	text := func(v, key string) (string, error) {
		if strings.TrimSpace(v) == "" {
			return "", errf(ErrConfiguration, "missing or invalid profile %s", key)
		}
		return strings.TrimSpace(v), nil
	}
	name, err := text(raw.ProfileName, "name")
	if err != nil {
		return Settings{}, err
	}
	version, err := text(raw.ProfileVersion, "version")
	if err != nil {
		return Settings{}, err
	}
	level, err := text(raw.ProfileLevel, "level")
	if err != nil {
		return Settings{}, err
	}
	providers := make([]string, 0, len(raw.Providers))
	seen := map[string]bool{}
	for _, p := range raw.Providers {
		p = strings.TrimSpace(p)
		if p == "" {
			return Settings{}, errf(ErrConfiguration, "provider names must not be empty")
		}
		if seen[p] {
			return Settings{}, errf(ErrConfiguration, "duplicate provider names")
		}
		seen[p] = true
		providers = append(providers, p)
	}
	if len(providers) == 0 {
		return Settings{}, errf(ErrConfiguration, "profile must configure at least one provider")
	}
	var unknown []string
	for _, p := range providers {
		if !supportedProviders[p] {
			unknown = append(unknown, p)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return Settings{}, errf(ErrConfiguration, "unknown provider: %s", strings.Join(unknown, ", "))
	}
	// Like the Django settings mapping: only the settings of configured providers are kept.
	parsed := map[string]map[string]string{}
	for _, p := range providers {
		values, ok := raw.ProviderSettings[p]
		if !ok || values == nil {
			return Settings{}, errf(ErrConfiguration, "missing or invalid settings for provider %s", p)
		}
		enabled, ok := values["enabled"]
		if !ok {
			return Settings{}, errf(ErrConfiguration, "missing enabled setting for provider %s", p)
		}
		var enabledText string
		switch v := enabled.(type) {
		case bool:
			enabledText = fmt.Sprint(v)
		case string:
			t := strings.ToLower(strings.TrimSpace(v))
			if t != "true" && t != "false" {
				return Settings{}, errf(ErrConfiguration, "invalid enabled setting for provider %s", p)
			}
			enabledText = t
		default:
			return Settings{}, errf(ErrConfiguration, "invalid enabled setting for provider %s", p)
		}
		out := map[string]string{}
		for k, v := range values {
			if k == "enabled" || v == nil {
				continue
			}
			s, ok := v.(string)
			if !ok {
				return Settings{}, errf(ErrConfiguration, "provider settings values must be text for provider %s", p)
			}
			out[k] = s
		}
		out["enabled"] = enabledText
		parsed[p] = out
	}
	maxIn, maxOut := raw.MaxInputBytes, raw.MaxOutputBytes
	if maxIn == 0 {
		maxIn = 100 << 20
	}
	if maxOut == 0 {
		maxOut = 120 << 20
	}
	if maxIn < 0 || maxOut < 0 {
		return Settings{}, errf(ErrConfiguration, "classification size limits must be positive")
	}
	actor := strings.TrimSpace(raw.Actor)
	if actor == "" {
		actor = "studio"
	}
	return Settings{
		Mode:           mode,
		Profile:        Profile{Name: name, Version: version, Level: level, Providers: providers, ProviderSettings: parsed},
		Actor:          actor,
		MaxInputBytes:  maxIn,
		MaxOutputBytes: maxOut,
		MaxConcurrent:  raw.MaxConcurrent,
	}, nil
}

// RawFromEnv reads the FILE_CLASSIFICATION_* variables with the defaults of
// ai/api/settings.py (actor default "work").
func RawFromEnv(getenv func(string) string) (Raw, error) {
	env := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	raw := Raw{
		Mode:           strings.ToLower(strings.TrimSpace(env("FILE_CLASSIFICATION_MODE", "enforce"))),
		ProfileName:    env("FILE_CLASSIFICATION_PROFILE_NAME", "c3"),
		ProfileVersion: env("FILE_CLASSIFICATION_PROFILE_VERSION", "2026-08"),
		ProfileLevel:   env("FILE_CLASSIFICATION_PROFILE_LEVEL", "C3"),
		Actor:          env("FILE_CLASSIFICATION_ACTOR", "work"),
	}
	for _, p := range strings.Split(env("FILE_CLASSIFICATION_PROVIDERS", "mip,gvdata"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			raw.Providers = append(raw.Providers, p)
		}
	}
	raw.ProviderSettings = DefaultProviderSettings(env)
	if js := getenv("FILE_CLASSIFICATION_PROVIDER_SETTINGS_JSON"); strings.TrimSpace(js) != "" {
		parsed, err := ParseProviderSettingsJSON(js)
		if err != nil {
			return Raw{}, err
		}
		raw.ProviderSettings = parsed
	}
	for key, dst := range map[string]*int64{"FILE_CLASSIFICATION_MAX_INPUT_BYTES": &raw.MaxInputBytes, "FILE_CLASSIFICATION_MAX_OUTPUT_BYTES": &raw.MaxOutputBytes} {
		if v := getenv(key); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				return Raw{}, errf(ErrConfiguration, "%s must be a positive integer", key)
			}
			*dst = n
		}
	}
	if v := getenv("FILE_CLASSIFICATION_MAX_CONCURRENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Raw{}, errf(ErrConfiguration, "FILE_CLASSIFICATION_MAX_CONCURRENT must be a positive integer")
		}
		raw.MaxConcurrent = n
	}
	return raw, nil
}
