// Comprehensive table-driven tests for Chain.Export filtering and a
// dedicated benchmark file for the 1000+ event performance budget.
//
// The original chain_test.go (Task 10) covered each filter as an isolated
// test. Task 30 deepens that into:
//   - one table-driven test that hits all 5 spec-mandated filter types
//     (plan_id single, plan_id multi, actor, action, time range) in a
//     single function so a regression in any branch is reported with one
//     test name
//   - one table-driven edge-case test (no matches, combined filters,
//     oversized limit, reversed range, etc.)
//   - one explicit multi-PlanIDs test (the new feature added by Task 30)
//
// The benchmarks live in export_bench_test.go alongside this file.
package audit

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/example/insurance-rule-rollout/internal/workflow"
)

// multiPlanFixture seeds three plans × three actions = 9 events. The IDs
// are intentionally chosen so each plan has a unique prefix the tests can
// grep for. Timestamps step by 1 minute so the time-range filter has a
// non-ambiguous bound to test against.
func multiPlanFixture() []workflow.AuditEvent {
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	return []workflow.AuditEvent{
		// plan-aaa: 4 events
		{
			EventID:   "evt-aaa-001",
			PlanID:    "plan-aaa",
			Actor:     "yang_jb",
			Action:    workflow.ActionSubmitDraft,
			Payload:   map[string]string{"from": "draft", "to": "pending"},
			Timestamp: base,
		},
		{
			EventID:   "evt-aaa-002",
			PlanID:    "plan-aaa",
			Actor:     "wang_xx",
			Action:    workflow.ActionApprove,
			Payload:   map[string]string{"from": "pending", "to": "approved"},
			Timestamp: base.Add(1 * time.Minute),
		},
		{
			EventID:   "evt-aaa-003",
			PlanID:    "plan-aaa",
			Actor:     "wang_xx",
			Action:    workflow.ActionRequest,
			Payload:   map[string]string{"note": "补医保对照单"},
			Timestamp: base.Add(2 * time.Minute),
		},
		{
			EventID:   "evt-aaa-004",
			PlanID:    "plan-aaa",
			Actor:     "liu_yy",
			Action:    workflow.ActionApply,
			Payload:   map[string]string{"from": "approved", "to": "applied"},
			Timestamp: base.Add(3 * time.Minute),
		},
		// plan-bbb: 3 events
		{
			EventID:   "evt-bbb-001",
			PlanID:    "plan-bbb",
			Actor:     "yang_jb",
			Action:    workflow.ActionSubmitDraft,
			Payload:   map[string]string{"from": "draft", "to": "pending"},
			Timestamp: base.Add(4 * time.Minute),
		},
		{
			EventID:   "evt-bbb-002",
			PlanID:    "plan-bbb",
			Actor:     "zhao_zz",
			Action:    workflow.ActionApprove,
			Payload:   map[string]string{"from": "pending", "to": "approved"},
			Timestamp: base.Add(5 * time.Minute),
		},
		{
			EventID:   "evt-bbb-003",
			PlanID:    "plan-bbb",
			Actor:     "zhao_zz",
			Action:    workflow.ActionApply,
			Payload:   map[string]string{"from": "approved", "to": "applied"},
			Timestamp: base.Add(6 * time.Minute),
		},
		// plan-ccc: 2 events
		{
			EventID:   "evt-ccc-001",
			PlanID:    "plan-ccc",
			Actor:     "yang_jb",
			Action:    workflow.ActionReject,
			Payload:   map[string]string{"from": "pending", "to": "rejected", "reason": "差异过大"},
			Timestamp: base.Add(7 * time.Minute),
		},
		{
			EventID:   "evt-ccc-002",
			PlanID:    "plan-ccc",
			Actor:     "wang_xx",
			Action:    workflow.ActionApprove,
			Payload:   map[string]string{"from": "pending", "to": "approved"},
			Timestamp: base.Add(8 * time.Minute),
		},
	}
}

// drainAndDecode consumes the export reader fully and decodes every JSONL
// row. Returning the []workflow.AuditEvent slice lets each test assert on
// the canonical decoded shape rather than parsing the raw bytes twice.
func drainAndDecode(t *testing.T, r io.Reader) []workflow.AuditEvent {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if len(data) == 0 {
		return nil
	}
	var out []workflow.AuditEvent
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev workflow.AuditEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode line %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

// TestExport_TableDriven_FiveFilterTypes runs all 5 spec-mandated filter
// shapes in a single test so a regression in any branch is named in one
// place. The test verifies:
//
//  1. plan_id single value (PlanID = "plan-aaa" → 4 rows)
//  2. plan_id multi value (PlanIDs = [plan-aaa, plan-ccc] → 6 rows)
//  3. actor single value (Actor = "yang_jb" → 3 rows)
//  4. action single value (Action = ActionApply → 2 rows)
//  5. time range (FromTime, ToTime inclusive-exclusive → 3 rows)
//
// Each sub-case asserts count + that every returned row satisfies the
// filter (so an off-by-one in matchesFilter cannot pass silently).
func TestExport_TableDriven_FiveFilterTypes(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, multiPlanFixture())
	base := multiPlanFixture()[0].Timestamp

	type wantShape struct {
		count         int
		eventIDs      []string              // exact list (order = JSONL order on disk)
		planIDs       []string              // optional: assert each row's plan_id
		actors        []string              // optional: assert each row's actor
		actions       []workflow.IssueAction // optional: assert each row's action
		timestampFrom time.Time
		timestampTo   time.Time
	}

	cases := []struct {
		name   string
		filter Filter
		want   wantShape
	}{
		{
			name:   "plan_id_single",
			filter: Filter{PlanID: "plan-aaa"},
			want: wantShape{
				count:    4,
				eventIDs: []string{"evt-aaa-001", "evt-aaa-002", "evt-aaa-003", "evt-aaa-004"},
				planIDs:  []string{"plan-aaa", "plan-aaa", "plan-aaa", "plan-aaa"},
			},
		},
		{
			name:   "plan_id_multi",
			filter: Filter{PlanIDs: []string{"plan-aaa", "plan-ccc"}},
			want: wantShape{
				count: 6,
				eventIDs: []string{
					"evt-aaa-001", "evt-aaa-002", "evt-aaa-003", "evt-aaa-004",
					"evt-ccc-001", "evt-ccc-002",
				},
				planIDs: []string{"plan-aaa", "plan-aaa", "plan-aaa", "plan-aaa", "plan-ccc", "plan-ccc"},
			},
		},
		{
			name:   "actor_single",
			filter: Filter{Actor: "yang_jb"},
			want: wantShape{
				count:    3,
				eventIDs: []string{"evt-aaa-001", "evt-bbb-001", "evt-ccc-001"},
				actors:   []string{"yang_jb", "yang_jb", "yang_jb"},
			},
		},
		{
			name:   "action_single",
			filter: Filter{Action: workflow.ActionApply},
			want: wantShape{
				count:    2,
				eventIDs: []string{"evt-aaa-004", "evt-bbb-003"},
				actions:  []workflow.IssueAction{workflow.ActionApply, workflow.ActionApply},
			},
		},
		{
			name: "time_range_inclusive_exclusive",
			filter: Filter{
				FromTime: base.Add(2 * time.Minute), // inclusive lower
				ToTime:   base.Add(5 * time.Minute), // exclusive upper
			},
			want: wantShape{
				count:    3,
				eventIDs: []string{"evt-aaa-003", "evt-aaa-004", "evt-bbb-001"},
				timestampFrom: base.Add(2 * time.Minute),
				timestampTo:   base.Add(5 * time.Minute),
			},
		},
	}

	c := NewChain(path)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := c.Export(context.Background(), tc.filter)
			if err != nil {
				t.Fatalf("export: %v", err)
			}
			got := drainAndDecode(t, r)

			if len(got) != tc.want.count {
				t.Fatalf("count = %d, want %d (event_ids = %v)",
					len(got), tc.want.count, eventIDList(got))
			}
			for i, wantID := range tc.want.eventIDs {
				if got[i].EventID != wantID {
					t.Fatalf("row %d event_id = %q, want %q",
						i, got[i].EventID, wantID)
				}
			}
			for i, wantPlan := range tc.want.planIDs {
				if got[i].PlanID != wantPlan {
					t.Fatalf("row %d plan_id = %q, want %q",
						i, got[i].PlanID, wantPlan)
				}
			}
			for i, wantActor := range tc.want.actors {
				if got[i].Actor != wantActor {
					t.Fatalf("row %d actor = %q, want %q",
						i, got[i].Actor, wantActor)
				}
			}
			for i, wantAction := range tc.want.actions {
				if got[i].Action != wantAction {
					t.Fatalf("row %d action = %q, want %q",
						i, got[i].Action, wantAction)
				}
			}
			if !tc.want.timestampFrom.IsZero() {
				for i, ev := range got {
					if ev.Timestamp.Before(tc.want.timestampFrom) {
						t.Fatalf("row %d timestamp %v before FromTime %v",
							i, ev.Timestamp, tc.want.timestampFrom)
					}
				}
			}
			if !tc.want.timestampTo.IsZero() {
				for i, ev := range got {
					if !ev.Timestamp.Before(tc.want.timestampTo) {
						t.Fatalf("row %d timestamp %v not before ToTime %v",
							i, ev.Timestamp, tc.want.timestampTo)
					}
				}
			}
		})
	}
}

// TestExport_TableDriven_EdgeCases locks down the corner cases that were
// easy to overlook in the original happy-path tests:
//
//   - no match returns 0 rows
//   - limit larger than data returns all matches
//   - limit exactly equal to data returns all matches
//   - offset past end returns 0 rows
//   - reversed time range (FromTime > ToTime) returns 0 rows
//   - combined plan_id + actor + action is AND'd
//   - empty PlanIDs slice is treated as "no constraint" (back-compat)
func TestExport_TableDriven_EdgeCases(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, multiPlanFixture())
	base := multiPlanFixture()[0].Timestamp

	cases := []struct {
		name   string
		filter Filter
		want   int
	}{
		{
			name:   "no_match",
			filter: Filter{PlanID: "plan-does-not-exist"},
			want:   0,
		},
		{
			name:   "limit_larger_than_data",
			filter: Filter{PlanID: "plan-aaa", Limit: 99},
			want:   4,
		},
		{
			name:   "limit_equal_to_data",
			filter: Filter{PlanID: "plan-aaa", Limit: 4},
			want:   4,
		},
		{
			name:   "offset_past_end",
			filter: Filter{PlanID: "plan-aaa", Offset: 99},
			want:   0,
		},
		{
			name: "reversed_time_range",
			filter: Filter{
				FromTime: base.Add(5 * time.Minute),
				ToTime:   base.Add(2 * time.Minute),
			},
			want: 0,
		},
		{
			name: "combined_plan_actor_action_AND",
			filter: Filter{
				PlanID: "plan-aaa",
				Actor:  "wang_xx",
				Action: workflow.ActionApprove,
			},
			want: 1,
		},
		{
			name:   "empty_planids_no_constraint",
			filter: Filter{PlanIDs: []string{}, Action: workflow.ActionReject},
			want:   1,
		},
		{
			name: "planid_and_planids_or",
			filter: Filter{
				PlanID:  "plan-bbb",
				PlanIDs: []string{"plan-ccc"},
			},
			want: 5, // 3 bbb + 2 ccc
		},
		{
			name: "limit_with_offset",
			filter: Filter{
				Action: workflow.ActionApprove,
				Offset: 1,
				Limit:  1,
			},
			want: 1, // 4 approves total → skip 1 → emit 1
		},
		{
			name: "time_range_single_event_window",
			filter: Filter{
				FromTime: base.Add(2 * time.Minute),
				ToTime:   base.Add(2*time.Minute + time.Nanosecond),
			},
			want: 1, // only evt-aaa-003 at exactly +2min
		},
	}

	c := NewChain(path)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := c.Export(context.Background(), tc.filter)
			if err != nil {
				t.Fatalf("export: %v", err)
			}
			got := drainAndDecode(t, r)
			if len(got) != tc.want {
				t.Fatalf("count = %d, want %d (event_ids = %v)",
					len(got), tc.want, eventIDList(got))
			}
		})
	}
}

// TestExport_PlanIDsMultiValue_DirectTest is a dedicated test for the new
// Task 30 feature. It exists in addition to the table-driven case above so
// the multi-value path has a self-explanatory name in the test output and
// a richer assertion (count + ordering + that no other plans leak).
func TestExport_PlanIDsMultiValue_DirectTest(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, multiPlanFixture())

	c := NewChain(path)
	r, err := c.Export(context.Background(), Filter{
		PlanIDs: []string{"plan-aaa", "plan-ccc"},
	})
	if err != nil {
		t.Fatalf("export PlanIDs: %v", err)
	}
	got := drainAndDecode(t, r)

	if len(got) != 6 {
		t.Fatalf("count = %d, want 6 (event_ids = %v)", len(got), eventIDList(got))
	}
	wantOrder := []string{
		"evt-aaa-001", "evt-aaa-002", "evt-aaa-003", "evt-aaa-004",
		"evt-ccc-001", "evt-ccc-002",
	}
	for i, want := range wantOrder {
		if got[i].EventID != want {
			t.Fatalf("row %d event_id = %q, want %q (full order: %v)",
				i, got[i].EventID, want, eventIDList(got))
		}
	}
	for _, ev := range got {
		if ev.PlanID != "plan-aaa" && ev.PlanID != "plan-ccc" {
			t.Fatalf("plan_id %q leaked into multi-value filter", ev.PlanID)
		}
	}

	// Empty PlanIDs combined with PlanID is the back-compat path.
	r2, err := c.Export(context.Background(), Filter{
		PlanID:  "plan-aaa",
		PlanIDs: []string{}, // explicitly empty
	})
	if err != nil {
		t.Fatalf("export PlanID+empty PlanIDs: %v", err)
	}
	got2 := drainAndDecode(t, r2)
	if len(got2) != 4 {
		t.Fatalf("PlanID + empty PlanIDs: count = %d, want 4", len(got2))
	}
}

// TestExport_FilterDoesNotMutateChain makes sure repeated Export calls
// with different filters are idempotent and don't leak cache state. The
// 飞检 inspector runs many queries in sequence, so a leaky filter would
// silently corrupt subsequent outputs.
func TestExport_FilterDoesNotMutateChain(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, multiPlanFixture())

	c := NewChain(path)

	// Run filter 1.
	got1 := drainAndDecode(t, mustExport(t, c, Filter{PlanID: "plan-aaa"}))
	// Run filter 2.
	got2 := drainAndDecode(t, mustExport(t, c, Filter{PlanID: "plan-bbb"}))
	// Re-run filter 1 — must produce the same rows as the first call.
	got1b := drainAndDecode(t, mustExport(t, c, Filter{PlanID: "plan-aaa"}))

	if len(got1) != 4 {
		t.Fatalf("filter1 first run count = %d, want 4", len(got1))
	}
	if len(got2) != 3 {
		t.Fatalf("filter2 count = %d, want 3", len(got2))
	}
	if len(got1b) != 4 {
		t.Fatalf("filter1 second run count = %d, want 4", len(got1b))
	}
	for i := range got1 {
		if got1[i].EventID != got1b[i].EventID {
			t.Fatalf("filter1 unstable: row %d = %q vs %q",
				i, got1[i].EventID, got1b[i].EventID)
		}
	}
}

// eventIDList is a debug helper: dumps the event IDs of a slice so failure
// messages tell the operator which rows the test actually saw.
func eventIDList(events []workflow.AuditEvent) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.EventID
	}
	return out
}

// mustExport wraps Export so each call site can be a single line.
func mustExport(t *testing.T, c *Chain, f Filter) io.Reader {
	t.Helper()
	r, err := c.Export(context.Background(), f)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	return r
}