package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// Model metadata and cost: the window and prices of a model come from the
// backend (InfoBackend), the cost is computed here from the usage of each run.

// InfoBackend is implemented by a backend that knows the context window, prices
// and capabilities of models. It must not block: it is asked on every render. A
// backend without it shows no window, cost or badges.
type InfoBackend interface {
	ModelInfo(ctx context.Context, id string) (client.ModelInfo, bool)
}

// costRow is the usage and price of one model in one run.
type costRow struct {
	Run    int
	Model  string
	U      Usage
	USD    float64
	Priced bool
}

// infoRefresh is how long a ClientBackend waits before asking the server again for a model it has no info on.
const infoRefresh = 30 * time.Second

func init() {
	register(
		command{name: "cost", help: "tokens and price of each run of this session", run: func(m *Model, _ string) tea.Cmd { m.cmdCost(); return nil }},
		command{name: "context", help: "how full the context window is", run: func(m *Model, _ string) tea.Cmd { m.cmdContext(); return nil }},
	)
}

// modelInfo is what the backend knows about model id.
func (m *Model) modelInfo(id string) (client.ModelInfo, bool) {
	if ib, ok := m.be.(InfoBackend); ok && id != "" {
		return ib.ModelInfo(m.ctx, id)
	}
	return client.ModelInfo{}, false
}

// contextWindow is the window of the current model: the model table's, else the one the run reported.
func (m *Model) contextWindow() int {
	if i, ok := m.modelInfo(m.be.Model()); ok && i.ContextWindow > 0 {
		return i.ContextWindow
	}
	return m.inp.sb.window
}

// price prices u with the info of model; unpriced when the model is unknown.
func (m *Model) price(model string, u Usage, prompt int) (usd float64, priced bool) {
	if i, ok := m.modelInfo(model); ok {
		return i.Cost(u.In, u.Out, u.Cached, prompt)
	}
	return 0, false
}

// recordRun books the finished run: its usage per model (RUN_FINISHED) or, when
// the event has none, the last usage reported while it ran.
func (m *Model) recordRun(us []agui.Usage) {
	sb := &m.inp.sb
	run := sb.nRuns + 1
	add := func(model string, u Usage) {
		if u.In+u.Out == 0 {
			return
		}
		if model == "" {
			model = m.be.Model()
		}
		usd, priced := m.price(model, u, sb.runMaxCtx)
		sb.rows = append(sb.rows, costRow{Run: run, Model: model, U: u, USD: usd, Priced: priced})
		sb.nRuns = run
	}
	for _, u := range us {
		add(u.Model, Usage{In: u.InputTokens, Out: u.OutputTokens, Cached: u.CachedInputTokens, Reasoning: u.ReasoningTokens})
	}
	if len(us) == 0 {
		add(m.be.Model(), sb.live)
	}
	sb.live, sb.runMaxCtx = Usage{}, 0
}

// rowsWithLive is the booked rows plus the run in progress.
func (m *Model) rowsWithLive() []costRow {
	sb := &m.inp.sb
	rows := sb.rows
	if l := sb.live; l.In+l.Out > 0 {
		model := m.be.Model()
		usd, priced := m.price(model, l, sb.runMaxCtx)
		rows = append(append([]costRow(nil), rows...), costRow{Run: sb.nRuns + 1, Model: model, U: l, USD: usd, Priced: priced})
	}
	return rows
}

// sessionCost totals the session. partial is true when some usage had no price
// (the total is then a lower bound); known is false when nothing could be priced.
func (m *Model) sessionCost() (usd float64, partial, known bool) {
	for _, r := range m.rowsWithLive() {
		if r.Priced {
			usd += r.USD
			known = true
		} else {
			partial = true
		}
	}
	return usd, partial, known
}

// fmtUSD formats a dollar amount with the precision its size deserves.
func fmtUSD(v float64) string {
	switch {
	case v < 0.01:
		return fmt.Sprintf("$%.4f", v)
	case v < 1:
		return fmt.Sprintf("$%.3f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

// fmtMtok formats a price per token as dollars per million tokens.
func fmtMtok(perToken float64) string {
	s := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", perToken*1e6), "0"), ".")
	if s == "" {
		s = "0"
	}
	return "$" + s
}

// priceLine is "$3/$15 per Mtok" ("free" for a zero price, "" when unpriced).
func priceLine(i client.ModelInfo) string {
	switch {
	case !i.Priced:
		return ""
	case i.InputPerToken == 0 && i.OutputPerToken == 0:
		return "free"
	}
	return fmtMtok(i.InputPerToken) + "/" + fmtMtok(i.OutputPerToken) + " per Mtok"
}

// costStatus fills the context and cost parts of the status line.
func (m *Model) costStatus(sv *StatusView) {
	sv.CtxUsed, sv.CtxWindow = m.inp.sb.ctx, m.contextWindow()
	if usd, partial, known := m.sessionCost(); known {
		sv.Cost = fmtUSD(usd)
		if partial {
			sv.Cost = "~" + sv.Cost
		}
	}
}

// resetCost forgets the cost of the session (a new or resumed conversation).
func (s *sidebar) resetCost() {
	s.rows, s.nRuns, s.live, s.runMaxCtx, s.ctx, s.window = nil, 0, Usage{}, 0, 0, 0
}

func (m *Model) cmdCost() {
	rows := m.rowsWithLive()
	if len(rows) == 0 {
		m.notice("cost: no run yet")
		return
	}
	var tot Usage
	var sum float64
	partial := false
	lines := []string{"cost of this session"}
	for _, r := range rows {
		tot.add(r.U)
		price := "–"
		if r.Priced {
			sum += r.USD
			price = fmtUSD(r.USD)
		} else {
			partial = true
		}
		lines = append(lines, fmt.Sprintf("  run %d · %s · %s · %s", r.Run, r.Model, tokensLine(r.U), price))
	}
	total := fmtUSD(sum)
	if partial {
		total = "~" + total + " (some models have no price)"
	}
	lines = append(lines, fmt.Sprintf("  total · %s · %s", tokensLine(tot), total))
	m.notice("%s", strings.Join(lines, "\n"))
}

func tokensLine(u Usage) string {
	s := fmt.Sprintf("in %s out %s", FormatTokens(u.In), FormatTokens(u.Out))
	if u.Cached > 0 {
		s += " cached " + FormatTokens(u.Cached)
	}
	return s
}

func (m *Model) cmdContext() {
	used, window := m.inp.sb.ctx, m.contextWindow()
	i, known := m.modelInfo(m.be.Model())
	var s string
	switch {
	case used == 0 && window == 0:
		s = "context: nothing reported yet"
	case window == 0:
		s = fmt.Sprintf("context: %s tokens in use (window unknown)", FormatTokens(used))
	default:
		s = fmt.Sprintf("context: %s of %s tokens in use (%d%%)", FormatTokens(used), formatWindow(window), used*100/window)
	}
	if known && i.MaxOutput > 0 {
		s += fmt.Sprintf(" · max output %s", formatWindow(i.MaxOutput))
	}
	m.notice("%s", s)
}

// warnVision tells that the current model is known not to take images.
func (m *Model) warnVision() {
	if i, ok := m.modelInfo(m.be.Model()); ok && !i.Vision {
		m.notice("%s is not listed as accepting images: it may ignore or reject the attachment", m.be.Model())
	}
}

// modelBadges is the one-line description of a model in the picker.
func (m *Model) modelBadges(id string) string {
	i, ok := m.modelInfo(id)
	if !ok {
		return ""
	}
	var p []string
	if i.ContextWindow > 0 {
		p = append(p, formatWindow(i.ContextWindow))
	}
	if pl := priceLine(i); pl != "" {
		p = append(p, pl)
	}
	if i.Vision {
		p = append(p, "vision")
	}
	if i.Reasoning {
		p = append(p, "reasoning")
	}
	return strings.Join(p, " · ")
}
