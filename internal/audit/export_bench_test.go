// Benchmarks for Chain.Append / Verify / Export. Run with:
//
//	go test -bench=. -benchtime=3x ./internal/audit/...
//	go test -bench=. -benchmem ./internal/audit/...
//
// The 1000-event append target from task.json is exercised by
// BenchmarkAppend_1000Events; the streaming Export path is covered by
// BenchmarkExport_1000Events_NoFilter / FilterByPlanID / FilterByTimeRange.
// Verify is benchmarked on a fully-populated chain.
package audit

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/example/insurance-rule-rollout/internal/workflow"
)

// benchFixtureEvent constructs a single deterministic AuditEvent for use
// in benchmark loops. Avoids touching map iteration order so a perf
// comparison between runs stays apples-to-apples.
func benchFixtureEvent(seq int, base time.Time) workflow.AuditEvent {
	return workflow.AuditEvent{
		EventID:   fmt.Sprintf("bench-evt-%05d", seq),
		PlanID:    "plan-bench",
		Actor:     "loadgen",
		Action:    workflow.ActionApprove,
		Payload:   map[string]string{"i": fmt.Sprintf("%d", seq), "note": "bench"},
		Timestamp: base.Add(time.Duration(seq) * time.Second),
	}
}

// fillChain appends n events to a fresh chain rooted at the given path.
// Used by the Append / Verify / Export benchmarks so they share one
// fixture generator.
func fillChain(tb testing.TB, path string, n int) {
	tb.Helper()
	c := NewChain(path)
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		if err := c.Append(context.Background(), benchFixtureEvent(i, base)); err != nil {
			tb.Fatalf("append %d: %v", i, err)
		}
	}
}

// BenchmarkAppend_1000Events measures the worst-case append loop from the
// task.json spec: 1000 sequential Append calls. Acceptance threshold is
// < 1s wall-clock; the report value is a function of hardware but should
// stay well under 1s on a developer laptop.
func BenchmarkAppend_1000Events(b *testing.B) {
	for i := 0; i < b.N; i++ {
		path := b.TempDir() + "/append-bench.jsonl"
		fillChain(b, path, 1000)
	}
}

// BenchmarkVerify_1000Events measures end-to-end SHA256 chain verification
// of a 1000-event chain. Acceptance threshold from task.json is < 1s.
func BenchmarkVerify_1000Events(b *testing.B) {
	// Pre-populate one shared chain outside the timed loop.
	path := b.TempDir() + "/verify-bench.jsonl"
	fillChain(b, path, 1000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := NewChain(path)
		if err := c.Verify(context.Background()); err != nil {
			b.Fatalf("verify: %v", err)
		}
	}
}

// BenchmarkExport_1000Events_NoFilter measures the streaming export with
// no predicate — every line must be copied out. Acceptance threshold from
// task.json is < 500ms.
func BenchmarkExport_1000Events_NoFilter(b *testing.B) {
	path := b.TempDir() + "/export-bench.jsonl"
	fillChain(b, path, 1000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := NewChain(path)
		r, err := c.Export(context.Background(), Filter{})
		if err != nil {
			b.Fatalf("export: %v", err)
		}
		if _, err := io.Copy(io.Discard, r); err != nil {
			b.Fatalf("copy: %v", err)
		}
	}
}

// BenchmarkExport_1000Events_FilterByPlanID measures export with the most
// common 飞检 query: pick out one plan_id from a large mixed chain.
func BenchmarkExport_1000Events_FilterByPlanID(b *testing.B) {
	path := b.TempDir() + "/export-plan-bench.jsonl"
	fillChain(b, path, 1000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := NewChain(path)
		r, err := c.Export(context.Background(), Filter{PlanID: "plan-bench"})
		if err != nil {
			b.Fatalf("export: %v", err)
		}
		if _, err := io.Copy(io.Discard, r); err != nil {
			b.Fatalf("copy: %v", err)
		}
	}
}

// BenchmarkExport_1000Events_FilterByTimeRange measures export with a
// half-window time range — exercises the FromTime + ToTime branch on a
// realistic volume.
func BenchmarkExport_1000Events_FilterByTimeRange(b *testing.B) {
	path := b.TempDir() + "/export-time-bench.jsonl"
	fillChain(b, path, 1000)
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := NewChain(path)
		r, err := c.Export(context.Background(), Filter{
			FromTime: base.Add(250 * time.Second),  // inclusive lower
			ToTime:   base.Add(750 * time.Second),  // exclusive upper
		})
		if err != nil {
			b.Fatalf("export: %v", err)
		}
		if _, err := io.Copy(io.Discard, r); err != nil {
			b.Fatalf("copy: %v", err)
		}
	}
}

// BenchmarkExport_1000Events_FilterByPlanIDs exercises the new Task 30
// multi-PlanIDs code path at scale: 3 plan_ids in one query.
func BenchmarkExport_1000Events_FilterByPlanIDs(b *testing.B) {
	// Build a mixed chain: half plan-bench, plus 250 each of plan-x / plan-y.
	path := b.TempDir() + "/export-planids-bench.jsonl"
	c := NewChain(path)
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 500; i++ {
		_ = c.Append(context.Background(), benchFixtureEvent(i, base))
	}
	for i := 0; i < 250; i++ {
		ev := benchFixtureEvent(500+i, base)
		ev.PlanID = "plan-x"
		ev.EventID = fmt.Sprintf("bench-x-%04d", i)
		_ = c.Append(context.Background(), ev)
	}
	for i := 0; i < 250; i++ {
		ev := benchFixtureEvent(750+i, base)
		ev.PlanID = "plan-y"
		ev.EventID = fmt.Sprintf("bench-y-%04d", i)
		_ = c.Append(context.Background(), ev)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c2 := NewChain(path)
		r, err := c2.Export(context.Background(), Filter{
			PlanIDs: []string{"plan-bench", "plan-y"},
		})
		if err != nil {
			b.Fatalf("export: %v", err)
		}
		if _, err := io.Copy(io.Discard, r); err != nil {
			b.Fatalf("copy: %v", err)
		}
	}
}

// BenchmarkAppend_Single measures the per-event Append cost. Useful for
// understanding the per-op amortisation in BenchmarkAppend_1000Events.
func BenchmarkAppend_Single(b *testing.B) {
	path := b.TempDir() + "/single-append-bench.jsonl"
	c := NewChain(path)
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	// Seed one event so cacheLoaded is true before the timed loop.
	if err := c.Append(context.Background(), benchFixtureEvent(0, base)); err != nil {
		b.Fatalf("seed: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ev := benchFixtureEvent(i+1, base)
		if err := c.Append(context.Background(), ev); err != nil {
			b.Fatalf("append: %v", err)
		}
	}
}