package product

import (
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/insights"
	"github.com/7ar-chat/harness/core/usage"
)

func TestInsightsRangeIsCutInTheCallersZone(t *testing.T) {
	now := time.Date(2026, 11, 24, 23, 30, 0, 0, time.UTC) // already the 25th in Tokyo
	from, to, loc, err := insightsRange("7d", "Asia/Tokyo", now)
	if err != nil || loc.String() != "Asia/Tokyo" {
		t.Fatal(err)
	}
	if got := from.In(loc).Format("2006-01-02"); got != "2026-11-19" {
		t.Fatalf("from = %s", got)
	}
	if got := to.In(loc).Format("2006-01-02"); got != "2026-11-26" {
		t.Fatalf("to = %s", got)
	}
	if _, _, _, err := insightsRange("2026-11-09..2026-11-01", "", now); err == nil {
		t.Fatal("reversed range accepted")
	}
	if _, _, _, err := insightsRange("30d", "Mars/Base", now); err == nil {
		t.Fatal("unknown zone accepted")
	}
	if _, _, _, err := insightsRange("9999d", "", now); err == nil {
		t.Fatal("huge range accepted")
	}
	f, tt, _, err := insightsRange("2026-11-01..2026-11-07", "", now)
	if err != nil || f.Day() != 1 || tt.Day() != 8 {
		t.Fatalf("%v %v %v", f, tt, err)
	}
}

func TestInsightsAccessAndShape(t *testing.T) {
	r := newSchedRig(t, nil, SchedulesOptions{})
	r.sessionRun("alice")
	base := "/api/projects/" + r.project + "/insights"

	r.json("GET", base, "eve", "", 404)
	r.json("GET", base+"?range=bogus", "alice", "", 400)
	r.json("GET", base+"?tz=Mars/Base", "alice", "", 400)
	r.json("GET", base+"/report?format=pdf", "alice", "", 400)
	r.json("GET", base+"/report", "eve", "", 404)
	got := r.json("GET", base+"?range=7d", "vic", "", 200) // a viewer may read it
	cost := got["cost"].(map[string]any)
	if cost["runs"].(float64) < 1 || cost["totalUsd"].(float64) <= 0 || len(cost["models"].([]any)) != 1 {
		t.Fatalf("cost: %v", cost)
	}
	if len(cost["byDay"].([]any)) != 7 {
		t.Fatalf("days: %v", cost["byDay"])
	}
	m := cost["models"].([]any)[0].(map[string]any)
	if m["cacheReadTokens"].(float64) <= 0 || m["cacheWriteTokens"].(float64) <= 0 {
		t.Fatalf("cache split missing: %v", m)
	}
	// The personal view is the caller's own: bob did nothing.
	if me := r.json("GET", "/api/me/insights", "bob", "", 200); me["cost"].(map[string]any)["runs"].(float64) != 0 {
		t.Fatalf("bob sees %v", me["cost"])
	}
	if me := r.json("GET", "/api/me/insights", "alice", "", 200); me["cost"].(map[string]any)["runs"].(float64) < 1 {
		t.Fatalf("alice sees %v", me["cost"])
	}
}

func TestThreadUsageIsLimitedToThoseWhoMayOpenTheThread(t *testing.T) {
	r := newSchedRig(t, nil, SchedulesOptions{})
	id := r.sessionRun("alice")

	got := r.json("GET", "/api/threads/"+id+"/usage", "bob", "", 200)
	if got["total"].(map[string]any)["runs"].(float64) != 1 || len(got["byModel"].([]any)) != 1 {
		t.Fatalf("usage: %v", got)
	}
	r.json("GET", "/api/threads/"+id+"/usage", "eve", "", 404)
}

func TestInsightsReportDownloadsAreSelfContainedAndStatic(t *testing.T) {
	r := newSchedRig(t, nil, SchedulesOptions{})
	r.sessionRun("alice")
	base := "/api/projects/" + r.project + "/insights/report?range=30d"

	resp := r.do("GET", base, "vic", "")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("csp = %q", csp)
	}
	assertSelfContained(t, string(body))
	if !strings.Contains(string(body), "<svg") {
		t.Fatal("no charts")
	}

	resp = r.do("GET", base+"&format=csv&download=1", "alice", "")
	csv, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.HasPrefix(string(csv), "id,title,role,state") || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("csv: %q %q", csv, resp.Header)
	}
	r.json("GET", base+"&format=json", "alice", "", 200)
}

var urlRe = regexp.MustCompile(`(?i)(https?:)?//[a-z0-9]`)

func assertSelfContained(t *testing.T, page string) {
	t.Helper()
	low := strings.ToLower(page)
	for _, bad := range []string{"<script", "javascript:", "<link", "<iframe", "<img", "@import", "url(", " onload=", " onclick=", " onerror=", "<object", "<embed"} {
		if strings.Contains(low, bad) {
			t.Fatalf("report contains %q", bad)
		}
	}
	// The only URL allowed is the SVG namespace declaration, which is not fetched.
	if loc := urlRe.FindAllString(strings.ReplaceAll(page, `xmlns="http://www.w3.org/2000/svg"`, ""), -1); len(loc) > 0 {
		t.Fatalf("report mentions a URL: %v", loc)
	}
}

func TestReportEscapesUntrustedTextAndIsSelfContained(t *testing.T) {
	// Arrange: a task whose title and a model whose name try to inject markup, a formula for the spreadsheet.
	evil := `<script>alert(1)</script><img src=http://evil/x onerror=alert(2)>`
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	in := insights.Input{From: from, To: from.AddDate(0, 0, 14), Now: from.AddDate(0, 0, 14), HasLedger: true,
		Tasks: []insights.TaskInput{{Task: factory.Task{ID: "t1", Title: evil, State: factory.StateDone, Attempts: 1,
			PR: factory.PullRequest{Number: 3}, CreatedAt: from.Add(time.Hour), FinishedAt: from.Add(2 * time.Hour), Spent: factory.Spend{USD: 1.5}}},
			{Task: factory.Task{ID: "t2", Title: `=HYPERLINK("http://evil","x")`, State: factory.StateBlocked, CreatedAt: from.Add(time.Hour), BlockedReason: evil}}},
		UsageTotal:    usage.UsageTotals{CostUSD: 1.5, Runs: 1},
		UsageByModel:  []usage.UsageRow{{Key: evil, UsageTotals: usage.UsageTotals{CostUSD: 1.5}}},
		UsageDayModel: []usage.UsageRow{{Key: "2026-11-02|" + evil, UsageTotals: usage.UsageTotals{CostUSD: 1.5}}},
		UsageByUser:   []usage.UsageRow{{Key: evil, UsageTotals: usage.UsageTotals{Runs: 1}}},
	}
	res := insights.Compute(in)

	// Act
	page, err := RenderInsightsHTML(evil, res, time.UTC)
	csv, _ := InsightsCSV(res)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"<script", "<img", "<svg><", "<a "} {
		if strings.Contains(string(page), bad) {
			t.Fatalf("raw %q in the page", bad)
		}
	}
	if !strings.Contains(string(page), "&lt;script&gt;") {
		t.Fatal("the title must appear escaped")
	}
	if strings.Contains(string(csv), ",=HYPERLINK") || strings.Contains(string(csv), "\n=HYPERLINK") {
		t.Fatalf("a formula cell survived: %s", csv)
	}
	if !strings.Contains(string(page), "agents merged 1 PR") {
		t.Fatalf("summary missing")
	}
}

func TestReportOfAnEmptyProjectRenders(t *testing.T) {
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	res := insights.Compute(insights.Input{From: from, To: from.AddDate(0, 0, 7), Now: from.AddDate(0, 0, 7)})
	page, err := RenderInsightsHTML("Empty", res, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	assertSelfContained(t, string(page))
	if !strings.Contains(string(page), "Insights appear after the first merged task") {
		t.Fatal("no empty-state sentence")
	}
}
