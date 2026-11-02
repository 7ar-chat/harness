package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"sync"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/hosts/inproc"
	"github.com/7ar-chat/harness/hosts/tui"
)

// engine is the in-process harness the terminal UI talks to. Its configuration
// is fixed once a harness runs, so connecting a provider builds a new harness
// over the same store and swaps the UI's client to it: no restart. It implements
// tui.Provisioner (the /connect flow and the first-run home screen).
type engine struct {
	root context.Context
	user string
	st   harness.Store
	mk   factory

	mu      sync.Mutex
	cfg     *config.Config
	model   string
	stop    context.CancelFunc
	lookup  func(string) (client.ModelInfo, bool)
	backend *tui.ClientBackend
}

// start builds a harness for cfg and returns the client of it. lenient builds
// one even without an API key (the first-run home screen: nothing can run until
// a provider is connected).
func (e *engine) start(cfg *config.Config, model string, lenient bool) (client.Client, error) {
	var h *harness.Harness
	var err error
	if lenient {
		h, err = harness.New(harness.Options{Config: cfg, Store: e.st, Version: "cli"})
	} else {
		h, err = e.mk(cfg, e.st)
	}
	if err != nil {
		return nil, err
	}
	ctx, stop := context.WithCancel(e.root)
	go func() { _ = h.Run(ctx) }()
	lookup := modelLookup(ctx, cfg)
	models := cfg.LLMModels
	if model != "" && !slices.Contains(models, model) {
		models = append([]string{model}, models...)
	}
	c := inproc.New(h, e.user, inproc.WithModels(models, model), inproc.WithModelInfo(lookup))

	e.mu.Lock()
	old := e.stop
	e.cfg, e.model, e.stop, e.lookup = cfg, model, stop, lookup
	e.mu.Unlock()
	if old != nil {
		old() // the previous harness is no longer used
	}
	return c, nil
}

// current is the configuration in use.
func (e *engine) current() *config.Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

func (e *engine) discover(ctx context.Context) ([]string, error) {
	return discoverer(e.current())(ctx)
}

func (e *engine) status(ctx context.Context) ([]tui.StatusItem, error) {
	return statusFunc(e.current())(ctx)
}

func (e *engine) info(id string) (client.ModelInfo, bool) {
	e.mu.Lock()
	l := e.lookup
	e.mu.Unlock()
	if l == nil {
		return client.ModelInfo{}, false
	}
	return l(id)
}

// --- tui.Connector ---

// Providers lists the built-in types and the named providers of the config
// files, each with where its key comes from (the environment, auth.json) or
// that it needs none.
func (e *engine) Providers() []tui.ProviderInfo {
	cfg := e.current()
	names := []string{"openai", "anthropic"}
	var named []string
	for n := range cfg.Providers {
		if n != "openai" && n != "anthropic" {
			named = append(named, n)
		}
	}
	sort.Strings(named)
	names = append(names, named...)
	keys, _ := config.ReadAuth(config.HomeDir())
	out := make([]tui.ProviderInfo, 0, len(names))
	for _, n := range names {
		pi := tui.ProviderInfo{Name: n, BaseURL: cfg.Providers[n].BaseURL}
		if c, err := config.LoadWith(config.LoadOptions{Provider: n}); err == nil {
			if pi.BaseURL == "" {
				pi.BaseURL = c.LLMBaseURL
			}
			switch {
			case c.LLMAPIKey != "" && c.LLMKeySource != "auth.json":
				pi.KeyFrom = c.LLMKeySource
			case c.LLMAPIKey == "" && c.RequireLLMKey() == nil:
				pi.NoKey = true
			}
		}
		_, pi.HasKey = keys[n]
		out = append(out, pi)
	}
	return out
}

// Save stores the key through the code path of `harness auth login`.
func (e *engine) Save(provider, key string) error {
	dir := config.HomeDir()
	if dir == "" {
		return errors.New("no home directory: set HAR_HOME")
	}
	return config.SetAuthKey(dir, provider, key)
}

// --- tui.Provisioner ---

func (e *engine) Configured() bool { return e.current().RequireLLMKey() == nil }

func (e *engine) Active() (string, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cfg.RequireLLMKey() != nil {
		return "", ""
	}
	return e.cfg.LLMProviderName, e.model
}

func (e *engine) ConfigPath() string {
	if d := config.UserDir(); d != "" {
		return filepath.Join(d, config.ConfigFile)
	}
	return ""
}

// Models lists what provider serves: the models its configuration names, then
// the ones it reports. When it cannot be asked the configured ones come back
// with the error.
func (e *engine) Models(ctx context.Context, provider string) ([]string, error) {
	cfg, err := config.LoadWith(config.LoadOptions{Provider: provider})
	if err != nil {
		return nil, err
	}
	ids := slices.Clone(cfg.LLMModels)
	found, err := llm.Discover(ctx, http.DefaultClient, llm.Options{Provider: cfg.LLMProvider, BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey})
	for _, id := range found {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, err
}

// Apply switches the engine to provider and model: a new harness over the same
// store, then the UI's client moves to it. The old one stops after the new one
// is built, so a failure leaves the UI as it was.
func (e *engine) Apply(ctx context.Context, provider, model string) error {
	cfg, err := config.LoadWith(config.LoadOptions{Provider: provider})
	if err != nil {
		return err
	}
	if err := cfg.RequireLLMKey(); err != nil {
		return err
	}
	if model == "" {
		model = cfg.LLMDefaultModel
	}
	c, err := e.start(cfg, model, false)
	if err != nil {
		return fmt.Errorf("start the engine: %w", err)
	}
	if b := e.backend; b != nil {
		b.C, b.ModelID = c, model
		if e.st == nil { // a memory store died with the old harness: the conversation starts again
			if th, err := c.CreateThread(ctx, ""); err == nil {
				b.ThreadID = th.ID
			}
		}
	}
	return nil
}

var _ tui.Provisioner = (*engine)(nil)
