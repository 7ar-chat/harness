package product

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/notify"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/core/workflows"
)

// Notifications: one place per project and one per person that says what is told to whom, where. The engine is
// core/notify; this file connects it to the product: the senders (Telegram, Slack, email, and the project's webhook,
// WeCom and Discord channels), the events (tasks, runs, budgets, schedules, workflows, members) and the HTTP routes.
//
// A project's owner adds channels (a webhook, a WeCom group bot, a Discord webhook) and chooses, per event, which of
// them hear it and whether the person it concerns is told as well. Each person chooses where their own items reach
// them (the app, Telegram, Slack, email, their own webhook), quiet hours and an hourly digest. Addresses and secrets
// are sealed with the server's secret key and never returned by any route or written to a log.
//
//	GET    /api/projects/{pid}/notifications                  → NotificationsView   (any member)
//	PUT    /api/projects/{pid}/notifications {events,dedupeMinutes,digest} → NotificationsView   (owner)
//	GET    /api/projects/{pid}/notification-channels          → [NotificationChannel]   (owner)
//	POST   /api/projects/{pid}/notification-channels {kind,name,url,secret?} → 201 NotificationChannel   (owner)
//	PUT    /api/projects/{pid}/notification-channels/{id} {name?,enabled?,url?,secret?,clearSecret?} → NotificationChannel   (owner)
//	DELETE /api/projects/{pid}/notification-channels/{id}     → 204   (owner)
//	POST   /api/projects/{pid}/notification-channels/{id}/test → NotificationTestResult   (owner)
//	GET    /api/projects/{pid}/notifications/log              → [NotificationLogEntry]   (owner; the last 100)
//	GET    /api/me/notifications                              → MyNotifications
//	PUT    /api/me/notifications {events,quietStart,quietEnd,timezone,digest,webhook?} → MyNotifications
//	POST   /api/me/notifications/test                         → NotificationTestResults   (to every channel set up for the caller)
//
// Without Options.Notifications the rules live in memory (the defaults still apply); without a SecretKey channels
// cannot be added (nothing could seal their address).

// NotificationsOptions configures the notification engine.
type NotificationsOptions struct {
	// Store holds rules, channels, preferences and the log (nil: in memory, lost on restart).
	Store notify.Store
	// AllowPrivate lets webhooks reach private and loopback addresses (an intranet chat server). The operator opts in
	// (HARNESS_NOTIFY_ALLOW_PRIVATE=1); a user cannot. Off: the guarded transport of core/netguard refuses them.
	AllowPrivate bool
	// Sleep replaces the wait between retries (tests).
	Sleep func(time.Duration)
}

const (
	maxNotificationChannels = 10
	notificationsDomain     = "harness-notifications:"
	notifyBudgetWindow      = 32 * 24 * time.Hour
)

// NotificationChannel is a project channel as the pages see it: never the address or the secret.
type NotificationChannel struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Hint      string    `json:"hint,omitempty"`
	HasSecret bool      `json:"hasSecret"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
}

// NotificationsView answers GET and PUT of a project's rules.
type NotificationsView struct {
	Events   []notify.EventInfo    `json:"events"`
	Rules    notify.Rules          `json:"rules"`
	Channels []NotificationChannel `json:"channels"`
	// CanEdit: the caller is an owner (may change rules and channels).
	CanEdit bool `json:"canEdit"`
	// CanSecure: the server can seal addresses, so channels can be added.
	CanSecure bool `json:"canSecure"`
}

// NotificationTestResult is the outcome of a test message.
type NotificationTestResult struct {
	Channel string `json:"channel"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
}

// NotificationTestResults answers the personal test button.
type NotificationTestResults struct {
	Results []NotificationTestResult `json:"results"`
}

// NotificationWebhook is what a person may see of their own webhook.
type NotificationWebhook struct {
	Hint      string `json:"hint,omitempty"`
	HasSecret bool   `json:"hasSecret"`
	Set       bool   `json:"set"`
}

// MyNotifications answers GET and PUT of a person's preferences.
type MyNotifications struct {
	Events []notify.EventInfo `json:"events"`
	// Effective is where each event reaches the person now (their choice, else the defaults).
	Effective map[string][]string `json:"effective"`
	// Available are the channels the person can choose: the app always, the others once linked or set up.
	Available []string `json:"available"`
	// Email is the address mail goes to (the profile's), empty when there is none.
	Email      string              `json:"email,omitempty"`
	QuietStart string              `json:"quietStart"`
	QuietEnd   string              `json:"quietEnd"`
	Timezone   string              `json:"timezone"`
	Digest     bool                `json:"digest"`
	Webhook    NotificationWebhook `json:"webhook"`
	// Recent is the last of the person's deliveries, to answer "why did I not get a message?".
	Recent []notify.LogEntry `json:"recent"`
}

type notifier struct {
	s            *server
	st           notify.Store
	d            *notify.Dispatcher
	box          *secretBox
	allowPrivate bool

	mu       sync.Mutex
	lastTest map[string]time.Time
}

// setupNotifications builds the engine; called by Install once the server has its stores.
func (s *server) setupNotifications() error {
	o := s.opt.Notifications
	if o == nil {
		o = &NotificationsOptions{}
	}
	n := &notifier{s: s, st: o.Store, allowPrivate: o.AllowPrivate, lastTest: map[string]time.Time{}}
	if n.st == nil {
		n.st = notify.NewMemStore(nil)
	}
	if len(s.opt.SecretKey) > 0 {
		var err error
		if n.box, err = newSecretBoxFor(notificationsDomain, s.opt.SecretKey); err != nil {
			return err
		}
	}
	client := notify.NewHTTPClient(o.AllowPrivate)
	mk := func(kind string) notify.Sender {
		return &notify.HTTPSender{Kind: kind, Client: client, Sleep: o.Sleep}
	}
	n.d = &notify.Dispatcher{
		Store: n.st,
		Senders: map[string]notify.Sender{
			notify.KindWebhook:      mk(notify.KindWebhook),
			notify.KindWeCom:        mk(notify.KindWeCom),
			notify.KindDiscord:      mk(notify.KindDiscord),
			string(notify.Telegram): notify.SenderFunc(n.sendTelegram),
			string(notify.Slack):    notify.SenderFunc(n.sendSlack),
			string(notify.Email):    notify.SenderFunc(n.sendEmail),
		},
		Open: func(sealed []byte) (string, string, error) {
			if n.box == nil {
				return "", "", errors.New("no secret key")
			}
			plain, err := n.box.open(sealed)
			if err != nil {
				return "", "", err
			}
			return notify.OpenPair(plain)
		},
		Linked: n.linked,
		Redact: func(t string) string { return harness.RedactSecrets(t) },
	}
	s.nf = n
	s.hub.tap = s.feedNotify
	go n.flushLoop()
	return nil
}

func (n *notifier) flushLoop() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		n.d.Flush(ctx)
		cancel()
	}
}

// linked lists the personal channels the person has set up.
func (n *notifier) linked(ctx context.Context, login string) []notify.Via {
	var out []notify.Via
	if n.s.team == nil {
		return out
	}
	if n.s.tg != nil {
		if _, err := n.s.team.GetConnector(ctx, login, store.ConnectorTelegram); err == nil {
			out = append(out, notify.Telegram)
		}
	}
	if n.s.sk != nil {
		if _, err := n.s.team.GetConnector(ctx, login, connectorSlack); err == nil {
			out = append(out, notify.Slack)
		}
	}
	if n.s.opt.Mail != nil {
		if u, err := n.s.team.GetUser(ctx, login); err == nil && u.Email != "" {
			out = append(out, notify.Email)
		}
	}
	return out
}

func plainText(m notify.Message) string {
	var b strings.Builder
	if m.Test {
		b.WriteString("[TEST] ")
	}
	b.WriteString(m.Title)
	if m.Text != "" {
		b.WriteString("\n" + m.Text)
	}
	if m.URL != "" {
		b.WriteString("\n" + m.URL)
	}
	return b.String()
}

func (n *notifier) sendTelegram(ctx context.Context, t notify.Target, m notify.Message) error {
	if n.s.tg == nil {
		return errors.New("Telegram is not set up on this server")
	}
	cn, err := n.s.team.GetConnector(ctx, t.Login, store.ConnectorTelegram)
	if err != nil {
		return errors.New("Telegram is not linked to your account")
	}
	if err := n.s.tg.send(ctx, cn.Account, plainText(m), nil); err != nil {
		slog.Warn("notifications: telegram failed", "error", err)
		return errors.New("Telegram refused the message")
	}
	return nil
}

func (n *notifier) sendSlack(ctx context.Context, t notify.Target, m notify.Message) error {
	if n.s.sk == nil {
		return errors.New("Slack is not set up on this server")
	}
	cn, err := n.s.team.GetConnector(ctx, t.Login, connectorSlack)
	if err != nil {
		return errors.New("Slack is not linked to your account")
	}
	if err := n.s.sk.dm(ctx, cn.Account, plainText(m)); err != nil {
		slog.Warn("notifications: slack failed", "error", err)
		return errors.New("Slack refused the message")
	}
	return nil
}

func (n *notifier) sendEmail(ctx context.Context, t notify.Target, m notify.Message) error {
	if n.s.opt.Mail == nil {
		return errors.New("this server has no mail server (SMTP) configured")
	}
	u, err := n.s.team.GetUser(ctx, t.Login)
	if err != nil || u.Email == "" {
		return errors.New("your profile has no email address")
	}
	subject := m.Title
	if m.Test {
		subject = "[TEST] " + subject
	}
	if m.ProjectName != "" {
		subject = "[" + m.ProjectName + "] " + subject
	}
	body := m.Text
	if m.URL != "" {
		body += "\n\n" + m.URL
	}
	if err := n.s.opt.Mail.Send(ctx, []string{u.Email}, subject, strings.TrimSpace(body)); err != nil {
		slog.Warn("notifications: mail failed", "error", err)
		return errors.New("the mail server refused the message")
	}
	return nil
}

// emit tells the engine about an event without holding up the caller.
func (s *server) emitNotification(e notify.Event) {
	if s.nf == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if e.ProjectID != "" && e.ProjectName == "" && s.team != nil {
			if p, err := s.team.GetProject(ctx, e.ProjectID); err == nil {
				e.ProjectName = p.Name
			}
		}
		s.nf.d.Emit(ctx, e)
	}()
}

// owners are the owners of a project: who project-level events (budget, new member) concern.
func (s *server) projectOwners(ctx context.Context, project string) []string {
	if s.team == nil || project == "" {
		return nil
	}
	ms, err := s.team.ListMembers(ctx, project)
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range ms {
		if m.Role == store.RoleOwner {
			out = append(out, m.Login)
		}
	}
	return out
}

func (s *server) appURL(path string) string {
	if base := s.configuredBase(); base != "" {
		return base + path
	}
	return ""
}

// feedNotify maps the live feed to notification events: runs that wait for an approval or an answer, and the budget
// check that follows every finished run. (Tasks, schedules and workflows report through their own hooks.)
func (s *server) feedNotify(ev FeedEvent) {
	if ev.Type != "run.finished" || ev.remote {
		return
	}
	switch ev.Status {
	case "waiting_approval", "waiting_input":
		kind, title := notify.ApprovalWaiting, "An agent is waiting for your approval"
		if ev.Status == "waiting_input" {
			kind, title = notify.QuestionWaiting, "An agent has a question for you"
		}
		login := ev.owner
		if login == "" {
			login = ev.Actor
		}
		s.emitNotification(notify.Event{Key: "run:" + ev.RunID + ":" + ev.Status, Kind: kind, ProjectID: ev.ProjectID, Title: title,
			URL: s.appURL("/#/t/" + ev.ThreadID), Login: login, Data: map[string]any{"threadId": ev.ThreadID, "runId": ev.RunID}})
	}
	if s.opt.Usage != nil && s.opt.Usage.Store != nil {
		go s.checkBudgets(ev.ProjectID, ev.owner)
	}
}

// checkBudgets tells about a spend cap that reached 80% or ran out, once a month.
func (s *server) checkBudgets(project, login string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	now := s.opt.Usage.now()
	month := now.Format("2006-01")
	check := func(scope, id, proj string, who []string, label string) {
		v, err := s.capOf(ctx, scope, id, now)
		if err != nil || v.MonthlyUSD <= 0 || !v.Warn {
			return
		}
		kind, name := notify.BudgetWarn, "warn"
		title := label + " reached 80% of its monthly spend cap"
		if v.Reached {
			kind, name = notify.BudgetReached, "reached"
			title = label + " reached its monthly spend cap: new runs are refused"
		}
		for _, l := range who {
			s.emitNotification(notify.Event{Key: fmt.Sprintf("budget:%s:%s:%s:%s", scope, id, month, name), Window: notifyBudgetWindow, Kind: kind, ProjectID: proj,
				Title: title, Body: fmt.Sprintf("$%.2f of $%.2f this month", v.SpentUSD, v.MonthlyUSD), Login: l,
				URL: s.appURL("/#/usage"), Data: map[string]any{"spentUsd": v.SpentUSD, "monthlyUsd": v.MonthlyUSD}})
		}
	}
	if project != "" {
		check("project", project, project, s.projectOwners(ctx, project), "The project")
	}
	if login != "" {
		check("user", login, "", []string{login}, "Your personal usage")
	}
}

// taskNotify reports a task that moved to a state somebody wants to hear about.
func (s *server) taskNotify(t factory.Task, to factory.State) {
	var kind notify.Kind
	var title string
	switch to {
	case factory.StateReview:
		kind, title = notify.TaskReview, "Ready for review: "+t.Title
	case factory.StateFailed:
		kind, title = notify.TaskFailed, "Task failed: "+t.Title
	case factory.StateDone:
		kind, title = notify.TaskDone, "Task done: "+t.Title
	default:
		return
	}
	url := t.PR.URL
	if url == "" && t.ProjectID != "" {
		url = s.appURL("/#/p/" + t.ProjectID + "?task=" + t.ID)
	}
	s.emitNotification(notify.Event{Key: "task:" + t.ID + ":" + string(to), Kind: kind, ProjectID: t.ProjectID, Title: title, URL: url,
		Login: t.Owner, Data: map[string]any{"taskId": t.ID, "state": string(to)}})
}

// escalate tells about a task that needs a human, through the rules: the project's channels, and the task owner
// wherever their preferences say. By default that is every channel the owner linked (Telegram, Slack, email), as
// before the rules existed. A project with a Slack channel keeps hearing it there, and then the owner is not also
// messaged on Slack.
func (s *server) escalate(ctx context.Context, t factory.Task, reason string) {
	e := notify.Event{Key: "task:" + t.ID + ":blocked:" + reason, Kind: notify.TaskBlocked, ProjectID: t.ProjectID, Title: "Task needs you: " + t.Title,
		Body: reason, Login: t.Owner, Data: map[string]any{"taskId": t.ID}}
	if base := s.configuredBase(); base != "" && t.ProjectID != "" {
		e.URL = base + "/#/projects/" + t.ProjectID
	}
	if s.sk != nil && s.team != nil && t.ProjectID != "" {
		if p, err := s.team.GetProject(ctx, t.ProjectID); err == nil && p.SlackChannel != "" {
			text := "[" + p.Name + "] " + plainText(notify.Message{Title: e.Title, Text: e.Body, URL: e.URL})
			if s.sk.send(ctx, slackDest{channel: p.SlackChannel}, text, nil) == nil {
				e.Skip = []notify.Via{notify.Slack}
			}
		}
	}
	s.emitNotification(e)
}

// workflowNotify reports a gate that waits for a person and a run that ended.
func (s *server) workflowNotify(c workflows.Change, title string) {
	run := c.Run
	url := s.appURL("/#/p/" + run.ProjectID + "?tab=workflows&run=" + run.ID)
	switch {
	case c.Kind == "gate":
		if st := run.Step(c.NodeID); st != nil && st.State == workflows.NodeWaiting {
			s.emitNotification(notify.Event{Key: "gate:" + run.ID + ":" + c.NodeID, Kind: notify.GateWaiting, ProjectID: run.ProjectID,
				Title: "A workflow is waiting for you: " + title, URL: url, Login: run.Owner, Data: map[string]any{"runId": run.ID, "node": c.NodeID}})
		}
	case c.Kind == "finished" && (run.State == workflows.RunDone || run.State == workflows.RunFailed):
		body := ""
		if run.State == workflows.RunFailed {
			body = run.Error
		}
		s.emitNotification(notify.Event{Key: "wfrun:" + run.ID, Kind: notify.WorkflowDone, ProjectID: run.ProjectID,
			Title: "Workflow " + string(run.State) + ": " + run.WorkflowName, Body: body, URL: url, Login: run.Owner, Data: map[string]any{"runId": run.ID, "state": string(run.State)}})
	}
}

// ---- routes ----

func (s *server) routeNotifications() {
	if s.nf == nil {
		return
	}
	s.Route("GET /projects/{pid}/notifications", s.getNotifications)
	s.Route("PUT /projects/{pid}/notifications", s.putNotifications)
	s.Route("GET /projects/{pid}/notifications/log", s.notificationLog)
	s.Route("GET /projects/{pid}/notification-channels", s.listNotificationChannels)
	s.Route("POST /projects/{pid}/notification-channels", s.createNotificationChannel)
	s.Route("PUT /projects/{pid}/notification-channels/{id}", s.updateNotificationChannel)
	s.Route("DELETE /projects/{pid}/notification-channels/{id}", s.deleteNotificationChannel)
	s.Route("POST /projects/{pid}/notification-channels/{id}/test", s.testNotificationChannel)
	s.Route("GET /me/notifications", s.getMyNotifications)
	s.Route("PUT /me/notifications", s.putMyNotifications)
	s.Route("POST /me/notifications/test", s.testMyNotifications)
}

func channelView(c notify.Channel, withHint bool) NotificationChannel {
	v := NotificationChannel{ID: c.ID, Kind: c.Kind, Name: c.Name, HasSecret: c.HasSecret, Enabled: c.Enabled, CreatedAt: c.CreatedAt}
	if withHint {
		v.Hint = c.Hint
	}
	return v
}

func (s *server) notificationsView(ctx context.Context, pid, role string) (NotificationsView, error) {
	rules, err := s.nf.st.GetRules(ctx, pid)
	if err != nil {
		return NotificationsView{}, err
	}
	chans, err := s.nf.st.ListChannels(ctx, pid)
	if err != nil {
		return NotificationsView{}, err
	}
	owner := store.RoleAtLeast(role, store.RoleOwner)
	v := NotificationsView{Events: notify.Events(), Rules: rules, Channels: []NotificationChannel{}, CanEdit: owner, CanSecure: s.nf.box != nil}
	if v.Rules.Events == nil {
		v.Rules.Events = map[notify.Kind]notify.Rule{}
	}
	for k, r := range v.Rules.Events {
		if r.Channels == nil {
			r.Channels = []string{}
			v.Rules.Events[k] = r
		}
	}
	for _, c := range chans {
		v.Channels = append(v.Channels, channelView(c, owner))
	}
	return v, nil
}

func (s *server) getNotifications(w http.ResponseWriter, r *http.Request, c Caller) {
	p, role, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	v, err := s.notificationsView(r.Context(), p.ID, role)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) putNotifications(w http.ResponseWriter, r *http.Request, c Caller) {
	p, role, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	var body notify.Rules
	if !decode(w, r, &body) {
		return
	}
	chans, err := s.nf.st.ListChannels(r.Context(), p.ID)
	if err != nil {
		fail(w, err)
		return
	}
	ids := map[string]bool{}
	for _, ch := range chans {
		ids[ch.ID] = true
	}
	body.ProjectID, body.UpdatedBy = p.ID, c.Actor
	if err := body.Validate(ids); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := s.nf.st.PutRules(r.Context(), body); err != nil {
		fail(w, err)
		return
	}
	s.note(Caller{Login: c.Actor, Actor: c.Actor, ProjectID: p.ID}, "", "notifications.rules", map[string]any{"dedupeMinutes": body.DedupeMinutes, "digest": body.Digest}, FeedEvent{Type: "project.changed"})
	v, err := s.notificationsView(r.Context(), p.ID, role)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) notificationLog(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	log, err := s.nf.st.ListLog(r.Context(), p.ID, "", 100)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, log)
}

func (s *server) listNotificationChannels(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	chans, err := s.nf.st.ListChannels(r.Context(), p.ID)
	if err != nil {
		fail(w, err)
		return
	}
	out := []NotificationChannel{}
	for _, ch := range chans {
		out = append(out, channelView(ch, true))
	}
	writeJSON(w, http.StatusOK, out)
}

type channelBody struct {
	Kind        string  `json:"kind"`
	Name        *string `json:"name"`
	URL         *string `json:"url"`
	Secret      *string `json:"secret"`
	ClearSecret bool    `json:"clearSecret"`
	Enabled     *bool   `json:"enabled"`
}

func (s *server) checkAddress(w http.ResponseWriter, kind, url string) bool {
	if err := notify.ValidateAddress(kind, url, !s.nf.allowPrivate); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return false
	}
	if len(url) > 2000 {
		writeError(w, http.StatusBadRequest, "invalid_request", "The address is too long.")
		return false
	}
	return true
}

func validChannelName(n string) bool {
	return n != "" && len(n) <= 60 && !strings.ContainsAny(n, "\r\n")
}

func (s *server) createNotificationChannel(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	if s.nf.box == nil {
		writeError(w, http.StatusNotImplemented, "notifications_unavailable", "This server has no secret key (HARNESS_SECRET_KEY), so channel addresses cannot be stored safely.")
		return
	}
	var body channelBody
	if !decode(w, r, &body) {
		return
	}
	known := false
	for _, k := range notify.ChannelKinds() {
		known = known || k == body.Kind
	}
	name, url, secret := strDeref(body.Name), strings.TrimSpace(strDeref(body.URL)), strDeref(body.Secret)
	switch {
	case !known:
		writeError(w, http.StatusBadRequest, "invalid_request", "kind must be webhook, wecom or discord.")
		return
	case !validChannelName(strings.TrimSpace(name)):
		writeError(w, http.StatusBadRequest, "invalid_request", "Give the channel a name of up to 60 characters.")
		return
	case secret != "" && body.Kind != notify.KindWebhook, len(secret) > 200:
		writeError(w, http.StatusBadRequest, "invalid_request", "Only a webhook has a signing secret (up to 200 characters).")
		return
	case !s.checkAddress(w, body.Kind, url):
		return
	}
	if existing, err := s.nf.st.ListChannels(r.Context(), p.ID); err == nil && len(existing) >= maxNotificationChannels {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("At most %d channels per project.", maxNotificationChannels))
		return
	}
	ch := notify.Channel{ID: uuid.NewString(), ProjectID: p.ID, Kind: body.Kind, Name: strings.TrimSpace(name), Hint: notify.HostHint(url), HasSecret: secret != "",
		Enabled: true, Sealed: s.nf.box.seal(notify.SealPair(url, secret)), CreatedBy: c.Actor, CreatedAt: time.Now().UTC()}
	if err := s.nf.st.PutChannel(r.Context(), ch); err != nil {
		fail(w, err)
		return
	}
	s.note(Caller{Login: c.Actor, Actor: c.Actor, ProjectID: p.ID}, "", "notifications.channel.add", map[string]any{"channel": ch.ID, "kind": ch.Kind, "name": ch.Name}, FeedEvent{Type: "project.changed"})
	writeJSON(w, http.StatusCreated, channelView(ch, true))
}

func strDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (s *server) channelOf(w http.ResponseWriter, r *http.Request, c Caller) (*store.Project, notify.Channel, bool) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return nil, notify.Channel{}, false
	}
	ch, err := s.nf.st.GetChannel(r.Context(), p.ID, r.PathValue("id"))
	if errors.Is(err, notify.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "No such channel.")
		return nil, notify.Channel{}, false
	}
	if err != nil {
		fail(w, err)
		return nil, notify.Channel{}, false
	}
	return p, ch, true
}

func (s *server) updateNotificationChannel(w http.ResponseWriter, r *http.Request, c Caller) {
	p, ch, ok := s.channelOf(w, r, c)
	if !ok {
		return
	}
	var body channelBody
	if !decode(w, r, &body) {
		return
	}
	if body.Name != nil {
		if !validChannelName(strings.TrimSpace(*body.Name)) {
			writeError(w, http.StatusBadRequest, "invalid_request", "Give the channel a name of up to 60 characters.")
			return
		}
		ch.Name = strings.TrimSpace(*body.Name)
	}
	if body.Enabled != nil {
		ch.Enabled = *body.Enabled
	}
	if body.URL != nil || body.Secret != nil || body.ClearSecret {
		if s.nf.box == nil {
			writeError(w, http.StatusNotImplemented, "notifications_unavailable", "This server has no secret key.")
			return
		}
		curURL, curSecret, err := s.nf.d.Open(ch.Sealed)
		if err != nil {
			writeError(w, http.StatusConflict, "unreadable", "The stored address cannot be read: remove the channel and add it again.")
			return
		}
		if body.URL != nil {
			curURL = strings.TrimSpace(*body.URL)
			if !s.checkAddress(w, ch.Kind, curURL) {
				return
			}
			ch.Hint = notify.HostHint(curURL)
		}
		if body.Secret != nil {
			if ch.Kind != notify.KindWebhook || len(*body.Secret) > 200 {
				writeError(w, http.StatusBadRequest, "invalid_request", "Only a webhook has a signing secret (up to 200 characters).")
				return
			}
			curSecret = *body.Secret
		}
		if body.ClearSecret {
			curSecret = ""
		}
		ch.HasSecret = curSecret != ""
		ch.Sealed = s.nf.box.seal(notify.SealPair(curURL, curSecret))
	}
	if err := s.nf.st.PutChannel(r.Context(), ch); err != nil {
		fail(w, err)
		return
	}
	s.note(Caller{Login: c.Actor, Actor: c.Actor, ProjectID: p.ID}, "", "notifications.channel.update", map[string]any{"channel": ch.ID, "name": ch.Name, "enabled": ch.Enabled}, FeedEvent{Type: "project.changed"})
	writeJSON(w, http.StatusOK, channelView(ch, true))
}

func (s *server) deleteNotificationChannel(w http.ResponseWriter, r *http.Request, c Caller) {
	p, ch, ok := s.channelOf(w, r, c)
	if !ok {
		return
	}
	if err := s.nf.st.DeleteChannel(r.Context(), p.ID, ch.ID); err != nil {
		fail(w, err)
		return
	}
	// The rules stop naming it.
	if rules, err := s.nf.st.GetRules(r.Context(), p.ID); err == nil {
		changed := false
		for k, rule := range rules.Events {
			kept := rule.Channels[:0:0]
			for _, id := range rule.Channels {
				if id != ch.ID {
					kept = append(kept, id)
				}
			}
			if len(kept) != len(rule.Channels) {
				rule.Channels = kept
				rules.Events[k] = rule
				changed = true
			}
		}
		if changed {
			_ = s.nf.st.PutRules(r.Context(), rules)
		}
	}
	s.note(Caller{Login: c.Actor, Actor: c.Actor, ProjectID: p.ID}, "", "notifications.channel.delete", map[string]any{"channel": ch.ID, "name": ch.Name}, FeedEvent{Type: "project.changed"})
	w.WriteHeader(http.StatusNoContent)
}

func (n *notifier) allowTest(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	if t, ok := n.lastTest[key]; ok && now.Sub(t) < time.Second {
		return false
	}
	n.lastTest[key] = now
	return true
}

func (s *server) testNotificationChannel(w http.ResponseWriter, r *http.Request, c Caller) {
	p, ch, ok := s.channelOf(w, r, c)
	if !ok {
		return
	}
	if !s.nf.allowTest(ch.ID) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Wait a moment before sending another test.")
		return
	}
	label := notify.ChannelLabel(ch)
	res := NotificationTestResult{Channel: ch.Name, Status: notify.Delivered}
	url, secret, err := s.nf.d.Open(ch.Sealed)
	if err == nil {
		err = s.nf.d.Test(r.Context(), p.ID, p.Name, c.Actor, ch.Kind, label, notify.Target{URL: url, Secret: secret})
	} else {
		err = errors.New("the stored address cannot be read: remove the channel and add it again")
	}
	if err != nil {
		res.Status, res.Error = notify.Failed, err.Error()
	}
	s.note(Caller{Login: c.Actor, Actor: c.Actor, ProjectID: p.ID}, "", "notifications.channel.test", map[string]any{"channel": ch.ID, "status": res.Status}, FeedEvent{Type: "project.changed"})
	writeJSON(w, http.StatusOK, res)
}

// ---- personal ----

func (s *server) myNotifications(ctx context.Context, login string) (MyNotifications, error) {
	prefs, err := s.nf.st.GetPrefs(ctx, login)
	if err != nil {
		return MyNotifications{}, err
	}
	linked := s.nf.linked(ctx, login)
	v := MyNotifications{Events: notify.Events(), Effective: map[string][]string{}, Available: []string{string(notify.InApp)},
		QuietStart: prefs.QuietStart, QuietEnd: prefs.QuietEnd, Timezone: prefs.Timezone, Digest: prefs.Digest}
	for _, l := range linked {
		v.Available = append(v.Available, string(l))
	}
	if prefs.SealedWebhook != nil {
		v.Available = append(v.Available, string(notify.Webhook))
		v.Webhook = NotificationWebhook{Set: true, Hint: prefs.WebhookHint, HasSecret: prefs.WebhookSecret}
	}
	for _, e := range v.Events {
		for _, via := range prefs.ViasFor(e.Kind, linked) {
			v.Effective[string(e.Kind)] = append(v.Effective[string(e.Kind)], string(via))
		}
	}
	if s.team != nil {
		if u, err := s.team.GetUser(ctx, login); err == nil {
			v.Email = u.Email
		}
	}
	if v.Recent, err = s.nf.st.ListLog(ctx, "", login, 20); err != nil {
		return MyNotifications{}, err
	}
	// The person's deliveries of project events are in the logs of those projects: gather the latest of them.
	if ps, err := s.team.ListProjects(ctx, login); err == nil {
		for _, pr := range ps {
			more, _ := s.nf.st.ListLog(ctx, pr.ID, login, 20)
			v.Recent = append(v.Recent, more...)
		}
	}
	sortLog(v.Recent)
	if len(v.Recent) > 20 {
		v.Recent = v.Recent[:20]
	}
	return v, nil
}

func sortLog(l []notify.LogEntry) {
	for i := 1; i < len(l); i++ {
		for j := i; j > 0 && l[j].At.After(l[j-1].At); j-- {
			l[j], l[j-1] = l[j-1], l[j]
		}
	}
}

func (s *server) getMyNotifications(w http.ResponseWriter, r *http.Request, c Caller) {
	v, err := s.myNotifications(r.Context(), c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type myNotificationsBody struct {
	Events     map[notify.Kind][]notify.Via `json:"events"`
	QuietStart string                       `json:"quietStart"`
	QuietEnd   string                       `json:"quietEnd"`
	Timezone   string                       `json:"timezone"`
	Digest     bool                         `json:"digest"`
	Webhook    *struct {
		URL    string `json:"url"`
		Secret string `json:"secret"`
		Clear  bool   `json:"clear"`
	} `json:"webhook"`
}

func (s *server) putMyNotifications(w http.ResponseWriter, r *http.Request, c Caller) {
	var body myNotificationsBody
	if !decode(w, r, &body) {
		return
	}
	ctx := r.Context()
	prefs, err := s.nf.st.GetPrefs(ctx, c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	linked := s.nf.linked(ctx, c.Actor)
	next := notify.Prefs{Login: c.Actor, Events: map[notify.Kind][]notify.Via{}, QuietStart: body.QuietStart, QuietEnd: body.QuietEnd,
		Timezone: body.Timezone, Digest: body.Digest, SealedWebhook: prefs.SealedWebhook, WebhookHint: prefs.WebhookHint, WebhookSecret: prefs.WebhookSecret}
	if body.Webhook != nil {
		switch {
		case body.Webhook.Clear:
			next.SealedWebhook, next.WebhookHint, next.WebhookSecret = nil, "", false
		case body.Webhook.URL != "":
			if s.nf.box == nil {
				writeError(w, http.StatusNotImplemented, "notifications_unavailable", "This server has no secret key, so a webhook address cannot be stored safely.")
				return
			}
			u := strings.TrimSpace(body.Webhook.URL)
			if !s.checkAddress(w, notify.KindWebhook, u) {
				return
			}
			if len(body.Webhook.Secret) > 200 {
				writeError(w, http.StatusBadRequest, "invalid_request", "The secret is too long.")
				return
			}
			next.SealedWebhook, next.WebhookHint, next.WebhookSecret = s.nf.box.seal(notify.SealPair(u, body.Webhook.Secret)), notify.HostHint(u), body.Webhook.Secret != ""
		}
	}
	// Keep only what differs from the defaults, so a channel linked later still gets the default events.
	for k, vias := range body.Events {
		next.Events[k] = vias
	}
	if err := next.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	for k, vias := range next.Events {
		if sameVias(notify.Prefs{Events: map[notify.Kind][]notify.Via{k: vias}}.ViasFor(k, nil), notify.Defaults(k, linked)) {
			delete(next.Events, k)
		}
	}
	if err := s.nf.st.PutPrefs(ctx, next); err != nil {
		fail(w, err)
		return
	}
	s.note(Caller{Login: c.Actor, Actor: c.Actor}, "", "notifications.prefs", map[string]any{"quiet": next.QuietStart != "", "digest": next.Digest, "events": len(next.Events)}, FeedEvent{Type: "notifications.changed"})
	v, err := s.myNotifications(ctx, c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func sameVias(a, b []notify.Via) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[notify.Via]bool{}
	for _, v := range a {
		m[v] = true
	}
	for _, v := range b {
		if !m[v] {
			return false
		}
	}
	return true
}

func (s *server) testMyNotifications(w http.ResponseWriter, r *http.Request, c Caller) {
	ctx := r.Context()
	if !s.nf.allowTest("me:" + c.Actor) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Wait a moment before sending another test.")
		return
	}
	prefs, err := s.nf.st.GetPrefs(ctx, c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	out := NotificationTestResults{Results: []NotificationTestResult{}}
	for _, via := range s.nf.linked(ctx, c.Actor) {
		out.Results = append(out.Results, s.testVia(ctx, c.Actor, via, notify.Target{Login: c.Actor}))
	}
	if prefs.SealedWebhook != nil {
		t, err := s.nf.d.Target(prefs.SealedWebhook)
		if err != nil {
			out.Results = append(out.Results, NotificationTestResult{Channel: notify.ViaLabel(notify.Webhook), Status: notify.Failed, Error: "your webhook address cannot be read: save it again"})
		} else {
			t.Login = c.Actor
			out.Results = append(out.Results, s.testVia(ctx, c.Actor, notify.Webhook, t))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) testVia(ctx context.Context, login string, via notify.Via, t notify.Target) NotificationTestResult {
	label := notify.ViaLabel(via)
	res := NotificationTestResult{Channel: label, Status: notify.Delivered}
	if err := s.nf.d.Test(ctx, "", "", login, string(via), label, t); err != nil {
		res.Status, res.Error = notify.Failed, err.Error()
	}
	return res
}

// dropProjectNotifications removes what a deleted project owned.
func (s *server) dropProjectNotifications(ctx context.Context, project string) {
	if s.nf == nil {
		return
	}
	if err := s.nf.st.DeleteProject(ctx, project); err != nil {
		slog.Warn("notifications: deleting the notifications of a deleted project failed", "project", project, "error", err)
	}
}
