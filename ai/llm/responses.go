package llm

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/llm/responses"
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
		OnRefusal: RecordRefusal, ReasoningSummary: "auto",
	})
}

// ReasoningItems returns the opaque reasoning items (Responses API) an
// assistant message was generated with, encoded for storage (nil: none).
func ReasoningItems(m *ai.Message) json.RawMessage {
	return responses.MarshalItems(responses.ItemsOf(m))
}

// SetReasoningItems attaches stored reasoning items to an assistant message of
// the history: the Responses client sends them back before the message's own
// items (other clients ignore them).
func SetReasoningItems(m *ai.Message, raw json.RawMessage) {
	responses.SetItems(m, responses.UnmarshalItems(raw))
}

// CopyReasoningItems gives dst the reasoning items of src.
func CopyReasoningItems(dst, src *ai.Message) {
	responses.SetItems(dst, responses.ItemsOf(src))
}
