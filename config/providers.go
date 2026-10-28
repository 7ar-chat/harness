package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Provider is a named model provider: an entry of the providers section of a
// config.yml. The built-in providers are the bare types "openai" and
// "anthropic" (LLM_BASE_URL defaults); a name defined here is selected with
// provider: <name> or -provider <name>.
type Provider struct {
	Name string
	// Type is "openai" (any OpenAI-compatible endpoint) or "anthropic".
	Type          string
	BaseURL       string
	SessionHeader string
	CacheTTL      string
	// APIKeyEnv is the NAME of the environment variable that holds the key.
	APIKeyEnv    string
	Models       []string
	DefaultModel string
	// Origin is the layer that last set a field of the provider.
	Origin Source
}

// merge lets the non-empty fields of o override p's.
func (p Provider) merge(o Provider) Provider {
	pick := func(a, b string) string {
		if b != "" {
			return b
		}
		return a
	}
	p.Type = pick(p.Type, o.Type)
	p.BaseURL = pick(p.BaseURL, o.BaseURL)
	p.SessionHeader = pick(p.SessionHeader, o.SessionHeader)
	p.CacheTTL = pick(p.CacheTTL, o.CacheTTL)
	p.APIKeyEnv = pick(p.APIKeyEnv, o.APIKeyEnv)
	p.DefaultModel = pick(p.DefaultModel, o.DefaultModel)
	if len(o.Models) > 0 {
		p.Models = o.Models
	}
	p.Origin = o.Origin
	return p
}

// parseProviders reads the providers section of a config.yml.
func parseProviders(file string, v any, src Source) (map[string]Provider, error) {
	errf := func(format string, a ...any) error { return fmt.Errorf("%s: "+format, append([]any{file}, a...)...) }
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errf("providers must map provider names to their definition")
	}
	out := map[string]Provider{}
	for _, name := range sortedKeys(m) {
		if !ValidProviderName(name) {
			return nil, errf("providers.%s: invalid provider name", name)
		}
		sec, ok := m[name].(map[string]any)
		if !ok {
			return nil, errf("providers.%s must be a section (key: value pairs)", name)
		}
		p := Provider{Name: name, Origin: src}
		for _, key := range sortedKeys(sec) {
			path := "providers." + name + "." + key
			str := func() (string, error) {
				s, _, err := convert(sec[key], kStr)
				if err != nil {
					return "", errf("%s: %v", path, err)
				}
				return strings.TrimSpace(s), nil
			}
			var err error
			switch key {
			case "api_key", "password", "secret", "token":
				return nil, errf("%s: secrets do not belong in a configuration file; set api_key_env to the NAME of the environment variable that holds it, or run `harness auth login %s`", path, name)
			case "type":
				if p.Type, err = str(); err == nil && p.Type != "openai" && p.Type != "anthropic" {
					err = errf("%s must be openai or anthropic, got %q", path, p.Type)
				}
			case "base_url":
				if p.BaseURL, err = str(); err == nil && p.BaseURL != "" {
					if verr := validURL(p.BaseURL); verr != nil {
						err = errf("%s: %v", path, verr)
					}
				}
			case "session_header":
				p.SessionHeader, err = str()
			case "cache_ttl":
				p.CacheTTL, err = str()
			case "default_model":
				p.DefaultModel, err = str()
			case "api_key_env":
				if p.APIKeyEnv, err = str(); err == nil && p.APIKeyEnv == "" {
					err = errf("%s must be the name of an environment variable", path)
				}
			case "models":
				s, _, cerr := convert(sec[key], kList)
				if cerr != nil {
					return nil, errf("%s: %v", path, cerr)
				}
				p.Models = trimAll(strings.Split(s, ","))
			default:
				return nil, errf("unknown key %q", path)
			}
			if err != nil {
				return nil, err
			}
		}
		out[name] = p
	}
	return out, nil
}

// selectProvider decides which provider is in use and folds a defined one into
// the settings (merged, and ly for -config-dump): its values sit above the flat
// keys of the files and below the environment variables. It returns the
// selected name and its definition (nil for a built-in type).
func selectProvider(flag string, procEnv map[string]string, merged map[string]string, ly *Layers, defs map[string]Provider) (string, *Provider) {
	name := strings.TrimSpace(flag)
	if name == "" {
		name = strings.TrimSpace(procEnv["LLM_PROVIDER"])
	}
	if name == "" {
		name = strings.TrimSpace(ly.Values["LLM_PROVIDER"])
	}
	if name == "" {
		name = "openai"
	}
	def, ok := defs[name]
	if !ok {
		merged["LLM_PROVIDER"] = name
		return name, nil
	}
	if def.Type == "" {
		def.Type = "openai"
		if name == "anthropic" {
			def.Type = "anthropic"
		}
	}
	merged["LLM_PROVIDER"] = def.Type
	set := func(key, val string) {
		if val == "" {
			return
		}
		if _, fromEnv := procEnv[key]; fromEnv {
			return
		}
		merged[key] = val
		ly.Values[key] = val
		ly.Sources[key] = def.Origin
	}
	set("LLM_BASE_URL", def.BaseURL)
	set("LLM_SESSION_HEADER", def.SessionHeader)
	set("LLM_CACHE_TTL", def.CacheTTL)
	set("LLM_DEFAULT_MODEL", def.DefaultModel)
	set("LLM_MODELS", strings.Join(def.Models, ","))
	return name, &def
}

// resolveAPIKey finds the key of the selected provider, in this order: LLM_API_KEY
// (environment, or a flat api_key_env of a file), the provider's api_key_env, the
// provider's entry of auth.json, the conventional variable of its type
// (OPENAI_API_KEY, ANTHROPIC_API_KEY; not for a named provider with its own
// base_url: that key is not for it). Nothing found is not an error here (see
// RequireLLMKey); an unusable auth.json is.
func (c *Config) resolveAPIKey(procEnv map[string]string, def *Provider, userDir string) error {
	ly := c.Layers
	c.keyHint = nil
	if c.LLMAPIKey != "" {
		if procEnv["LLM_API_KEY"] != "" {
			c.LLMKeySource = "env LLM_API_KEY"
		} else if src, ok := ly.Sources["LLM_API_KEY"]; ok {
			c.LLMKeySource = string(src) + " api_key_env"
		} else {
			c.LLMKeySource = "LLM_API_KEY"
		}
		return nil
	}
	if def != nil && def.APIKeyEnv != "" {
		if v := procEnv[def.APIKeyEnv]; v != "" {
			c.LLMAPIKey, c.LLMKeySource = v, "env "+def.APIKeyEnv
			return nil
		}
		c.keyHint = append(c.keyHint, "export "+def.APIKeyEnv+" (the api_key_env of provider "+c.LLMProviderName+")")
	}
	if userDir != "" {
		keys, err := ReadAuth(userDir)
		if err != nil {
			return err
		}
		if k := keys[c.LLMProviderName]; k != "" {
			c.LLMAPIKey, c.LLMKeySource = k, "auth.json"
			ly.AuthFile = AuthPath(userDir)
			return nil
		}
	}
	if def == nil || def.BaseURL == "" {
		conv := "OPENAI_API_KEY"
		if c.LLMProvider == "anthropic" {
			conv = "ANTHROPIC_API_KEY"
		}
		if v := procEnv[conv]; v != "" {
			c.LLMAPIKey, c.LLMKeySource = v, "env "+conv
			return nil
		}
		c.keyHint = append(c.keyHint, "export "+conv)
	}
	return nil
}

// RequireLLMKey returns a helpful error when the selected provider has no API
// key. Endpoints on the local machine (Ollama, llama.cpp ...) need none.
func (c *Config) RequireLLMKey() error {
	if c.LLMAPIKey != "" {
		return nil
	}
	if u, err := url.Parse(c.LLMBaseURL); err == nil {
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return nil
		}
	}
	hint := append([]string{"run `harness auth login " + c.LLMProviderName + "`", "export LLM_API_KEY"}, c.keyHint...)
	return errors.New("no API key for provider " + c.LLMProviderName + ": " + strings.Join(hint, ", or "))
}
