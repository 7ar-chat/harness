package tools

// ReadOnly is an optional interface of a tool: a tool whose ReadOnly reports
// true has no side effects (it reads, searches or loads text) and may run in
// parallel with the other read-only calls of a step (harness v3 §2).
type ReadOnly interface {
	ReadOnly() bool
}
