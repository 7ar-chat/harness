// Package notify is the notification rules engine: what happened (an Event), who wants to hear about it (a project's
// rules, a person's preferences) and where it goes (in the app, Telegram, Slack, email, a webhook, WeCom, Discord).
//
// The rules are plain. A project's owner chooses, per event, which of the project's channels get it and whether the
// person it concerns is told too. Each person chooses, per event, where their own items reach them. Defaults are
// sensible: the app plus the channels the person linked, for what needs a human; the quiet events stay in the app.
// Quiet hours hold what would reach someone's phone, a digest batches the non-urgent, and the same event within a few
// minutes collapses into one message. Everything that was sent, held or refused is in the delivery log.
package notify

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// Kind is what happened.
type Kind string

// The events a rule can name.
const (
	TaskBlocked     Kind = "task.blocked"
	TaskReview      Kind = "task.review"
	TaskFailed      Kind = "task.failed"
	TaskDone        Kind = "task.done"
	ApprovalWaiting Kind = "approval.waiting"
	QuestionWaiting Kind = "question.waiting"
	BudgetWarn      Kind = "budget.warn"
	BudgetReached   Kind = "budget.reached"
	ScheduleFailed  Kind = "schedule.failed"
	GateWaiting     Kind = "workflow.gate"
	WorkflowDone    Kind = "workflow.finished"
	DigestReady     Kind = "digest.ready"
	MemberJoined    Kind = "member.joined"
	// Test is the message of a Test button; it is sent whatever the rules say.
	Test Kind = "test"
)

// EventInfo describes an event for a person choosing what to receive.
type EventInfo struct {
	Kind  Kind   `json:"kind"`
	Label string `json:"label"`
	Help  string `json:"help"`
	// Urgent events need a person: they are never batched into a digest. Default says whether the person's linked
	// channels (Telegram, Slack...) get it without them choosing; otherwise it stays in the app.
	Urgent  bool `json:"urgent"`
	Default bool `json:"default"`
}

var events = []EventInfo{
	{TaskBlocked, "A task is blocked", "A task stopped and needs a person to unblock it.", true, true},
	{TaskReview, "A task is ready for review", "A task finished its work and its pull request is ready.", false, true},
	{TaskFailed, "A task failed", "A task ran out of attempts or hit an error.", true, true},
	{TaskDone, "A task is done or merged", "A task finished or its pull request was merged.", false, false},
	{ApprovalWaiting, "An approval is waiting", "An agent asks for permission before it goes on.", true, true},
	{QuestionWaiting, "A question is waiting", "An agent asks you something and waits for the answer.", true, true},
	{BudgetWarn, "Spend reached 80% of the cap", "A monthly spend cap is almost used up.", false, true},
	{BudgetReached, "Spend cap reached", "New runs are refused until the cap is raised.", true, true},
	{ScheduleFailed, "A scheduled run failed", "A schedule could not run or its run failed.", true, true},
	{GateWaiting, "A workflow gate is waiting", "A workflow stopped at a step that needs a person.", true, true},
	{WorkflowDone, "A workflow run finished", "A workflow run completed or failed.", false, false},
	{DigestReady, "The weekly digest is ready", "The weekly summary of what the team did.", false, false},
	{MemberJoined, "A new member joined", "Someone joined the project.", false, false},
}

// Events lists every event a rule can name, in the order the pages show them.
func Events() []EventInfo { return append([]EventInfo(nil), events...) }

// Info returns the description of k (ok false for an unknown kind).
func Info(k Kind) (EventInfo, bool) {
	for _, e := range events {
		if e.Kind == k {
			return e, true
		}
	}
	return EventInfo{}, false
}

// Via is a way a person is reached.
type Via string

// The personal channels. InApp is always on: the app's inbox and live feed.
const (
	InApp    Via = "inapp"
	Telegram Via = "telegram"
	Slack    Via = "slack"
	Email    Via = "email"
	Webhook  Via = "webhook"
)

// Vias lists the personal channels in display order.
func Vias() []Via { return []Via{InApp, Telegram, Slack, Email, Webhook} }

// Channel kinds of a project: a group destination the owner added.
const (
	KindWebhook = "webhook"
	KindWeCom   = "wecom"
	KindDiscord = "discord"
)

// ChannelKinds lists the kinds of project channel.
func ChannelKinds() []string { return []string{KindWebhook, KindWeCom, KindDiscord} }

// Event is something that happened, as the product reports it.
type Event struct {
	// Key identifies the occurrence for de-duplication ("task:123:blocked"); empty means kind, project and title.
	Key  string `json:"key,omitempty"`
	Kind Kind   `json:"kind"`
	// ProjectID and ProjectName say where it happened ("" for a personal event).
	ProjectID   string `json:"projectId,omitempty"`
	ProjectName string `json:"projectName,omitempty"`
	Title       string `json:"title"`
	Body        string `json:"body,omitempty"`
	// URL opens the thing in the app.
	URL string `json:"url,omitempty"`
	// Login is the person it concerns (the owner of the task, the creator of the schedule); "" for none.
	Login string         `json:"login,omitempty"`
	At    time.Time      `json:"at"`
	Data  map[string]any `json:"data,omitempty"`
	// Window overrides the project's de-duplication window for this event (a monthly warning is told once a month).
	Window time.Duration `json:"-"`
	// Skip lists personal channels the sender already covered another way.
	Skip []Via `json:"-"`
}

// Rule is what a project does with one event.
type Rule struct {
	// Channels are the ids of the project's channels that get it.
	Channels []string `json:"channels"`
	// Person: the person it concerns is told as well, where their own preferences say.
	Person bool `json:"person"`
}

// DefaultDedupeMinutes is how long the same event is collapsed unless the project chose otherwise.
const DefaultDedupeMinutes = 10

// Rules are a project's notification rules.
type Rules struct {
	ProjectID string `json:"projectId"`
	// Events holds a rule per event; an event without one uses DefaultRule.
	Events map[Kind]Rule `json:"events"`
	// DedupeMinutes: the same event key within this many minutes is sent once (0: every one is sent).
	DedupeMinutes int `json:"dedupeMinutes"`
	// Digest batches the non-urgent events of the project's channels into one message an hour.
	Digest    bool      `json:"digest"`
	UpdatedBy string    `json:"updatedBy,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// DefaultRule is the rule of an event the project did not set: no project channel, the person is told.
func DefaultRule() Rule { return Rule{Channels: []string{}, Person: true} }

// DefaultRules are the rules of a project that never changed them.
func DefaultRules(project string) Rules {
	return Rules{ProjectID: project, Events: map[Kind]Rule{}, DedupeMinutes: DefaultDedupeMinutes}
}

// RuleFor is the rule of k, with the default when the project set none.
func (r Rules) RuleFor(k Kind) Rule {
	if x, ok := r.Events[k]; ok {
		if x.Channels == nil {
			x.Channels = []string{}
		}
		return x
	}
	return DefaultRule()
}

// Validate checks rules a person submitted: known events, plausible numbers. channelIDs are the project's channels.
func (r Rules) Validate(channelIDs map[string]bool) error {
	if r.DedupeMinutes < 0 || r.DedupeMinutes > 24*60 {
		return errors.New("dedupeMinutes must be between 0 and 1440")
	}
	for k, rule := range r.Events {
		if _, ok := Info(k); !ok {
			return errors.New("unknown event " + string(k))
		}
		for _, id := range rule.Channels {
			if !channelIDs[id] {
				return errors.New("unknown channel " + id)
			}
		}
	}
	return nil
}

// Channel is a destination of a project: a webhook, a WeCom group bot or a Discord webhook. Its address and secret
// are sealed by the server; a Channel never leaves the server with them, only the Hint (the host).
type Channel struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	// Hint is what a person may be shown of the address: its host.
	Hint string `json:"hint"`
	// HasSecret: a signing secret was set (webhook only).
	HasSecret bool `json:"hasSecret"`
	Enabled   bool `json:"enabled"`
	// Sealed is the encrypted {url, secret}; never sent anywhere.
	Sealed    []byte    `json:"sealed,omitempty"`
	CreatedBy string    `json:"createdBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// Prefs are a person's choices.
type Prefs struct {
	Login string `json:"login"`
	// Events holds, per event, where it reaches the person; an event without an entry uses Defaults.
	Events map[Kind][]Via `json:"events"`
	// QuietStart and QuietEnd ("22:00", "07:00") hold what would reach a phone, a chat or a mailbox until the quiet
	// hours end; both empty: no quiet hours. Timezone is an IANA name (default UTC).
	QuietStart string `json:"quietStart"`
	QuietEnd   string `json:"quietEnd"`
	Timezone   string `json:"timezone"`
	// Digest batches the non-urgent events outside the app into one message an hour.
	Digest bool `json:"digest"`
	// WebhookHint and SealedWebhook are the person's own webhook ("webhook" via), sealed like a project's.
	WebhookHint   string    `json:"webhookHint"`
	WebhookSecret bool      `json:"webhookSecret"`
	SealedWebhook []byte    `json:"sealedWebhook,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Defaults is where an event reaches a person who chose nothing: the app, plus the linked channels for the events
// that need them.
func Defaults(k Kind, linked []Via) []Via {
	out := []Via{InApp}
	if info, ok := Info(k); ok && info.Default {
		for _, v := range linked {
			if v != InApp && v != Webhook {
				out = append(out, v)
			}
		}
	}
	return out
}

// ViasFor is where k reaches the person: their choice, else the defaults. InApp is always included.
func (p Prefs) ViasFor(k Kind, linked []Via) []Via {
	chosen, ok := p.Events[k]
	if !ok {
		return Defaults(k, linked)
	}
	seen := map[Via]bool{InApp: true}
	out := []Via{InApp}
	for _, v := range chosen {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// ValidatePrefs checks preferences a person submitted.
func (p Prefs) Validate() error {
	for k, vias := range p.Events {
		if _, ok := Info(k); !ok {
			return errors.New("unknown event " + string(k))
		}
		for _, v := range vias {
			if !validVia(v) {
				return errors.New("unknown channel " + string(v))
			}
		}
	}
	if (p.QuietStart == "") != (p.QuietEnd == "") {
		return errors.New("set both the start and the end of the quiet hours, or neither")
	}
	if p.QuietStart != "" {
		if _, err := parseClock(p.QuietStart); err != nil {
			return errors.New("quietStart must look like 22:00")
		}
		if _, err := parseClock(p.QuietEnd); err != nil {
			return errors.New("quietEnd must look like 07:00")
		}
		if p.QuietStart == p.QuietEnd {
			return errors.New("the quiet hours start and end at the same time")
		}
	}
	if p.Timezone != "" {
		if _, err := time.LoadLocation(p.Timezone); err != nil {
			return errors.New("unknown timezone " + p.Timezone)
		}
	}
	return nil
}

func validVia(v Via) bool {
	for _, x := range Vias() {
		if x == v {
			return true
		}
	}
	return false
}

func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

// Quiet reports whether the quiet hours of the person cover now, and when they end if they do.
func (p Prefs) Quiet(now time.Time) (bool, time.Time) {
	if p.QuietStart == "" || p.QuietEnd == "" {
		return false, time.Time{}
	}
	loc := time.UTC
	if p.Timezone != "" {
		if l, err := time.LoadLocation(p.Timezone); err == nil {
			loc = l
		}
	}
	start, err1 := parseClock(p.QuietStart)
	end, err2 := parseClock(p.QuietEnd)
	if err1 != nil || err2 != nil {
		return false, time.Time{}
	}
	n := now.In(loc)
	cur := n.Hour()*60 + n.Minute()
	in := false
	if start < end {
		in = cur >= start && cur < end
	} else {
		in = cur >= start || cur < end
	}
	if !in {
		return false, time.Time{}
	}
	day := time.Date(n.Year(), n.Month(), n.Day(), end/60, end%60, 0, 0, loc)
	if !day.After(n) {
		day = day.AddDate(0, 0, 1)
	}
	return true, day
}

// Delivery states of a log entry.
const (
	Delivered = "delivered"
	Failed    = "failed"
	// Held: waiting for the quiet hours to end or for the hourly digest.
	Held = "held"
	// Collapsed: the same event was sent a moment ago.
	Collapsed = "collapsed"
	// Skipped: nothing could be sent (not linked, no address).
	Skipped = "skipped"
)

// LogEntry is one delivery attempt, as the log shows it.
type LogEntry struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"projectId,omitempty"`
	Login     string    `json:"login,omitempty"`
	Event     Kind      `json:"event"`
	Title     string    `json:"title"`
	Channel   string    `json:"channel"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	At        time.Time `json:"at"`
}

// MaxLog is how many entries a store keeps per project (and for the entries without one).
const MaxLog = 200

// ErrNotFound is returned by a store for a missing channel.
var ErrNotFound = errors.New("notify: not found")

// SortChannels orders channels by name, then id, so every store lists them alike.
func SortChannels(cs []Channel) {
	sort.Slice(cs, func(a, b int) bool {
		if x, y := strings.ToLower(cs[a].Name), strings.ToLower(cs[b].Name); x != y {
			return x < y
		}
		return cs[a].ID < cs[b].ID
	})
}
