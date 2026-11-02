package agui

// Harness v2 event types and names.
const (
	StateSnapshot = "STATE_SNAPSHOT"

	CustomCompaction = "work.compaction"
	CustomTitle      = "work.title"
	CustomGoal       = "work.goal"
	CustomMemory     = "work.memory"
	CustomApproval   = "work.approval"
)

// Interrupt is one AG-UI 1.0 interrupt of a RUN_FINISHED{outcome: interrupt}.
type Interrupt struct {
	ID             string         `json:"id"`
	Reason         string         `json:"reason"`
	Message        string         `json:"message"`
	ToolCallID     string         `json:"toolCallId"`
	ResponseSchema map[string]any `json:"responseSchema"`
	// Approval is the pending approval record (reason tool_approval).
	Approval map[string]any `json:"approval,omitempty"`
}

// RunInterruptedEvent builds RUN_FINISHED with outcome interrupt (the run waits for the user).
func RunInterruptedEvent(threadID, runID string, interrupts []Interrupt, usage []Usage) Event {
	ev := RunFinishedEvent(threadID, runID, "interrupt", usage)
	ev["outcome"] = map[string]any{"type": "interrupt", "interrupts": interrupts}
	return ev
}

// StateSnapshotEvent builds STATE_SNAPSHOT.
func StateSnapshotEvent(snapshot map[string]any) Event {
	return New(StateSnapshot, map[string]any{"snapshot": snapshot})
}
