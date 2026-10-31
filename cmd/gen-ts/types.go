package main

import (
	"maps"
	"slices"

	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/features/memory"
	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/product"
)

// roots are the Go types the web app reads or sends, under their TypeScript names. The ones of product are
// listed by product.WireTypes (they are unexported views next to their routes); everything they reference is
// generated too.
func roots() []root {
	rs := []root{
		{"ModelList", client.ModelList{}},
		{"ModelInfo", client.ModelInfo{}},
		// product.threadView (the web's Thread) embeds it.
		{"ThreadBase", client.Thread{}},
		{"Message", client.Message{}},
		{"Author", client.Author{}},
		{"Run", client.Run{}},
		{"RunRequest", client.RunRequest{}},
		{"ResumeRequest", client.ResumeRequest{}},
		{"WorkspaceFile", client.File{}},
		{"Command", client.Command{}},
		{"Attachment", client.Attachment{}},
		{"PlanItem", store.PlanItem{}},
		{"Project", store.Project{}},
		{"Invite", store.Invite{}},
		{"AuditEntry", store.AuditEntry{}},
		{"MemoryServiceStats", memory.Stats{}},
		// The "memory" object of the memory tools' results; the memory pages' entry (product.memoryEntryView) is
		// the same document plus content, proposal and source thread.
		{"MemoryToolEntry", memory.Entry{}},
	}
	wire := product.WireTypes()
	for _, name := range slices.Sorted(maps.Keys(wire)) {
		rs = append(rs, root{name, wire[name]})
	}
	return rs
}

// enums are the string unions of the wire format. The Go fields are plain strings; overrides below type the
// fields that hold one.
func enums() []enum {
	return []enum{
		{"Role", []string{store.RoleOwner, store.RoleMember, store.RoleViewer}},
		{"ThreadKind", []string{store.ThreadChat, store.ThreadSupervisor}},
		{"ThreadStatus", []string{"idle", store.StatusQueued, store.StatusRunning, "waiting_approval", "waiting_input", "failed"}},
		{"RunStatus", []string{store.StatusQueued, store.StatusRunning, store.StatusCompleted, store.StatusFailed, store.StatusCancelled, store.StatusInterrupted}},
		{"RunMode", []string{client.ModeBuild, client.ModePlan}},
		{"SandboxMode", []string{store.SandboxAuto, store.SandboxOn, store.SandboxOff}},
		{"ApprovalDecision", []string{client.DecisionApprove, client.DecisionDeny, client.DecisionRespond}},
		{"MessageRole", []string{store.RoleUser, store.RoleAssistant, store.RoleTool, "system"}},
		{"AuthorKind", []string{store.AuthorUser, store.AuthorSupervisor, store.AuthorSystem}},
		{"FileType", []string{"file", "directory"}},
		{"ConnectorKind", []string{"github", "telegram"}},
		{"MemoryStatus", []string{store.MemoryActive, store.MemoryPending, store.MemoryDeleted}},
		{"FeedType", []string{"thread.created", "thread.updated", "thread.deleted", "run.started", "run.finished", "message.sent",
			"member.changed", "project.changed", "memory.changed"}},
	}
}

// overrides give the TypeScript type of one field, by "<Go type>.<json name>" (the Go type that declares the
// field, so a field promoted from an embedded struct is keyed by the embedded type).
func overrides() map[string]string {
	return map[string]string{
		"client.Message.role":           "MessageRole",
		"client.Author.kind":            "AuthorKind",
		"client.Run.status":             "RunStatus",
		"client.SendOptions.mode":       "RunMode",
		"client.SendOptions.sandbox":    "SandboxMode",
		"client.ResumeRequest.mode":     "RunMode",
		"client.ResumeRequest.decision": "ApprovalDecision",
		"client.File.type":              "FileType",
		// The pending approval is store.Approval.Payload(), a map: its documented keys are typed in extras.
		"client.Thread.pendingApproval": "PendingApproval | null",

		"memory.Entry.status":              "MemoryStatus",
		"memory.Entry.metadata":            "MemoryMetadata",
		"product.memoryEntryView.status":   "MemoryStatus",
		"product.memoryEntryView.metadata": "MemoryMetadata",
		"store.Invite.role":                "Role",
		"product.memberView.role":          "Role",
		"product.projectView.role":         "Role",
		"product.threadView.role":          "Role",
		"product.threadView.kind":          "ThreadKind",
		"product.threadView.status":        "ThreadStatus",
		"product.threadSummary.kind":       "ThreadKind",
		"product.threadSummary.status":     "ThreadStatus",
		"product.connectorView.kind":       "ConnectorKind",
		"product.FeedEvent.type":           "FeedType",
	}
}

// extras are declarations the generator cannot derive from a struct.
func extras() []extra {
	return []extra{{"MemoryMetadata", `/** The free metadata of a memory entry (a map); the keys the pages read are typed. */
export interface MemoryMetadata {
  pinned?: boolean;
  auto?: boolean;
  reason?: string;
  run_id?: string;
  source?: string;
  file?: string;
  /** The previous versions of the entry, newest first. */
  history?: MemoryHistoryItem[];
  [k: string]: unknown;
}

/** One replaced version of a memory entry (metadata.history). */
export interface MemoryHistoryItem {
  title?: string;
  content: string;
  replaced_at?: string;
  thread?: string;
}
`}, {"PendingApproval", `/** The risky tool call waiting for a decision: store.Approval.Payload(), a map (its "id" is the interrupt id of the resume). */
export interface PendingApproval {
  id: string;
  toolName?: string;
  toolInputPreview?: string;
  runId?: string;
  toolCallId?: string;
  [k: string]: unknown;
}
`}}
}
