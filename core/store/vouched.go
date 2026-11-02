package store

// Vouched reports a run one of the alive checks of InterruptStaleRuns says is still running.
func Vouched(r *Run, alive []func(*Run) bool) bool {
	for _, ok := range alive {
		if ok != nil && ok(r) {
			return true
		}
	}
	return false
}
