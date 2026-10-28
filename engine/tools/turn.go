package tools

// TurnCall is a tool call of the current turn made by an earlier run of the
// same turn (a run resumed after an approval or an ask_user answer).
type TurnCall struct {
	Name      string
	Arguments string
	Result    string
	Failed    bool
}

// TurnObserver is an optional interface of a tool: at the start of a run
// that resumes a turn, the agent gives it the turn's earlier calls, so its
// state (e.g. the plan StopGuard) survives the resume (A-21).
type TurnObserver interface {
	ObserveTurn(calls []TurnCall)
}
