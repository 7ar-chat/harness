package llm

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/engine/llm/responses"
)

// responsesModel is the Responses API client of a model in LLM_RESPONSES_MODELS:
// {LLM_BASE_URL}/responses over the provider's HTTP client (timeouts, CallInfo).
func (p *Provider) responsesModel(name string) (model.ToolCallingChatModel, error) {
	base := p.cfg.LLMBaseURL
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+p.cfg.LLMAPIKey)
	return responses.New(responses.Config{
		URL: strings.TrimRight(base, "/") + "/responses", Model: name, Name: name, HTTPClient: p.client, Header: header,
		OnRefusal: RecordRefusal, ReasoningSummary: "auto",
	})
}

// ReasoningItems returns the opaque reasoning items (Responses API) an
// assistant message was generated with, encoded for storage (nil: none).
func ReasoningItems(m *schema.Message) json.RawMessage {
	return responses.MarshalItems(responses.ItemsOf(m))
}

// SetReasoningItems attaches stored reasoning items to an assistant message of
// the history: the Responses client sends them back before the message's own
// items (other clients ignore them).
func SetReasoningItems(m *schema.Message, raw json.RawMessage) {
	responses.SetItems(m, responses.UnmarshalItems(raw))
}

// CopyReasoningItems gives dst the reasoning items of src.
func CopyReasoningItems(dst, src *schema.Message) {
	responses.SetItems(dst, responses.ItemsOf(src))
}
