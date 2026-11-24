package notify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Dispatcher applies the rules to events: it works out who gets what where, collapses repeats, holds what the quiet
// hours or the digest say, sends the rest, and logs every outcome. It is safe for concurrent use.
type Dispatcher struct {
	Store Store
	// Senders by channel: KindWebhook, KindWeCom, KindDiscord for project channels, and string(Telegram), string(Slack),
	// string(Email), string(Webhook) for people.
	Senders map[string]Sender
	// Open decrypts the sealed address and secret of a channel (nil: nothing can be sent to one).
	Open func(sealed []byte) (url, secret string, err error)
	// Linked says which personal channels the person has set up (Telegram, Slack, Email); nil: none.
	Linked func(ctx context.Context, login string) []Via
	// Redact masks credentials in the text of a message before it leaves (nil: none).
	Redact func(string) string
	// Now replaces time.Now (tests).
	Now func() time.Time

	mu    sync.Mutex
	seen  map[string]time.Time
	queue []queued
}

// sealedPair is what a channel seals.
type sealedPair struct {
	URL    string `json:"url"`
	Secret string `json:"secret,omitempty"`
}

// SealPair serialises the address and secret for sealing.
func SealPair(url, secret string) []byte {
	b, _ := json.Marshal(sealedPair{url, secret})
	return b
}

// OpenPair is the inverse of SealPair.
func OpenPair(b []byte) (url, secret string, err error) {
	var p sealedPair
	if err = json.Unmarshal(b, &p); err != nil {
		return "", "", err
	}
	return p.URL, p.Secret, nil
}

type queued struct {
	key   string // the target: "chan:<id>" or "person:<login>:<via>"
	label string
	due   time.Time
	t     Target
	m     Message
	quiet bool // held by quiet hours (the person's) rather than a digest
	login string
	// project is whose log the delivery goes to.
	project string
	prefs   bool // personal target
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (d *Dispatcher) redact(s string) string {
	if d.Redact == nil {
		return s
	}
	return d.Redact(s)
}

func (d *Dispatcher) message(e Event, test bool) Message {
	m := Message{Event: e.Kind, ProjectID: e.ProjectID, ProjectName: e.ProjectName, Title: d.redact(e.Title), Text: d.redact(e.Body), URL: e.URL, At: e.At, Test: test}
	if m.ProjectName == "" {
		m.ProjectName = e.ProjectID
	}
	if len(e.Data) > 0 {
		m.Data = map[string]any{}
		for k, v := range e.Data {
			if s, ok := v.(string); ok {
				v = d.redact(s)
			}
			m.Data[k] = v
		}
	}
	return m
}

func (d *Dispatcher) log(ctx context.Context, e Event, login, channel, status string, err error) {
	ent := LogEntry{ID: newID(), ProjectID: e.ProjectID, Login: login, Event: e.Kind, Title: d.redact(e.Title), Channel: channel, Status: status, At: d.now()}
	if err != nil {
		ent.Error = d.redact(err.Error())
		if len(ent.Error) > 300 {
			ent.Error = ent.Error[:300]
		}
	}
	if err := d.Store.AppendLog(ctx, ent); err != nil {
		slog.Warn("notify: writing the delivery log failed", "error", err)
	}
}

// duplicate reports whether the target got this key within the window, and records the send otherwise.
func (d *Dispatcher) duplicate(target, key string, window time.Duration) bool {
	if window <= 0 {
		return false
	}
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen == nil {
		d.seen = map[string]time.Time{}
	}
	if len(d.seen) > 5000 {
		for k, t := range d.seen {
			if now.Sub(t) > 24*time.Hour {
				delete(d.seen, k)
			}
		}
	}
	k := target + "|" + key
	if t, ok := d.seen[k]; ok && now.Sub(t) < window {
		return true
	}
	d.seen[k] = now
	return false
}

// ChannelLabel is how the log names a project channel.
func ChannelLabel(c Channel) string { return c.Name + " (" + c.Kind + ")" }

// ViaLabel is how the log names a personal channel.
func ViaLabel(v Via) string {
	switch v {
	case InApp:
		return "In the app"
	case Telegram:
		return "Telegram"
	case Slack:
		return "Slack message"
	case Email:
		return "Email"
	case Webhook:
		return "Your webhook"
	}
	return string(v)
}

// Emit applies the rules to e. It returns when every delivery was attempted or held.
func (d *Dispatcher) Emit(ctx context.Context, e Event) {
	if e.Kind == "" || d.Store == nil {
		return
	}
	if e.At.IsZero() {
		e.At = d.now()
	}
	if e.Key == "" {
		e.Key = string(e.Kind) + "|" + e.ProjectID + "|" + e.Title
	}
	info, _ := Info(e.Kind)
	rules := DefaultRules(e.ProjectID)
	if e.ProjectID != "" {
		if r, err := d.Store.GetRules(ctx, e.ProjectID); err == nil {
			rules = r
		}
	}
	rule := rules.RuleFor(e.Kind)
	window := time.Duration(rules.DedupeMinutes) * time.Minute
	if e.Window > 0 {
		window = e.Window
	}
	msg := d.message(e, false)

	// The project's channels.
	for _, id := range rule.Channels {
		c, err := d.Store.GetChannel(ctx, e.ProjectID, id)
		if err != nil || !c.Enabled {
			continue
		}
		label := ChannelLabel(c)
		if d.duplicate("chan:"+c.ID, e.Key, window) {
			d.log(ctx, e, "", label, Collapsed, nil)
			continue
		}
		t, err := d.target(c.Sealed)
		if err != nil {
			d.log(ctx, e, "", label, Failed, fmt.Errorf("the address could not be read; add the channel again"))
			continue
		}
		if rules.Digest && !info.Urgent {
			d.hold(queued{key: "chan:" + c.ID, label: label, due: d.now().Add(time.Hour), t: t, m: msg, project: e.ProjectID}, e, "")
			continue
		}
		d.deliver(ctx, e, "", c.Kind, label, t, msg)
	}

	// The person.
	if rule.Person && e.Login != "" {
		d.tellPerson(ctx, e, msg, info, window)
	}
}

// Target opens the sealed address and secret of a channel or a personal webhook.
func (d *Dispatcher) Target(sealed []byte) (Target, error) { return d.target(sealed) }

func (d *Dispatcher) target(sealed []byte) (Target, error) {
	if d.Open == nil {
		return Target{}, fmt.Errorf("no key")
	}
	u, s, err := d.Open(sealed)
	return Target{URL: u, Secret: s}, err
}

func (d *Dispatcher) tellPerson(ctx context.Context, e Event, msg Message, info EventInfo, window time.Duration) {
	prefs, err := d.Store.GetPrefs(ctx, e.Login)
	if err != nil {
		return
	}
	var linked []Via
	if d.Linked != nil {
		linked = d.Linked(ctx, e.Login)
	}
	haveLink := map[Via]bool{}
	for _, v := range linked {
		haveLink[v] = true
	}
	if prefs.SealedWebhook != nil {
		haveLink[Webhook] = true
	}
	quiet, until := prefs.Quiet(d.now())
	for _, v := range prefs.ViasFor(e.Kind, linked) {
		if v == InApp {
			continue // the feed and the inbox are written by the product itself
		}
		if slices.Contains(e.Skip, v) {
			continue
		}
		label := ViaLabel(v)
		if !haveLink[v] {
			if _, chosen := prefs.Events[e.Kind]; chosen {
				d.log(ctx, e, e.Login, label, Skipped, fmt.Errorf("%s is not set up for you", strings.ToLower(label)))
			}
			continue
		}
		if d.duplicate("person:"+e.Login+":"+string(v), e.Key, window) {
			d.log(ctx, e, e.Login, label, Collapsed, nil)
			continue
		}
		t := Target{Login: e.Login}
		if v == Webhook {
			if t, err = d.target(prefs.SealedWebhook); err != nil {
				d.log(ctx, e, e.Login, label, Failed, fmt.Errorf("your webhook address could not be read; save it again"))
				continue
			}
			t.Login = e.Login
		}
		q := queued{key: "person:" + e.Login + ":" + string(v), label: label, t: t, m: msg, login: e.Login, project: e.ProjectID, prefs: true}
		switch {
		case quiet:
			q.due, q.quiet = until, true
			d.hold(q, e, e.Login)
		case prefs.Digest && !info.Urgent:
			q.due = d.now().Add(time.Hour)
			d.hold(q, e, e.Login)
		default:
			d.deliver(ctx, e, e.Login, string(v), label, t, msg)
		}
	}
}

func (d *Dispatcher) hold(q queued, e Event, login string) {
	d.mu.Lock()
	d.queue = append(d.queue, q)
	d.mu.Unlock()
	d.log(context.Background(), e, login, q.label, Held, nil)
}

func (d *Dispatcher) deliver(ctx context.Context, e Event, login, kind, label string, t Target, m Message) {
	s := d.Senders[kind]
	if s == nil {
		d.log(ctx, e, login, label, Skipped, fmt.Errorf("this server cannot send %s", strings.ToLower(label)))
		return
	}
	if err := s.Send(ctx, t, m); err != nil {
		d.log(ctx, e, login, label, Failed, err)
		return
	}
	d.log(ctx, e, login, label, Delivered, nil)
}

// Held returns how many messages wait for the quiet hours to end or for the digest.
func (d *Dispatcher) Held() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.queue)
}

// Flush sends what was held and is due: a person's messages once their quiet hours are over, and the digests an
// hour after the first message of a batch. The messages of one target go as one. Call it every minute.
func (d *Dispatcher) Flush(ctx context.Context) {
	now := d.now()
	d.mu.Lock()
	groups := map[string][]queued{}
	var rest []queued
	for _, q := range d.queue {
		groups[q.key] = append(groups[q.key], q)
	}
	var keys []string
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var send [][]queued
	for _, k := range keys {
		g := groups[k]
		due := false
		for _, q := range g {
			if !q.due.After(now) {
				due = true
			}
		}
		if due && g[0].prefs {
			// still quiet: keep waiting
			if p, err := d.Store.GetPrefs(ctx, g[0].login); err == nil {
				if quiet, until := p.Quiet(now); quiet {
					for i := range g {
						g[i].due = until
					}
					due = false
				}
			}
		}
		if due {
			send = append(send, g)
		} else {
			rest = append(rest, g...)
		}
	}
	d.queue = rest
	d.mu.Unlock()
	for _, g := range send {
		d.sendBatch(ctx, g)
	}
}

func (d *Dispatcher) sendBatch(ctx context.Context, g []queued) {
	first := g[0]
	var m Message
	if len(g) == 1 {
		m = first.m
	} else {
		var lines []string
		for _, q := range g {
			l := "- "
			if q.m.ProjectName != "" {
				l += "[" + q.m.ProjectName + "] "
			}
			l += q.m.Title
			if q.m.URL != "" {
				l += " " + q.m.URL
			}
			lines = append(lines, l)
		}
		m = Message{Event: DigestReady, ProjectID: first.project, ProjectName: first.m.ProjectName, Title: fmt.Sprintf("%d updates", len(g)), Text: strings.Join(lines, "\n"), At: d.now()}
		if first.prefs {
			m.ProjectName = ""
		}
	}
	kind := first.label
	var sk string
	switch {
	case strings.HasPrefix(first.key, "chan:"):
		if c, err := d.Store.GetChannel(ctx, first.project, strings.TrimPrefix(first.key, "chan:")); err == nil {
			sk = c.Kind
		}
	default:
		sk = first.key[strings.LastIndex(first.key, ":")+1:]
	}
	e := Event{Kind: m.Event, ProjectID: first.project, Title: m.Title}
	d.deliver(ctx, e, first.login, sk, kind, first.t, m)
}

// Test sends a clearly marked test message to one target and returns the outcome. It ignores rules, quiet hours and
// de-duplication, and is logged like any delivery.
func (d *Dispatcher) Test(ctx context.Context, project, projectName, login, kind, label string, t Target) error {
	s := d.Senders[kind]
	e := Event{Kind: Test, ProjectID: project, Title: "Test message from 7ar", Login: login}
	if s == nil {
		err := fmt.Errorf("this server cannot send %s", strings.ToLower(label))
		d.log(ctx, e, login, label, Skipped, err)
		return err
	}
	m := Message{Event: Test, ProjectID: project, ProjectName: projectName, Title: "Test message from 7ar", Text: "If you can read this, notifications reach you here. Nothing needs your attention.", At: d.now(), Test: true}
	err := s.Send(ctx, t, m)
	if err != nil {
		d.log(ctx, e, login, label, Failed, err)
		return err
	}
	d.log(ctx, e, login, label, Delivered, nil)
	return nil
}
