package delegate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/engine/agent"
)

// Section tells a thread that was started for a factory task what it is for:
// its role instructions (the thread's own when set, else the role's from
// roles; nil is DefaultRoles), the task, the repository and branch, and the
// policy it runs under. Ordinary threads get nothing.
func Section(st factory.Store, roles map[string]Role) agent.PromptSection {
	if roles == nil {
		roles = DefaultRoles()
	}
	return func(ctx context.Context, in agent.Input) string {
		if st == nil || in.Run == nil || in.Run.ThreadID == "" {
			return ""
		}
		ti, err := st.GetThreadInfo(ctx, in.Run.ThreadID)
		if err != nil {
			if !errors.Is(err, factory.ErrNotFound) {
				slog.Warn("delegate: thread info unavailable", "thread_id", in.Run.ThreadID, "error", err)
			}
			return ""
		}
		return renderSection(ti, roles)
	}
}

func renderSection(ti *factory.ThreadInfo, roles map[string]Role) string {
	instr := strings.TrimSpace(ti.Instructions)
	if instr == "" {
		if r, ok := lookup(roles, ti.Role); ok {
			instr = strings.TrimSpace(r.Instructions)
		}
	}
	var meta []string
	if ti.Role != "" {
		meta = append(meta, "role: "+ti.Role)
	}
	if ti.TaskID != "" {
		meta = append(meta, "task: "+ti.TaskID)
	}
	if ti.Repo != "" {
		repo := "repository: " + ti.Repo
		if ti.Branch != "" {
			repo += " (branch " + ti.Branch
			if ti.BaseBranch != "" {
				repo += ", based on " + ti.BaseBranch
			}
			repo += ")"
		}
		meta = append(meta, repo)
	}
	if ti.Policy != "" {
		meta = append(meta, "policy: "+ti.Policy)
	}
	if ti.ReadOnly {
		meta = append(meta, "read-only: you cannot change anything")
	}
	if instr == "" && len(meta) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Your assignment\n")
	if len(meta) > 0 {
		b.WriteString(strings.Join(meta, " | ") + "\n")
	}
	if instr != "" {
		b.WriteString("\n" + instr + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// Roster lists the roles a thread may delegate to. Add it only where the
// delegate tool is offered; the tool's own description carries the same list
// for models that do not read the system prompt closely, so this is the short form.
// The optional argument is the catalogue (default: DefaultRoles).
func Roster(catalogue ...map[string]Role) agent.PromptSection {
	roles := DefaultRoles()
	if len(catalogue) > 0 && catalogue[0] != nil {
		roles = catalogue[0]
	}
	return func(_ context.Context, _ agent.Input) string {
		var b strings.Builder
		b.WriteString("## Delegation\nYou can hand self-contained work to sub-agents with delegate (one) or delegate_many (several at once). Roles:\n")
		for _, r := range sortedRoles(roles) {
			fmt.Fprintf(&b, "- %s", r.Name)
			if r.Description != "" {
				b.WriteString(": " + r.Description)
			}
			b.WriteByte('\n')
		}
		b.WriteString("A sub-agent does not see this conversation; put everything it needs in the task. Verify what it reports before relying on it.")
		return b.String()
	}
}
