// Package llm builds eino chat models for the configured provider.
package llm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"

	"github.com/GhaziBenDahmane/harness/engine/llm/anthropic"
)

// Factory returns a chat model by id. Tests inject a fake.
type Factory interface {
	Model(ctx context.Context, name string) (model.ToolCallingChatModel, error)
}

// Options are the settings of a Provider; the composition root fills them
// from its configuration.
type Options struct {
	// Provider selects the wire protocol: "anthropic", else OpenAI-compatible.
	Provider string
	// BaseURL and APIKey address the endpoint.
	BaseURL string
	APIKey  string
	// Models is the allow-list of model ids; any other id is refused.
	Models []string
	// ResponsesModels are the Models called through the OpenAI Responses API
	// (the rest use Chat Completions).
	ResponsesModels []string
	// TimeoutSeconds bounds connecting and waiting for the response headers
	// (0: unbounded).
	TimeoutSeconds int
	// SessionHeader is the name of a request header that carries the thread id
	// (empty: none).
	SessionHeader string
	// CacheTTL is the prompt-cache lifetime of the anthropic provider ("5m" or "1h").
	CacheTTL string
}

// modelAllowed reports whether a model id is in the allow-list.
func (o Options) modelAllowed(name string) bool { return slices.Contains(o.Models, name) }

// usesResponses reports whether a model is called through the Responses API.
func (o Options) usesResponses(name string) bool {
	for _, m := range o.ResponsesModels {
		if strings.TrimSpace(m) == name {
			return true
		}
	}
	return false
}

// Provider is the eino-ext/openai backed Factory.
type Provider struct {
	cfg    Options
	client *http.Client
	mu     sync.Mutex
	models map[string]model.ToolCallingChatModel
}

// New builds the factory for o.Provider. eino-ext ignores
// ChatModelConfig.Timeout when an HTTP client is injected, so the client's
// transport bounds the call instead: Options.TimeoutSeconds for connecting and
// for the response headers (an http.Client.Timeout would also cut long
// streams; the agent bounds the gaps between chunks).
func New(cfg Options) *Provider {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.TimeoutSeconds > 0 {
		d := time.Duration(cfg.TimeoutSeconds) * time.Second
		tr.ResponseHeaderTimeout = d
		tr.TLSHandshakeTimeout = min(d, 30*time.Second)
		tr.DialContext = (&net.Dialer{Timeout: min(d, 30*time.Second), KeepAlive: 30 * time.Second}).DialContext
	}
	return NewWithTransport(cfg, tr)
}

// NewWithTransport is New over another base transport (tests: a fake
// provider); the CallInfo observation and session header still apply.
func NewWithTransport(cfg Options, base http.RoundTripper) *Provider {
	var rt http.RoundTripper = &observeTransport{base: base}
	if cfg.SessionHeader != "" {
		rt = &sessionTransport{base: rt, header: cfg.SessionHeader}
	}
	return &Provider{cfg: cfg, client: &http.Client{Transport: rt}, models: map[string]model.ToolCallingChatModel{}}
}

func (p *Provider) Model(ctx context.Context, name string) (model.ToolCallingChatModel, error) {
	if !p.cfg.modelAllowed(name) {
		return nil, fmt.Errorf("model %q is not allowed", name)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.models[name]; ok {
		return m, nil
	}
	if p.cfg.usesResponses(name) {
		m, err := p.responsesModel(name)
		if err != nil {
			return nil, err
		}
		p.models[name] = m
		return m, nil
	}
	if p.cfg.Provider == "anthropic" {
		m, err := anthropic.New(anthropic.Config{
			BaseURL: p.cfg.BaseURL, APIKey: p.cfg.APIKey, Model: name, HTTPClient: p.client, CacheTTL: p.cfg.CacheTTL,
		})
		if err != nil {
			return nil, err
		}
		p.models[name] = m
		return m, nil
	}
	mc := &einoopenai.ChatModelConfig{
		Model:      name,
		HTTPClient: p.client,
		Timeout:    time.Duration(p.cfg.TimeoutSeconds) * time.Second,
		APIKey:     p.cfg.APIKey,
		BaseURL:    p.cfg.BaseURL,
	}
	m, err := einoopenai.NewChatModel(ctx, mc)
	if err != nil {
		return nil, err
	}
	p.models[name] = m
	return m, nil
}
