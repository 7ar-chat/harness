package product

import (
	"context"
	"net/http"
)

// GET /api/me/activity-summary: what the caller has done, counted from the audit log, so the getting-started checklist
// ticks for anyone who ever did the thing (in any browser, on any day). It reads only the caller's own entries:
// the audit rows of the projects they belong to and their personal ones, filtered to them as the actor.
//
//	approvalsDecided: tool approvals answered (run.resume), tasks resumed after they asked for a person (task.resume),
//	                  workflow gates approved or sent back
//	tasksHandedOff:   tasks created (task.create)
//	sessionsStarted:  sessions created (thread.create)
//	membersInvited:   invitations sent (member.invite)

// ActivitySummary is the answer.
type ActivitySummary struct {
	ApprovalsDecided int `json:"approvalsDecided"`
	TasksHandedOff   int `json:"tasksHandedOff"`
	SessionsStarted  int `json:"sessionsStarted"`
	MembersInvited   int `json:"membersInvited"`
}

const activityAuditLimit = 5000

func (s *server) routeActivity() {
	if s.team == nil {
		return
	}
	s.Route("GET /me/activity-summary", s.myActivitySummary)
}

func (s *server) myActivitySummary(w http.ResponseWriter, r *http.Request, c Caller) {
	v, err := s.activitySummary(r.Context(), c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, v)
}

func (s *server) activitySummary(ctx context.Context, login string) (ActivitySummary, error) {
	projects, err := s.team.ListProjects(ctx, login)
	if err != nil {
		return ActivitySummary{}, err
	}
	ids := []string{""} // "" holds the entries that belong to no project
	for _, p := range projects {
		ids = append(ids, p.ID)
	}
	var out ActivitySummary
	for _, id := range ids {
		entries, err := s.team.ListAudit(ctx, id, activityAuditLimit)
		if err != nil {
			return ActivitySummary{}, err
		}
		for _, e := range entries {
			if e.Actor != login {
				continue
			}
			switch e.Action {
			case "run.resume":
				if dec, _ := e.Detail["decision"].(string); dec == "approve" || dec == "deny" || dec == "allow" {
					out.ApprovalsDecided++
				}
			case "task.resume", "workflow.gate.approve", "workflow.gate.send-back":
				out.ApprovalsDecided++
			case "task.create":
				out.TasksHandedOff++
			case "thread.create":
				out.SessionsStarted++
			case "member.invite":
				out.MembersInvited++
			}
		}
	}
	return out, nil
}
