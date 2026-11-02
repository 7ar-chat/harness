package llm

import (
	"net/http"
	"strings"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/responses"
)

// responsesModel is the Responses API client of a model in Options.ResponsesModels:
// {BaseURL}/responses over the provider's HTTP client (timeouts, CallInfo).
func (p *Provider) responsesModel(name string) (ai.Model, error) {
	base := p.cfg.BaseURL
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	return responses.New(responses.Config{
		URL: strings.TrimRight(base, "/") + "/responses", Model: name, Name: name, HTTPClient: p.client, Header: header,
		OnRefusal: RecordRefusal, ReasoningSummary: "auto", Logger: p.cfg.Logger,
	})
}
