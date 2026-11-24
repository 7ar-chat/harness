package usagestore_test

import (
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/usage"
)

func TestThreadFilterAndDayModelGroupInTheCallersZone(t *testing.T) {
	eachStore(t, func(t *testing.T, s usage.Store) {
		late := time.Date(2026, 11, 3, 23, 30, 0, 0, time.UTC) // already the 4th in Tokyo
		must(t, s.Record(ctx, []usage.Entry{
			entry("r1", "gpt-a", "ann", "p1", usage.SourceTask, late, 1),
			entry("r2", "gpt-b", "ann", "p1", usage.SourceSession, late, 2),
		}))

		one, err := s.Report(ctx, usage.Query{ThreadID: "th-r2"})
		must(t, err)
		tokyo, _ := time.LoadLocation("Asia/Tokyo")
		rows, err := s.Report(ctx, usage.Query{Group: usage.GroupDayModel, Loc: tokyo})
		must(t, err)

		if one.Total.CostUSD != 2 || one.Total.Runs != 1 {
			t.Fatalf("thread total = %+v", one.Total)
		}
		if len(rows.Rows) != 2 || rows.Rows[0].Key != "2026-11-04|gpt-a" || rows.Rows[1].Key != "2026-11-04|gpt-b" {
			t.Fatalf("rows = %+v", rows.Rows)
		}
	})
}
