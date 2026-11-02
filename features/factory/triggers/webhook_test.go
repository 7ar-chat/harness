package triggers_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/features/factory/triggers"
)

var secret = []byte("s3cret")

func sign(key, body []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

type hookEnv struct {
	st     *factorystore.Mem
	wh     *triggers.Webhook
	mu     sync.Mutex
	events []factory.Event
}

func newHook(t *testing.T, key []byte) *hookEnv {
	t.Helper()
	e := &hookEnv{st: factorystore.NewMem(func() time.Time { return at("2026-10-31T12:00:00Z") })}
	e.wh = triggers.NewWebhook(triggers.Options{
		Store: e.st, Controller: &controller{st: e.st}, Secret: key,
		Now: func() time.Time { return at("2026-10-31T12:00:00Z") },
		OnEvent: func(_ context.Context, ev factory.Event) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.events = append(e.events, ev)
		},
	})
	return e
}

func (e *hookEnv) add(t *testing.T, tr factory.Trigger) factory.Trigger {
	t.Helper()
	if tr.Owner == "" {
		tr.Owner = "alice"
	}
	tr.Kind, tr.Enabled = factory.TriggerWebhook, true
	got, err := e.st.PutTrigger(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	return *got
}

type delivery struct {
	event, id string
	body      string
	sig       string // "" signs with secret
	method    string
}

func (e *hookEnv) send(d delivery) *httptest.ResponseRecorder {
	method := d.method
	if method == "" {
		method = http.MethodPost
	}
	req := httptest.NewRequest(method, "/webhooks/github", strings.NewReader(d.body))
	if d.event != "" {
		req.Header.Set("X-GitHub-Event", d.event)
	}
	if d.id != "" {
		req.Header.Set("X-GitHub-Delivery", d.id)
	}
	sig := d.sig
	if sig == "" {
		sig = sign(secret, []byte(d.body))
	}
	if sig != "none" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	rec := httptest.NewRecorder()
	e.wh.ServeHTTP(rec, req)
	e.wh.Wait() // OnEvent runs after the response: let it finish before the test looks at what it saw
	return rec
}

func created(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d %q, want 202", rec.Code, rec.Body.String())
	}
	var resp struct {
		Created []string `json:"created"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if resp.Created == nil {
		t.Fatalf("body %q: created must be a list, not null", rec.Body.String())
	}
	return resp.Created
}

const issueLabeled = `{
  "action": "labeled",
  "issue": {
    "url": "https://api.github.com/repos/acme/app/issues/12",
    "html_url": "https://github.com/acme/app/issues/12",
    "number": 12, "title": "Login returns 500", "body": "Steps:\n1. open /login",
    "user": {"login": "bob"},
    "labels": [{"id": 1, "name": "bug"}, {"id": 2, "name": "agent"}],
    "state": "open"
  },
  "label": {"id": 2, "name": "agent", "color": "ededed"},
  "repository": {"id": 99, "full_name": "acme/app", "private": true},
  "sender": {"login": "carol", "type": "User"}
}`

func TestWebhookRejectsBadRequestsBeforeAnyEffect(t *testing.T) {
	tests := []struct {
		name string
		key  []byte
		d    delivery
		want int
	}{
		{"missing signature", secret, delivery{event: "ping", id: "d1", body: `{}`, sig: "none"}, http.StatusUnauthorized},
		{"wrong signature", secret, delivery{event: "ping", id: "d1", body: `{}`, sig: sign([]byte("other"), []byte(`{}`))}, http.StatusUnauthorized},
		{"signature of another body", secret, delivery{event: "ping", id: "d1", body: `{"a":1}`, sig: sign(secret, []byte(`{}`))}, http.StatusUnauthorized},
		{"signature without scheme", secret, delivery{event: "ping", id: "d1", body: `{}`, sig: hex.EncodeToString([]byte("x"))}, http.StatusUnauthorized},
		{"signature not hex", secret, delivery{event: "ping", id: "d1", body: `{}`, sig: "sha256=zz"}, http.StatusUnauthorized},
		{"sha1 signature only", secret, delivery{event: "ping", id: "d1", body: `{}`, sig: "sha1=" + strings.Repeat("0", 40)}, http.StatusUnauthorized},
		{"no secret configured", nil, delivery{event: "ping", id: "d1", body: `{}`}, http.StatusServiceUnavailable},
		{"GET", secret, delivery{method: http.MethodGet, event: "ping", id: "d1", body: `{}`}, http.StatusMethodNotAllowed},
		{"missing event header", secret, delivery{id: "d1", body: `{}`}, http.StatusBadRequest},
		{"body is not json", secret, delivery{event: "issues", id: "d1", body: `not json`}, http.StatusBadRequest},
		{"issues without issue", secret, delivery{event: "issues", id: "d1", body: `{"action":"opened"}`}, http.StatusBadRequest},
		{"body over 1 MiB", secret, delivery{event: "ping", id: "d1", body: `{"zen":"` + strings.Repeat("a", 1<<20) + `"}`}, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			e := newHook(t, tc.key)
			e.add(t, factory.Trigger{Name: "all", Event: "ping", Template: factory.Template{Title: "x"}})

			// Act
			rec := e.send(tc.d)

			// Assert
			if rec.Code != tc.want {
				t.Errorf("status = %d (%s), want %d", rec.Code, strings.TrimSpace(rec.Body.String()), tc.want)
			}
			if len(e.events) != 0 || len(allTasks(t, e.st)) != 0 {
				t.Errorf("rejected delivery had effects: %d events, %d tasks", len(e.events), len(allTasks(t, e.st)))
			}
			if seen, _ := e.st.SeenDelivery(context.Background(), "d1"); seen {
				t.Error("a rejected delivery was recorded as seen: GitHub's redelivery would be dropped")
			}
		})
	}
}

func TestWebhookAcceptsASignedDeliveryAndAnswersWithTheCreatedTasks(t *testing.T) {
	// Arrange
	e := newHook(t, secret)
	tr := e.add(t, factory.Trigger{
		Name: "agent label", Event: "issues.labeled", Match: map[string]string{"label": "agent"},
		Template: factory.Template{Title: "Fix #{{.number}}: {{.title}}", Spec: "{{.body}}\n{{.URL}}", Repo: "{{.Repo}}", BaseBranch: "main"},
	})

	// Act
	rec := e.send(delivery{event: "issues", id: "d1", body: issueLabeled})

	// Assert
	ids := created(t, rec)
	if len(ids) != 1 {
		t.Fatalf("created %v, want one task", ids)
	}
	task, err := e.st.GetTask(context.Background(), ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if task.Title != "Fix #12: Login returns 500" || task.Repo != "acme/app" || task.BaseBranch != "main" || task.Owner != "alice" ||
		!strings.HasSuffix(task.Spec, "https://github.com/acme/app/issues/12") || !strings.HasPrefix(task.Spec, "Steps:\n1. open /login") {
		t.Errorf("task = %+v", task)
	}
	wantSource := factory.Source{Kind: factory.SourceGitHub, Key: "github:issue:acme/app#12:" + tr.ID, URL: "https://github.com/acme/app/issues/12"}
	if task.Source != wantSource {
		t.Errorf("source = %+v, want %+v", task.Source, wantSource)
	}
}

func TestWebhookDuplicateDeliveryIsAnsweredAndIgnored(t *testing.T) {
	// Arrange
	e := newHook(t, secret)
	e.add(t, factory.Trigger{Name: "t", Event: "issues", Template: factory.Template{Title: "x"}})
	first := e.send(delivery{event: "issues", id: "same", body: issueLabeled})

	// Act
	second := e.send(delivery{event: "issues", id: "same", body: issueLabeled})

	// Assert
	if first.Code != http.StatusAccepted {
		t.Fatalf("first = %d", first.Code)
	}
	if second.Code != http.StatusOK || second.Body.String() != "duplicate" {
		t.Errorf("second = %d %q, want 200 duplicate", second.Code, second.Body.String())
	}
	if len(e.events) != 1 || len(allTasks(t, e.st)) != 1 {
		t.Errorf("%d OnEvent calls, %d tasks; want 1 and 1", len(e.events), len(allTasks(t, e.st)))
	}
}

func TestWebhookLabelTriggerCreatesExactlyOneTaskOnRedelivery(t *testing.T) {
	// Arrange: GitHub redelivers with a new delivery id (manual redelivery).
	e := newHook(t, secret)
	e.add(t, factory.Trigger{Name: "agent", Event: "issues.labeled", Match: map[string]string{"label": "agent"}, Template: factory.Template{Title: "Fix {{.title}}"}})

	// Act
	first := created(t, e.send(delivery{event: "issues", id: "d1", body: issueLabeled}))
	second := created(t, e.send(delivery{event: "issues", id: "d2", body: issueLabeled}))

	// Assert
	if len(first) != 1 || len(second) != 0 {
		t.Errorf("created %v then %v, want one then none", first, second)
	}
	if n := len(allTasks(t, e.st)); n != 1 {
		t.Errorf("%d tasks, want 1", n)
	}
	if len(e.events) != 2 {
		t.Errorf("OnEvent called %d times, want 2 (each delivery is a distinct event)", len(e.events))
	}
}

func TestWebhookTwoTriggersOnOneEventEachCreateTheirTask(t *testing.T) {
	// Arrange
	e := newHook(t, secret)
	e.add(t, factory.Trigger{Name: "a", Event: "issues.labeled", Template: factory.Template{Title: "A {{.number}}"}})
	e.add(t, factory.Trigger{Name: "b", Event: "issues.labeled", Template: factory.Template{Title: "B {{.number}}"}})

	// Act
	ids := created(t, e.send(delivery{event: "issues", id: "d1", body: issueLabeled}))

	// Assert
	if len(ids) != 2 || len(allTasks(t, e.st)) != 2 {
		t.Errorf("created %v, want 2 tasks", ids)
	}
}

func TestWebhookTriggerSelection(t *testing.T) {
	tests := []struct {
		name    string
		trigger factory.Trigger
		want    int
	}{
		{"exact event", factory.Trigger{Event: "issues.labeled"}, 1},
		{"event without action matches every action", factory.Trigger{Event: "issues"}, 1},
		{"other action", factory.Trigger{Event: "issues.opened"}, 0},
		{"other event", factory.Trigger{Event: "issue_comment"}, 0},
		{"prefix of another event name is not a match", factory.Trigger{Event: "issue"}, 0},
		{"match equal", factory.Trigger{Event: "issues", Match: map[string]string{"label": "agent", "repo": "acme/app"}}, 1},
		{"match differs", factory.Trigger{Event: "issues", Match: map[string]string{"label": "docs"}}, 0},
		{"one of two keys differs", factory.Trigger{Event: "issues", Match: map[string]string{"label": "agent", "repo": "acme/other"}}, 0},
		{"star matches non-empty", factory.Trigger{Event: "issues", Match: map[string]string{"label": "*"}}, 1},
		{"star does not match missing field", factory.Trigger{Event: "issues", Match: map[string]string{"conclusion": "*"}}, 0},
		{"comma list field contains the value", factory.Trigger{Event: "issues", Match: map[string]string{"labels": "bug"}}, 1},
		{"comma list field lacks the value", factory.Trigger{Event: "issues", Match: map[string]string{"labels": "wontfix"}}, 0},
		{"comma list is not a substring match", factory.Trigger{Event: "issues", Match: map[string]string{"labels": "bu"}}, 0},
		{"sender is exposed for matching", factory.Trigger{Event: "issues", Match: map[string]string{"sender": "carol"}}, 1},
		{"disabled trigger", factory.Trigger{Event: "issues", Match: map[string]string{"x": ""}}, 1}, // replaced below
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			e := newHook(t, secret)
			tc.trigger.Name = "t"
			tc.trigger.Template = factory.Template{Title: "x"}
			if tc.name == "disabled trigger" {
				tc.trigger.Match = nil
				got := e.add(t, tc.trigger)
				got.Enabled = false
				if _, err := e.st.PutTrigger(context.Background(), got); err != nil {
					t.Fatal(err)
				}
				tc.want = 0
			} else {
				e.add(t, tc.trigger)
			}

			// Act
			ids := created(t, e.send(delivery{event: "issues", id: "d1", body: issueLabeled}))

			// Assert
			if len(ids) != tc.want {
				t.Errorf("created %d tasks, want %d", len(ids), tc.want)
			}
		})
	}
}

func TestWebhookIgnoresCronTriggersAndSkipsTemplatesThatFail(t *testing.T) {
	// Arrange
	e := newHook(t, secret)
	cron := factory.Trigger{Owner: "a", Name: "cron", Kind: factory.TriggerCron, Event: "issues", Schedule: "@daily", Enabled: true, Template: factory.Template{Title: "cron"}}
	if _, err := e.st.PutTrigger(context.Background(), cron); err != nil {
		t.Fatal(err)
	}
	e.add(t, factory.Trigger{Name: "broken", Event: "issues", Template: factory.Template{Title: "{{index .title 999}}"}})
	e.add(t, factory.Trigger{Name: "syntax", Event: "issues", Template: factory.Template{Title: "{{.title"}})
	e.add(t, factory.Trigger{Name: "good", Event: "issues", Template: factory.Template{Title: "ok {{.number}}"}})

	// Act
	ids := created(t, e.send(delivery{event: "issues", id: "d1", body: issueLabeled}))

	// Assert
	if len(ids) != 1 {
		t.Fatalf("created %v, want only the good trigger's task", ids)
	}
	if task, _ := e.st.GetTask(context.Background(), ids[0]); task.Title != "ok 12" {
		t.Errorf("title = %q", task.Title)
	}
}

func TestWebhookTemplateSeesFieldsAndEventValues(t *testing.T) {
	// Arrange
	e := newHook(t, secret)
	e.add(t, factory.Trigger{Name: "t", Event: "issue_comment.created", Template: factory.Template{
		Title: "{{.Name}} on {{.Repo}}",
		Spec:  "key={{.Key}} url={{.URL}} sender={{.sender}} comment={{.comment}} missing=[{{.nothing}}]",
	}})
	body := `{"action":"created","issue":{"number":5,"title":"T","html_url":"https://github.com/acme/app/issues/5","labels":[]},
	  "comment":{"body":"/agent fix this","html_url":"https://github.com/acme/app/issues/5#issuecomment-1"},
	  "repository":{"full_name":"acme/app"},"sender":{"login":"dave"}}`

	// Act
	ids := created(t, e.send(delivery{event: "issue_comment", id: "d1", body: body}))

	// Assert
	if len(ids) != 1 {
		t.Fatalf("created %v", ids)
	}
	task, _ := e.st.GetTask(context.Background(), ids[0])
	if task.Title != "issue_comment.created on acme/app" {
		t.Errorf("title = %q", task.Title)
	}
	want := "key=github:issue:acme/app#5 url=https://github.com/acme/app/issues/5#issuecomment-1 sender=dave comment=/agent fix this missing=[]"
	if task.Spec != want {
		t.Errorf("spec = %q\nwant   %q", task.Spec, want)
	}
}

func TestWebhookTaskTemplateFieldsAreCopied(t *testing.T) {
	// Arrange
	e := newHook(t, secret)
	e.add(t, factory.Trigger{Name: "t", Event: "issues", ProjectID: "p1", Template: factory.Template{
		Title: "x", Role: factory.RolePlanner, Policy: factory.PolicyAutonomous, Model: "m", Budget: factory.Budget{MaxUSD: 2},
		MaxAttempts: 4, AutoMerge: true, Labels: []string{"factory"}, Priority: 7,
	}})

	// Act
	ids := created(t, e.send(delivery{event: "issues", id: "d1", body: issueLabeled}))

	// Assert
	task, _ := e.st.GetTask(context.Background(), ids[0])
	if task.ProjectID != "p1" || task.Role != factory.RolePlanner || task.Policy != factory.PolicyAutonomous || task.Model != "m" ||
		task.Budget.MaxUSD != 2 || task.MaxAttempts != 4 || !task.AutoMerge || task.Priority != 7 || len(task.Labels) != 1 {
		t.Errorf("task = %+v", task)
	}
}

func TestWebhookPassesEveryDeliveryToOnEventEvenWithoutTriggers(t *testing.T) {
	// Arrange
	e := newHook(t, secret)

	// Act
	ids := created(t, e.send(delivery{event: "issues", id: "d9", body: issueLabeled}))

	// Assert
	if len(ids) != 0 || len(e.events) != 1 {
		t.Fatalf("created %v, %d events", ids, len(e.events))
	}
	ev := e.events[0]
	if ev.DeliveryID != "d9" || ev.Name != "issues.labeled" || ev.Key != "github:issue:acme/app#12" || ev.At.IsZero() {
		t.Errorf("event = %+v", ev)
	}
}

func TestWebhookBotSenderIsExposedNotFiltered(t *testing.T) {
	// Arrange
	e := newHook(t, secret)
	e.add(t, factory.Trigger{Name: "t", Event: "issues", Template: factory.Template{Title: "x"}})
	body := strings.Replace(issueLabeled, `"login": "carol"`, `"login": "dependabot[bot]"`, 1)

	// Act
	ids := created(t, e.send(delivery{event: "issues", id: "d1", body: body}))

	// Assert
	if len(ids) != 1 || len(e.events) != 1 || e.events[0].Fields["sender"] != "dependabot[bot]" {
		t.Errorf("created %v, events %+v", ids, e.events)
	}
}

func TestWebhookWithoutAKeyUsesTheDeliveryForIdempotency(t *testing.T) {
	// Arrange: a ping has no issue or PR; two triggers still get one task each per delivery.
	e := newHook(t, secret)
	e.add(t, factory.Trigger{Name: "t", Event: "ping", Template: factory.Template{Title: "pinged"}})

	// Act
	first := created(t, e.send(delivery{event: "ping", id: "p1", body: `{"zen":"Keep it logically awesome.","repository":{"full_name":"acme/app"}}`}))
	second := created(t, e.send(delivery{event: "ping", id: "p2", body: `{"zen":"Keep it logically awesome."}`}))

	// Assert
	if len(first) != 1 || len(second) != 1 {
		t.Errorf("created %v then %v; each distinct delivery of a keyless event creates its task", first, second)
	}
}

func TestWebhookResponseContentType(t *testing.T) {
	// Arrange
	e := newHook(t, secret)

	// Act
	rec := e.send(delivery{event: "ping", id: "d1", body: `{}`})

	// Assert
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"created":[]`)) {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestWebhookRepoMatchIgnoresCaseButNotTheRepository(t *testing.T) {
	cases := []struct {
		name  string
		match string
		want  int
	}{
		{"same spelling", "acme/app", 1},
		{"other case", "Acme/App", 1},
		{"another repository", "acme/other", 0},
		{"a prefix is not the repository", "acme/ap", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			e := newHook(t, secret)
			e.add(t, factory.Trigger{
				Name: "t", Event: "issues.labeled", Match: map[string]string{"repo": c.match, "label": "agent"},
				Template: factory.Template{Title: "Fix #{{.number}}", Spec: "x", Repo: "acme/app"},
			})

			// Act
			rec := e.send(delivery{event: "issues", id: "d-" + c.name, body: issueLabeled})

			// Assert
			if got := len(created(t, rec)); got != c.want {
				t.Errorf("created %d tasks, want %d", got, c.want)
			}
		})
	}
}

func TestWebhookAnswersWithoutWaitingForASlowEventListener(t *testing.T) {
	// Arrange: the listener (the pull request loop calling GitHub) blocks until released. GitHub gives up
	// on a delivery after 10 seconds, so the response must not depend on it.
	release := make(chan struct{})
	started := make(chan struct{})
	st := factorystore.NewMem(nil)
	wh := triggers.NewWebhook(triggers.Options{Store: st, Controller: &controller{st: st}, Secret: secret,
		OnEvent: func(ctx context.Context, _ factory.Event) {
			close(started)
			<-release
		}})
	req := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(issueLabeled))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-GitHub-Delivery", "slow-1")
	req.Header.Set("X-Hub-Signature-256", sign(secret, []byte(issueLabeled)))
	rec := httptest.NewRecorder()

	// Act
	answered := make(chan struct{})
	go func() { wh.ServeHTTP(rec, req); close(answered) }()

	// Assert: the listener is running and the handler has already answered.
	<-started
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("the response waited for the event listener")
	}
	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", rec.Code)
	}
	close(release)
	wh.Wait()
}

func TestWebhookSurvivesAPanickingEventListener(t *testing.T) {
	// Arrange
	st := factorystore.NewMem(nil)
	wh := triggers.NewWebhook(triggers.Options{Store: st, Controller: &controller{st: st}, Secret: secret,
		OnEvent: func(context.Context, factory.Event) { panic("listener bug") }})
	req := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(issueLabeled))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-GitHub-Delivery", "panic-1")
	req.Header.Set("X-Hub-Signature-256", sign(secret, []byte(issueLabeled)))
	rec := httptest.NewRecorder()

	// Act
	wh.ServeHTTP(rec, req)
	wh.Wait()

	// Assert: the delivery is still accepted and the process is alive.
	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", rec.Code)
	}
}
