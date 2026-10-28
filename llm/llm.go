// Package llm builds eino chat models for the configured provider.
package llm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/llm/anthropic"
)

// Factory returns a chat model by id. Tests inject a fake.
type Factory interface {
	Model(ctx context.Context, name string) (model.ToolCallingChatModel, error)
}

// Provider is the eino-ext/openai backed Factory.
type Provider struct {
	cfg    *config.Config
	client *http.Client
	mu     sync.Mutex
	models map[string]model.ToolCallingChatModel
}

// New builds the factory for cfg.LLMProvider. eino-ext ignores
// ChatModelConfig.Timeout when an HTTP client is injected, so the client's
// transport bounds the call instead: LLM_TIMEOUT_SECONDS for connecting and
// for the response headers (an http.Client.Timeout would also cut long
// streams; the agent bounds the gaps between chunks).
func New(cfg *config.Config) *Provider {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.LLMTimeout > 0 {
		d := time.Duration(cfg.LLMTimeout) * time.Second
		tr.ResponseHeaderTimeout = d
		tr.TLSHandshakeTimeout = min(d, 30*time.Second)
		tr.DialContext = (&net.Dialer{Timeout: min(d, 30*time.Second), KeepAlive: 30 * time.Second}).DialContext
	}
	return NewWithTransport(cfg, tr)
}

// NewWithTransport is New over another base transport (tests: a fake
// provider); the CallInfo observation and session header still apply.
func NewWithTransport(cfg *config.Config, base http.RoundTripper) *Provider {
	var rt http.RoundTripper = &observeTransport{base: base}
	if cfg.LLMSessionHeader != "" {
		rt = &sessionTransport{base: rt, header: cfg.LLMSessionHeader}
	}
	return &Provider{cfg: cfg, client: &http.Client{Transport: rt}, models: map[string]model.ToolCallingChatModel{}}
}

func (p *Provider) Model(ctx context.Context, name string) (model.ToolCallingChatModel, error) {
	if !p.cfg.ModelAllowed(name) {
		return nil, fmt.Errorf("model %q is not allowed", name)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.models[name]; ok {
		return m, nil
	}
	if p.cfg.UsesResponses(name) {
		m, err := p.responsesModel(name)
		if err != nil {
			return nil, err
		}
		p.models[name] = m
		return m, nil
	}
	if p.cfg.LLMProvider == "anthropic" {
		m, err := anthropic.New(anthropic.Config{
			BaseURL: p.cfg.LLMBaseURL, APIKey: p.cfg.LLMAPIKey, Model: name, HTTPClient: p.client, CacheTTL: p.cfg.LLMCacheTTL,
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
		Timeout:    time.Duration(p.cfg.LLMTimeout) * time.Second,
		APIKey:     p.cfg.LLMAPIKey,
		BaseURL:    p.cfg.LLMBaseURL,
	}
	m, err := einoopenai.NewChatModel(ctx, mc)
	if err != nil {
		return nil, err
	}
	p.models[name] = m
	return m, nil
}
