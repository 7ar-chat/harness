package harness

import (
	"time"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/features/schedules"
	"github.com/7ar-chat/harness/features/usageledger"
	"github.com/7ar-chat/harness/modelinfo"
)

// The scheduler and the usage ledger are features; the product reaches them through the facade, which is the one
// layer allowed to import features (see layers_test.go).

// ScheduleOutcome is what an executor reports for one fired schedule.
type ScheduleOutcome = schedules.Outcome

// ScheduleExecutor runs one fired schedule.
type ScheduleExecutor = schedules.Executor

// ScheduleLocker is the cross-replica lock a scheduler may take.
type ScheduleLocker = schedules.Locker

// ScheduleOptions configures a ScheduleService.
type ScheduleOptions = schedules.Options

// ScheduleService fires due schedules.
type ScheduleService = schedules.Service

// NewScheduleService builds the scheduler.
func NewScheduleService(o ScheduleOptions) *ScheduleService { return schedules.New(o) }

// PreviewSchedule returns the next n fire times of a cron expression in a time zone.
func PreviewSchedule(expr, tz string, n int, from time.Time) ([]time.Time, error) {
	return schedules.Preview(expr, tz, n, from)
}

// UsageAttribution says whom a finished run's spend is booked to.
type UsageAttribution = usageledger.Attribution

// UsageLedgerOptions configures the end-of-run ledger hook.
type UsageLedgerOptions = usageledger.Options

// RunCost prices a run's usage of one model; unpriced reports a model with no known price.
func RunCost(m agui.Usage, model string, price func(string) (modelinfo.Info, bool)) (usd float64, unpriced bool) {
	return usageledger.Cost(m, model, price)
}
