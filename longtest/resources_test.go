//go:build unix

package longtest_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/engine/tools"

	"github.com/7ar-chat/harness/longtest"
)

const (
	// maxHeapGrowth bounds the live heap left after a 150-step run (after a GC, harness still
	// referenced) over the heap before it. The run holds the whole thread in the in-memory
	// store and event log (about 150 tool outputs of up to 60 KB each, plus their events), so
	// tens of MiB are expected; 128 MiB would mean something grows without bound.
	maxHeapGrowth = 128 << 20
	// maxPeakHeap bounds the heap in use at any model call during the run.
	maxPeakHeap = 256 << 20
)

func mib(b uint64) float64 { return float64(b) / (1 << 20) }

func TestLongSessionResourcesStayBounded(t *testing.T) {
	// Arrange / Act
	o := longtest.Run(t, longtest.Scenario{Steps: 150, Window: 60000, NoMask: true, Sample: true})

	// Assert
	if o.Status != "finished" {
		t.Fatalf("status %q", o.Status)
	}
	peak := slices.Max(o.Heap)
	growth := int64(o.HeapAfter) - int64(o.HeapBefore)
	t.Logf("heap before %.1f MiB, after %.1f MiB (growth %.1f MiB), peak %.1f MiB; goroutines %d -> %d; %d steps in %v (%.1f ms/step, model %v)",
		mib(o.HeapBefore), mib(o.HeapAfter), float64(growth)/(1<<20), mib(peak), o.GoroutinesBefore, o.GoroutinesAfter,
		o.Steps, o.Wall.Round(1e6), float64(o.Wall.Milliseconds())/float64(o.Steps), o.ModelTime.Round(1e6))
	if growth > maxHeapGrowth {
		t.Errorf("heap grew by %.1f MiB (bound %d MiB)", float64(growth)/(1<<20), maxHeapGrowth>>20)
	}
	if peak > maxPeakHeap {
		t.Errorf("peak heap %.1f MiB (bound %d MiB)", mib(peak), maxPeakHeap>>20)
	}
	// Heap in use must not climb steadily: the last third of the run is not much above the middle third
	// once compaction has cut the working set (sampled values include garbage, so compare the maxima).
	n := len(o.Heap)
	if mid, end := slices.Max(o.Heap[n/3:2*n/3]), slices.Max(o.Heap[2*n/3:]); end > mid+mid/2+(32<<20) {
		t.Errorf("heap climbs: max %.1f MiB in the middle third, %.1f MiB in the last", mib(mid), mib(end))
	}
	if o.GoroutinesAfter > o.GoroutinesBefore {
		t.Errorf("goroutine leak: %d before the run, %d after", o.GoroutinesBefore, o.GoroutinesAfter)
	}
}

// BenchmarkLongSessionHarnessOnly is BenchmarkLongSession with an in-process tool
// returning a 20 KB result: no bash, no ripgrep, so ms/step is the harness alone
// (loop, store, event log, masking, compaction, request building).
func BenchmarkLongSessionHarnessOnly(b *testing.B) {
	payload := strings.Repeat("0123456789abcdef", 1250) // 20 KB
	noop := longtest.ExtraTool("noop", func(context.Context, string) (string, error) { return payload, nil })
	for b.Loop() {
		o := longtest.Run(b, longtest.Scenario{
			Steps: 150, Window: 60000, Tools: []tools.Provider{noop},
			Action: func(step int) (string, string) { return "noop", fmt.Sprintf(`{"n":%d}`, step) },
		})
		if o.Status != "finished" {
			b.Fatalf("status %q", o.Status)
		}
		b.ReportMetric(float64(o.Wall.Microseconds())/1000/float64(o.Steps), "ms/step")
		b.ReportMetric(float64(o.HeapAfter)/(1<<20), "heapMiB")
	}
}

// BenchmarkLongSession measures the harness's own cost per step: a 150-step run with an
// instant scripted model, so what is timed is the loop, the event log, the store, the
// compaction/masking work and the local tools (bash, ripgrep) themselves.
func BenchmarkLongSession(b *testing.B) {
	for b.Loop() {
		o := longtest.Run(b, longtest.Scenario{Steps: 150, Window: 60000})
		if o.Status != "finished" {
			b.Fatalf("status %q", o.Status)
		}
		b.ReportMetric(float64(o.Wall.Microseconds())/1000/float64(o.Steps), "ms/step")
		b.ReportMetric(float64(o.HeapAfter)/(1<<20), "heapMiB")
	}
}
