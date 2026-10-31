package agent

import (
	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
)

// toolRunState is the per-run state of the tool-side features.
type toolRunState struct {
	// verify is the turn's deliverable verification state.
	verify verifyState
	// summarizer is the name of the model that summarizes (set with it).
	summarizer string
}

// turnMessages are the messages of the current turn: from the last user
// message that is not a steer (a resumed run continues the turn of the run it
// resumes: approvals and ask_user answers carry no user message).
func turnMessages(msgs []*store.Message) []*store.Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if m := msgs[i]; m.Role == store.RoleUser && !m.IsSteer() {
			return msgs[i:]
		}
	}
	return msgs
}

// resumeTurn restores the turn's state kept by earlier runs of the same turn
// (A-21): deliverable verification, loop and stuck guards.
func (st *runState) resumeTurn(msgs []*store.Message) {
	turn := turnMessages(msgs)
	st.seedVerification(turn)
	st.seedGuards(turn)
}

// UsageOf is the usage a model call reported (zero when none), for counting
// auxiliary calls (title, goal judge) in the run's usage.
func UsageOf(model string, msg *ai.Message) agui.Usage {
	u := agui.Usage{Model: model}
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
		return u
	}
	r := msg.ResponseMeta.Usage
	u.InputTokens, u.OutputTokens, u.TotalTokens = r.PromptTokens, r.CompletionTokens, r.TotalTokens
	u.ReasoningTokens = r.CompletionTokenDetails.ReasoningTokens
	return u
}
