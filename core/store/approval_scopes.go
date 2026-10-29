package store

import "strings"

// Approval scopes and expiry.
const (
	// ApprovalScopeOnce approves the paused call only (default).
	ApprovalScopeOnce = "once"
	// ApprovalScopeRun approves the tool kind for the rest of the run chain
	// (the run and its resume runs): "Allow for this task".
	ApprovalScopeRun = "run"

	// ApprovalExpired is the status of a pending approval older than APPROVAL_TTL_HOURS.
	ApprovalExpired = "expired"
	// ApprovalExpiredResult is the tool result of an expired approval.
	ApprovalExpiredResult = "The approval expired."
)

// ToolKind is the kind a scope-run approval covers: the normalized tool name
// (lower case, "-" and "_" removed; approvals.Kind).
func ToolKind(toolName string) string {
	n := strings.ToLower(strings.TrimSpace(toolName))
	return strings.NewReplacer("-", "", "_", "").Replace(n)
}

// ApprovedToolsAfter is the approved tool kinds of a resume run: those of the
// paused run plus the decided approval's kind when approved for the run.
func ApprovedToolsAfter(paused []string, a *Approval) []string {
	out := append([]string{}, paused...)
	if a == nil || a.Decision != DecisionApprove || a.Scope != ApprovalScopeRun {
		return out
	}
	k := ToolKind(a.ToolName)
	for _, t := range out {
		if t == k {
			return out
		}
	}
	return append(out, k)
}
