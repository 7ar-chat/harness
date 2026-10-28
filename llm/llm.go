// Package llm builds eino chat models for the configured provider.
package llm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"

	"github.com/GhaziBenDahmane/harness/auth"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/llm/anthropic"
)

// Factory returns a chat model by id. Tests inject a fake.
type Factory interface {
	Model(ctx context.Context, name string) (model.ToolCallingChatModel, error)
}

// GatewayDummyKey is replaced by the llm-gateway.
const GatewayDummyKey = "NOT_IMPORTANT_REPLACED_BY_LLM_GATEWAY"

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
// provider); the gateway headers and the CallInfo observation still apply.
func NewWithTransport(cfg *config.Config, base http.RoundTripper) *Provider {
	var rt http.RoundTripper = &observeTransport{base: base}
	if cfg.LLMProvider == "gateway" {
		rt = &gatewayTransport{base: rt, useCase: cfg.LLMUseCase}
	}
	if cfg.LLMSessionHeader != "" {
		rt = &sessionTransport{base: rt, header: cfg.LLMSessionHeader}
	}
	return &Provider{cfg: cfg, client: &http.Client{Transport: rt}, models: map[string]model.ToolCallingChatModel{}}
}

type gatewayTransport struct {
	base    http.RoundTripper
	useCase string
}

func (t *gatewayTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("X-Use-Case", t.useCase)
	if ui := auth.UserinfoFromContext(req.Context()); ui != "" {
		req.Header.Set("x-userinfo", ui)
	}
	return t.base.RoundTrip(req)
}

// DefaultGatewayProvider is the gateway provider of model ids without a "provider/" prefix.
const DefaultGatewayProvider = "openai"

// SplitModel splits an LLM_MODELS id "provider/model" (e.g.
// "deepseek/DeepSeek-V4.1-Flash"); ids without a prefix use the openai provider.
func SplitModel(id string) (provider, name string) {
	if p, n, ok := strings.Cut(id, "/"); ok && p != "" && n != "" {
		return p, n
	}
	return DefaultGatewayProvider, id
}

// wireName is the model id sent in the request body: the Studio gateway carries the
// provider in its URL, so only the bare name is sent; any other OpenAI-compatible
// endpoint (a router, OpenRouter) gets the id exactly as configured, slashes included.
func (p *Provider) wireName(id string) string {
	if p.cfg.LLMProvider == "gateway" {
		_, bare := SplitModel(id)
		return bare
	}
	return id
}

// GatewayBaseURL is the Azure-style base URL of a model on the Studio llm-gateway:
// {gateway}/usecase/{uc}/providers/{provider}/models/{model}/.
func GatewayBaseURL(gatewayURL, useCase, modelID string) string {
	provider, name := SplitModel(modelID)
	return strings.TrimRight(gatewayURL, "/") + "/usecase/" + useCase + "/providers/" + provider + "/models/" + name + "/"
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
	bare := p.wireName(name)
	mc := &einoopenai.ChatModelConfig{
		Model:      bare,
		HTTPClient: p.client,
		Timeout:    time.Duration(p.cfg.LLMTimeout) * time.Second,
	}
	switch p.cfg.LLMProvider {
	case "gateway":
		mc.APIKey = GatewayDummyKey
		mc.BaseURL = GatewayBaseURL(p.cfg.LLMGatewayURL, p.cfg.LLMUseCase, name)
		if provider, _ := SplitModel(name); provider == DefaultGatewayProvider {
			// Azure OpenAI deployments: …/openai/deployments/{model}/chat/completions?api-version=…
			mc.ByAzure = true
			mc.APIVersion = p.cfg.LLMAPIVersion
			mc.AzureModelMapperFunc = func(m string) string { return m }
		} else {
			// Other gateway providers (DeepSeek) speak plain OpenAI: …/chat/completions, no api-version.
			mc.BaseURL = strings.TrimRight(mc.BaseURL, "/")
		}
	default:
		mc.APIKey = p.cfg.LLMAPIKey
		mc.BaseURL = p.cfg.LLMBaseURL
	}
	m, err := einoopenai.NewChatModel(ctx, mc)
	if err != nil {
		return nil, err
	}
	p.models[name] = m
	return m, nil
}
