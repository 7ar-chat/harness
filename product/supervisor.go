package product

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/approvals"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// The supervisor is the user's global agent: a personal thread (kind
// "supervisor") whose agent sees every session the user can see and can look
// after them. It runs without a sandbox (it has no code tools) and with
// interactive approvals: reading is free, but sending to, cancelling,
// approving for and spawning sessions are risky tools the user confirms in
// its chat. Everything it does is recorded as sent by the supervisor on behalf
// of the user, in the messages and in the project audit log.
//
//	GET /api/supervisor                 → the caller's supervisor thread (created on first use), like GET /api/threads/{id}
//	GET /api/supervisor/settings        → {watch, stuckMinutes}
//	PUT /api/supervisor/settings {watch,stuckMinutes}
//
// With watch on, a watcher tells the supervisor when a session waits for the
// user (approval, question), fails, or looks stuck, and the supervisor runs to
// deal with it (one run at a time, batched).

// assistantTitle is the name of the user's assistant (their supervisor thread).
const assistantTitle = "My assistant"

// Supervisor installs the global agent into a harness and a web server.
type Supervisor struct {
	team store.TeamStore
	s    *server
	// StuckAfter is how long a running session may stay silent before the
	// watcher calls it stuck (default 10 minutes).
	StuckAfter time.Duration
	// Cooldown is the least time between two watcher-started runs for a user (default 2 minutes).
	Cooldown time.Duration

	mu      sync.Mutex
	pending map[string][]string // user → attention lines
	last    map[string]time.Time
	stuck   map[string]string // thread id → run id already reported
}

// NewSupervisor returns the supervisor over the team store. Pass Tools() in
// harness.Options.Tools and Feature() in the features, then Options.Supervisor
// to the web server.
func NewSupervisor(team store.TeamStore) *Supervisor {
	approvals.RegisterRisky("session_send", "session_cancel", "session_approve", "session_answer", "session_spawn")
	return &Supervisor{team: team, StuckAfter: 10 * time.Minute, Cooldown: 2 * time.Minute,
		pending: map[string][]string{}, last: map[string]time.Time{}, stuck: map[string]string{}}
}

// Tools is the tool provider: the supervisor tools, offered on supervisor threads only.
func (sv *Supervisor) Tools() tools.Provider { return supTools{sv} }

// Feature adds the supervisor's instructions to the prompt of its thread.
func (sv *Supervisor) Feature() harness.Feature {
	return harness.FeatureFunc("supervisor", func(b *harness.Builder) error {
		b.Section(func(ctx context.Context, in agent.Input) string {
			if sv.isSupervisorThread(ctx, in.Run.ThreadID) {
				return supervisorPrompt
			}
			return ""
		})
		return nil
	})
}

const supervisorPrompt = `# You are the user's assistant

You are the user's personal assistant (the supervisor of their coding agents). You do not write code or touch repositories yourself. You watch the user's coding agents ("sessions") across their projects and help them: report what is running, what waits for them, what failed or looks stuck, and act on their behalf when asked.

- Start by reading: list_sessions gives every session the user can see with its status; read_session shows the latest messages, the pending approval or question, and who wrote what.
- Messages in sessions are written by several people and agents. Always say who asked for what.
- Acting tools (session_send, session_approve, session_answer, session_cancel, session_spawn) are confirmed by the user before they run. Say plainly what you intend and why. Never approve a command you have not read; for a pending approval show the tool and its input first. Deny or ask the user when it looks destructive (deleting, force-pushing, touching credentials).
- When a message starts with "[watcher]", the system noticed sessions needing attention: read them, summarize in a few lines, and propose or take the next step. Do not loop: one pass, then wait for the user.
- Be brief. Use short lists with session titles and projects, not ids, unless the user asks.`

func (sv *Supervisor) isSupervisorThread(ctx context.Context, threadID string) bool {
	if sv.team == nil {
		return false
	}
	m, err := sv.team.GetThreadMeta(ctx, threadID)
	return err == nil && m.Kind == store.ThreadSupervisor
}

// bind connects the supervisor to its server and starts the watcher.
func (sv *Supervisor) bind(s *server) {
	sv.s = s
	go sv.watch(context.Background())
}

// ---- the thread and its settings ----

func (s *server) routeSupervisor() {
	s.Route("GET /supervisor", s.getSupervisor)
	s.Route("GET /supervisor/settings", s.getSupervisorSettings)
	s.Route("PUT /supervisor/settings", s.putSupervisorSettings)
}

// supervisorThread returns the user's supervisor thread id, creating it.
func (s *server) supervisorThread(ctx context.Context, login string) (string, error) {
	if m, err := s.team.SupervisorThread(ctx, login); err == nil {
		if t, err := s.H.Store.GetThread(ctx, m.Owner, m.ThreadID); err == nil {
			if t.Title == "Global agent" { // the assistant's former name
				title := assistantTitle
				_, _ = s.H.Store.UpdateThread(ctx, m.Owner, m.ThreadID, store.ThreadPatch{Title: &title})
			}
			return m.ThreadID, nil
		}
	}
	th, err := s.H.Store.CreateThread(ctx, login, assistantTitle)
	if err != nil {
		return "", err
	}
	if err := s.team.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: th.ID, Owner: login, CreatedBy: login, Kind: store.ThreadSupervisor}); err != nil {
		return "", err
	}
	return th.ID, nil
}

func (s *server) getSupervisor(w http.ResponseWriter, r *http.Request, c Caller) {
	id, err := s.supervisorThread(r.Context(), c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	r.SetPathValue("id", id)
	s.getThreadTeam(w, r, Caller{Login: c.Actor, Actor: c.Actor})
}

type supervisorSettings struct {
	Watch        bool `json:"watch"`
	StuckMinutes int  `json:"stuckMinutes"`
}

func (sv *Supervisor) settings(ctx context.Context, login string) supervisorSettings {
	out := supervisorSettings{StuckMinutes: int(sv.StuckAfter / time.Minute)}
	prefs, err := sv.team.UserPrefs(ctx, login)
	if err != nil {
		return out
	}
	if m, ok := prefs["supervisor"].(map[string]any); ok {
		out.Watch, _ = m["watch"].(bool)
		switch v := m["stuckMinutes"].(type) { // float64 after a JSON round trip, int straight from memory
		case float64:
			if v >= 1 {
				out.StuckMinutes = int(v)
			}
		case int:
			if v >= 1 {
				out.StuckMinutes = v
			}
		}
	}
	return out
}

func (s *server) getSupervisorSettings(w http.ResponseWriter, r *http.Request, c Caller) {
	if s.sup == nil {
		writeError(w, http.StatusNotImplemented, "not_supported", "The supervisor is not enabled on this server.")
		return
	}
	writeJSON(w, http.StatusOK, s.sup.settings(r.Context(), c.Actor))
}

func (s *server) putSupervisorSettings(w http.ResponseWriter, r *http.Request, c Caller) {
	if s.sup == nil {
		writeError(w, http.StatusNotImplemented, "not_supported", "The supervisor is not enabled on this server.")
		return
	}
	var body supervisorSettings
	if !decode(w, r, &body) {
		return
	}
	if body.StuckMinutes < 1 || body.StuckMinutes > 24*60 {
		body.StuckMinutes = int(s.sup.StuckAfter / time.Minute)
	}
	prefs, err := s.team.UserPrefs(r.Context(), c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	prefs["supervisor"] = map[string]any{"watch": body.Watch, "stuckMinutes": body.StuckMinutes}
	if err := s.team.SetUserPrefs(r.Context(), c.Actor, prefs); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// forceSupervisorRun applies the supervisor thread's run settings: no sandbox, approvals on.
func (s *server) forceSupervisorRun(r *http.Request, threadID string, o *client.SendOptions) {
	if s.sup != nil && s.sup.isSupervisorThread(r.Context(), threadID) {
		o.Sandbox, o.Interactive = "off", true
	}
}

// ---- the tools ----

type supTools struct{ sv *Supervisor }

func (supTools) Name() string { return "supervisor" }

func (p supTools) Tools(ctx context.Context, rc tools.RunContext) ([]tool.BaseTool, func(), error) {
	sv := p.sv
	if sv.s == nil || !sv.isSupervisorThread(ctx, rc.ThreadID) {
		return nil, func() {}, nil
	}
	me := rc.Login // the supervisor thread is personal: its owner is the user
	var out []tool.BaseTool
	add := func(t tool.BaseTool, err error) {
		if err == nil {
			out = append(out, t)
		}
	}
	add(utils.InferTool("list_sessions", "List the sessions (agent conversations) the user can see, with project, creator, status (idle, queued, running, waiting_approval, waiting_input, failed) and what they wait for. Optionally only one project or status.",
		func(ctx context.Context, in listArgs) (any, error) { return sv.listSessions(ctx, me, in) }))
	add(utils.InferTool("read_session", "Read a session: its status, the latest messages with their authors, and any pending approval or question (with the ids to answer them).",
		func(ctx context.Context, in readArgs) (any, error) { return sv.readSession(ctx, me, in) }))
	add(utils.InferTool("session_send", "Send a message to a session as the supervisor on the user's behalf: steers it if it is running, otherwise starts a new run.",
		func(ctx context.Context, in sendArgs) (any, error) { return sv.sendToSession(ctx, me, in) }))
	add(utils.InferTool("session_approve", "Approve or deny the tool call a session is waiting on. always=true approves that tool for the rest of the run.",
		func(ctx context.Context, in approveArgs) (any, error) { return sv.approveSession(ctx, me, in) }))
	add(utils.InferTool("session_answer", "Answer the question a session asked the user (ask_user).",
		func(ctx context.Context, in answerArgs) (any, error) { return sv.answerSession(ctx, me, in) }))
	add(utils.InferTool("session_cancel", "Stop the running run of a session.",
		func(ctx context.Context, in cancelArgs) (any, error) { return sv.cancelSession(ctx, me, in) }))
	add(utils.InferTool("session_spawn", "Start a new session in a project with a task for its agent.",
		func(ctx context.Context, in spawnArgs) (any, error) { return sv.spawnSession(ctx, me, in) }))
	return out, func() {}, nil
}

type listArgs struct {
	ProjectID string `json:"project_id,omitempty" jsonschema:"description=Only this project"`
	Status    string `json:"status,omitempty" jsonschema:"description=Only sessions with this status"`
}

type readArgs struct {
	ThreadID string `json:"thread_id" jsonschema:"description=The session id"`
	Last     int    `json:"last_messages,omitempty" jsonschema:"description=How many recent messages to return (default 12, max 50)"`
}

type sendArgs struct {
	ThreadID string `json:"thread_id" jsonschema:"description=The session id"`
	Message  string `json:"message" jsonschema:"description=What to tell the session's agent"`
}

type approveArgs struct {
	ThreadID string `json:"thread_id"`
	Decision string `json:"decision" jsonschema:"enum=approve,enum=deny"`
	Always   bool   `json:"always,omitempty"`
}

type answerArgs struct {
	ThreadID string `json:"thread_id"`
	Text     string `json:"text"`
}

type cancelArgs struct {
	ThreadID string `json:"thread_id"`
}

type spawnArgs struct {
	ProjectID string `json:"project_id" jsonschema:"description=The project to start the session in"`
	Task      string `json:"task" jsonschema:"description=The first message to the new session's agent"`
	Title     string `json:"title,omitempty"`
}

var errSupervisorUnbound = errors.New("the supervisor is not connected to a server")

func (sv *Supervisor) listSessions(ctx context.Context, me string, in listArgs) (any, error) {
	var all []threadSummary
	if in.ProjectID != "" {
		if _, err := sv.s.requireProject(ctx, in.ProjectID, me, store.RoleViewer); err != nil {
			return nil, err
		}
		ths, err := sv.s.projectThreads(ctx, in.ProjectID)
		if err != nil {
			return nil, err
		}
		all = ths
	} else {
		doc, err := sv.s.overviewFor(ctx, me)
		if err != nil {
			return nil, err
		}
		all = append(all, doc.Personal...)
		for _, p := range doc.Projects {
			all = append(all, p.Threads...)
		}
	}
	names := sv.s.projectNames(ctx, me)
	type row struct {
		ThreadID string `json:"thread_id"`
		Title    string `json:"title"`
		Project  string `json:"project,omitempty"`
		By       string `json:"created_by,omitempty"`
		Status   string `json:"status"`
		Waiting  string `json:"waiting_for,omitempty"`
		Updated  string `json:"updated"`
	}
	var rows []row
	for _, t := range all {
		if t.Kind == store.ThreadSupervisor || (in.Status != "" && t.Status != in.Status) {
			continue
		}
		wf := ""
		switch {
		case t.Tool != "":
			wf = "approval for tool " + t.Tool
		case t.Question != "":
			wf = "answer to: " + t.Question
		}
		rows = append(rows, row{t.ID, t.Title, names[t.ProjectID], t.CreatedBy, t.Status, wf, t.UpdatedAt.UTC().Format(time.RFC3339)})
	}
	sort.SliceStable(rows, func(i, j int) bool { return attentionRank(rows[i].Status) < attentionRank(rows[j].Status) })
	return map[string]any{"sessions": rows, "count": len(rows)}, nil
}

func attentionRank(status string) int {
	switch status {
	case "waiting_approval", "waiting_input":
		return 0
	case "failed":
		return 1
	case "running", "queued":
		return 2
	}
	return 3
}

func (sv *Supervisor) readSession(ctx context.Context, me string, in readArgs) (any, error) {
	c, err := sv.s.access(ctx, me, in.ThreadID, store.RoleViewer)
	if err != nil {
		return nil, err
	}
	cl := sv.client(c, "")
	th, err := cl.GetThread(ctx, in.ThreadID)
	if err != nil {
		return nil, err
	}
	sv.s.withAuthors(ctx, th)
	n := in.Last
	if n <= 0 {
		n = 12
	}
	n = min(n, 50)
	msgs := th.Messages
	if len(msgs) > n {
		msgs = msgs[len(msgs)-n:]
	}
	type msg struct {
		Role    string   `json:"role"`
		Author  string   `json:"author,omitempty"`
		Content string   `json:"content,omitempty"`
		Tools   []string `json:"tool_calls,omitempty"`
		Error   string   `json:"error,omitempty"`
	}
	out := make([]msg, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == store.RoleReasoning {
			continue
		}
		mm := msg{Role: m.Role, Content: clip(m.Content, 1500), Error: clip(m.Error, 300)}
		if m.Role == store.RoleTool {
			mm.Content = clip(m.Content, 400)
		}
		if a := m.Author; a != nil {
			mm.Author = a.Name
			if mm.Author == "" {
				mm.Author = a.Login
			}
			if a.Kind == store.AuthorSupervisor {
				mm.Author = "supervisor (for " + a.OnBehalfOf + ")"
			}
		}
		for _, tc := range m.ToolCalls {
			mm.Tools = append(mm.Tools, tc.Function.Name+"("+clip(tc.Function.Arguments, 160)+")")
		}
		out = append(out, mm)
	}
	res := map[string]any{"thread_id": th.ID, "title": th.Title, "messages": out}
	if th.ActiveRun != nil {
		res["active_run"] = th.ActiveRun
	}
	if th.PendingApproval != nil {
		res["pending_approval"] = th.PendingApproval
	}
	if th.PendingQuestion != nil {
		res["pending_question"] = th.PendingQuestion
	}
	return res, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// client is a client acting on the thread of c as the supervisor on behalf of the user.
func (sv *Supervisor) client(c Caller, onBehalfOf string) client.Client {
	if onBehalfOf == "" {
		onBehalfOf = c.Actor
	}
	return client.InProcess(sv.s.H, c.Login, client.WithModels(sv.s.O.Models, sv.s.O.DefaultModel),
		client.WithAuthor(harness.Author{Kind: store.AuthorSupervisor, OnBehalfOf: onBehalfOf}))
}

// audit records a supervisor action.
func (sv *Supervisor) audit(c Caller, me, threadID, action string, detail map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = sv.team.Audit(ctx, store.AuditEntry{ProjectID: c.ProjectID, ThreadID: threadID, Actor: "supervisor", OnBehalfOf: me, Action: action, Detail: detail})
}

func (sv *Supervisor) sendToSession(ctx context.Context, me string, in sendArgs) (any, error) {
	if strings.TrimSpace(in.Message) == "" {
		return nil, errors.New("message is required")
	}
	c, err := sv.s.access(ctx, me, in.ThreadID, store.RoleMember)
	if err != nil {
		return nil, err
	}
	cl := sv.client(c, me)
	th, err := cl.GetThread(ctx, in.ThreadID)
	if err != nil {
		return nil, err
	}
	sc := Caller{Login: c.Login, Actor: "supervisor", ProjectID: c.ProjectID}
	if th.ActiveRun != nil {
		if err := cl.Steer(ctx, th.ActiveRun.ID, in.Message); err != nil {
			return nil, err
		}
		sv.audit(c, me, in.ThreadID, "supervisor.steer", nil)
		sv.s.note(sc, in.ThreadID, "supervisor.steer", nil, FeedEvent{Type: "message.sent", RunID: th.ActiveRun.ID})
		return map[string]any{"ok": true, "delivered_as": "steer"}, nil
	}
	if th.PendingApproval != nil || th.PendingQuestion != nil {
		return nil, errors.New("the session is waiting for an approval or an answer: use session_approve or session_answer")
	}
	run, err := cl.Send(ctx, in.ThreadID, in.Message, client.SendOptions{})
	if err != nil {
		return nil, err
	}
	sv.audit(c, me, in.ThreadID, "supervisor.send", nil)
	sv.s.AfterRun(sc, run, "supervisor.send", nil)
	return map[string]any{"ok": true, "delivered_as": "new_run", "run_id": run.ID}, nil
}

func (sv *Supervisor) approveSession(ctx context.Context, me string, in approveArgs) (any, error) {
	if in.Decision != client.DecisionApprove && in.Decision != client.DecisionDeny {
		return nil, errors.New("decision must be approve or deny")
	}
	c, err := sv.s.access(ctx, me, in.ThreadID, store.RoleMember)
	if err != nil {
		return nil, err
	}
	cl := sv.client(c, me)
	th, err := cl.GetThread(ctx, in.ThreadID)
	if err != nil {
		return nil, err
	}
	if th.PendingApproval == nil {
		return nil, errors.New("the session has no pending approval")
	}
	id, _ := th.PendingApproval["id"].(string)
	tool, _ := th.PendingApproval["toolName"].(string)
	run, err := cl.Resume(ctx, in.ThreadID, client.ResumeRequest{InterruptID: id, Reason: client.ReasonToolApproval, Decision: in.Decision, Always: in.Always, Interactive: true})
	if err != nil {
		return nil, err
	}
	sc := Caller{Login: c.Login, Actor: "supervisor", ProjectID: c.ProjectID}
	d := map[string]any{"decision": in.Decision, "tool": tool, "always": in.Always}
	sv.audit(c, me, in.ThreadID, "supervisor.approval", d)
	sv.s.AfterRun(sc, run, "supervisor.approval", d)
	return map[string]any{"ok": true, "decision": in.Decision, "tool": tool, "run_id": run.ID}, nil
}

func (sv *Supervisor) answerSession(ctx context.Context, me string, in answerArgs) (any, error) {
	c, err := sv.s.access(ctx, me, in.ThreadID, store.RoleMember)
	if err != nil {
		return nil, err
	}
	cl := sv.client(c, me)
	th, err := cl.GetThread(ctx, in.ThreadID)
	if err != nil {
		return nil, err
	}
	if th.PendingQuestion == nil {
		return nil, errors.New("the session has no pending question")
	}
	run, err := cl.Resume(ctx, in.ThreadID, client.ResumeRequest{InterruptID: th.PendingQuestion.InterruptID, Reason: "ask_user", Text: in.Text, Interactive: true})
	if err != nil {
		return nil, err
	}
	sc := Caller{Login: c.Login, Actor: "supervisor", ProjectID: c.ProjectID}
	sv.audit(c, me, in.ThreadID, "supervisor.answer", nil)
	sv.s.AfterRun(sc, run, "supervisor.answer", nil)
	return map[string]any{"ok": true, "run_id": run.ID}, nil
}

func (sv *Supervisor) cancelSession(ctx context.Context, me string, in cancelArgs) (any, error) {
	c, err := sv.s.access(ctx, me, in.ThreadID, store.RoleMember)
	if err != nil {
		return nil, err
	}
	cl := sv.client(c, me)
	th, err := cl.GetThread(ctx, in.ThreadID)
	if err != nil {
		return nil, err
	}
	if th.ActiveRun == nil {
		return nil, errors.New("the session has no running run")
	}
	if err := cl.Cancel(ctx, th.ActiveRun.ID); err != nil {
		return nil, err
	}
	sv.audit(c, me, in.ThreadID, "supervisor.cancel", nil)
	sv.s.note(Caller{Login: c.Login, Actor: "supervisor", ProjectID: c.ProjectID}, in.ThreadID, "supervisor.cancel", nil, FeedEvent{Type: "message.sent", RunID: th.ActiveRun.ID})
	return map[string]any{"ok": true}, nil
}

func (sv *Supervisor) spawnSession(ctx context.Context, me string, in spawnArgs) (any, error) {
	if strings.TrimSpace(in.Task) == "" {
		return nil, errors.New("task is required")
	}
	if _, err := sv.s.requireProject(ctx, in.ProjectID, me, store.RoleMember); err != nil {
		return nil, err
	}
	th, err := sv.s.H.Store.CreateThread(ctx, me, in.Title)
	if err != nil {
		return nil, err
	}
	if err := sv.team.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: th.ID, ProjectID: in.ProjectID, Owner: me, CreatedBy: me, Kind: store.ThreadChat}); err != nil {
		_, _ = sv.s.H.Store.DeleteThread(ctx, me, th.ID)
		return nil, err
	}
	c := Caller{Login: me, Actor: me, ProjectID: in.ProjectID}
	run, err := sv.client(c, me).Send(ctx, th.ID, in.Task, client.SendOptions{})
	if err != nil {
		return nil, err
	}
	sc := Caller{Login: me, Actor: "supervisor", ProjectID: in.ProjectID}
	sv.audit(c, me, th.ID, "supervisor.spawn", nil)
	sv.s.note(sc, th.ID, "thread.create", nil, FeedEvent{Type: "thread.created", Title: th.Title})
	sv.s.AfterRun(sc, run, "supervisor.spawn", nil)
	return map[string]any{"ok": true, "thread_id": th.ID, "run_id": run.ID}, nil
}

// ---- helpers on the server ----

func (s *server) overviewFor(ctx context.Context, actor string) (*overviewDoc, error) {
	personal, err := s.personalThreads(ctx, actor, 50)
	if err != nil {
		return nil, err
	}
	list, err := s.team.ListProjects(ctx, actor)
	if err != nil {
		return nil, err
	}
	doc := &overviewDoc{Personal: nonNil(personal), Projects: []overviewProject{}}
	for _, p := range list {
		ths, err := s.projectThreads(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		doc.Projects = append(doc.Projects, overviewProject{projectView{p.Project, p.Role}, nonNil(ths)})
	}
	return doc, nil
}

func (s *server) projectNames(ctx context.Context, actor string) map[string]string {
	out := map[string]string{}
	if list, err := s.team.ListProjects(ctx, actor); err == nil {
		for _, p := range list {
			out[p.ID] = p.Name
		}
	}
	return out
}

// ---- the watcher ----

// watch turns feed events (and silence) into attention for the users who turned the watcher on.
// It sees the events of this process only: with a FeedBus every replica runs a watcher and each
// event is produced on exactly one replica, so each is handled once.
func (sv *Supervisor) watch(ctx context.Context) {
	sub, done := sv.s.hub.subscribe(localOnly)
	defer done()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	scan := time.NewTicker(time.Minute)
	defer scan.Stop()
	for {
		select {
		case ev := <-sub.ch:
			sv.onEvent(ctx, ev)
		case <-tick.C:
			sv.flush(ctx)
		case <-scan.C:
			sv.scanStuck(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// attentionEvent says whether a feed event needs the user, and describes it.
func attentionEvent(ev FeedEvent) (string, bool) {
	if ev.Type != "run.finished" {
		return "", false
	}
	switch ev.Status {
	case "waiting_approval":
		return "waits for an approval", true
	case "waiting_input":
		return "asks a question", true
	case "failed", "interrupted":
		return "failed", true
	}
	return "", false
}

// recipients are the users who should hear about an event on a thread.
func (sv *Supervisor) recipients(ctx context.Context, projectID, owner string) []string {
	if projectID == "" {
		return []string{owner}
	}
	members, err := sv.team.ListMembers(ctx, projectID)
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range members {
		if store.RoleAtLeast(m.Role, store.RoleMember) {
			out = append(out, m.Login)
		}
	}
	return out
}

func (sv *Supervisor) onEvent(ctx context.Context, ev FeedEvent) {
	what, ok := attentionEvent(ev)
	if !ok || ev.ThreadID == "" || sv.isSupervisorThread(ctx, ev.ThreadID) {
		return
	}
	sv.queue(ctx, ev.ProjectID, ev.owner, ev.ThreadID, what)
}

func (sv *Supervisor) queue(ctx context.Context, projectID, owner, threadID, what string) {
	title := threadID
	if t, err := sv.s.H.Store.GetThread(ctx, owner, threadID); err == nil && t.Title != "" {
		title = t.Title
	}
	line := fmt.Sprintf("session %q (thread_id %s) %s", title, threadID, what)
	for _, u := range sv.recipients(ctx, projectID, owner) {
		if !sv.settings(ctx, u).Watch {
			continue
		}
		sv.mu.Lock()
		sv.pending[u] = append(sv.pending[u], line)
		sv.mu.Unlock()
	}
}

// scanStuck reports running sessions that went quiet.
func (sv *Supervisor) scanStuck(ctx context.Context) {
	if sv.team == nil {
		return
	}
	// Only sessions of users watching matter; walk their projects.
	seen := map[string]bool{}
	users, _ := sv.watchers(ctx)
	for _, u := range users {
		after := time.Duration(sv.settings(ctx, u).StuckMinutes) * time.Minute
		doc, err := sv.s.overviewFor(ctx, u)
		if err != nil {
			continue
		}
		var all []threadSummary
		all = append(all, doc.Personal...)
		for _, p := range doc.Projects {
			all = append(all, p.Threads...)
		}
		for _, t := range all {
			if t.Status != "running" || time.Since(t.UpdatedAt) < after || seen[t.ID+t.RunID] {
				continue
			}
			seen[t.ID+t.RunID] = true
			sv.mu.Lock()
			already := sv.stuck[t.ID] == t.RunID
			sv.stuck[t.ID] = t.RunID
			sv.mu.Unlock()
			// Every replica scans; the bus lets one of them report a stuck run.
			if already || !sv.s.hub.claim("stuck:"+t.RunID, 24*time.Hour) {
				continue
			}
			sv.queue(ctx, t.ProjectID, t.CreatedBy, t.ID, fmt.Sprintf("has been running without activity for over %d minutes", int(after/time.Minute)))
		}
	}
}

// watchers lists the users with the watcher on.
func (sv *Supervisor) watchers(ctx context.Context) ([]string, error) {
	users, err := sv.team.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, u := range users {
		if sv.settings(ctx, u.Login).Watch {
			out = append(out, u.Login)
		}
	}
	return out, nil
}

// flush starts a supervisor run for each user with queued attention.
func (sv *Supervisor) flush(ctx context.Context) {
	sv.mu.Lock()
	users := make([]string, 0, len(sv.pending))
	for u, l := range sv.pending {
		if len(l) > 0 {
			users = append(users, u)
		}
	}
	sv.mu.Unlock()
	for _, u := range users {
		sv.mu.Lock()
		wait := time.Since(sv.last[u]) < sv.Cooldown
		lines := append([]string(nil), sv.pending[u]...)
		sv.mu.Unlock()
		if wait {
			continue
		}
		id, err := sv.s.supervisorThread(ctx, u)
		if err != nil {
			continue
		}
		msg := "[watcher] These sessions need attention:\n- " + strings.Join(dedupe(lines), "\n- ")
		cl := client.InProcess(sv.s.H, u, client.WithModels(sv.s.O.Models, sv.s.O.DefaultModel),
			client.WithAuthor(harness.Author{Kind: store.AuthorSystem, OnBehalfOf: u}))
		run, err := cl.Send(ctx, id, msg, client.SendOptions{Sandbox: "off", Interactive: true})
		if err != nil {
			continue // busy or failing: try again at the next tick
		}
		sv.mu.Lock()
		delete(sv.pending, u)
		sv.last[u] = time.Now()
		sv.mu.Unlock()
		if sv.s.tg != nil {
			sv.s.tg.markRun(run.ID)
		}
		sv.s.AfterRun(Caller{Login: u, Actor: u}, run, "supervisor.watch", nil)
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
