// Package ai is the vocabulary an agent speaks with language models and tools: messages, tool
// descriptions, call options, streams and the helpers to build tools from Go functions.
//
// It depends on nothing outside the standard library and one JSON-schema reflector, so a provider
// client or a tool set can be written against it alone. The sub-packages build on it:
//
//	ai/openai     OpenAI Chat Completions client (and compatible gateways)
//	ai/llm        provider factory, error classification, retry delays; llm/anthropic, llm/responses clients
//	ai/agent      the tool-calling loop, streaming under a watchdog, parallel tool scheduling
//	ai/compaction keeping a conversation inside a context window
//	ai/llm/llmtest a scripted fake model for tests
package ai

// Role is who wrote a Message.
type Role string

// Roles.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// FunctionCall is the function a ToolCall invokes; Arguments is a JSON object (possibly a fragment
// while a stream is in flight).
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolCall is a request of the model to run a tool.
type ToolCall struct {
	// Index orders the fragments of one call in a stream (nil: the chunk is a whole call).
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// PartType is the kind of a Part.
type PartType string

// Part types.
const (
	PartText  PartType = "text"
	PartImage PartType = "image"
)

// ImageDetail is the fidelity an image is read at ("" is auto).
type ImageDetail string

// Image is an image given to the model: a URL or base64 data with its media type.
type Image struct {
	URL        string      `json:"url,omitempty"`
	Base64Data string      `json:"base64data,omitempty"`
	MIMEType   string      `json:"mime_type,omitempty"`
	Detail     ImageDetail `json:"detail,omitempty"`
}

// Part is one element of a multimodal user message.
type Part struct {
	Type  PartType       `json:"type"`
	Text  string         `json:"text,omitempty"`
	Image *Image         `json:"image,omitempty"`
	Extra map[string]any `json:"extra,omitempty"`
}

// TokenUsage is what a call cost in tokens.
type TokenUsage struct {
	PromptTokens       int                `json:"prompt_tokens"`
	PromptTokenDetails PromptTokenDetails `json:"prompt_token_details"`
	CompletionTokens   int                `json:"completion_tokens"`
	TotalTokens        int                `json:"total_tokens"`
	// CompletionTokenDetails breaks the completion tokens down.
	CompletionTokenDetails CompletionTokenDetails `json:"completion_token_details"`
}

// PromptTokenDetails breaks the prompt tokens down.
type PromptTokenDetails struct {
	// CachedTokens were read from the provider's prompt cache.
	CachedTokens int `json:"cached_tokens"`
	// CacheWriteTokens were written to it.
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// CompletionTokenDetails breaks the completion tokens down.
type CompletionTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// ResponseMeta is what the provider reported about an answer.
type ResponseMeta struct {
	FinishReason string      `json:"finish_reason,omitempty"`
	Usage        *TokenUsage `json:"usage,omitempty"`
}

// Message is one turn of a conversation, or a chunk of one while it streams.
type Message struct {
	Role Role `json:"role"`
	// Content is the text of the message.
	Content string `json:"content"`
	// Parts, when not empty, replace Content for a user message with images.
	Parts []Part `json:"parts,omitempty"`
	Name  string `json:"name,omitempty"`
	// ToolCalls are the calls an assistant message asks for.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID and ToolName say which call a tool message answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	// ResponseMeta is set on answers.
	ResponseMeta *ResponseMeta `json:"response_meta,omitempty"`
	// ReasoningContent is the thinking the model returned.
	ReasoningContent string `json:"reasoning_content,omitempty"`
	// Extra carries provider-specific and harness-specific values with the message.
	Extra map[string]any `json:"extra,omitempty"`
}

// SystemMessage is a system message.
func SystemMessage(content string) *Message { return &Message{Role: RoleSystem, Content: content} }

// UserMessage is a user message.
func UserMessage(content string) *Message { return &Message{Role: RoleUser, Content: content} }

// AssistantMessage is an assistant message with its tool calls.
func AssistantMessage(content string, toolCalls []ToolCall) *Message {
	return &Message{Role: RoleAssistant, Content: content, ToolCalls: toolCalls}
}

// ToolMessageOption customises ToolMessage.
type ToolMessageOption func(*Message)

// WithToolName records the name of the tool a result answers.
func WithToolName(name string) ToolMessageOption { return func(m *Message) { m.ToolName = name } }

// ToolMessage is the result of the tool call toolCallID.
func ToolMessage(content, toolCallID string, opts ...ToolMessageOption) *Message {
	m := &Message{Role: RoleTool, Content: content, ToolCallID: toolCallID}
	for _, o := range opts {
		o(m)
	}
	return m
}
