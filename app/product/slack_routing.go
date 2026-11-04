package product

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/7ar-chat/harness/core/store"
)

// Where Slack notifications go.
//
// A project may name a Slack channel (project settings). Then what happens to its sessions is announced there once,
// for the whole team: a question (with its choices as buttons), a failure, a run waiting for an approval. A tool
// approval is still decided in a direct message, by the person it concerns, never in the channel. A project without
// a channel keeps the earlier behaviour: every linked member is told in a direct message.

// slackChannelID matches a channel id as Slack writes it (public C…, private G…).
var slackChannelID = regexp.MustCompile(`^[CG][A-Z0-9]{8,}$`)

// resolveChannel turns what an owner typed (a channel id, or #name) into the channel id. An empty input clears.
func (b *slackBridge) resolveChannel(ctx context.Context, in string) (string, error) {
	in = strings.TrimSpace(in)
	if in == "" || slackChannelID.MatchString(in) {
		return in, nil
	}
	name := strings.ToLower(strings.TrimPrefix(in, "#"))
	cursor := ""
	for page := 0; page < 20; page++ {
		var out struct {
			Channels []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"channels"`
			Meta struct {
				Next string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		err := b.sl.call(ctx, "conversations.list", map[string]any{
			"types": "public_channel,private_channel", "exclude_archived": true, "limit": 200, "cursor": cursor}, &out)
		if err != nil {
			return "", fmt.Errorf("could not look the channel up (give its id instead): %w", err)
		}
		for _, c := range out.Channels {
			if strings.ToLower(c.Name) == name {
				return c.ID, nil
			}
		}
		if cursor = out.Meta.Next; cursor == "" {
			break
		}
	}
	return "", fmt.Errorf("no channel named #%s that the app can see; invite it to the channel or give the channel id", name)
}

// checkChannel makes the app able to post in the channel (joining a public one) and says hello there.
func (b *slackBridge) checkChannel(ctx context.Context, channel, project string) error {
	_ = b.sl.call(ctx, "conversations.join", map[string]any{"channel": channel}, nil) // fails for a private channel: ignored
	return b.post(ctx, slackDest{channel: channel}, fmt.Sprintf("This channel now receives the notifications of the project %q: questions, failures and approvals of its sessions.", project), nil)
}

// announce tells the project's people that a session needs them (see the comment at the top of this file).
func (b *slackBridge) announce(ctx context.Context, ev FeedEvent, what string) {
	title := ev.ThreadID
	if t, err := b.s.H.Store.GetThread(ctx, ev.owner, ev.ThreadID); err == nil && t.Title != "" {
		title = t.Title
	}
	line := fmt.Sprintf("Session %q %s.", title, what)
	var proj *store.Project
	link := ""
	if ev.ProjectID != "" {
		proj, _ = b.s.team.GetProject(ctx, ev.ProjectID)
		if base := b.s.configuredBase(); base != "" {
			link = base + "/#/projects/" + ev.ProjectID
		}
		if proj != nil {
			line = fmt.Sprintf("[%s] %s", proj.Name, line)
		}
	}
	var q *store.PendingQuestion
	if ev.Status == "waiting_input" {
		if th, err := b.assistantClient(ev.owner).GetThread(ctx, ev.ThreadID); err == nil {
			q = th.PendingQuestion
		}
	}
	base := slackAction{login: ev.owner, thread: ev.ThreadID, project: ev.ProjectID}
	hint := ""
	if link != "" {
		hint = "Or answer with your own words in the web app: " + link
	}
	message := func() (string, []any) {
		if q != nil {
			return b.questionMessage(base, q, line+"\n", hint)
		}
		if link != "" {
			return line + "\n" + link, nil
		}
		return line, nil
	}

	toChannel := false
	if proj != nil && proj.SlackChannel != "" {
		toChannel = true
		if ev.RunID == "" || !b.markTold("ch:"+ev.RunID) {
			text, blocks := message()
			if err := b.send(ctx, slackDest{channel: proj.SlackChannel}, text, blocks); err != nil {
				toChannel = false // the app cannot post there: fall back to direct messages
			}
		}
	}
	for _, u := range b.s.sup.recipients(ctx, ev.ProjectID, ev.owner) {
		if b.s.sup.settings(ctx, u).Watch {
			continue // the watcher puts it to the assistant, whose answer is delivered
		}
		if toChannel && !(ev.Status == "waiting_approval" && u == ev.owner) {
			continue // the channel has it; only the approval still goes to the person it concerns
		}
		cn, err := b.s.team.GetConnector(ctx, u, connectorSlack)
		if err != nil {
			continue
		}
		text, blocks := message()
		if toChannel {
			text, blocks = line+" Decide it in the web app."+map[bool]string{true: "\n" + link, false: ""}[link != ""], nil
		}
		_ = b.send(ctx, slackDest{channel: cn.Account}, text, blocks)
	}
}

// questionMessage renders an ask_user question: its choices become buttons (a select menu past five), so the person
// answers with one click. header comes before the question, hint after it when the question also takes free text.
func (b *slackBridge) questionMessage(base slackAction, q *store.PendingQuestion, header, hint string) (string, []any) {
	text := header + q.Question
	for _, o := range q.Options {
		if o.Description != "" {
			text += fmt.Sprintf("\n• *%s*: %s", o.Label, o.Description)
		}
	}
	if len(q.Options) == 0 {
		return text, nil
	}
	if q.AllowFreeText && hint != "" {
		text += "\n_" + hint + "_"
	}
	options := q.Options
	if len(options) > 100 {
		options = options[:100]
	}
	var elems []any
	for i, o := range options {
		a := base
		a.question, a.answer, a.interrupt = true, o.Label, q.InterruptID
		value := "ap:" + b.addAction(a)
		label := map[string]any{"type": "plain_text", "text": clip(o.Label, 70)}
		if len(options) <= 5 {
			elems = append(elems, map[string]any{"type": "button", "action_id": fmt.Sprintf("opt%d", i), "text": label, "value": value})
		} else {
			elems = append(elems, map[string]any{"text": label, "value": value})
		}
	}
	var actions map[string]any
	if len(options) <= 5 {
		actions = map[string]any{"type": "actions", "elements": elems}
	} else {
		actions = map[string]any{"type": "actions", "elements": []any{map[string]any{
			"type": "static_select", "action_id": "pick", "placeholder": map[string]any{"type": "plain_text", "text": "Choose an answer"}, "options": elems}}}
	}
	return text, []any{
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": clip(text, 2900)}},
		actions,
	}
}
