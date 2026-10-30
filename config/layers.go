package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/caarlos0/env/v11"
	"gopkg.in/yaml.v3"
)

// Source says where a setting's value comes from.
type Source string

// The layers, from the highest priority to the lowest.
const (
	SourceFlag    Source = "flag"
	SourceEnv     Source = "env"
	SourceProject Source = "project file"
	SourceUser    Source = "user file"
)

// LoadOptions tune LoadWith. The zero value reads the process environment and
// the files of the working directory and of the home directory (what Load does).
type LoadOptions struct {
	// Dir is where the project directory is searched from (walking up to the
	// git root). Default: LOCAL_WORKSPACE of the environment, else the working directory.
	Dir string
	// UserDir is the user configuration directory (default ~/.har).
	UserDir string
	// Environ is the process environment ("K=V" entries; default os.Environ()).
	Environ []string
	// Provider selects a named provider (the -provider flag); it wins over
	// LLM_PROVIDER and the files.
	Provider string
	// NoFiles ignores the configuration files (environment only).
	NoFiles bool
}

// Layers records how a Config was assembled: the files read and, for every
// setting taken from the environment or a file, its source.
type Layers struct {
	// ProjectRoot is the project directory (the parent of the .har directory,
	// else the git root, else the starting directory); ProjectDir is its .har
	// directory ("" when there is none).
	ProjectRoot, ProjectDir string
	UserDir                 string
	// AuthFile is the credentials file when the API key came from it.
	AuthFile string
	// ProjectFile and UserFile are the config.yml files that were read ("" if absent).
	ProjectFile, UserFile string
	// Values and Sources are keyed by the environment variable name.
	Values  map[string]string
	Sources map[string]Source
	// MCPSources lists, in priority order (lowest first), the files MCP servers were read from.
	MCPSources []string
	// MCPSkipped are the MCP servers that were not started and why.
	MCPSkipped []string
	// MCPPending are the stdio servers of the (trusted) project that wait for the user's approval of their command.
	MCPPending []MCPServer
	// Trusted says the project is trusted (see TrustFile); TrustedBy says how ("trust.json" or the environment).
	Trusted   bool
	TrustedBy string
	// Ignored lists the settings of the project that were not honoured, and why (an untrusted project
	// cannot loosen the permissions).
	Ignored []string

	// paths maps an environment variable name to the key of the project file that set it (agents.explore.model).
	paths map[string]string
}

// Load reads the configuration: built-in defaults, then ~/.har/config.yml, then
// the nearest <repo>/.har/config.yml, then the process environment (the command
// line flags of a host are applied by the host on top).
func Load() (*Config, error) { return LoadWith(LoadOptions{}) }

// LoadWith is Load with options.
func LoadWith(o LoadOptions) (*Config, error) {
	environ := o.Environ
	if environ == nil {
		environ = os.Environ()
	}
	procEnv := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			procEnv[k] = v
		}
	}
	known := knownEnvNames()
	ly := &Layers{Values: map[string]string{}, Sources: map[string]Source{}, paths: map[string]string{}}

	var mcpFiles []mcpDoc
	var allowStdioUser bool
	providers := map[string]Provider{}
	userDir := ""
	if !o.NoFiles {
		lookup := func(k string) string { return procEnv[k] }
		userDir = o.UserDir
		if userDir == "" {
			userDir = userDirFrom(lookup)
		}
		if noUserDir(lookup) {
			userDir = "" // HARNESS_NO_USER_DIR: nothing is read from the user directory
		}
		ly.UserDir = userDir
		start := o.Dir
		if start == "" {
			start = procEnv["LOCAL_WORKSPACE"]
		}
		if start == "" {
			start, _ = os.Getwd()
		}
		ly.ProjectDir, ly.ProjectRoot, _ = FindProjectDir(start)
		switch {
		case trustedFromEnv(lookup):
			ly.Trusted, ly.TrustedBy = true, "env "+TrustEnv
		case userDir != "" && ly.ProjectRoot != "":
			ok, err := IsTrusted(userDir, ly.ProjectRoot)
			if err != nil {
				return nil, err
			}
			if ok {
				ly.Trusted, ly.TrustedBy = true, TrustPath(userDir)
			}
		}
		perms := map[string][]string{}
		for _, layer := range []struct {
			dir  string
			src  Source
			user bool
		}{{userDir, SourceUser, true}, {ly.ProjectDir, SourceProject, false}} {
			if layer.dir == "" {
				continue
			}
			file := filepath.Join(layer.dir, ConfigFile)
			data, err := os.ReadFile(file)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", file, err)
			}
			doc, err := parseConfigFile(file, data, layer.user, procEnv, known)
			if err != nil {
				return nil, err
			}
			if layer.user {
				ly.UserFile = file
				allowStdioUser = doc.allowStdio
			} else {
				ly.ProjectFile = file
			}
			if !layer.user && !ly.Trusted {
				ly.tightenOnly(doc)
			}
			for k, v := range doc.values {
				ly.Values[k] = v
				ly.Sources[k] = layer.src
				if p := doc.paths[k]; p != "" && !layer.user {
					ly.paths[k] = p
				}
			}
			for name, p := range doc.providers {
				if prev, ok := providers[name]; ok {
					p = prev.merge(p)
				}
				providers[name] = p
			}
			for k, v := range doc.lists {
				perms[k] = append(perms[k], v...)
				ly.Sources[k] = layer.src
			}
			if len(doc.mcp) > 0 {
				mcpFiles = append(mcpFiles, mcpDoc{file: file, servers: doc.mcp, source: layer.src})
			}
		}
		for k, v := range perms { // allow/ask/deny add up across the files
			ly.Values[k] = strings.Join(v, "\n")
		}
	}

	merged := map[string]string{}
	for k, v := range ly.Values {
		merged[k] = v
	}
	for k, v := range procEnv {
		merged[k] = v
		if known[k] {
			ly.Values[k] = v
			ly.Sources[k] = SourceEnv
		}
	}
	name, def := selectProvider(o.Provider, procEnv, merged, ly, providers)
	if err := ly.checkProviderSelection(o.Provider, name, providers); err != nil {
		return nil, err
	}
	cfg := &Config{}
	cfg.envLookup = func(name string) (string, bool) { v, ok := procEnv[name]; return v, ok }
	cfg.envSet = func(name string) bool { _, ok := merged[name]; return ok }
	if err := env.ParseWithOptions(cfg, env.Options{Environment: merged}); err != nil {
		return nil, err
	}
	cfg.Layers = ly
	cfg.LLMProviderName, cfg.Providers = name, providers
	if allowStdioUser {
		if _, set := procEnv["HARNESS_MCP_STDIO"]; !set {
			cfg.MCPAllowStdio = true
		}
	}
	if err := cfg.validateProjectModels(); err != nil {
		return nil, err
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	if err := cfg.resolveAPIKey(procEnv, def, userDir); err != nil {
		return nil, err
	}
	if !o.NoFiles {
		if err := cfg.loadMCPFiles(mcpFiles); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// envIsSet reports whether the merged environment sets name.
func (c *Config) envIsSet(name string) bool {
	if c.envSet != nil {
		return c.envSet(name)
	}
	_, ok := os.LookupEnv(name)
	return ok
}

// ---- the YAML schema ----

type kind int

const (
	kStr kind = iota
	kInt
	kBool
	kFloat
	kList // comma separated
	kMap  // k=v,k=v
	kLines
)

type field struct {
	env  string
	kind kind
}

func f(env string, k kind) field { return field{env, k} }

// schema maps the YAML keys to environment variables: every leaf is a field,
// every other value is a nested section.
var schema = map[string]any{
	"provider":        f("LLM_PROVIDER", kStr),
	"base_url":        f("LLM_BASE_URL", kStr),
	"models":          f("LLM_MODELS", kList),
	"default_model":   f("LLM_DEFAULT_MODEL", kStr),
	"explore_model":   f("EXPLORE_MODEL", kStr),
	"session_header":  f("LLM_SESSION_HEADER", kStr),
	"cache_ttl":       f("LLM_CACHE_TTL", kStr),
	"timeout_seconds": f("LLM_TIMEOUT_SECONDS", kInt),
	"profile":         f("AGENT_PROFILE", kStr),
	"tools":           f("AGENT_TOOLS", kList),
	"code_checks":     f("CODE_CHECKS", kStr),

	"context_windows":  f("LLM_CONTEXT_WINDOWS", kMap),
	"reasoning_models": f("LLM_REASONING_MODELS", kList),
	"vision_models":    f("LLM_VISION_MODELS", kList),
	"model_labels":     f("LLM_MODEL_LABELS", kMap),
	"prompt_variants":  f("LLM_PROMPT_VARIANTS", kMap),
	"presets":          f("LLM_PRESETS", kStr),
	"default_preset":   f("LLM_DEFAULT_PRESET", kStr),

	"limits": map[string]any{
		"max_steps":              f("AGENT_MAX_STEPS", kInt),
		"max_tool_output_chars":  f("AGENT_MAX_TOOL_OUTPUT_CHARS", kInt),
		"tool_output_spill":      f("AGENT_TOOL_OUTPUT_SPILL_CHARS", kInt),
		"step_retries":           f("LLM_STEP_RETRIES", kInt),
		"stream_idle_seconds":    f("LLM_STREAM_IDLE_SECONDS", kInt),
		"call_max_seconds":       f("LLM_CALL_MAX_SECONDS", kInt),
		"max_output_tokens":      f("LLM_MAX_OUTPUT_TOKENS", kStr),
		"compaction_threshold":   f("COMPACTION_THRESHOLD", kFloat),
		"compaction_model":       f("COMPACTION_MODEL", kStr),
		"keep_recent_tool_calls": f("COMPACTION_KEEP_RECENT_TOOL_RESULTS", kInt),
	},

	"sandbox": map[string]any{
		"backend":                f("SANDBOX_BACKEND", kStr),
		"url":                    f("OPENSANDBOX_URL", kStr),
		"image":                  f("OPENSANDBOX_IMAGE", kStr),
		"use_server_proxy":       f("OPENSANDBOX_USE_SERVER_PROXY", kBool),
		"credential_proxy":       f("OPENSANDBOX_CREDENTIAL_PROXY", kBool),
		"workspace":              f("LOCAL_WORKSPACE", kStr),
		"state_dir":              f("LOCAL_STATE_DIR", kStr),
		"timeout_seconds":        f("SANDBOX_TIMEOUT_SECONDS", kInt),
		"command_timeout":        f("SANDBOX_COMMAND_TIMEOUT_SECONDS", kInt),
		"cpu":                    f("SANDBOX_CPU", kStr),
		"memory":                 f("SANDBOX_MEMORY", kStr),
		"egress_allowlist":       f("SANDBOX_EGRESS_ALLOWLIST", kList),
		"egress_unrestricted":    f("SANDBOX_EGRESS_UNRESTRICTED", kBool),
		"max_sandboxes_per_user": f("MAX_SANDBOXES_PER_USER", kInt),
	},

	"permissions": map[string]any{
		"mode":  f("AGENT_PERMISSIONS", kStr),
		"allow": f("AGENT_PERMISSIONS_ALLOW", kLines),
		"ask":   f("AGENT_PERMISSIONS_ASK", kLines),
		"deny":  f("AGENT_PERMISSIONS_DENY", kLines),
	},

	"skills": map[string]any{
		"dir":       f("SKILLS_DIR", kStr),
		"in_coding": f("SKILLS_IN_CODING", kBool),
	},

	"memory": map[string]any{
		"write_mode":             f("HARNESS_MEMORY_WRITE_MODE", kStr),
		"delete_mode":            f("HARNESS_MEMORY_DELETE_MODE", kStr),
		"default_scope":          f("HARNESS_MEMORY_DEFAULT_SCOPE", kStr),
		"embedding_backend":      f("HARNESS_MEMORY_EMBEDDING_BACKEND", kStr),
		"recall_injection":       f("HARNESS_MEMORY_RECALL_INJECTION", kBool),
		"max_entries":            f("HARNESS_MEMORY_MAX_ENTRIES", kInt),
		"max_content_bytes":      f("HARNESS_MEMORY_MAX_CONTENT_BYTES", kInt),
		"consolidation_hours":    f("HARNESS_MEMORY_CONSOLIDATION_HOURS", kInt),
		"deleted_retention_days": f("HARNESS_MEMORY_DELETED_RETENTION_DAYS", kInt),
		"in_coding":              f("MEMORY_IN_CODING", kBool),
	},
	// "mcp", "env" and the api_key_env keys are handled in code.
}

// secretName matches the names whose values must never be written in a file or printed.
var secretName = regexp.MustCompile(`(?i)(KEY|SECRET|PASSWORD|TOKEN|DSN)`)

// knownEnvNames lists the environment variables of Config (its env tags).
func knownEnvNames() map[string]bool {
	out := map[string]bool{}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if tag := sf.Tag.Get("env"); tag != "" && tag != "-" {
				name, _, _ := strings.Cut(tag, ",")
				out[name] = true
			}
			if sf.Type.Kind() == reflect.Struct && sf.Tag.Get("env") == "" {
				walk(sf.Type)
			}
		}
	}
	walk(reflect.TypeOf(Config{}))
	return out
}

type fileDoc struct {
	paths      map[string]string // env name -> the key of the file that set it
	values     map[string]string
	lists      map[string][]string // permissions allow/ask/deny, appended across files
	mcp        map[string]any
	providers  map[string]Provider
	allowStdio bool
}

// parseConfigFile validates a config.yml and translates it into environment
// variable values. Unknown keys and plain secrets are errors naming the file.
func parseConfigFile(file string, data []byte, user bool, procEnv map[string]string, known map[string]bool) (*fileDoc, error) {
	var root map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&root); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	doc := &fileDoc{values: map[string]string{}, paths: map[string]string{}, lists: map[string][]string{}}
	errf := func(format string, a ...any) error { return fmt.Errorf("%s: "+format, append([]any{file}, a...)...) }

	var walk func(prefix string, sec map[string]any, sch map[string]any) error
	walk = func(prefix string, sec map[string]any, sch map[string]any) error {
		for _, key := range sortedKeys(sec) {
			path := prefix + key
			val := sec[key]
			if !user {
				if why, bad := projectForbidden(path); bad {
					return errf("%s", forbiddenMessage(path, why))
				}
			}
			if key == "api_key" || key == "password" || key == "secret" || key == "token" {
				return errf("%s: secrets do not belong in a configuration file; set %s_env to the NAME of the environment variable that holds it", path, key)
			}
			if key == "api_key_env" && (prefix == "" || prefix == "sandbox.") {
				name, ok := val.(string)
				if !ok || strings.TrimSpace(name) == "" {
					return errf("%s must be the name of an environment variable", path)
				}
				target := "LLM_API_KEY"
				if prefix == "sandbox." {
					target = "OPENSANDBOX_API_KEY"
				}
				secret, set := procEnv[strings.TrimSpace(name)]
				if !set || secret == "" {
					return errf("%s: the environment variable %s is not set", path, name)
				}
				doc.values[target] = secret
				continue
			}
			entry, ok := sch[key]
			if !ok {
				return errf("unknown key %q", path)
			}
			switch e := entry.(type) {
			case field:
				s, _, err := convert(val, e.kind)
				if err != nil {
					return errf("%s: %v", path, err)
				}
				if e.kind == kLines {
					doc.lists[e.env] = append(doc.lists[e.env], strings.Split(s, "\n")...)
					continue
				}
				doc.values[e.env] = s
				doc.paths[e.env] = path
			case map[string]any:
				sub, ok := val.(map[string]any)
				if !ok {
					return errf("%s must be a section (key: value pairs)", path)
				}
				if err := walk(path+".", sub, e); err != nil {
					return err
				}
			}
		}
		return nil
	}
	// handled specially
	special := map[string]any{}
	plain := map[string]any{}
	for k, v := range root {
		switch k {
		case "mcp", "env", "providers", "agents":
			special[k] = v
		default:
			plain[k] = v
		}
	}
	if !user {
		for _, k := range []string{"providers", "env"} {
			if _, ok := special[k]; ok {
				if why, bad := projectForbidden(k); bad {
					return nil, errf("%s", forbiddenMessage(k, why))
				}
			}
		}
	}
	if err := walk("", plain, schema); err != nil {
		return nil, err
	}
	if v, ok := special["agents"]; ok {
		if err := parseAgents(doc, v, errf); err != nil {
			return nil, err
		}
	}
	if v, ok := special["env"]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, errf("env must be a section of ENV_NAME: value pairs")
		}
		for _, name := range sortedKeys(m) {
			if !known[name] {
				return nil, errf("env.%s is not a setting of the harness", name)
			}
			if secretName.MatchString(name) {
				return nil, errf("env.%s: secrets do not belong in a configuration file; export %s in the environment", name, name)
			}
			s, _, err := convert(m[name], kStr)
			if err != nil {
				return nil, errf("env.%s: %v", name, err)
			}
			doc.values[name] = s
		}
	}
	if v, ok := special["providers"]; ok {
		ps, err := parseProviders(file, v, SourceUser)
		if err != nil {
			return nil, err
		}
		doc.providers = ps
	}
	if v, ok := special["mcp"]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, errf("mcp must be a section")
		}
		for _, key := range sortedKeys(m) {
			switch key {
			case "servers":
				srv, ok := m[key].(map[string]any)
				if !ok {
					return nil, errf("mcp.servers must map server names to their definition")
				}
				doc.mcp = srv
			case "allow_stdio":
				b, ok := m[key].(bool)
				if !ok {
					return nil, errf("mcp.allow_stdio must be true or false")
				}
				if !user {
					why, _ := projectForbidden("mcp.allow_stdio")
					return nil, errf("%s", forbiddenMessage("mcp.allow_stdio", why))
				}
				doc.allowStdio = b
			case "call_timeout_seconds":
				s, _, err := convert(m[key], kInt)
				if err != nil {
					return nil, errf("mcp.call_timeout_seconds: %v", err)
				}
				doc.values["MCP_CALL_TIMEOUT_SECONDS"] = s
			default:
				return nil, errf("unknown key %q", "mcp."+key)
			}
		}
	}
	if v, ok := doc.values["SANDBOX_BACKEND"]; ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "", "local", "opensandbox", "none":
		default:
			return nil, errf("sandbox.backend must be local, opensandbox or none, got %q", v)
		}
	}
	return doc, nil
}

// UserConfigHint is where a setting a project may not make belongs.
const UserConfigHint = "~/.har/config.yml"

// projectForbidden says whether the key (a dotted path) may not appear in a project config.yml, and why.
// A repository you clone must never decide where data or credentials go, or which models and providers exist.
func projectForbidden(path string) (why string, bad bool) {
	top, _, _ := strings.Cut(path, ".")
	switch top {
	case "providers", "base_url", "api_key_env", "api_key", "session_header", "cache_ttl":
		return "a repository must not choose where your requests and API key are sent", true
	case "models", "presets", "default_preset":
		return "a repository must not decide which models exist", true
	case "timeout_seconds":
		return "it tunes the connection to your provider, which is yours to configure", true
	case "sandbox":
		return "the sandbox backend, its address and its limits belong to you, not to a repository", true
	case "env":
		return "env: can set any setting, including the ones a repository may not set", true
	}
	if path == "mcp.allow_stdio" {
		return "a repository must not enable running its own programs", true
	}
	return "", false
}

func forbiddenMessage(path, why string) string {
	return fmt.Sprintf("%s cannot be set in a project file (%s): put it in %s (or the environment)", path, why, UserConfigHint)
}

// parseAgents reads the agents section: roles mapped to the model (and profile) they use. Only the roles
// that have a setting today are available; the others are rejected rather than silently ignored.
func parseAgents(doc *fileDoc, v any, errf func(string, ...any) error) error {
	roles, ok := v.(map[string]any)
	if !ok {
		return errf("agents must map roles to their settings (agents.default, agents.explore, agents.compaction)")
	}
	for _, role := range sortedKeys(roles) {
		sec, ok := roles[role].(map[string]any)
		if !ok {
			return errf("agents.%s must be a section (key: value pairs)", role)
		}
		var allowed map[string]string // key -> environment variable
		switch role {
		case "default":
			allowed = map[string]string{"model": "LLM_DEFAULT_MODEL", "profile": "AGENT_PROFILE"}
		case "explore":
			allowed = map[string]string{"model": "EXPLORE_MODEL"}
		case "compaction":
			allowed = map[string]string{"model": "COMPACTION_MODEL"}
		case "title", "goal_judge":
			return errf("agents.%s is not available yet: that role uses the model of the run", role)
		default:
			return errf("unknown key %q (roles: default, explore, compaction)", "agents."+role)
		}
		for _, key := range sortedKeys(sec) {
			path := "agents." + role + "." + key
			if role == "default" && key == "reasoning_effort" {
				return errf("%s is not available yet: choose a preset (default_preset) in your user file", path)
			}
			envName, ok := allowed[key]
			if !ok {
				return errf("unknown key %q", path)
			}
			s, _, err := convert(sec[key], kStr)
			if err != nil {
				return errf("%s: %v", path, err)
			}
			doc.values[envName] = strings.TrimSpace(s)
			doc.paths[envName] = path
		}
	}
	return nil
}

// PermissionStrictness ranks the permission modes: the higher, the stricter. "" is ask.
func PermissionStrictness(mode string) (int, bool) {
	m := strings.ToLower(strings.TrimSpace(mode))
	m = strings.NewReplacer("_", "-").Replace(m)
	switch m {
	case "readonly", "read-only":
		return 4, true
	case "", "ask":
		return 3, true
	case "auto-edit", "autoedit":
		return 2, true
	case "yolo":
		return 1, true
	case "yolo-unsafe":
		return 0, true
	}
	return 0, false
}

// tightenOnly drops from the project file of an untrusted project what would loosen the permissions: allow
// rules and a mode less strict than the one already in force. The ignored items are recorded.
func (ly *Layers) tightenOnly(doc *fileDoc) {
	file := ly.ProjectFile
	if n := len(doc.lists["AGENT_PERMISSIONS_ALLOW"]); n > 0 {
		delete(doc.lists, "AGENT_PERMISSIONS_ALLOW")
		ly.ignore("%s: permissions.allow (%d rule(s)) ignored: the project is not trusted (run `harness trust` to honour it)", file, n)
	}
	mode, ok := doc.values["AGENT_PERMISSIONS"]
	if !ok {
		return
	}
	base := ly.Values["AGENT_PERMISSIONS"] // the user file's, "" is ask
	pr, pok := PermissionStrictness(mode)
	br, _ := PermissionStrictness(base)
	if pok && pr < br {
		delete(doc.values, "AGENT_PERMISSIONS")
		if base == "" {
			base = "ask"
		}
		ly.ignore("%s: permissions.mode %q ignored: it is looser than %q and the project is not trusted (run `harness trust` to honour it)", file, mode, base)
	}
}

func (ly *Layers) ignore(format string, a ...any) {
	ly.Ignored = append(ly.Ignored, fmt.Sprintf(format, a...))
}

// checkProviderSelection refuses a provider chosen by the project that the user did not define.
func (ly *Layers) checkProviderSelection(flag, name string, defs map[string]Provider) error {
	if strings.TrimSpace(flag) != "" || ly.Sources["LLM_PROVIDER"] != SourceProject {
		return nil
	}
	if _, ok := defs[name]; ok || name == "openai" || name == "anthropic" {
		return nil
	}
	return fmt.Errorf("%s: provider %q (project .har/config.yml provider) is not configured by you: define it under providers.%s in %s, or choose openai, anthropic or a provider you defined", ly.ProjectFile, name, name, UserConfigHint)
}

// validateProjectModels checks that every model the project selected is one the user configured: the
// catalog is the user's model list of the selected provider, plus the models the user set themselves.
func (c *Config) validateProjectModels() error {
	ly := c.Layers
	if ly == nil || ly.ProjectFile == "" {
		return nil
	}
	catalog := map[string]bool{}
	for _, m := range trimAll(c.LLMModels) {
		catalog[m] = true
	}
	sel := []struct{ env, val string }{
		{"LLM_DEFAULT_MODEL", c.LLMDefaultModel}, {"EXPLORE_MODEL", c.ExploreModel}, {"COMPACTION_MODEL", c.CompactionModel},
	}
	for _, s := range sel {
		if _, set := ly.Sources[s.env]; set && ly.Sources[s.env] != SourceProject {
			catalog[strings.TrimSpace(s.val)] = true
		}
	}
	where := "models"
	if _, ok := c.Providers[c.LLMProviderName]; ok {
		where = "providers." + c.LLMProviderName + ".models"
	}
	for _, s := range sel {
		v := strings.TrimSpace(s.val)
		if ly.Sources[s.env] != SourceProject || v == "" || catalog[v] {
			continue
		}
		path := ly.paths[s.env]
		return fmt.Errorf("model %s (project .har/config.yml %s) is not configured by you: add it to %s in %s", v, path, where, UserConfigHint)
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// convert renders a YAML value as the text the environment parser expects.
func convert(v any, k kind) (string, bool, error) {
	scalar := func(x any) (string, error) {
		switch t := x.(type) {
		case string:
			return t, nil
		case bool:
			return strconv.FormatBool(t), nil
		case int:
			return strconv.Itoa(t), nil
		case int64:
			return strconv.FormatInt(t, 10), nil
		case float64:
			return strconv.FormatFloat(t, 'g', -1, 64), nil
		case nil:
			return "", nil
		}
		return "", fmt.Errorf("expected a single value, got %T", x)
	}
	switch k {
	case kStr, kInt, kBool, kFloat:
		s, err := scalar(v)
		if err != nil {
			return "", false, err
		}
		switch k {
		case kInt:
			if _, err := strconv.Atoi(s); err != nil {
				return "", false, fmt.Errorf("expected an integer, got %q", s)
			}
		case kBool:
			if _, err := strconv.ParseBool(s); err != nil {
				return "", false, fmt.Errorf("expected true or false, got %q", s)
			}
		case kFloat:
			if _, err := strconv.ParseFloat(s, 64); err != nil {
				return "", false, fmt.Errorf("expected a number, got %q", s)
			}
		}
		return s, false, nil
	case kList, kLines:
		var items []string
		switch t := v.(type) {
		case []any:
			for _, x := range t {
				s, err := scalar(x)
				if err != nil {
					return "", true, err
				}
				items = append(items, s)
			}
		case string:
			items = []string{t}
		default:
			return "", true, fmt.Errorf("expected a list, got %T", v)
		}
		sep := ","
		if k == kLines {
			sep = "\n"
		}
		return strings.Join(items, sep), true, nil
	case kMap:
		m, ok := v.(map[string]any)
		if !ok {
			if s, isStr := v.(string); isStr {
				return s, false, nil
			}
			return "", false, fmt.Errorf("expected key: value pairs, got %T", v)
		}
		var items []string
		for _, key := range sortedKeys(m) {
			s, err := scalar(m[key])
			if err != nil {
				return "", false, err
			}
			items = append(items, key+"="+s)
		}
		return strings.Join(items, ","), false, nil
	}
	return "", false, fmt.Errorf("unsupported value")
}

// ---- introspection ----

// Dump writes the effective configuration to w: every setting that does not
// have its built-in default value, with where it comes from. flags are the
// settings given on the command line (name -> value), which win over the rest.
// Secrets are masked.
func (c *Config) Dump(w io.Writer, flags map[string]string) {
	ly := c.Layers
	if ly == nil {
		ly = &Layers{}
	}
	show := func(label, v string) string {
		if secretName.MatchString(label) {
			if v == "" {
				return ""
			}
			return "****"
		}
		return v
	}
	fmt.Fprintf(w, "project root: %s\n", orNone(ly.ProjectRoot))
	fmt.Fprintf(w, "project file: %s\n", orNone(ly.ProjectFile))
	fmt.Fprintf(w, "user file:    %s\n", orNone(ly.UserFile))
	fmt.Fprintf(w, "auth file:    %s\n", orNone(ly.AuthFile))
	if ly.ProjectRoot != "" {
		if ly.Trusted {
			fmt.Fprintf(w, "trust:        project trusted (%s)\n", ly.TrustedBy)
		} else {
			fmt.Fprintf(w, "trust:        project not trusted (run `harness trust` to let it add allow rules and MCP servers)\n")
		}
	}
	if c.LLMProviderName != "" {
		fmt.Fprintf(w, "provider:     %s (type %s)\n", c.LLMProviderName, c.LLMProvider)
	}
	for _, src := range ly.MCPSources {
		fmt.Fprintf(w, "mcp file:     %s\n", src)
	}
	fmt.Fprintln(w, "precedence:   flag > env > project file > user file > default")
	fmt.Fprintln(w)
	type row struct{ name, val, src string }
	var rows []row
	for name, v := range ly.Values {
		if name == "LLM_API_KEY" {
			continue // shown below with the source that supplied it
		}
		rows = append(rows, row{name, show(name, v), string(ly.Sources[name])})
	}
	for name, v := range flags {
		rows = append(rows, row{name, show(name, v), string(SourceFlag)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	for _, r := range rows {
		if r.val == "" && r.src == string(SourceEnv) {
			continue
		}
		fmt.Fprintf(w, "%-40s %-30s (%s)\n", r.name, r.val, r.src)
	}
	if c.LLMAPIKey != "" {
		fmt.Fprintf(w, "%-40s %-30s (%s)\n", "LLM_API_KEY", "****", c.LLMKeySource)
	}
	if n := len(c.MCPServers); n > 0 {
		fmt.Fprintf(w, "\nmcp servers:\n")
		for _, s := range c.MCPServers {
			fmt.Fprintf(w, "  %-20s %s (%s)\n", s.Name, s.Describe(), s.Origin)
		}
	}
	if len(ly.MCPSkipped) > 0 {
		fmt.Fprintf(w, "\nmcp servers not started:\n")
		for _, s := range ly.MCPSkipped {
			fmt.Fprintf(w, "  %s\n", s)
		}
	}
	if len(ly.Ignored) > 0 {
		fmt.Fprintf(w, "\nignored:\n")
		for _, s := range ly.Ignored {
			fmt.Fprintf(w, "  %s\n", s)
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
