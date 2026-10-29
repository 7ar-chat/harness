// Package agui builds AG-UI v1.0 events emitted by the runtime.
package agui

// Event type names.
const (
	RunStarted              = "RUN_STARTED"
	RunFinished             = "RUN_FINISHED"
	RunError                = "RUN_ERROR"
	StepStarted             = "STEP_STARTED"
	StepFinished            = "STEP_FINISHED"
	ReasoningStart          = "REASONING_START"
	ReasoningEnd            = "REASONING_END"
	ReasoningMessageStart   = "REASONING_MESSAGE_START"
	ReasoningMessageContent = "REASONING_MESSAGE_CONTENT"
	ReasoningMessageEnd     = "REASONING_MESSAGE_END"
	TextMessageStart        = "TEXT_MESSAGE_START"
	TextMessageContent      = "TEXT_MESSAGE_CONTENT"
	TextMessageEnd          = "TEXT_MESSAGE_END"
	ToolCallStart           = "TOOL_CALL_START"
	ToolCallArgs            = "TOOL_CALL_ARGS"
	ToolCallEnd             = "TOOL_CALL_END"
	ToolCallResult          = "TOOL_CALL_RESULT"
	Custom                  = "CUSTOM"
)

// Custom event names.
const (
	CustomToolOutput = "work.tool_output"
	CustomSteer      = "work.steer"
	CustomUsage      = "work.usage"
	CustomSubagent   = "work.subagent"
)

// Phases of a work.subagent event.
const (
	SubagentStart = "start"
	SubagentStep  = "step"
	SubagentTool  = "tool"
	SubagentText  = "text"
	SubagentEnd   = "end"
)

// RUN_ERROR codes.
const (
	CodeModelError  = "model_error"
	CodeInterrupted = "interrupted"
	CodeMaxSteps    = "max_steps"
	CodeInternal    = "internal"
)

// Event is an AG-UI event; "type" is always set, "timestamp" is added by the event log.
type Event map[string]any

// Type returns the event type.
func (e Event) Type() string { s, _ := e["type"].(string); return s }

// IsTerminal reports whether the event ends a run stream.
func IsTerminal(eventType string) bool { return eventType == RunFinished || eventType == RunError }

// New builds an event of the given type with the given fields.
func New(eventType string, fields map[string]any) Event {
	e := Event{"type": eventType}
	for k, v := range fields {
		e[k] = v
	}
	return e
}

// RunStartedEvent builds RUN_STARTED.
func RunStartedEvent(threadID, runID string) Event {
	return New(RunStarted, map[string]any{"threadId": threadID, "runId": runID, "protocolVersion": "1.0"})
}

// Usage is the per-model token usage reported in RUN_FINISHED.
type Usage struct {
	Model           string `json:"model"`
	InputTokens     int    `json:"inputTokens"`
	OutputTokens    int    `json:"outputTokens"`
	TotalTokens     int    `json:"totalTokens"`
	ReasoningTokens int    `json:"reasoningTokens"`
	// CachedInputTokens are input tokens served from the provider's prompt cache (0 when not reported).
	CachedInputTokens int `json:"cachedInputTokens"`
}

// RunFinishedEvent builds RUN_FINISHED with outcome success|cancelled.
func RunFinishedEvent(threadID, runID, outcome string, usage []Usage) Event {
	if usage == nil {
		usage = []Usage{}
	}
	return New(RunFinished, map[string]any{
		"threadId": threadID, "runId": runID,
		"outcome": map[string]any{"type": outcome},
		"usage":   usage,
	})
}

// RunErrorEvent builds RUN_ERROR.
func RunErrorEvent(code, message string) Event {
	return New(RunError, map[string]any{"code": code, "message": message})
}

// SteerEvent announces a steer: delivered=true when the model of this run got it,
// false when it was kept in the thread for the next run instead.
func SteerEvent(id, text string, delivered bool) Event {
	return CustomEvent(CustomSteer, map[string]any{"id": id, "text": text, "delivered": delivered})
}

// CustomEvent builds a CUSTOM event.
func CustomEvent(name string, value any) Event {
	return New(Custom, map[string]any{"name": name, "value": value})
}

// SubagentEvent builds the CUSTOM work.subagent event of a tool that runs a
// sub-agent: value carries toolCallId, phase (SubagentStart…SubagentEnd),
// agent, step, tool{name,args,result,isError}, text, model and usage{in,out,cached}.
func SubagentEvent(value map[string]any) Event { return CustomEvent(CustomSubagent, value) }
