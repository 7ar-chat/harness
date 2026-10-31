package product

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/hosts/inproc"
)

// Telegram lets a user talk to their assistant from Telegram, and tells them
// when a session needs them. The server long-polls the Bot API (no public
// webhook is needed, so it works on a laptop). A user links their chat from
// the profile page: the page shows a deep link carrying a one-time code, and
// the bot's "/start <code>" ties that chat to the account. Only linked private
// chats are served.
//
// In a linked chat: a message goes to the assistant (it steers the running
// assistant run, answers its question, or starts a run); its replies come
// back as messages, and an approval it waits for arrives with Approve / Deny
// buttons. /status lists the sessions. Process-wide: with several server
// replicas only one may run the bot (Telegram allows one poller per token).
type Telegram struct {
	// Token is the bot token from @BotFather.
	Token string
	// Username is the bot's name without @ (read with getMe when empty).
	Username string
	// API is the Bot API base (default https://api.telegram.org).
	API string
	// HTTP is the client used (default: one with a 40 s timeout).
	HTTP *http.Client
	// PollSeconds is the long-poll duration (default 25).
	PollSeconds int
}

func (t *Telegram) api() string {
	if t.API != "" {
		return strings.TrimRight(t.API, "/")
	}
	return "https://api.telegram.org"
}

func (t *Telegram) client() *http.Client {
	if t.HTTP != nil {
		return t.HTTP
	}
	return &http.Client{Timeout: 40 * time.Second}
}

type tgResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
}

func (t *Telegram) call(ctx context.Context, method string, params any, out any) error {
	b, _ := json.Marshal(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.api()+"/bot"+t.Token+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client().Do(req)
	if err != nil {
		return redactToken(err, t.Token)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var r tgResponse
	if err := json.Unmarshal(data, &r); err != nil || !r.OK {
		return fmt.Errorf("telegram %s: %s", method, r.Description)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// redactToken keeps the bot token out of logged transport errors.
func redactToken(err error, token string) error {
	return errors.New(strings.ReplaceAll(err.Error(), token, "***"))
}

type linkPending struct {
	Code      string    `json:"code"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64  `json:"message_id"`
		Text      string `json:"text"`
		Chat      struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
		From struct {
			Username  string `json:"username"`
			FirstName string `json:"first_name"`
		} `json:"from"`
	} `json:"message"`
	Callback *struct {
		ID      string `json:"id"`
		Data    string `json:"data"`
		Message *struct {
			MessageID int64 `json:"message_id"`
			Chat      struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	} `json:"callback_query"`
}

// telegramBridge is the bot's side of the server.
type telegramBridge struct {
	s  *server
	t  *Telegram
	mu sync.Mutex
	// codes are the pending link codes → login.
	codes map[string]linkEntry
	// actions are the buttons sent: short key → what they do.
	actions map[string]tgAction
	// runs are the assistant runs whose result goes to Telegram: those started from it, and the watcher's.
	runs map[string]bool
	// told remembers the approvals already sent to a chat, so a status event is not announced twice.
	told map[string]bool
}

type linkEntry struct {
	login   string
	expires time.Time
}

type tgAction struct {
	login, thread, interrupt string
	approve                  bool
	expires                  time.Time
}

func newTelegramBridge(s *server) *telegramBridge {
	return &telegramBridge{s: s, t: s.opt.Telegram, codes: map[string]linkEntry{}, actions: map[string]tgAction{}, runs: map[string]bool{}, told: map[string]bool{}}
}

func (b *telegramBridge) pendingFor(login string) *linkPending {
	b.mu.Lock()
	defer b.mu.Unlock()
	for code, e := range b.codes {
		if e.login == login && time.Now().Before(e.expires) {
			return &linkPending{Code: code, URL: b.linkURL(code), ExpiresAt: e.expires}
		}
	}
	return nil
}

func (b *telegramBridge) linkURL(code string) string {
	return "https://t.me/" + b.t.Username + "?start=" + code
}

func (b *telegramBridge) newCode(login string) linkPending {
	raw := make([]byte, 9)
	_, _ = rand.Read(raw)
	code := b64(raw)
	b.mu.Lock()
	defer b.mu.Unlock()
	for c, e := range b.codes { // one pending code per user, and drop the stale ones
		if e.login == login || time.Now().After(e.expires) {
			delete(b.codes, c)
		}
	}
	exp := time.Now().Add(10 * time.Minute)
	b.codes[code] = linkEntry{login, exp}
	return linkPending{Code: code, URL: b.linkURL(code), ExpiresAt: exp}
}

func (b *telegramBridge) takeCode(code string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.codes[code]
	delete(b.codes, code)
	return e.login, ok && time.Now().Before(e.expires)
}

func (b *telegramBridge) addAction(a tgAction) string {
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

// markRun routes the result of a run to Telegram.
func (b *telegramBridge) markRun(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.runs) > 1000 {
		b.runs = map[string]bool{}
	}
	b.runs[id] = true
}

func (b *telegramBridge) takeRun(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	ok := b.runs[id]
	delete(b.runs, id)
	return ok
}

func (b *telegramBridge) action(key string) (tgAction, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	a, ok := b.actions[key]
	return a, ok && time.Now().Before(a.expires)
}

// ---- routes ----

func (s *server) telegramReady(w http.ResponseWriter) bool {
	if s.tg == nil {
		writeError(w, http.StatusNotImplemented, "not_configured", "Telegram is not configured on this server.")
		return false
	}
	return true
}

func (s *server) telegramLink(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.telegramReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.tg.newCode(c.Actor))
}

func (s *server) telegramTest(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.telegramReady(w) {
		return
	}
	cn, err := s.team.GetConnector(r.Context(), c.Actor, store.ConnectorTelegram)
	if err != nil {
		writeError(w, http.StatusConflict, "not_connected", "Connect Telegram first.")
		return
	}
	if err := s.tg.send(r.Context(), cn.Account, "Hello from your assistant. This chat is linked.", nil); err != nil {
		writeError(w, http.StatusBadGateway, "telegram_error", "Telegram could not be reached.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) telegramDisconnect(w http.ResponseWriter, r *http.Request, c Caller) {
	if err := s.team.DeleteConnector(r.Context(), c.Actor, store.ConnectorTelegram); err != nil && !errors.Is(err, store.ErrNotFound) {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- the bot ----

func (b *telegramBridge) run(ctx context.Context) {
	if b.t.Username == "" {
		var me struct {
			Username string `json:"username"`
		}
		for ctx.Err() == nil {
			if err := b.t.call(ctx, "getMe", struct{}{}, &me); err == nil {
				b.t.Username = me.Username
				break
			} else {
				slog.Warn("telegram: getMe failed", "error", err)
				sleepCtx(ctx, 10*time.Second)
			}
		}
	}
	sub, done := b.s.hub.subscribe(nil) // the bridge runs in one replica and hears every replica's events
	defer done()
	go b.deliver(ctx, sub)
	var offset int64
	for ctx.Err() == nil {
		poll := b.t.PollSeconds
		if poll <= 0 {
			poll = 25
		}
		var updates []tgUpdate
		err := b.t.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": poll, "allowed_updates": []string{"message", "callback_query"}}, &updates)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("telegram: polling failed", "error", err)
				sleepCtx(ctx, 5*time.Second)
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			b.handle(ctx, u)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (b *telegramBridge) send(ctx context.Context, chat string, text string, buttons [][]map[string]string) error {
	id, err := strconv.ParseInt(chat, 10, 64)
	if err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		text = "(empty)"
	}
	runes := []rune(text)
	for len(runes) > 3800 {
		if err := b.t.call(ctx, "sendMessage", map[string]any{"chat_id": id, "text": string(runes[:3800])}, nil); err != nil {
			return err
		}
		runes = runes[3800:]
	}
	p := map[string]any{"chat_id": id, "text": string(runes)}
	if buttons != nil {
		p["reply_markup"] = map[string]any{"inline_keyboard": buttons}
	}
	return b.t.call(ctx, "sendMessage", p, nil)
}

func (b *telegramBridge) handle(ctx context.Context, u tgUpdate) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("telegram: update handler panicked", "panic", r)
		}
	}()
	switch {
	case u.Callback != nil:
		b.handleCallback(ctx, u)
	case u.Message != nil && u.Message.Chat.Type == "private" && u.Message.Text != "":
		b.handleMessage(ctx, u)
	}
}

func (b *telegramBridge) handleMessage(ctx context.Context, u tgUpdate) {
	m := u.Message
	chat := strconv.FormatInt(m.Chat.ID, 10)
	text := strings.TrimSpace(m.Text)
	if code, ok := strings.CutPrefix(text, "/start"); ok && strings.TrimSpace(code) != "" {
		b.link(ctx, chat, strings.TrimSpace(code), m.From.Username, m.From.FirstName)
		return
	}
	cn, err := b.s.team.FindConnector(ctx, store.ConnectorTelegram, chat)
	if err != nil {
		_ = b.send(ctx, chat, "This chat is not linked to an account. Open your profile in the web app and press Connect Telegram.", nil)
		return
	}
	login := cn.Login
	switch strings.Fields(text)[0] {
	case "/start", "/help":
		_ = b.send(ctx, chat, "I am your assistant. Write to me as in the web app. /status lists your sessions.", nil)
		return
	case "/status":
		_ = b.send(ctx, chat, b.status(ctx, login), nil)
		return
	}
	reply, err := b.toAssistant(ctx, login, text)
	if err != nil {
		slog.Warn("telegram: delivering to the assistant failed", "error", err)
		reply = "I could not pass that on: " + err.Error()
	}
	if reply != "" {
		_ = b.send(ctx, chat, reply, nil)
	}
}

func (b *telegramBridge) link(ctx context.Context, chat, code, username, first string) {
	login, ok := b.takeCode(code)
	if !ok {
		_ = b.send(ctx, chat, "This link expired. Press Connect Telegram again in the web app.", nil)
		return
	}
	label := first
	if username != "" {
		label = "@" + username
	}
	data := b.s.box.seal([]byte(chat))
	err := b.s.team.SetConnector(ctx, store.Connector{Login: login, Kind: store.ConnectorTelegram, Account: chat, Label: label, Data: data})
	switch {
	case errors.Is(err, store.ErrConflict):
		_ = b.send(ctx, chat, "This Telegram chat is already linked to another account.", nil)
	case err != nil:
		_ = b.send(ctx, chat, "Linking failed; try again.", nil)
	default:
		b.s.note(Caller{Actor: login}, "", "connector.telegram", nil, FeedEvent{Type: "project.changed", affects: login})
		_ = b.send(ctx, chat, "Linked. Write to me here as you would in the web app; I will tell you when a session needs you.", nil)
	}
}

// assistantClient acts for the user on their assistant thread.
func (b *telegramBridge) assistantClient(login string) client.Client {
	s := b.s
	return inproc.New(s.H, login, inproc.WithModels(s.O.Models, s.O.DefaultModel),
		inproc.WithAuthor(harness.Author{Login: login, Kind: store.AuthorUser}))
}

// toAssistant puts a Telegram message into the assistant's thread: it answers
// a pending question, steers a running run, or starts a run. The reply comes
// back through deliver when the run ends.
func (b *telegramBridge) toAssistant(ctx context.Context, login, text string) (string, error) {
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
		return "I am waiting for your decision on a request; use its Approve / Deny buttons.", nil
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
	b.markRun(run.ID)
	b.s.AfterRun(Caller{Login: login, Actor: login}, run, "run.send", map[string]any{"via": "telegram"})
	return "", nil
}

func (b *telegramBridge) status(ctx context.Context, login string) string {
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

func (b *telegramBridge) handleCallback(ctx context.Context, u tgUpdate) {
	cb := u.Callback
	answer := func(text string) {
		_ = b.t.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": cb.ID, "text": text}, nil)
	}
	key, ok := strings.CutPrefix(cb.Data, "ap:")
	act, found := b.action(key)
	if !ok || !found || cb.Message == nil {
		answer("This request is no longer valid.")
		return
	}
	// Only the chat linked to the account may decide.
	chat := strconv.FormatInt(cb.Message.Chat.ID, 10)
	if cn, err := b.s.team.FindConnector(ctx, store.ConnectorTelegram, chat); err != nil || cn.Login != act.login {
		answer("Not allowed.")
		return
	}
	cl := b.assistantClient(act.login)
	th, err := cl.GetThread(ctx, act.thread)
	if err != nil || th.PendingApproval == nil || th.PendingApproval["id"] != act.interrupt {
		answer("Already handled.")
		return
	}
	decision := client.DecisionDeny
	if act.approve {
		decision = client.DecisionApprove
	}
	run, err := cl.Resume(ctx, act.thread, client.ResumeRequest{InterruptID: act.interrupt, Reason: client.ReasonToolApproval, Decision: decision, Interactive: true})
	if err != nil {
		answer("That did not work.")
		return
	}
	b.mu.Lock()
	delete(b.actions, key)
	b.mu.Unlock()
	b.s.note(Caller{Login: act.login, Actor: act.login}, act.thread, "approval.telegram", map[string]any{"decision": decision}, FeedEvent{Type: "message.sent"})
	b.markRun(run.ID)
	b.s.AfterRun(Caller{Login: act.login, Actor: act.login}, run, "run.resume", map[string]any{"decision": decision, "via": "telegram"})
	answer(map[bool]string{true: "Approved", false: "Denied"}[act.approve])
	_ = b.t.call(ctx, "editMessageReplyMarkup", map[string]any{"chat_id": cb.Message.Chat.ID, "message_id": cb.Message.MessageID, "reply_markup": map[string]any{"inline_keyboard": [][]any{}}}, nil)
}

// deliver sends the assistant's results to Telegram, and tells linked users
// about sessions that need them.
func (b *telegramBridge) deliver(ctx context.Context, sub *subscriber) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-sub.ch:
			if ev.Type != "run.finished" || ev.ThreadID == "" {
				continue
			}
			b.onFinished(ctx, ev)
		}
	}
}

func (b *telegramBridge) onFinished(ctx context.Context, ev FeedEvent) {
	if b.s.sup == nil {
		return
	}
	if b.s.sup.isSupervisorThread(ctx, ev.ThreadID) {
		if !b.takeRun(ev.RunID) {
			return // a web chat: the answer is on screen
		}
		cn, err := b.s.team.GetConnector(ctx, ev.owner, store.ConnectorTelegram)
		if err != nil {
			return
		}
		b.assistantResult(ctx, ev, *cn)
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
		if cn, err := b.s.team.GetConnector(ctx, u, store.ConnectorTelegram); err == nil {
			_ = b.send(ctx, cn.Account, fmt.Sprintf("Session %q %s.", title, what), nil)
		}
	}
}

func (b *telegramBridge) assistantResult(ctx context.Context, ev FeedEvent, cn store.Connector) {
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
		yes := b.addAction(tgAction{login: login, thread: ev.ThreadID, interrupt: id, approve: true})
		no := b.addAction(tgAction{login: login, thread: ev.ThreadID, interrupt: id})
		text := fmt.Sprintf("I want to run %s:\n%s", tool, clip(preview, 1500))
		_ = b.send(ctx, cn.Account, text, [][]map[string]string{{{"text": "Approve", "callback_data": "ap:" + yes}, {"text": "Deny", "callback_data": "ap:" + no}}})
	case th.PendingQuestion != nil:
		_ = b.send(ctx, cn.Account, th.PendingQuestion.Question, nil)
	default:
		for i := len(th.Messages) - 1; i >= 0; i-- {
			if m := th.Messages[i]; m.Role == store.RoleAssistant && strings.TrimSpace(m.Content) != "" {
				_ = b.send(ctx, cn.Account, m.Content, nil)
				return
			}
			if th.Messages[i].Role == store.RoleUser && th.Messages[i].Author != nil && th.Messages[i].Author.Kind != store.AuthorSystem {
				break
			}
		}
		if ev.Status == "failed" {
			_ = b.send(ctx, cn.Account, "That run failed.", nil)
		}
	}
}

// markTold reports whether an approval was already announced (and remembers it otherwise).
func (b *telegramBridge) markTold(id string) bool {
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
