package llm

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/llm/responses"
)

// responsesModel is the Responses API client of a model in LLM_RESPONSES_MODELS:
// the gateway's Azure-style …/providers/openai/models/{m}/openai/responses?api-version=…
// (or {LLM_BASE_URL}/responses for the openai provider), over the provider's
// HTTP client (timeouts, X-Use-Case, x-userinfo, CallInfo).
func (p *Provider) responsesModel(name string) (model.ToolCallingChatModel, error) {
	_, bare := SplitModel(name)
	header := http.Header{}
	var u string
	switch p.cfg.LLMProvider {
	case "gateway":
		u = GatewayBaseURL(p.cfg.LLMGatewayURL, p.cfg.LLMUseCase, name) + "openai/responses?api-version=" + url.QueryEscape(p.cfg.LLMAPIVersion)
		header.Set("api-key", GatewayDummyKey)
	default:
		u = strings.TrimRight(p.cfg.LLMBaseURL, "/") + "/responses"
		header.Set("Authorization", "Bearer "+p.cfg.LLMAPIKey)
	}
	return responses.New(responses.Config{
		URL: u, Model: bare, Name: name, HTTPClient: p.client, Header: header,
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
