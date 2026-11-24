package product

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"html"
	"html/template"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/7ar-chat/harness/core/insights"
	"github.com/7ar-chat/harness/core/store"
)

// projectInsightsReport serves the exportable report: format=html (default), csv or json. The HTML is one
// self-contained page: inline CSS and SVG, no script, no request, so it opens offline and survives being emailed.
func (s *server) projectInsightsReport(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	from, to, loc, ok := s.insightsParams(w, r)
	if !ok {
		return
	}
	res, err := s.computeInsights(r.Context(), p.ID, "", from, to, loc)
	if err != nil {
		fail(w, err)
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "html"
	}
	name := "insights-" + slug(p.Name) + "-" + from.In(loc).Format("2006-01-02") + "-to-" + to.Add(-time.Second).In(loc).Format("2006-01-02")
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, no-store")
	var body []byte
	switch format {
	case "html":
		body, err = RenderInsightsHTML(p.Name, res, loc)
		h.Set("Content-Type", "text/html; charset=utf-8")
		// The page is static: nothing may run in it or be fetched by it, even if a bug let markup through.
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; sandbox")
		name += ".html"
	case "csv":
		body, err = InsightsCSV(res)
		h.Set("Content-Type", "text/csv; charset=utf-8")
		name += ".csv"
	case "json":
		writeJSONDownload(w, r, name+".json", res)
		return
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "format must be html, csv or json.")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	if r.URL.Query().Get("download") != "" {
		h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	}
	_, _ = w.Write(body)
}

func writeJSONDownload(w http.ResponseWriter, r *http.Request, name string, v any) {
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	}
	writeJSON(w, http.StatusOK, v)
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "project"
	}
	return out[:min(len(out), 40)]
}

// InsightsCSV is the task table. Cells that a spreadsheet would run as a formula are prefixed with a quote.
func InsightsCSV(res insights.Result) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"id", "title", "role", "state", "source", "created_at", "finished_at", "cycle_seconds", "human_wait_seconds", "rework_rounds", "attempts", "cost_usd", "tokens", "unpriced", "pr_url", "executors"})
	ts := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	for _, t := range res.Tasks {
		_ = w.Write([]string{safeCell(t.ID), safeCell(t.Title), safeCell(t.Role), string(t.State), safeCell(t.Source), ts(t.CreatedAt), ts(t.FinishedAt),
			strconv.FormatFloat(t.CycleSec, 'f', 0, 64), strconv.FormatFloat(t.HumanWaitSec, 'f', 0, 64), strconv.Itoa(t.Rework), strconv.Itoa(t.Attempts),
			strconv.FormatFloat(t.CostUSD, 'f', 4, 64), strconv.FormatInt(t.Tokens, 10), strconv.FormatBool(t.Unpriced), safeCell(t.PRURL), safeCell(t.Executors)})
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

func safeCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// ---- HTML ----

// Categorical palette (light surface), in the fixed order; the sixth colour is "other".
var reportColors = []string{"#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#6e6d68"}

type reportData struct {
	Project string
	R       insights.Result
	Range   string
	Metrics []reportMetric
	Charts  []reportChart
	Cards   []reportCard
	Models  []reportModel
	Tasks   []reportTask
}

type reportMetric struct{ Label, Value, Hint string }
type reportChart struct {
	Title, Note string
	SVG         template.HTML
	Legend      []reportLegend
}
type reportLegend struct{ Color, Label string }
type reportCard struct {
	Title string
	Items []reportTask
	Empty string
}
type reportTask struct{ ID, Title, State, Value, Detail string }
type reportModel struct {
	Name                                       string
	Runs                                       int64
	Input, Output, CacheRead, CacheWrite, Cost string
	Unpriced                                   bool
}

// RenderInsightsHTML renders the self-contained report. Every piece of user text goes through html/template; the SVG
// charts are built from numbers and escaped labels only.
func RenderInsightsHTML(project string, res insights.Result, loc *time.Location) ([]byte, error) {
	d, c := res.Delivery, res.Cost
	rng := fmt.Sprintf("%s to %s", res.From.In(loc).Format("2 Jan 2006"), res.To.Add(-time.Second).In(loc).Format("2 Jan 2006"))
	data := reportData{Project: project, R: res, Range: rng}
	perPR := "n/a"
	if d.Merged > 0 {
		perPR = insights.Money(c.PerMergedUSD)
	}
	cycle := "n/a"
	if d.Merged > 0 {
		cycle = insights.Duration(d.CycleMedianSec)
	}
	data.Metrics = []reportMetric{
		{"Merged PRs", strconv.Itoa(d.Merged), "Pull requests the agents delivered and that were merged."},
		{"First-pass success", pct(d, d.Merged), "Merged with no failed CI run, no review round and a single attempt."},
		{"Cost per PR", perPR, "All model spend in the period divided by merged PRs."},
		{"Time to merge", cycle, "Median time from a task's first run to its merge."},
	}
	data.Charts = reportCharts(res)
	data.Cards = []reportCard{
		{"Slowest tasks", refs(res.Attention.Slowest, func(v float64) string { return insights.Duration(v) }), "Nothing finished or running yet."},
		{"Blocked the longest", refs(res.Attention.BlockedLongest, func(v float64) string { return insights.Duration(v) }), "No task is waiting for a person."},
		{"Most expensive", refs(res.Attention.MostExpensive, insights.Money), "No priced spend yet."},
		{"Most reworked", refs(res.Attention.MostReworked, func(v float64) string { return fmt.Sprintf("%.0f rounds", v) }), "No task needed rework."},
	}
	for _, m := range c.Models {
		data.Models = append(data.Models, reportModel{m.Model, m.Runs, num(m.InputTokens), num(m.OutputTokens), num(m.CacheReadTokens), num(m.CacheWriteTokens), insights.Money(m.CostUSD), m.Unpriced})
	}
	for i, t := range res.Tasks {
		if i == 50 {
			break
		}
		cyc := ""
		if t.CycleSec > 0 {
			cyc = insights.Duration(t.CycleSec)
		}
		data.Tasks = append(data.Tasks, reportTask{t.ID, t.Title, string(t.State), insights.Money(t.CostUSD), cyc})
	}
	var buf bytes.Buffer
	if err := reportTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func pct(d insights.Delivery, merged int) string {
	if merged == 0 {
		return "n/a"
	}
	return insights.Percent(d.FirstPassRate)
}

func refs(in []insights.TaskRef, val func(float64) string) []reportTask {
	out := make([]reportTask, 0, len(in))
	for _, r := range in {
		out = append(out, reportTask{ID: r.ID, Title: r.Title, State: string(r.State), Value: val(r.Value), Detail: r.Detail})
	}
	return out
}

func num(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// ---- charts: inline SVG built from numbers and escaped labels ----

func reportCharts(res insights.Result) []reportChart {
	var out []reportChart
	// Throughput.
	var wl []string
	var wv []float64
	for _, w := range res.Flow.Weeks {
		wl = append(wl, shortDay(w.Week))
		wv = append(wv, float64(w.Merged))
	}
	out = append(out, reportChart{Title: "Merged PRs per week", Note: "Weeks start on Monday.", SVG: barSVG(wl, wv, reportColors[0], func(v float64) string { return strconv.Itoa(int(v)) }, "Merged PRs per week")})
	// Cost by day, stacked by model.
	out = append(out, costChart(res))
	// Cycle time distribution.
	var cl []string
	var cv []float64
	for _, b := range res.Delivery.CycleBuckets {
		cl = append(cl, b.Label)
		cv = append(cv, float64(b.Count))
	}
	out = append(out, reportChart{Title: "Time to merge", Note: "How many merged PRs took how long, from first run to merge.", SVG: barSVG(cl, cv, reportColors[0], func(v float64) string { return strconv.Itoa(int(v)) }, "Distribution of time to merge")})
	// Working vs waiting.
	d := res.Delivery
	out = append(out, reportChart{Title: "Where the time goes", Note: "Across merged PRs: agents working versus waiting for a person.",
		SVG: splitSVG(d.WorkingSec, d.WaitingSec)})
	if d.WorkingSec+d.WaitingSec > 0 {
		out[len(out)-1].Legend = []reportLegend{{reportColors[0], "Agents working " + insights.Duration(d.WorkingSec)}, {reportColors[1], "Waiting on a person " + insights.Duration(d.WaitingSec)}}
	}
	return out
}

func shortDay(d string) string {
	if t, err := time.Parse("2006-01-02", d); err == nil {
		return t.Format("2 Jan")
	}
	return d
}

func esc(s string) string { return html.EscapeString(s) }

const (
	chartW, chartH         = 640.0, 220.0
	padL, padR, padT, padB = 44.0, 8.0, 12.0, 34.0
)

func niceMax(v float64) float64 {
	if v <= 0 {
		return 1
	}
	exp := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if v <= m*exp {
			return m * exp
		}
	}
	return 10 * exp
}

func svgOpen(label string) string {
	return fmt.Sprintf(`<svg viewBox="0 0 %.0f %.0f" role="img" aria-label="%s" xmlns="http://www.w3.org/2000/svg" class="chart">`, chartW, chartH, esc(label))
}

// axes draws the baseline, a mid and a top gridline with their labels.
func axes(maxV float64, f func(float64) string) string {
	var b strings.Builder
	for i, frac := range []float64{0, 0.5, 1} {
		y := chartH - padB - frac*(chartH-padT-padB)
		cls := "grid"
		if i == 0 {
			cls = "base"
		}
		fmt.Fprintf(&b, `<line class="%s" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/><text class="ax" x="%.1f" y="%.1f" text-anchor="end">%s</text>`,
			cls, padL, chartW-padR, y, y, padL-6, y+4, esc(f(maxV*frac)))
	}
	return b.String()
}

func xLabels(labels []string, slot float64) string {
	step := 1
	for float64(len(labels))/float64(step) > 10 {
		step++
	}
	var b strings.Builder
	for i, l := range labels {
		if i%step != 0 {
			continue
		}
		fmt.Fprintf(&b, `<text class="ax" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, padL+slot*(float64(i)+0.5), chartH-padB+16, esc(l))
	}
	return b.String()
}

func barSVG(labels []string, vals []float64, color string, f func(float64) string, title string) template.HTML {
	maxV := 0.0
	for _, v := range vals {
		maxV = math.Max(maxV, v)
	}
	if maxV = math.Max(maxV, 4); maxV <= 20 { // counts: an even top, so the middle tick is a whole number
		maxV = math.Ceil(maxV/2) * 2
	} else {
		maxV = niceMax(maxV)
	}
	var b strings.Builder
	b.WriteString(svgOpen(title))
	b.WriteString(axes(maxV, f))
	if len(vals) > 0 {
		slot := (chartW - padL - padR) / float64(len(vals))
		bw := math.Min(slot*0.62, 44)
		for i, v := range vals {
			h := v / maxV * (chartH - padT - padB)
			x := padL + slot*float64(i) + (slot-bw)/2
			if v > 0 {
				fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="3" fill="%s"><title>%s: %s</title></rect>`, x, chartH-padB-h, bw, h, color, esc(labels[i]), esc(f(v)))
				fmt.Fprintf(&b, `<text class="val" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, x+bw/2, chartH-padB-h-4, esc(f(v)))
			}
		}
		b.WriteString(xLabels(labels, slot))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func costChart(res insights.Result) reportChart {
	days := res.Cost.ByDay
	// The five dearest models get a colour, the rest fold into "Other".
	top := map[string]int{}
	var legend []reportLegend
	for i, m := range res.Cost.Models {
		if i < 5 {
			top[m.Model] = i
			legend = append(legend, reportLegend{reportColors[i], m.Model})
		}
	}
	if len(res.Cost.Models) > 5 {
		legend = append(legend, reportLegend{reportColors[5], "Other"})
	}
	maxV := 0.0
	for _, d := range days {
		maxV = math.Max(maxV, d.USD)
	}
	maxV = niceMax(maxV)
	var b strings.Builder
	b.WriteString(svgOpen("Cost per day, split by model"))
	b.WriteString(axes(maxV, insights.Money))
	if len(days) > 0 {
		slot := (chartW - padL - padR) / float64(len(days))
		bw := math.Max(math.Min(slot*0.7, 28), 1.5)
		var labels []string
		for _, d := range days {
			labels = append(labels, shortDay(d.Day))
		}
		for i, d := range days {
			x := padL + slot*float64(i) + (slot-bw)/2
			y := chartH - padB
			models := make([]string, 0, len(d.ByModel))
			for m := range d.ByModel {
				models = append(models, m)
			}
			sort.Slice(models, func(a, c int) bool {
				ia, oka := top[models[a]]
				ic, okc := top[models[c]]
				if !oka {
					ia = 5
				}
				if !okc {
					ic = 5
				}
				if ia != ic {
					return ia < ic
				}
				return models[a] < models[c]
			})
			for _, m := range models {
				v := d.ByModel[m]
				if v <= 0 {
					continue
				}
				ci, ok := top[m]
				if !ok {
					ci = 5
				}
				h := v / maxV * (chartH - padT - padB)
				y -= h
				fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s" stroke="#fff" stroke-width="1"><title>%s, %s: %s</title></rect>`, x, y, bw, math.Max(h, 0.5), reportColors[ci], esc(d.Day), esc(m), esc(insights.Money(v)))
			}
		}
		b.WriteString(xLabels(labels, slot))
	}
	b.WriteString(`</svg>`)
	note := "Spend per day from the usage ledger."
	if res.Cost.UnpricedRuns > 0 {
		note += fmt.Sprintf(" %d run(s) used a model without a price and are not in these dollars.", res.Cost.UnpricedRuns)
	}
	return reportChart{Title: "Cost per day", Note: note, SVG: template.HTML(b.String()), Legend: legend}
}

func splitSVG(working, waiting float64) template.HTML {
	total := working + waiting
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`<svg viewBox="0 0 %.0f 56" role="img" aria-label="Working versus waiting on a person" xmlns="http://www.w3.org/2000/svg" class="chart split">`, chartW))
	if total <= 0 {
		b.WriteString(`<text class="ax" x="0" y="30">No merged PRs in this period.</text></svg>`)
		return template.HTML(b.String())
	}
	w := (chartW - 2) * working / total
	fmt.Fprintf(&b, `<rect x="0" y="8" width="%.1f" height="32" rx="4" fill="%s"><title>Agents working: %s</title></rect>`, w, reportColors[0], esc(insights.Duration(working)))
	fmt.Fprintf(&b, `<rect x="%.1f" y="8" width="%.1f" height="32" rx="4" fill="%s"><title>Waiting on a person: %s</title></rect>`, w+2, chartW-2-w, reportColors[1], esc(insights.Duration(waiting)))
	fmt.Fprintf(&b, `<text class="val inv" x="8" y="29">%s</text>`, esc(insights.Percent(working/total)))
	fmt.Fprintf(&b, `<text class="val inv" x="%.1f" y="29" text-anchor="end">%s</text></svg>`, chartW-8, esc(insights.Percent(waiting/total)))
	return template.HTML(b.String())
}

var reportTmpl = template.Must(template.New("report").Funcs(template.FuncMap{
	"dur":   insights.Duration,
	"money": insights.Money,
	"pct":   insights.Percent,
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Insights: {{.Project}}</title>
<style>
:root{color-scheme:light;--ink:#0b0b0b;--ink2:#52514e;--line:#dcdbd5;--bg:#fcfcfb;--soft:#f3f2ee}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
main{max-width:860px;margin:0 auto;padding:32px 20px 64px}
h1{font-size:26px;margin:0 0 4px}h2{font-size:18px;margin:36px 0 12px}h3{font-size:15px;margin:0 0 4px}
.sub{color:var(--ink2);margin:0 0 20px}
.summary{font-size:17px;border-left:4px solid #2a78d6;background:var(--soft);padding:14px 16px;border-radius:4px;margin:0 0 24px}
.metrics{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:12px;margin:0 0 8px}
.metric{border:1px solid var(--line);border-radius:8px;padding:12px 14px;background:#fff}
.metric b{display:block;font-size:28px;line-height:1.2}.metric span{font-weight:600}.metric small{display:block;color:var(--ink2);margin-top:4px}
.chart{width:100%;height:auto;display:block;overflow:visible}
.chart .grid{stroke:var(--line);stroke-width:1}.chart .base{stroke:#9a998f;stroke-width:1}
.chart .ax{fill:var(--ink2);font-size:11px}.chart .val{fill:var(--ink);font-size:11px;font-weight:600}.chart .inv{fill:#fff;font-size:13px}
.note{color:var(--ink2);margin:0 0 6px;font-size:13px}
.legend{display:flex;flex-wrap:wrap;gap:6px 16px;margin:6px 0 0;padding:0;list-style:none;font-size:13px}
.legend i{display:inline-block;width:10px;height:10px;border-radius:2px;margin-right:6px}
figure{margin:0 0 24px;break-inside:avoid}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(380px,1fr));gap:16px}
.card{border:1px solid var(--line);border-radius:8px;padding:12px 14px;background:#fff;break-inside:avoid}
.card ol{margin:6px 0 0;padding-left:20px}.card li{margin:4px 0}.card small{color:var(--ink2);display:block}
table{border-collapse:collapse;width:100%;font-size:13px}th,td{text-align:right;padding:6px 8px;border-bottom:1px solid var(--line)}
th:first-child,td:first-child{text-align:left}th{color:var(--ink2);font-weight:600}
td.l,th.l{text-align:left}
.tablewrap{overflow-x:auto}
.foot{color:var(--ink2);font-size:12px;margin-top:40px}
@media print{body{background:#fff;font-size:12px}main{padding:0}.card,.metric{border-color:#bbb}h2{break-after:avoid}.tablewrap{overflow:visible}}
</style></head><body><main>
<h1>Insights: {{.Project}}</h1>
<p class="sub">{{.Range}} ({{.R.TZ}}). Generated {{.R.GeneratedAt.Format "2 Jan 2006 15:04"}} UTC.</p>
<p class="summary">{{.R.Summary}}</p>
<div class="metrics">{{range .Metrics}}<div class="metric"><span>{{.Label}}</span><b>{{.Value}}</b><small>{{.Hint}}</small></div>{{end}}</div>

<h2>Delivery</h2>
<p>Started {{.R.Delivery.Started}}, merged {{.R.Delivery.Merged}}, failed {{.R.Delivery.Failed}}, cancelled {{.R.Delivery.Cancelled}}; {{.R.Delivery.Blocked}} waiting for a person now.
{{if .R.Delivery.Merged}}Merge rate {{pct .R.Delivery.MergeRate}}. Time to merge: median {{dur .R.Delivery.CycleMedianSec}}, slowest tenth {{dur .R.Delivery.CycleP90Sec}}. Rework rounds per PR: {{printf "%.1f" .R.Delivery.ReworkPerPR}}.{{end}}
{{if .R.Delivery.NeededHumanOverDay}} {{.R.Delivery.NeededHumanOverDay}} task(s) needed a person for more than a day.{{end}}</p>
{{range .Charts}}<figure><h3>{{.Title}}</h3><p class="note">{{.Note}}</p>{{.SVG}}{{if .Legend}}<ul class="legend">{{range .Legend}}<li><i style="background:{{.Color}}"></i>{{.Label}}</li>{{end}}</ul>{{end}}</figure>{{end}}

<h2>What needs attention</h2>
<div class="cards">{{range .Cards}}<div class="card"><h3>{{.Title}}</h3>{{if .Items}}<ol>{{range .Items}}<li>{{.Title}} <b>{{.Value}}</b>{{if .Detail}}<small>{{.Detail}}</small>{{end}}</li>{{end}}</ol>{{else}}<small>{{.Empty}}</small>{{end}}</div>{{end}}</div>

<h2>Cost by model</h2>
<p>Total {{money .R.Cost.TotalUSD}} over {{.R.Cost.Runs}} run(s). Cache reads and writes are listed apart: they are priced differently from ordinary input.{{if .R.Cost.UnpricedRuns}} {{.R.Cost.UnpricedRuns}} run(s) used a model without a known price, so the total is a lower bound.{{end}}</p>
{{if .Models}}<div class="tablewrap"><table><thead><tr><th>Model</th><th>Runs</th><th>Input</th><th>Output</th><th>Cache read</th><th>Cache write</th><th>Cost</th></tr></thead><tbody>
{{range .Models}}<tr><td>{{.Name}}{{if .Unpriced}} (no price){{end}}</td><td>{{.Runs}}</td><td>{{.Input}}</td><td>{{.Output}}</td><td>{{.CacheRead}}</td><td>{{.CacheWrite}}</td><td>{{.Cost}}</td></tr>{{end}}</tbody></table></div>{{else}}<p class="note">No model usage in this period.</p>{{end}}

<h2>Quality and people</h2>
<p>Reviewer: {{.R.Quality.ReviewApproved}} approved, {{.R.Quality.ReviewBlocked}} asked for changes ({{pct .R.Quality.ReviewBlockRate}} blocked). Policy: {{.R.Quality.PolicyDenied}} denied, {{.R.Quality.PolicyAsked}} asked a person. Loop guard stopped {{.R.Quality.LoopGuardTrips}} task(s).</p>
{{if .R.People}}<div class="tablewrap"><table><thead><tr><th>Member</th><th>Sessions</th><th>Runs</th><th>Approvals decided</th><th>Median wait</th><th>Cost</th></tr></thead><tbody>
{{range .R.People}}<tr><td>{{.Login}}</td><td>{{.Sessions}}</td><td>{{.Runs}}</td><td>{{.Approvals}}</td><td>{{if .Latency.N}}{{dur .Latency.MedianSec}}{{else}}n/a{{end}}</td><td>{{money .CostUSD}}</td></tr>{{end}}</tbody></table></div>{{end}}
{{if or .R.Waiting.Tools.N .R.Waiting.Tasks.N .R.Waiting.Gates.N}}<p>Waiting on a person, by what was waited for (median, decided): tool approvals {{if .R.Waiting.Tools.N}}{{dur .R.Waiting.Tools.MedianSec}} ({{.R.Waiting.Tools.N}}){{else}}n/a{{end}}; task escalations {{if .R.Waiting.Tasks.N}}{{dur .R.Waiting.Tasks.MedianSec}} ({{.R.Waiting.Tasks.N}}){{else}}n/a{{end}}; workflow gates {{if .R.Waiting.Gates.N}}{{dur .R.Waiting.Gates.MedianSec}} ({{.R.Waiting.Gates.N}}){{else}}n/a{{end}}.</p>{{end}}
{{if .R.Workflows.Runs}}<p>Workflows: {{.R.Workflows.Runs}} run(s), {{pct .R.Workflows.SuccessRate}} succeeded{{if .R.Workflows.SlowestNode}}; slowest step {{.R.Workflows.SlowestNode}} ({{dur .R.Workflows.SlowestNodeSec}}){{end}}{{if .R.Workflows.GateWaits}}; gates waited {{dur .R.Workflows.GateWaitMedianSec}} (median){{end}}.</p>{{end}}

<h2>Tasks</h2>
{{if .Tasks}}<div class="tablewrap"><table><thead><tr><th>Task</th><th class="l">State</th><th>Time to finish</th><th>Cost</th></tr></thead><tbody>
{{range .Tasks}}<tr><td>{{.Title}}</td><td class="l">{{.State}}</td><td>{{.Detail}}</td><td>{{.Value}}</td></tr>{{end}}</tbody></table></div>{{if gt (len .R.Tasks) 50}}<p class="note">The 50 newest of {{len .R.Tasks}} tasks. The CSV export has them all.</p>{{end}}{{else}}<p class="note">No tasks in this period.</p>{{end}}
<p class="foot">Self-contained report: no scripts, no external requests. Days are cut in {{.R.TZ}}. Waiting on a person means time a task spent blocked (an approval, a question, ready to merge).</p>
</main></body></html>
`))
