package product

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/hosts/inproc"
)

// connectorSlack is the Connector kind of a linked Slack user (Account is the Slack user id).
const connectorSlack = "slack"

// Slack lets a user talk to their assistant from Slack, and tells them when a
// session needs them. Slack calls the server (the Events API), so the server
// must be reachable from the internet; the two routes are
//
//	POST /api/slack/events        Event Subscriptions request URL
//	POST /api/slack/interactive   Interactivity request URL (the Approve / Deny buttons)
//
// and every request is checked against the app's signing secret.
//
// A user links their Slack account from the profile page: the page shows a
// one-time code, and sending "link <code>" to the app in a direct message ties
// that Slack user to the account. Then:
//   - a direct message to the app, or an @mention of it in a channel, goes to the
//     user's assistant (it steers the running run, answers its question, or starts a
//     run); the answer comes back where the message was written (in the thread of
//     a mention);
//   - a tool approval the assistant waits for arrives as a direct message with
//     Approve / Deny buttons, never in a channel;
//   - "status" lists the sessions, "help" says what the app does.
//
// Only linked users are served. Process-wide: run one replica with the bridge.
type Slack struct {
	// Token is the bot token (xoxb-…) of the Slack app.
	Token string
	// SigningSecret verifies that requests come from Slack.
	SigningSecret string
	// API is the Web API base (default https://slack.com/api).
	API string
	// HTTP is the client used (default: one with a 20 s timeout).
	HTTP *http.Client
}

func (sl *Slack) api() string {
	if sl.API != "" {
		return strings.TrimRight(sl.API, "/")
	}
	return "https://slack.com/api"
}

func (sl *Slack) client() *http.Client {
	if sl.HTTP != nil {
		return sl.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (sl *Slack) call(ctx context.Context, method string, params any, out any) error {
	b, _ := json.Marshal(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sl.api()+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+sl.Token)
	resp, err := sl.client().Do(req)
	if err != nil {
		return redactToken(err, sl.Token)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &r); err != nil || !r.OK {
		return fmt.Errorf("slack %s: %s", method, r.Error)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// verify checks the X-Slack-Signature of a request body.
func (sl *Slack) verify(h http.Header, body []byte, now time.Time) bool {
	ts := h.Get("X-Slack-Request-Timestamp")
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || sl.SigningSecret == "" {
		return false
	}
	if d := now.Sub(time.Unix(n, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, []byte(sl.SigningSecret))
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(h.Get("X-Slack-Signature")))
}

// slackDest is where the answer to a message goes.
type slackDest struct {
	channel  string
	threadTS string
}

type slackAction struct {
	login, thread, interrupt string
	approve                  bool
	expires                  time.Time
}

// slackBridge is the app's side of the server.
type slackBridge struct {
	s  *server
	sl *Slack
	// botID is the app's own user id (read with auth.test), to strip its mention from a message.
	botID string
	mu    sync.Mutex
	codes map[string]linkEntry
	// actions are the buttons sent: short key → what they do.
	actions map[string]slackAction
	// runs are the assistant runs whose result goes to Slack: run id → where.
	runs map[string]slackDest
	// told remembers the approvals already sent, so a status event is not announced twice.
	told map[string]bool
	// seen are the event ids handled (Slack redelivers an event it thinks was not received).
	seen map[string]time.Time
}

func newSlackBridge(s *server) *slackBridge {
	return &slackBridge{s: s, sl: s.opt.Slack, codes: map[string]linkEntry{}, actions: map[string]slackAction{},
		runs: map[string]slackDest{}, told: map[string]bool{}, seen: map[string]time.Time{}}
}

func (b *slackBridge) pendingFor(login string) *linkPending {
	b.mu.Lock()
	defer b.mu.Unlock()
	for code, e := range b.codes {
		if e.login == login && time.Now().Before(e.expires) {
			return &linkPending{Code: code, ExpiresAt: e.expires}
		}
	}
	return nil
}

func (b *slackBridge) newCode(login string) linkPending {
	raw := make([]byte, 6)
	_, _ = rand.Read(raw)
	code := strings.ToLower(hex.EncodeToString(raw))
	b.mu.Lock()
	defer b.mu.Unlock()
	for c, e := range b.codes { // one pending code per user, and drop the stale ones
		if e.login == login || time.Now().After(e.expires) {
			delete(b.codes, c)
		}
	}
	exp := time.Now().Add(10 * time.Minute)
	b.codes[code] = linkEntry{login, exp}
	return linkPending{Code: code, ExpiresAt: exp}
}

func (b *slackBridge) takeCode(code string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.codes[strings.ToLower(code)]
	delete(b.codes, strings.ToLower(code))
	return e.login, ok && time.Now().Before(e.expires)
}

func (b *slackBridge) addAction(a slackAction) string {
	raw := make([]byte, 6)
	_, _ = rand.Read(raw)
	key := b64(raw)
	a.expires = time.Now().Add(24 * time.Hour)
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, v := range b.actions {
		if time.Now().After(v.expires) {
			delete(b.actions, k)
		}
	}
	b.actions[key] = a
	return key
}

func (b *slackBridge) action(key string) (slackAction, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	a, ok := b.actions[key]
	return a, ok && time.Now().Before(a.expires)
}

func (b *slackBridge) markRun(id string, d slackDest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.runs) > 1000 {
		b.runs = map[string]slackDest{}
	}
	b.runs[id] = d
}

func (b *slackBridge) takeRun(id string) (slackDest, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d, ok := b.runs[id]
	delete(b.runs, id)
	return d, ok
}

// firstSeen reports whether an event id is new (and remembers it).
func (b *slackBridge) firstSeen(id string) bool {
	if id == "" {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	for k, t := range b.seen {
		if now.Sub(t) > 10*time.Minute {
			delete(b.seen, k)
		}
	}
	if _, dup := b.seen[id]; dup {
		return false
	}
	b.seen[id] = now
	return true
}

func (b *slackBridge) markTold(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.told[id] {
		return true
	}
	if len(b.told) > 1000 {
		b.told = map[string]bool{}
	}
	b.told[id] = true
	return false
}

// ---- routes ----

func (s *server) slackReady(w http.ResponseWriter) bool {
	if s.sk == nil {
		writeError(w, http.StatusNotImplemented, "not_configured", "Slack is not configured on this server.")
		return false
	}
	return true
}

func (s *server) slackLink(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.slackReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.sk.newCode(c.Actor))
}

func (s *server) slackTest(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.slackReady(w) {
		return
	}
	cn, err := s.team.GetConnector(r.Context(), c.Actor, connectorSlack)
	if err != nil {
		writeError(w, http.StatusConflict, "not_connected", "Connect Slack first.")
		return
	}
	if err := s.sk.send(r.Context(), slackDest{channel: cn.Account}, "Hello from your assistant. This Slack account is linked.", nil); err != nil {
		writeError(w, http.StatusBadGateway, "slack_error", "Slack could not be reached.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) slackDisconnect(w http.ResponseWriter, r *http.Request, c Caller) {
	if err := s.team.DeleteConnector(r.Context(), c.Actor, connectorSlack); err != nil && !errors.Is(err, store.ErrNotFound) {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) routeSlack() {
	s.Route("POST /connectors/slack/link", s.slackLink)
	s.Route("POST /connectors/slack/test", s.slackTest)
	s.Route("DELETE /connectors/slack", s.slackDisconnect)
	if s.opt.Slack == nil || s.box == nil {
		return
	}
	s.sk = newSlackBridge(s)
	for _, prefix := range []string{"/api", ""} {
		s.Mux.HandleFunc("POST "+prefix+"/slack/events", s.sk.events)
		s.Mux.HandleFunc("POST "+prefix+"/slack/interactive", s.sk.interactive)
	}
	go s.sk.run(context.Background())
}

// run reads the app's own id and then delivers the assistant's results.
func (b *slackBridge) run(ctx context.Context) {
	for ctx.Err() == nil {
		var me struct {
			UserID string `json:"user_id"`
		}
		if err := b.sl.call(ctx, "auth.test", struct{}{}, &me); err == nil {
			b.mu.Lock()
			b.botID = me.UserID
			b.mu.Unlock()
			break
		} else {
			slog.Warn("slack: auth.test failed", "error", err)
			sleepCtx(ctx, 10*time.Second)
		}
	}
	sub, done := b.s.hub.subscribe(nil) // the bridge runs in one replica and hears every replica's events
	defer done()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-sub.ch:
			if ev.Type == "run.finished" && ev.ThreadID != "" {
				b.onFinished(ctx, ev)
			}
		}
	}
}

// ---- incoming ----

type slackEnvelope struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	EventID   string `json:"event_id"`
	Event     struct {
		Type        string `json:"type"`
		Subtype     string `json:"subtype"`
		BotID       string `json:"bot_id"`
		User        string `json:"user"`
		Text        string `json:"text"`
		Channel     string `json:"channel"`
		ChannelType string `json:"channel_type"`
		TS          string `json:"ts"`
		ThreadTS    string `json:"thread_ts"`
	} `json:"event"`
}

func (b *slackBridge) readSigned(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || !b.sl.verify(r.Header, body, time.Now()) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return nil, false
	}
	return body, true
}

func (b *slackBridge) events(w http.ResponseWriter, r *http.Request) {
	body, ok := b.readSigned(w, r)
	if !ok {
		return
	}
	var env slackEnvelope
	if json.Unmarshal(body, &env) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if env.Type == "url_verification" {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, env.Challenge)
		return
	}
	w.WriteHeader(http.StatusOK) // Slack wants the answer within 3 s; the work goes on in the background
	if env.Type != "event_callback" || !b.firstSeen(env.EventID) {
		return
	}
	ev := env.Event
	if ev.BotID != "" || ev.User == "" || strings.TrimSpace(ev.Text) == "" {
		return
	}
	switch {
	case ev.Type == "app_mention" && ev.Subtype == "":
	case ev.Type == "message" && ev.ChannelType == "im" && (ev.Subtype == "" || ev.Subtype == "thread_broadcast"):
	default:
		return
	}
	go b.handleMessage(context.WithoutCancel(r.Context()), env)
}

var slackMention = regexp.MustCompile(`<@[A-Z0-9]+(\|[^>]*)?>`)

func (b *slackBridge) cleanText(text string) string {
	b.mu.Lock()
	bot := b.botID
	b.mu.Unlock()
	if bot != "" {
		text = strings.ReplaceAll(text, "<@"+bot+">", "")
	} else if loc := slackMention.FindStringIndex(text); loc != nil && loc[0] == 0 {
		text = text[loc[1]:]
	}
	return strings.TrimSpace(text)
}

func (b *slackBridge) handleMessage(ctx context.Context, env slackEnvelope) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("slack: event handler panicked", "panic", r)
		}
	}()
	ev := env.Event
	im := ev.ChannelType == "im"
	dest := slackDest{channel: ev.Channel}
	if !im { // a mention: answer in its thread
		dest.threadTS = ev.ThreadTS
		if dest.threadTS == "" {
			dest.threadTS = ev.TS
		}
	}
	text := b.cleanText(ev.Text)
	words := strings.Fields(text)
	if len(words) == 0 {
		words = []string{"help"}
	}
	if im && strings.EqualFold(words[0], "link") && len(words) == 2 {
		b.link(ctx, dest, ev.User, words[1])
		return
	}
	cn, err := b.s.team.FindConnector(ctx, connectorSlack, ev.User)
	if err != nil {
		_ = b.send(ctx, dest, "Your Slack account is not linked to an account. Open your profile in the web app, press Connect Slack, and send me `link <code>` in a direct message.", nil)
		return
	}
	login := cn.Login
	switch strings.ToLower(words[0]) {
	case "help":
		if len(words) == 1 {
			_ = b.send(ctx, dest, "I am your assistant. Send me a direct message or mention me in a channel and I answer as in the web app. `status` lists your sessions.", nil)
			return
		}
	case "status":
		if len(words) == 1 {
			_ = b.send(ctx, dest, b.status(ctx, login), nil)
			return
		}
	}
	reply, err := b.toAssistant(ctx, login, text, dest)
	if err != nil {
		slog.Warn("slack: delivering to the assistant failed", "error", err)
		reply = "I could not pass that on: " + err.Error()
	}
	if reply != "" {
		_ = b.send(ctx, dest, reply, nil)
	}
}

func (b *slackBridge) link(ctx context.Context, dest slackDest, user, code string) {
	login, ok := b.takeCode(code)
	if !ok {
		_ = b.send(ctx, dest, "This code expired. Press Connect Slack again in the web app.", nil)
		return
	}
	label := "Slack " + user
	var info struct {
		User struct {
			Name     string `json:"name"`
			RealName string `json:"real_name"`
		} `json:"user"`
	}
	if b.sl.call(ctx, "users.info", map[string]string{"user": user}, &info) == nil {
		if info.User.RealName != "" {
			label = info.User.RealName
		} else if info.User.Name != "" {
			label = info.User.Name
		}
	}
	err := b.s.team.SetConnector(ctx, store.Connector{Login: login, Kind: connectorSlack, Account: user, Label: label, Data: b.s.box.seal([]byte(user))})
	switch {
	case errors.Is(err, store.ErrConflict):
		_ = b.send(ctx, dest, "This Slack account is already linked to another account.", nil)
	case err != nil:
		_ = b.send(ctx, dest, "Linking failed; try again.", nil)
	default:
		b.s.note(Caller{Actor: login}, "", "connector.slack", nil, FeedEvent{Type: "project.changed", affects: login})
		_ = b.send(ctx, dest, "Linked. Message me here or mention me in a channel; I will tell you when a session needs you.", nil)
	}
}

func (b *slackBridge) assistantClient(login string) client.Client {
	s := b.s
	return inproc.New(s.H, login, inproc.WithModels(s.O.Models, s.O.DefaultModel),
		inproc.WithAuthor(harness.Author{Login: login, Kind: store.AuthorUser}))
}

// toAssistant puts a Slack message into the assistant's thread: it answers a
// pending question, steers a running run, or starts a run. The reply comes back
// through onFinished when the run ends.
func (b *slackBridge) toAssistant(ctx context.Context, login, text string, dest slackDest) (string, error) {
	id, err := b.s.supervisorThread(ctx, login)
	if err != nil {
		return "", err
	}
	cl := b.assistantClient(login)
	th, err := cl.GetThread(ctx, id)
	if err != nil {
		return "", err
	}
	var run *client.Run
	switch {
	case th.PendingApproval != nil:
		return "I am waiting for your decision on a request; use the Approve / Deny buttons in our direct messages.", nil
	case th.PendingQuestion != nil:
		run, err = cl.Resume(ctx, id, client.ResumeRequest{InterruptID: th.PendingQuestion.InterruptID, Reason: "ask_user", Text: text, Interactive: true})
	case th.ActiveRun != nil:
		return "", cl.Steer(ctx, th.ActiveRun.ID, text)
	default:
		run, err = cl.Send(ctx, id, text, client.SendOptions{Sandbox: "off", Interactive: true})
	}
	if err != nil {
		return "", err
	}
	b.markRun(run.ID, dest)
	b.s.AfterRun(Caller{Login: login, Actor: login}, run, "run.send", map[string]any{"via": "slack"})
	return "", nil
}

func (b *slackBridge) status(ctx context.Context, login string) string {
	doc, err := b.s.overviewFor(ctx, login)
	if err != nil {
		return "I could not read your sessions."
	}
	var all []threadSummary
	all = append(all, doc.Personal...)
	for _, p := range doc.Projects {
		all = append(all, p.Threads...)
	}
	var lines []string
	for _, t := range all {
		if t.Kind == store.ThreadSupervisor || t.Status == "idle" {
			continue
		}
		title := t.Title
		if title == "" {
			title = "(untitled)"
		}
		lines = append(lines, fmt.Sprintf("%s: %s", strings.ReplaceAll(t.Status, "_", " "), title))
	}
	if len(lines) == 0 {
		return "No session is running or waiting."
	}
	return strings.Join(lines, "\n")
}

// interactive handles the Approve / Deny buttons.
func (b *slackBridge) interactive(w http.ResponseWriter, r *http.Request) {
	body, ok := b.readSigned(w, r)
	if !ok {
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var p struct {
		Type string `json:"type"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
		Message struct {
			TS string `json:"ts"`
		} `json:"message"`
		Actions []struct {
			Value string `json:"value"`
		} `json:"actions"`
	}
	if json.Unmarshal([]byte(form.Get("payload")), &p) != nil || p.Type != "block_actions" || len(p.Actions) == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	value, user, channel, ts := p.Actions[0].Value, p.User.ID, p.Channel.ID, p.Message.TS
	go b.handleButton(context.WithoutCancel(r.Context()), value, user, channel, ts)
}

func (b *slackBridge) handleButton(ctx context.Context, value, user, channel, ts string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("slack: button handler panicked", "panic", r)
		}
	}()
	dest := slackDest{channel: channel}
	key, ok := strings.CutPrefix(value, "ap:")
	act, found := b.action(key)
	if !ok || !found {
		_ = b.send(ctx, dest, "This request is no longer valid.", nil)
		return
	}
	// Only the Slack user linked to the account may decide.
	if cn, err := b.s.team.FindConnector(ctx, connectorSlack, user); err != nil || cn.Login != act.login {
		_ = b.send(ctx, dest, "Not allowed.", nil)
		return
	}
	cl := b.assistantClient(act.login)
	th, err := cl.GetThread(ctx, act.thread)
	if err != nil || th.PendingApproval == nil || th.PendingApproval["id"] != act.interrupt {
		b.finishButton(ctx, channel, ts, "Already handled.")
		return
	}
	decision := client.DecisionDeny
	if act.approve {
		decision = client.DecisionApprove
	}
	run, err := cl.Resume(ctx, act.thread, client.ResumeRequest{InterruptID: act.interrupt, Reason: client.ReasonToolApproval, Decision: decision, Interactive: true})
	if err != nil {
		_ = b.send(ctx, dest, "That did not work.", nil)
		return
	}
	b.mu.Lock()
	delete(b.actions, key)
	b.mu.Unlock()
	b.s.note(Caller{Login: act.login, Actor: act.login}, act.thread, "approval.slack", map[string]any{"decision": decision}, FeedEvent{Type: "message.sent"})
	b.markRun(run.ID, dest)
	b.s.AfterRun(Caller{Login: act.login, Actor: act.login}, run, "run.resume", map[string]any{"decision": decision, "via": "slack"})
	b.finishButton(ctx, channel, ts, map[bool]string{true: "Approved.", false: "Denied."}[act.approve])
}

// finishButton replaces the message that carried the buttons.
func (b *slackBridge) finishButton(ctx context.Context, channel, ts, text string) {
	_ = b.sl.call(ctx, "chat.update", map[string]any{"channel": channel, "ts": ts, "text": text, "blocks": []any{}}, nil)
}

// ---- outgoing ----

// slackText turns the assistant's markdown into Slack's mrkdwn.
func slackText(s string) string {
	s = slackLink.ReplaceAllString(s, "<$2|$1>")
	s = slackHeading.ReplaceAllString(s, "*$1*")
	s = slackBold.ReplaceAllString(s, "*$1*")
	return s
}

var (
	slackLink    = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^)\s]+)\)`)
	slackHeading = regexp.MustCompile(`(?m)^#{1,6}\s+(.+?)\s*$`)
	slackBold    = regexp.MustCompile(`\*\*(.+?)\*\*`)
)

// send posts text (split to fit Slack) and, on the last part, the blocks (buttons).
func (b *slackBridge) send(ctx context.Context, d slackDest, text string, blocks []any) error {
	text = strings.TrimSpace(slackText(text))
	if text == "" {
		text = "(empty)"
	}
	runes := []rune(text)
	for len(runes) > 3500 {
		if err := b.post(ctx, d, string(runes[:3500]), nil); err != nil {
			return err
		}
		runes = runes[3500:]
	}
	return b.post(ctx, d, string(runes), blocks)
}

func (b *slackBridge) post(ctx context.Context, d slackDest, text string, blocks []any) error {
	p := map[string]any{"channel": d.channel, "text": text, "unfurl_links": false}
	if d.threadTS != "" {
		p["thread_ts"] = d.threadTS
	}
	if blocks != nil {
		p["blocks"] = blocks
	}
	return b.sl.call(ctx, "chat.postMessage", p, nil)
}

// send on the server's side: a direct message to a Slack user.
func (sk *slackBridge) dm(ctx context.Context, user, text string) error {
	return sk.send(ctx, slackDest{channel: user}, text, nil)
}

func (b *slackBridge) onFinished(ctx context.Context, ev FeedEvent) {
	if b.s.sup == nil {
		return
	}
	if b.s.sup.isSupervisorThread(ctx, ev.ThreadID) {
		dest, ok := b.takeRun(ev.RunID)
		if !ok {
			return // a web chat: the answer is on screen
		}
		cn, err := b.s.team.GetConnector(ctx, ev.owner, connectorSlack)
		if err != nil {
			return
		}
		b.assistantResult(ctx, ev, *cn, dest)
		return
	}
	what, ok := attentionEvent(ev)
	if !ok {
		return
	}
	title := ev.ThreadID
	if t, err := b.s.H.Store.GetThread(ctx, ev.owner, ev.ThreadID); err == nil && t.Title != "" {
		title = t.Title
	}
	for _, u := range b.s.sup.recipients(ctx, ev.ProjectID, ev.owner) {
		if b.s.sup.settings(ctx, u).Watch {
			continue // the watcher puts it to the assistant, whose answer is delivered
		}
		if cn, err := b.s.team.GetConnector(ctx, u, connectorSlack); err == nil {
			_ = b.dm(ctx, cn.Account, fmt.Sprintf("Session %q %s.", title, what))
		}
	}
}

func (b *slackBridge) assistantResult(ctx context.Context, ev FeedEvent, cn store.Connector, dest slackDest) {
	login := cn.Login
	th, err := b.assistantClient(login).GetThread(ctx, ev.ThreadID)
	if err != nil {
		return
	}
	switch {
	case th.PendingApproval != nil:
		id, _ := th.PendingApproval["id"].(string)
		tool, _ := th.PendingApproval["toolName"].(string)
		preview, _ := th.PendingApproval["toolInputPreview"].(string)
		if b.markTold(id) {
			return
		}
		yes := b.addAction(slackAction{login: login, thread: ev.ThreadID, interrupt: id, approve: true})
		no := b.addAction(slackAction{login: login, thread: ev.ThreadID, interrupt: id})
		text := fmt.Sprintf("I want to run %s:\n```%s```", tool, clip(strings.ReplaceAll(preview, "```", "'''"), 1500))
		blocks := []any{
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}},
			map[string]any{"type": "actions", "elements": []any{
				map[string]any{"type": "button", "action_id": "approve", "style": "primary", "text": map[string]any{"type": "plain_text", "text": "Approve"}, "value": "ap:" + yes},
				map[string]any{"type": "button", "action_id": "deny", "style": "danger", "text": map[string]any{"type": "plain_text", "text": "Deny"}, "value": "ap:" + no},
			}},
		}
		if dest.threadTS != "" { // a mention in a channel: the request itself goes to the direct messages
			_ = b.send(ctx, dest, "I need your approval for this; I sent it to you in a direct message.", nil)
		}
		_ = b.send(ctx, slackDest{channel: cn.Account}, text, blocks)
	case th.PendingQuestion != nil:
		_ = b.send(ctx, dest, th.PendingQuestion.Question, nil)
	default:
		for i := len(th.Messages) - 1; i >= 0; i-- {
			if m := th.Messages[i]; m.Role == store.RoleAssistant && strings.TrimSpace(m.Content) != "" {
				_ = b.send(ctx, dest, m.Content, nil)
				return
			}
			if th.Messages[i].Role == store.RoleUser && th.Messages[i].Author != nil && th.Messages[i].Author.Kind != store.AuthorSystem {
				break
			}
		}
		if ev.Status == "failed" {
			_ = b.send(ctx, dest, "That run failed.", nil)
		}
	}
}
