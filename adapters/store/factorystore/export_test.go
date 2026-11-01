package factorystore

import "time"

// Writes is the number of times the state file was replaced; tests use it to prove that a burst of
// events is coalesced into few writes.
func (m *Mem) Writes() int64 { return m.writes.Load() }

// SetFlushInterval shortens the coalescing window so tests need not wait 200 ms.
func (m *Mem) SetFlushInterval(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushEvery = d
}

// Failures is the number of trailing flushes that failed to write.
func (m *Mem) Failures() int64 { return m.failures.Load() }
