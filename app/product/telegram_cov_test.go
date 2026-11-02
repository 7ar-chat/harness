package product

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/store"
)

const cnTGToken = "123:SECRET"

// cnTG is a Telegram Bot API whose failures the test steers and whose traffic it records.
type cnTG struct {
	srv *httptest.Server

	mu      sync.Mutex
	queue   []map[string]any
	nextID  int
	sent    []map[string]any
	answers []map[string]any
	calls   []cnTGCall
	fail    map[string]string // method → description of the error to answer with
	wake    chan struct{}
}

type cnTGCall struct {
	method string
	body   map[string]any
}

func newCnTG(t *testing.T) *cnTG {
	f := &cnTG{nextID: 1, fail: map[string]string{}, wake: make(chan struct{}, 1)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.calls = append(f.calls, cnTGCall{method, in})
		desc, failing := f.fail[method]
		f.mu.Unlock()
		if !strings.Contains(r.URL.Path, "/bot"+cnTGToken+"/") {
			http.Error(w, "wrong token", http.StatusUnauthorized)
			return
		}
		if failing {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": desc})
			return
		}
		ok := func(v any) { json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": v}) }
		switch method {
		case "getMe":
			ok(map[string]any{"username": "discovered_bot"})
		case "getUpdates":
			for range 2 {
				f.mu.Lock()
				if len(f.queue) > 0 {
					q := f.queue
					f.queue = nil
					f.mu.Unlock()
					ok(q)
					return
				}
				f.mu.Unlock()
				select {
				case <-f.wake:
				case <-time.After(100 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
			}
			ok([]any{})
		case "sendMessage":
			f.mu.Lock()
			f.sent = append(f.sent, in)
			f.mu.Unlock()
			ok(map[string]any{"message_id": 1})
		case "answerCallbackQuery":
			f.mu.Lock()
			f.answers = append(f.answers, in)
			f.mu.Unlock()
			ok(true)
		default:
			ok(true)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *cnTG) setFail(method, desc string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if desc == "" {
		delete(f.fail, method)
		return
	}
	f.fail[method] = desc
}

func (f *cnTG) push(u map[string]any) {
	f.mu.Lock()
	u["update_id"] = f.nextID
	f.nextID++
	f.queue = append(f.queue, u)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *cnTG) say(chat int, text string) { f.sayAs(chat, "private", text, "alice_tg", "Alice") }

func (f *cnTG) sayAs(chat int, kind, text, username, first string) {
	f.push(map[string]any{"message": map[string]any{"message_id": 1, "text": text,
		"chat": map[string]any{"id": chat, "type": kind}, "from": map[string]any{"username": username, "first_name": first}}})
}

func (f *cnTG) press(chat int, data string) {
	f.push(map[string]any{"callback_query": map[string]any{"id": "cb-" + data, "data": data,
		"message": map[string]any{"message_id": 7, "chat": map[string]any{"id": chat}}}})
}

func (f *cnTG) messagesTo(chat int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.sent {
		if int(m["chat_id"].(float64)) == chat {
			out = append(out, m["text"].(string))
		}
	}
	return out
}

func (f *cnTG) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.method == method {
			n++
		}
	}
	return n
}

func (f *cnTG) callsOf(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c.body)
		}
	}
	return out
}

func (f *cnTG) answerTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.answers {
		out = append(out, a["text"].(string))
	}
	return out
}

// waitSent waits for a message to chat containing text and returns it.
func (f *cnTG) waitSent(t *testing.T, chat int, contains string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, m := range f.sent {
			if int(m["chat_id"].(float64)) == chat && strings.Contains(m["text"].(string), contains) {
				f.mu.Unlock()
				return m
			}
		}
		f.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.Fatalf("no message to chat %d containing %q; sent: %v", chat, contains, f.sent)
	return nil
}

func (f *cnTG) waitAnswer(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, a := range f.answerTexts() {
			if strings.Contains(a, text) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no callback answer containing %q; answers: %v", text, f.answerTexts())
}

// cnBridge is a bridge over a server with a team store and a feed hub but no harness: enough for everything that
// does not talk to the assistant.
func cnBridge(t *testing.T, tg *cnTG, tm store.TeamStore) *telegramBridge {
	t.Helper()
	box, _ := newSecretBox(cnKey)
	s := &server{team: tm, box: box, hub: newHub()}
	s.opt.Telegram = &Telegram{Token: cnTGToken, Username: "harness_bot", API: tg.srv.URL, PollSeconds: 1}
	return newTelegramBridge(s)
}

// cnPanicErr makes cnFaultTeam.FindConnector panic for the given account.
type cnPanicErr struct{ account string }

func (e cnPanicErr) Error() string { return "panic on " + e.account }

func (f *cnFaultTeam) FindConnector(ctx context.Context, kind, account string) (*store.Connector, error) {
	if err := f.err("FindConnector"); err != nil {
		if p, ok := err.(cnPanicErr); ok && p.account == account {
			panic(p)
		}
		if _, ok := err.(cnPanicErr); !ok {
			return nil, err
		}
	}
	return f.TeamStore.FindConnector(ctx, kind, account)
}

func TestTelegramCallReportsAPIAndTransportErrorsWithoutTheToken(t *testing.T) {
	// Arrange
	tg := newCnTG(t)
	api := &Telegram{Token: cnTGToken, API: tg.srv.URL + "/"}
	ctx := context.Background()

	// Act / Assert: the bot API's own error text reaches the caller.
	tg.setFail("sendMessage", "Bad Request: chat not found")
	if err := api.call(ctx, "sendMessage", map[string]any{"chat_id": 1}, nil); err == nil || !strings.Contains(err.Error(), "chat not found") || !strings.Contains(err.Error(), "sendMessage") {
		t.Fatalf("API error = %v", err)
	}
	tg.setFail("sendMessage", "")
	// A result decodes into out; a bad result type is an error rather than a zero value.
	var me struct {
		Username string `json:"username"`
	}
	if err := api.call(ctx, "getMe", struct{}{}, &me); err != nil || me.Username != "discovered_bot" {
		t.Fatalf("getMe = %+v, %v", me, err)
	}
	var wrong []int
	if err := api.call(ctx, "getMe", struct{}{}, &wrong); err == nil {
		t.Fatal("decoded an object into a slice")
	}
	// A wrong token is a 401 with a non-JSON body: reported as an error.
	if err := (&Telegram{Token: "bad", API: tg.srv.URL}).call(ctx, "getMe", struct{}{}, nil); err == nil {
		t.Fatal("a rejected request succeeded")
	}
	// A transport error carries the request URL, which contains the token: it must be redacted.
	tg.srv.Close()
	err := api.call(ctx, "getMe", struct{}{}, nil)
	if err == nil || strings.Contains(err.Error(), cnTGToken) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("transport error = %v", err)
	}
}

func TestTelegramDefaults(t *testing.T) {
	// Arrange / Act
	tg := &Telegram{}

	// Assert
	if tg.api() != "https://api.telegram.org" {
		t.Fatalf("default API = %q", tg.api())
	}
	if c := tg.client(); c.Timeout < 30*time.Second {
		t.Fatalf("default client timeout = %v: a long poll needs more than its own duration", c.Timeout)
	}
	custom := &http.Client{}
	if (&Telegram{HTTP: custom}).client() != custom {
		t.Fatal("custom client ignored")
	}
}

func TestTelegramSendSplitsLongTextOnRuneBoundaries(t *testing.T) {
	// Arrange
	tg := newCnTG(t)
	b := cnBridge(t, tg, newCnFaultTeam())
	ctx := context.Background()
	buttons := [][]map[string]string{{{"text": "Approve", "callback_data": "ap:x"}}}
	long := strings.Repeat("é", 3800*2+10) // two bytes per rune: a byte split would corrupt it

	// Act
	if err := b.send(ctx, "42", long, buttons); err != nil {
		t.Fatal(err)
	}

	// Assert: three messages, each valid UTF-8 of at most 3800 runes, in order, and only the last has the buttons.
	sent := tg.callsOf("sendMessage")
	if len(sent) != 3 {
		t.Fatalf("sent %d messages, want 3", len(sent))
	}
	var joined strings.Builder
	for i, m := range sent {
		text := m["text"].(string)
		if !utf8.ValidString(text) || utf8.RuneCountInString(text) > 3800 || m["chat_id"] != float64(42) {
			t.Fatalf("message %d: %d runes, chat %v", i, utf8.RuneCountInString(text), m["chat_id"])
		}
		if _, has := m["reply_markup"]; has != (i == 2) {
			t.Fatalf("message %d has buttons = %v", i, has)
		}
		joined.WriteString(text)
	}
	if joined.String() != long {
		t.Fatal("the pieces do not add up to the text")
	}
	// Exactly the limit is one message; surrounding whitespace is trimmed; nothing at all is "(empty)".
	tg.mu.Lock()
	tg.calls = nil
	tg.mu.Unlock()
	_ = b.send(ctx, "42", "  "+strings.Repeat("a", 3800)+"\n", nil)
	_ = b.send(ctx, "42", " \n\t", nil)
	got := tg.callsOf("sendMessage")
	if len(got) != 2 || utf8.RuneCountInString(got[0]["text"].(string)) != 3800 || got[1]["text"] != "(empty)" {
		t.Fatalf("limit/empty messages = %v", got)
	}
	if _, has := got[0]["reply_markup"]; has {
		t.Fatal("buttons added without being asked for")
	}
}

func TestTelegramSendFailsFastOnBadChatsAndAPIErrors(t *testing.T) {
	// Arrange
	tg := newCnTG(t)
	b := cnBridge(t, tg, newCnFaultTeam())

	// Act / Assert: a chat id that is not a number never reaches the API.
	if err := b.send(context.Background(), "alice", "hi", nil); err == nil || tg.count("sendMessage") != 0 {
		t.Fatalf("non-numeric chat: %v, %d calls", err, tg.count("sendMessage"))
	}
	// A failing first piece stops the rest: no half-delivered long answer after an error.
	tg.setFail("sendMessage", "Forbidden: bot was blocked by the user")
	err := b.send(context.Background(), "7", strings.Repeat("x", 9000), nil)
	if err == nil || !strings.Contains(err.Error(), "blocked") || tg.count("sendMessage") != 1 {
		t.Fatalf("err %v after %d calls", err, tg.count("sendMessage"))
	}
}

func TestTelegramLinkCodesAreSingleUseExpiringAndPerUser(t *testing.T) {
	// Arrange
	b := cnBridge(t, newCnTG(t), newCnFaultTeam())

	// Act
	first := b.newCode("alice")
	second := b.newCode("alice")
	other := b.newCode("bob")

	// Assert: a user has one pending code (a new one replaces the old), and codes differ per user.
	if first.Code == second.Code || second.Code == other.Code {
		t.Fatal("codes repeat")
	}
	if _, ok := b.takeCode(first.Code); ok {
		t.Fatal("a replaced code still works")
	}
	if p := b.pendingFor("alice"); p == nil || p.Code != second.Code || p.URL != "https://t.me/harness_bot?start="+second.Code {
		t.Fatalf("pending = %+v", p)
	}
	if b.pendingFor("carol") != nil {
		t.Fatal("pending code for a user without one")
	}
	if login, ok := b.takeCode(second.Code); !ok || login != "alice" {
		t.Fatalf("take = %q, %v", login, ok)
	}
	if _, ok := b.takeCode(second.Code); ok {
		t.Fatal("a code worked twice")
	}
	// An expired code is refused (and consumed), is no longer pending, and is dropped when the next code is made.
	b.mu.Lock()
	b.codes[other.Code] = linkEntry{"bob", time.Now().Add(-time.Second)}
	b.mu.Unlock()
	if b.pendingFor("bob") != nil {
		t.Fatal("an expired code is shown as pending")
	}
	if _, ok := b.takeCode(other.Code); ok {
		t.Fatal("an expired code linked an account")
	}
	b.mu.Lock()
	b.codes["stale"] = linkEntry{"zed", time.Now().Add(-time.Hour)}
	b.mu.Unlock()
	b.newCode("carol")
	b.mu.Lock()
	_, kept := b.codes["stale"]
	b.mu.Unlock()
	if kept {
		t.Fatal("stale codes pile up")
	}
	if time.Until(first.ExpiresAt) < 9*time.Minute || time.Until(first.ExpiresAt) > 10*time.Minute {
		t.Fatalf("expiry in %v", time.Until(first.ExpiresAt))
	}
}

func TestTelegramActionsExpireAndBookkeepingIsBounded(t *testing.T) {
	// Arrange
	b := cnBridge(t, newCnTG(t), newCnFaultTeam())

	// Act / Assert: an action is found until it expires; adding one prunes the expired.
	key := b.addAction(tgAction{login: "alice", thread: "t", interrupt: "i", approve: true})
	if a, ok := b.action(key); !ok || a.login != "alice" || !a.approve {
		t.Fatalf("action = %+v, %v", a, ok)
	}
	if _, ok := b.action("nope"); ok {
		t.Fatal("unknown key found")
	}
	b.mu.Lock()
	a := b.actions[key]
	a.expires = time.Now().Add(-time.Second)
	b.actions[key] = a
	b.mu.Unlock()
	if _, ok := b.action(key); ok {
		t.Fatal("an expired action can still be pressed")
	}
	b.addAction(tgAction{})
	b.mu.Lock()
	_, kept := b.actions[key]
	b.mu.Unlock()
	if kept {
		t.Fatal("expired actions are never pruned")
	}
	// Runs: taken once; the table is reset rather than growing without bound.
	b.markRun("r1")
	if !b.takeRun("r1") || b.takeRun("r1") || b.takeRun("never") {
		t.Fatal("takeRun must be true exactly once for a marked run")
	}
	for i := range 1001 {
		b.markRun(fmt.Sprint("run-", i))
	}
	b.markRun("last")
	b.mu.Lock()
	n := len(b.runs)
	b.mu.Unlock()
	if n > 1002 || !b.takeRun("last") {
		t.Fatalf("runs table size %d", n)
	}
	// An approval is announced once; the memory is bounded too.
	if b.markTold("ap1") || !b.markTold("ap1") || b.markTold("ap2") {
		t.Fatal("markTold must be false the first time and true after")
	}
	for i := range 1002 {
		b.markTold(fmt.Sprint("a-", i))
	}
	b.mu.Lock()
	n = len(b.told)
	b.mu.Unlock()
	if n > 1002 {
		t.Fatalf("told table size %d", n)
	}
}

func TestTelegramLinkOutcomes(t *testing.T) {
	// Arrange
	tg := newCnTG(t)
	tm := newCnFaultTeam()
	cnUsers(t, tm, "alice", "bob")
	b := cnBridge(t, tg, tm)
	ctx := context.Background()
	account := func(login string) *store.Connector {
		c, err := tm.GetConnector(ctx, login, store.ConnectorTelegram)
		if err != nil {
			return nil
		}
		return c
	}

	// Act / Assert: a link without a username is labelled with the first name, and the chat id is the account.
	b.link(ctx, "111", b.newCode("alice").Code, "", "Alice")
	if c := account("alice"); c == nil || c.Account != "111" || c.Label != "Alice" {
		t.Fatalf("connector = %+v", c)
	}
	if cn, _ := tm.GetConnector(ctx, "alice", store.ConnectorTelegram); string(cn.Data) == "111" {
		t.Fatal("the chat id is stored in clear in the connector data")
	}
	tg.waitSent(t, 111, "Linked")
	// A username wins over the first name.
	b.link(ctx, "111", b.newCode("alice").Code, "alice_tg", "Alice")
	if c := account("alice"); c.Label != "@alice_tg" {
		t.Fatalf("label = %q", c.Label)
	}
	// The same chat cannot be linked to a second account, and the first link is untouched.
	b.link(ctx, "111", b.newCode("bob").Code, "bob_tg", "Bob")
	tg.waitSent(t, 111, "already linked to another account")
	if account("bob") != nil || account("alice").Account != "111" {
		t.Fatal("a chat was linked to two accounts")
	}
	// A store failure is reported to the chat and links nothing.
	tm.failOn("SetConnector", errCnBoom)
	b.link(ctx, "222", b.newCode("bob").Code, "bob_tg", "Bob")
	tg.waitSent(t, 222, "Linking failed")
	tm.failOn("SetConnector", nil)
	if account("bob") != nil {
		t.Fatal("a failed link left a connector")
	}
	// An unknown or expired code tells the user to start again, and the code is not revealed as valid or not.
	b.link(ctx, "333", "bogus", "x", "X")
	tg.waitSent(t, 333, "expired")
}

func TestTelegramPollingLoop(t *testing.T) {
	// Arrange: a bridge whose bot username is unknown, a linked chat 111, and a store that panics for chat 666.
	tg := newCnTG(t)
	tm := newCnFaultTeam()
	cnUsers(t, tm, "alice")
	b := cnBridge(t, tg, tm)
	b.t.Username = ""
	box, _ := newSecretBox(cnKey)
	if err := tm.SetConnector(context.Background(), store.Connector{Login: "alice", Kind: store.ConnectorTelegram, Account: "111", Label: "@a", Data: box.seal([]byte("111"))}); err != nil {
		t.Fatal(err)
	}
	tm.failOn("FindConnector", cnPanicErr{"666"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the polling loop did not stop")
		}
	})

	// Act
	tg.sayAs(-100, "group", "hello group", "u", "U")                                                         // groups are not served
	tg.push(map[string]any{"message": map[string]any{"chat": map[string]any{"id": 111, "type": "private"}}}) // no text
	tg.push(map[string]any{"edited_message": map[string]any{}})                                              // not a kind of update we handle
	tg.say(666, "this update makes the handler panic")
	tg.say(111, "/help")
	tg.say(222, "/start")  // /start without a code from an unlinked chat
	tg.say(222, "/start ") // trailing space is not a code either

	// Assert: the loop survives the panic and answers what comes after it.
	tg.waitSent(t, 111, "I am your assistant")
	tg.waitSent(t, 222, "not linked")
	if got := tg.messagesTo(222); len(got) != 2 {
		t.Fatalf("chat 222 got %v", got)
	}
	if got := tg.messagesTo(-100); len(got) != 0 {
		t.Fatalf("a group chat was answered: %v", got)
	}
	if got := tg.messagesTo(666); len(got) != 0 {
		t.Fatalf("answered an update whose handler panicked: %v", got)
	}
	if tg.count("getMe") != 1 || b.t.Username != "discovered_bot" {
		t.Fatalf("getMe calls %d, username %q", tg.count("getMe"), b.t.Username)
	}
	if p := b.newCode("alice"); !strings.HasPrefix(p.URL, "https://t.me/discovered_bot?start=") {
		t.Fatalf("link url %q uses an unresolved bot name", p.URL)
	}
	// The offset moves past the last update, so nothing is handled twice.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		polls := tg.callsOf("getUpdates")
		if last := polls[len(polls)-1]; len(polls) > 1 && last["offset"] == float64(8) && last["timeout"] == float64(1) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("getUpdates offsets: %v", tg.callsOf("getUpdates"))
}

func TestTelegramPollingLoopStopsWhileRetrying(t *testing.T) {
	for name, method := range map[string]string{"getMe fails": "getMe", "getUpdates fails": "getUpdates"} {
		t.Run(name, func(t *testing.T) {
			// Arrange: the API is down; the loop is in its back-off wait.
			tg := newCnTG(t)
			tg.setFail(method, "Bad Gateway")
			b := cnBridge(t, tg, newCnFaultTeam())
			if method == "getMe" {
				b.t.Username = ""
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { b.run(ctx); close(done) }()
			deadline := time.Now().Add(5 * time.Second)
			for tg.count(method) == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}

			// Act
			cancel()

			// Assert: shutdown does not wait for the 5-10 s back-off.
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("the loop kept running after its context ended")
			}
			if tg.count(method) != 1 {
				t.Fatalf("%s called %d times before the back-off", method, tg.count(method))
			}
		})
	}
}

func TestTelegramCallbacksRefuseStaleAndForeignButtons(t *testing.T) {
	// Arrange
	tg := newCnTG(t)
	tm := newCnFaultTeam()
	cnUsers(t, tm, "alice")
	b := cnBridge(t, tg, tm)
	ctx := context.Background()
	live := b.addAction(tgAction{login: "alice", thread: "t", interrupt: "i", approve: true})
	expired := b.addAction(tgAction{login: "alice", thread: "t", interrupt: "i"})
	b.mu.Lock()
	a := b.actions[expired]
	a.expires = time.Now().Add(-time.Minute)
	b.actions[expired] = a
	b.mu.Unlock()
	press := func(data string, withMessage bool) {
		u := tgUpdate{Callback: &struct {
			ID      string `json:"id"`
			Data    string `json:"data"`
			Message *struct {
				MessageID int64 `json:"message_id"`
				Chat      struct {
					ID int64 `json:"id"`
				} `json:"chat"`
			} `json:"message"`
		}{ID: "cb", Data: data}}
		if withMessage {
			u.Callback.Message = &struct {
				MessageID int64 `json:"message_id"`
				Chat      struct {
					ID int64 `json:"id"`
				} `json:"chat"`
			}{MessageID: 5}
			u.Callback.Message.Chat.ID = 999
		}
		b.handle(ctx, u)
	}

	// Act
	press("garbage", true)     // not our prefix
	press("ap:unknown", true)  // unknown key
	press("ap:"+expired, true) // expired
	press("ap:"+live, false)   // the message the button was on is gone
	press("ap:"+live, true)    // from a chat that is not linked to anyone

	// Assert
	answers := tg.answerTexts()
	want := []string{"no longer valid", "no longer valid", "no longer valid", "no longer valid", "Not allowed"}
	if len(answers) != len(want) {
		t.Fatalf("answers = %v", answers)
	}
	for i, w := range want {
		if !strings.Contains(answers[i], w) {
			t.Errorf("answer %d = %q, want %q", i, answers[i], w)
		}
	}
	// A refused press keeps the button usable for the right chat.
	if _, ok := b.action(live); !ok {
		t.Fatal("a refused press consumed the action")
	}
}

// ---- through the assistant ----

// cnTGModel plays the assistant (it has list_sessions) and the project sessions, by what the user last wrote.
func cnTGModel(target func() string) *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		assistant := false
		for _, tl := range c.Tools {
			assistant = assistant || toolName(tl) == "list_sessions"
		}
		if c.Messages[len(c.Messages)-1].Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("done")}
		}
		said := ""
		for i := len(c.Messages) - 1; i >= 0; i-- {
			if c.Messages[i].Role == ai.RoleUser {
				said = c.Messages[i].Content
				break
			}
		}
		switch {
		case strings.Contains(said, "block"):
			return llmtest.Turn{Chunks: llmtest.Text("working"), Block: true}
		case strings.Contains(said, "boom"):
			return llmtest.Turn{Err: errCnBoom}
		case strings.Contains(said, "long"):
			return llmtest.Turn{Chunks: llmtest.Text(strings.Repeat("a", 3000) + "\n" + strings.Repeat("b", 3000))}
		case strings.Contains(said, "ask"):
			return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "q1", "ask_user", `{"question":"Which colour?"}`), "tool_calls")}}
		case assistant && strings.Contains(said, "approve"):
			return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "session_send", `{"thread_id":"`+target()+`","message":"please add tests"}`), "tool_calls")}}
		}
		return llmtest.Turn{Chunks: llmtest.Text("ack")}
	})
}

type cnTGEnv struct {
	*rig
	tg      *cnTG
	tm      *cnFaultTeam
	project string
	target  string
}

// newCnTGEnv has alice (owner), bob (member) and carol (viewer) of project "api", all known to the store.
func newCnTGEnv(t *testing.T) *cnTGEnv {
	t.Helper()
	e := &cnTGEnv{tg: newCnTG(t), tm: newCnFaultTeam()}
	cnUsers(t, e.tm, "alice", "bob", "carol")
	e.rig = newRig(t, rigOpts{team: true, model: cnTGModel(func() string { return e.target }), sup: NewSupervisor(e.tm), prod: Options{Team: e.tm, SecretKey: cnKey,
		Telegram: &Telegram{Token: cnTGToken, Username: "harness_bot", API: e.tg.srv.URL, PollSeconds: 1}}})
	e.project = e.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)["id"].(string)
	e.json("PUT", "/api/projects/"+e.project+"/members/bob", "alice", `{"role":"member"}`, 204)
	e.json("PUT", "/api/projects/"+e.project+"/members/carol", "alice", `{"role":"viewer"}`, 204)
	return e
}

func (e *cnTGEnv) link(t *testing.T, user string, chat int) {
	t.Helper()
	code := e.json("POST", "/api/connectors/telegram/link", user, `{}`, 200)["code"].(string)
	e.tg.say(chat, "/start "+code)
	e.tg.waitSent(t, chat, "Linked")
}

func (e *cnTGEnv) projectThread(user, title string) string {
	return e.json("POST", "/api/threads", user, `{"projectId":"`+e.project+`","title":"`+title+`"}`, 201)["id"].(string)
}

func TestTelegramConnectorRoutes(t *testing.T) {
	// Arrange
	e := newCnTGEnv(t)

	// Act / Assert: nothing to disconnect is not an error; a store failure is.
	if got := e.do("DELETE", "/api/connectors/telegram", "alice", "").StatusCode; got != 204 {
		t.Fatalf("disconnect while not linked = %d", got)
	}
	e.tm.failOn("DeleteConnector", errCnBoom)
	if got := e.do("DELETE", "/api/connectors/telegram", "alice", "").StatusCode; got != 500 {
		t.Fatalf("disconnect with a failing store = %d", got)
	}
	e.tm.failOn("DeleteConnector", nil)
	// A new link code replaces the previous one, which then no longer links anything.
	first := e.json("POST", "/api/connectors/telegram/link", "alice", `{}`, 200)
	second := e.json("POST", "/api/connectors/telegram/link", "alice", `{}`, 200)
	if first["code"] == second["code"] || second["url"] != "https://t.me/harness_bot?start="+second["code"].(string) {
		t.Fatalf("links %v %v", first, second)
	}
	e.tg.say(111, "/start "+first["code"].(string))
	e.tg.waitSent(t, 111, "expired")
	e.tg.say(111, "/start "+second["code"].(string))
	e.tg.waitSent(t, 111, "Linked")
	// Once linked the listing shows the account and no pending code; a test message that Telegram refuses is a 502.
	body := rawBody(e.do("GET", "/api/connectors", "alice", "").Body)
	if !strings.Contains(body, `"account":"111"`) || strings.Contains(body, second["code"].(string)) {
		t.Fatalf("connectors = %s", body)
	}
	e.tg.setFail("sendMessage", "Forbidden: bot was blocked by the user")
	if got := cnErrCode(e.json("POST", "/api/connectors/telegram/test", "alice", `{}`, 502)); got != "telegram_error" {
		t.Fatalf("code %q", got)
	}
	e.tg.setFail("sendMessage", "")
	// Linking is per account: bob sees no pending code of alice's.
	if body := rawBody(e.do("GET", "/api/connectors", "bob", "").Body); strings.Contains(body, `"pending"`) {
		t.Fatalf("bob sees a pending link: %s", body)
	}
}

func TestTelegramCommandsAndReplies(t *testing.T) {
	// Arrange
	e := newCnTGEnv(t)
	e.link(t, "alice", 111)

	// Act / Assert: /help and /start explain; /status says when nothing is going on.
	e.tg.say(111, "/help")
	e.tg.waitSent(t, 111, "/status lists your sessions")
	e.tg.say(111, "/start")
	deadline := time.Now().Add(5 * time.Second)
	for len(e.tg.messagesTo(111)) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := e.tg.messagesTo(111); !strings.Contains(got[len(got)-1], "I am your assistant") {
		t.Fatalf("/start (no code) answer = %v", got)
	}
	e.tg.say(111, "/status")
	e.tg.waitSent(t, 111, "No session is running or waiting.")

	// A session that is running shows up with its status and title; the assistant's own thread and idle ones do not.
	busy := e.projectThread("alice", "Fix login")
	e.projectThread("alice", "Idle one")
	e.json("POST", "/api/threads/"+busy+"/runs", "alice", `{"message":"block"}`, 202)
	e.tg.say(111, "/status")
	m := e.tg.waitSent(t, 111, "Fix login")
	text := m["text"].(string)
	if !strings.Contains(text, "running: Fix login") || strings.Contains(text, "Idle one") || strings.Contains(text, "Assistant") {
		t.Fatalf("status = %q", text)
	}
}

func TestTelegramRepliesAreDeliveredWhenTheRunEnds(t *testing.T) {
	// Arrange
	e := newCnTGEnv(t)
	e.link(t, "alice", 111)

	// Act / Assert: a long answer arrives in pieces that add up.
	e.tg.say(111, "write something long")
	deadline := time.Now().Add(10 * time.Second)
	for len(e.tg.messagesTo(111)) < 3 && time.Now().Before(deadline) { // the confirmation and the two pieces
		time.Sleep(5 * time.Millisecond)
	}
	var got strings.Builder
	for _, s := range e.tg.messagesTo(111)[1:] { // after the "Linked" confirmation
		got.WriteString(s)
	}
	if want := strings.Repeat("a", 3000) + "\n" + strings.Repeat("b", 3000); got.String() != want {
		t.Fatalf("the long answer arrived as %d bytes in %d messages", got.Len(), len(e.tg.messagesTo(111)))
	}
	if n := len(e.tg.messagesTo(111)); n != 3 { // "Linked", then two pieces
		t.Fatalf("messages = %d, want the confirmation and two pieces", n)
	}
	// A run that fails before the assistant answers is reported rather than left silent.
	e.tg.say(111, "this will boom")
	e.tg.waitSent(t, 111, "That run failed.")
	// A message from the web chat is not copied to Telegram: only runs started from Telegram are delivered.
	web := e.json("GET", "/api/supervisor", "alice", "", 200)["id"].(string)
	before := len(e.tg.messagesTo(111))
	e.waitIdle(web, "alice")
	finished(t, e.stream(e.startRun("alice", web, "hello from the web"), "alice"))
	e.waitIdle(web, "alice")
	e.tg.say(111, "/help") // an answer after the web run proves the earlier event had its chance
	e.tg.waitSent(t, 111, "I am your assistant")
	if n := len(e.tg.messagesTo(111)); n != before+1 {
		t.Fatalf("a web-chat answer leaked to Telegram: %v", e.tg.messagesTo(111))
	}
}

func TestTelegramMessageSteersARunningAssistant(t *testing.T) {
	// Arrange: the assistant is busy with a blocked run started from Telegram.
	e := newCnTGEnv(t)
	e.link(t, "alice", 111)
	sup := e.json("GET", "/api/supervisor", "alice", "", 200)["id"].(string) // created first: its creation is not atomic
	e.tg.say(111, "please block")
	var run string
	deadline := time.Now().Add(10 * time.Second)
	for run == "" && time.Now().Before(deadline) {
		if ar, ok := e.json("GET", "/api/threads/"+sup, "alice", "", 200)["activeRun"].(map[string]any); ok {
			run = ar["id"].(string)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if run == "" {
		t.Fatalf("the assistant never started: sent %v, thread %v", e.tg.messagesTo(111), e.json("GET", "/api/threads/"+sup, "alice", "", 200))
	}

	// Act
	e.tg.say(111, "and block again")
	e.tg.say(111, "/help")
	e.tg.waitSent(t, 111, "I am your assistant")

	// Assert: no error was reported to the user and no second run was started.
	for _, m := range e.tg.messagesTo(111) {
		if strings.Contains(m, "could not pass") {
			t.Fatalf("steering failed: %s", m)
		}
	}
	if ar, _ := e.json("GET", "/api/threads/"+sup, "alice", "", 200)["activeRun"].(map[string]any); ar == nil || ar["id"] != run {
		t.Fatalf("active run = %v, want %s", ar, run)
	}
	e.json("POST", "/api/runs/"+run+"/cancel", "alice", "", 202)
}

func TestTelegramApprovalButtons(t *testing.T) {
	// Arrange: the assistant wants to message a session, which needs approval.
	e := newCnTGEnv(t)
	e.target = e.projectThread("alice", "Target")
	e.link(t, "alice", 111)
	e.link(t, "bob", 222)
	e.tg.say(111, "please approve this")
	m := e.tg.waitSent(t, 111, "session_send")
	kb := m["reply_markup"].(map[string]any)["inline_keyboard"].([]any)[0].([]any)
	approve, deny := kb[0].(map[string]any)["callback_data"].(string), kb[1].(map[string]any)["callback_data"].(string)

	// A message while the decision is pending is not sent on to the assistant.
	e.tg.say(111, "hurry up")
	e.tg.waitSent(t, 111, "waiting for your decision")
	// Another linked account cannot decide for alice.
	e.tg.press(222, deny)
	e.tg.waitAnswer(t, "Not allowed")

	// Act: alice denies.
	e.tg.press(111, deny)
	e.tg.waitAnswer(t, "Denied")

	// Assert: the keyboard is removed, the assistant continues with the refusal, and the session got nothing.
	deadline := time.Now().Add(10 * time.Second)
	for e.tg.count("editMessageReplyMarkup") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if edits := e.tg.callsOf("editMessageReplyMarkup"); len(edits) != 1 || edits[0]["chat_id"] != float64(111) || edits[0]["message_id"] != float64(7) {
		t.Fatalf("keyboard edits = %v", edits)
	}
	e.tg.waitSent(t, 111, "done")
	msgs, _ := e.json("GET", "/api/threads/"+e.target, "alice", "", 200)["messages"].([]any)
	for _, msg := range msgs {
		if strings.Contains(msg.(map[string]any)["content"].(string), "add tests") {
			t.Fatalf("a denied message reached the session: %v", msg)
		}
	}
	// Pressing the other button of the same request afterwards, or the same one again, changes nothing.
	e.tg.press(111, approve)
	e.tg.waitAnswer(t, "Already handled")
	e.tg.press(111, deny)
	e.tg.waitAnswer(t, "no longer valid")
}

func TestTelegramQuestionsAreAnsweredFromTheChat(t *testing.T) {
	// Arrange
	e := newCnTGEnv(t)
	e.link(t, "alice", 111)

	// Act: the assistant asks, the user answers in the chat.
	e.tg.say(111, "please ask me something")
	e.tg.waitSent(t, 111, "Which colour?")
	e.tg.say(111, "blue")

	// Assert: the answer resumed the run (it ended with the model's closing "done") and is recorded as the tool result.
	e.tg.waitSent(t, 111, "done")
	sup := e.json("GET", "/api/supervisor", "alice", "", 200)
	var answered bool
	for _, m := range sup["messages"].([]any) {
		m := m.(map[string]any)
		answered = answered || (m["role"] == "tool" && strings.Contains(m["content"].(string), "blue"))
	}
	if !answered || sup["pendingQuestion"] != nil {
		t.Fatalf("supervisor thread = %v", sup)
	}
}

func TestTelegramTellsLinkedMembersWhenASessionNeedsThem(t *testing.T) {
	// Arrange: alice (owner) and carol (viewer) are linked; bob (member) is not.
	e := newCnTGEnv(t)
	e.link(t, "alice", 111)
	e.link(t, "carol", 333)
	th := e.projectThread("alice", "Fix login")

	// Act: a run in the project's session fails.
	e.json("POST", "/api/threads/"+th+"/runs", "alice", `{"message":"boom"}`, 202)

	// Assert: the member who can act is told, with the session's title; a viewer is not.
	e.tg.waitSent(t, 111, `Session "Fix login" failed.`)
	e.tg.say(333, "/help")
	e.tg.waitSent(t, 333, "I am your assistant")
	for _, m := range e.tg.messagesTo(333) {
		if strings.Contains(m, "Fix login") {
			t.Fatalf("a viewer was notified: %q", m)
		}
	}
	// A run that completes normally is not an event worth a message.
	ok := e.projectThread("alice", "Quiet task")
	finished(t, e.stream(e.startRun("alice", ok, "all fine"), "alice"))
	e.tg.say(111, "/help")
	deadline := time.Now().Add(5 * time.Second)
	for len(e.tg.messagesTo(111)) < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for _, m := range e.tg.messagesTo(111) {
		if strings.Contains(m, "Quiet task") {
			t.Fatalf("notified about a completed run: %q", m)
		}
	}
}
