package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// bytesRepeat returns a fixed-size buffer filled with b. Used to build
// long-payload test fixtures without skipping length-zero corner cases.
func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// retryDSN builds a unique in-memory DSN per test so file::memory:?cache=shared
// state never leaks across tests (this regression was caught during task 9
// development; the pattern is preserved here for the new table-driven tests).
func retryDSN(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("file:test-retry-%d-%d?mode=memory&cache=shared",
		time.Now().UnixNano(), os.Getpid())
}

// newRetryQueue builds an isolated in-memory RetryQueue.
func newRetryQueue(t *testing.T) *RetryQueue {
	t.Helper()
	q, err := OpenRetryQueue(retryDSN(t))
	if err != nil {
		t.Fatalf("OpenRetryQueue: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

// TestRetryQueue_EnqueueTableDriven covers the happy-path and rejection
// branches of Enqueue. Each row seeds the queue with a single payload and
// asserts (a) the error return matches expected, (b) the count afterwards
// matches the expectation, (c) nil receiver / nil DB rejects.
func TestRetryQueue_EnqueueTableDriven(t *testing.T) {
	cases := []struct {
		name      string
		channel   string
		payload   []byte
		lastErr   error
		wantErr   bool
		wantCount int
	}{
		{
			name:      "feishu_no_error",
			channel:   "feishu",
			payload:   []byte(`{"x":1}`),
			lastErr:   nil,
			wantErr:   false,
			wantCount: 1,
		},
		{
			name:      "wecom_with_error",
			channel:   "wecom",
			payload:   []byte(`{"y":2}`),
			lastErr:   errors.New("transient webhook fail"),
			wantErr:   false,
			wantCount: 1,
		},
		{
			name:      "empty_channel_rejected",
			channel:   "",
			payload:   []byte(`{}`),
			lastErr:   nil,
			wantErr:   true,
			wantCount: 0,
		},
		{
			name:      "empty_payload_rejected",
			channel:   "feishu",
			payload:   nil,
			lastErr:   nil,
			wantErr:   true,
			wantCount: 0,
		},
		{
			name:      "long_payload_accepted",
			channel:   "feishu",
			payload:   bytesRepeat('A', 4096), // 4 KB JSON body — well within reason
			lastErr:   nil,
			wantErr:   false,
			wantCount: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := newRetryQueue(t)
			ctx := context.Background()

			err := q.Enqueue(ctx, tc.channel, tc.payload, tc.lastErr)
			if (err != nil) != tc.wantErr {
				t.Errorf("Enqueue error: got %v, wantErr=%v", err, tc.wantErr)
			}
			n, cErr := q.Count(ctx)
			if cErr != nil {
				t.Fatalf("Count: %v", cErr)
			}
			if n != tc.wantCount {
				t.Errorf("Count: got %d, want %d", n, tc.wantCount)
			}
		})
	}

	t.Run("nil_receiver_rejected", func(t *testing.T) {
		var q *RetryQueue
		if err := q.Enqueue(context.Background(), "feishu", []byte(`{}`), nil); err == nil {
			t.Error("Enqueue on nil receiver must error")
		}
	})
}

// TestRetryQueue_SweepTableDriven covers the four Sweep branches: row due +
// success (deleted), row due + fail (retry_count++), row not due (skip), mixed.
// Each row uses an httptest.Server to control delivery semantics.
func TestRetryQueue_SweepTableDriven(t *testing.T) {
	cases := []struct {
		name          string
		setup         func(t *testing.T, q *RetryQueue) // populate queue
		advanceTime   time.Duration                    // sleep before sweep to make rows due
		serverStatus  int                              // server response code
		wantDelivered int
		wantRemaining int
	}{
		{
			name: "single_due_row_success",
			setup: func(t *testing.T, q *RetryQueue) {
				if err := q.Enqueue(context.Background(), "feishu", []byte(`{}`), nil); err != nil {
					t.Fatalf("Enqueue: %v", err)
				}
			},
			advanceTime:   InitialBackoff + 100*time.Millisecond,
			serverStatus:  http.StatusOK,
			wantDelivered: 1,
			wantRemaining: 0,
		},
		{
			name: "single_due_row_permanent_failure_keeps_row",
			setup: func(t *testing.T, q *RetryQueue) {
				if err := q.Enqueue(context.Background(), "feishu", []byte(`{}`), errors.New("transient")); err != nil {
					t.Fatalf("Enqueue: %v", err)
				}
			},
			advanceTime:   InitialBackoff + 100*time.Millisecond,
			serverStatus:  http.StatusUnauthorized, // 401 → permanent; row stays, retry_count++
			wantDelivered: 0,
			wantRemaining: 1,
		},
		{
			name: "row_not_due_skipped",
			setup: func(t *testing.T, q *RetryQueue) {
				if err := q.Enqueue(context.Background(), "feishu", []byte(`{}`), nil); err != nil {
					t.Fatalf("Enqueue: %v", err)
				}
			},
			advanceTime:   0, // row is not yet due
			serverStatus:  http.StatusOK,
			wantDelivered: 0,
			wantRemaining: 1,
		},
		{
			name: "mixed_due_and_not_due_only_due_processed",
			setup: func(t *testing.T, q *RetryQueue) {
				ctx := context.Background()
				// Row 1: enqueued first, then waits long enough to be due.
				if err := q.Enqueue(ctx, "feishu", []byte(`due`), nil); err != nil {
					t.Fatalf("Enqueue1: %v", err)
				}
				time.Sleep(InitialBackoff + 50*time.Millisecond) // row 1 due at T1=now+1s; we are now past that
				if err := q.Enqueue(ctx, "feishu", []byte(`fresh`), nil); err != nil {
					t.Fatalf("Enqueue2: %v", err)
				}
				// Row 2 next_retry_at = now + 1s; Sweep will fire immediately
				// so row 2 is NOT yet due (≤ 50 ms after enqueue < 1 s next_retry_at).
			},
			advanceTime:   0, // no extra sleep — row 2 must remain not-due
			serverStatus:  http.StatusOK,
			wantDelivered: 1,
			wantRemaining: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := newRetryQueue(t)
			ctx := context.Background()
			tc.setup(t, q)

			// Build a static sender.
			hits := new(int32)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(hits, 1)
				w.WriteHeader(tc.serverStatus)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()
			sender := NewFeishuSenderWithHTTP(srv.URL, srv.Client())

			if tc.advanceTime > 0 {
				time.Sleep(tc.advanceTime)
			}

			delivered, err := q.Sweep(ctx, sender)
			if err != nil {
				t.Fatalf("Sweep: %v", err)
			}
			if delivered != tc.wantDelivered {
				t.Errorf("delivered: got %d, want %d", delivered, tc.wantDelivered)
			}
			n, _ := q.Count(ctx)
			if n != tc.wantRemaining {
				t.Errorf("Count after sweep: got %d, want %d", n, tc.wantRemaining)
			}
		})
	}
}

// TestRetryQueue_CleanupTableDriven covers Cleanup branches: rows older than
// maxAge removed, rows younger kept, cutoff=0 removes everything.
func TestRetryQueue_CleanupTableDriven(t *testing.T) {
	cases := []struct {
		name    string
		seed    func(t *testing.T, q *RetryQueue)
		maxAge  time.Duration
		wait    time.Duration // wait before calling cleanup so rows age past cutoff
		wantDel int
		wantLef int
	}{
		{
			name: "row_older_than_max_age_removed",
			seed: func(t *testing.T, q *RetryQueue) {
				if err := q.Enqueue(context.Background(), "feishu", []byte(`{}`), nil); err != nil {
					t.Fatalf("Enqueue: %v", err)
				}
			},
			maxAge:  500 * time.Millisecond,
			wait:    1 * time.Second, // sleep so the row is older than maxAge
			wantDel: 1,
			wantLef: 0,
		},
		{
			name: "row_younger_than_max_age_kept",
			seed: func(t *testing.T, q *RetryQueue) {
				if err := q.Enqueue(context.Background(), "feishu", []byte(`{}`), nil); err != nil {
					t.Fatalf("Enqueue: %v", err)
				}
			},
			maxAge:  1 * time.Hour,
			wait:    100 * time.Millisecond,
			wantDel: 0,
			wantLef: 1,
		},
		{
			name: "zero_max_age_removes_everything",
			seed: func(t *testing.T, q *RetryQueue) {
				for i := 0; i < 3; i++ {
					if err := q.Enqueue(context.Background(), "feishu", []byte(`{}`), nil); err != nil {
						t.Fatalf("Enqueue: %v", err)
					}
				}
			},
			maxAge:  0,
			wait:    0,
			wantDel: 3,
			wantLef: 0,
		},
		{
			name: "mixed_ages_only_old_removed",
			seed: func(t *testing.T, q *RetryQueue) {
				if err := q.Enqueue(context.Background(), "feishu", []byte(`old`), nil); err != nil {
					t.Fatalf("Enqueue old: %v", err)
				}
				time.Sleep(600 * time.Millisecond)
				if err := q.Enqueue(context.Background(), "feishu", []byte(`new`), nil); err != nil {
					t.Fatalf("Enqueue new: %v", err)
				}
			},
			maxAge:  500 * time.Millisecond,
			wait:    0,
			wantDel: 1, // the old row
			wantLef: 1, // the new row
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := newRetryQueue(t)
			ctx := context.Background()
			tc.seed(t, q)

			if tc.wait > 0 {
				time.Sleep(tc.wait)
			}

			deleted, err := q.Cleanup(ctx, tc.maxAge)
			if err != nil {
				t.Fatalf("Cleanup: %v", err)
			}
			if deleted != tc.wantDel {
				t.Errorf("deleted: got %d, want %d", deleted, tc.wantDel)
			}
			left, _ := q.Count(ctx)
			if left != tc.wantLef {
				t.Errorf("left after cleanup: got %d, want %d", left, tc.wantLef)
			}
		})
	}
}

// TestRetryQueue_BackoffScheduleTableDriven is an expanded, fully table-
// driven version of TestRetryQueue_BackoffSchedule from webhook_test.go. The
// spec calls for "24h 过期清理" so we also assert the cap stays well below
// 24h at any retry count.
func TestRetryQueue_BackoffScheduleTableDriven(t *testing.T) {
	cases := []struct {
		n    int
		want time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 32 * time.Second},
		{6, 64 * time.Second},
		{7, 128 * time.Second}, // ~2 min — still under MaxBackoff (30m)
		{12, MaxBackoff},       // 2^12 = 4096s ≈ 68m > 30m cap
		{60, MaxBackoff},       // far in future: still capped
		{-1, 1 * time.Second},  // negative n normalises to 0 → 1s
	}

	for _, c := range cases {
		got := backoff(c.n)
		if got != c.want {
			t.Errorf("backoff(%d): got %v, want %v", c.n, got, c.want)
		}
		// Spec invariant: no backoff value should exceed 24h (Cleanup boundary).
		if got > 24*time.Hour {
			t.Errorf("backoff(%d) = %v exceeds 24h cleanup window", c.n, got)
		}
	}
}

// TestRetryQueue_NilGuardsTableDriven covers the four nil-receiver guards
// (Enqueue / Sweep / Cleanup / Count) as a table so the run log shows each
// branch cleanly.
func TestRetryQueue_NilGuardsTableDriven(t *testing.T) {
	cases := []struct {
		name string
		fn   func() error
	}{
		{"Enqueue", func() error {
			var q *RetryQueue
			return q.Enqueue(context.Background(), "feishu", []byte(`{}`), nil)
		}},
		{"Sweep", func() error {
			var q *RetryQueue
			_, err := q.Sweep(context.Background(), nil)
			return err
		}},
		{"Cleanup", func() error {
			var q *RetryQueue
			_, err := q.Cleanup(context.Background(), time.Hour)
			return err
		}},
		{"Count", func() error {
			var q *RetryQueue
			_, err := q.Count(context.Background())
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.fn(); err == nil {
				t.Errorf("%s on nil receiver must error", c.name)
			}
		})
	}
}

// TestRetryQueue_Sweeper_RunOnce is the unit-level sanity check for the
// new Sweeper: a one-shot RunOnce must perform exactly one Sweep + one
// Cleanup pass and return deterministic counters.
func TestRetryQueue_Sweeper_RunOnce(t *testing.T) {
	q := newRetryQueue(t)
	ctx := context.Background()

	// Seed one due row + one not-yet-due row.
	if err := q.Enqueue(ctx, "feishu", []byte(`{}`), nil); err != nil {
		t.Fatalf("Enqueue1: %v", err)
	}
	time.Sleep(InitialBackoff + 50*time.Millisecond)
	if err := q.Enqueue(ctx, "feishu", []byte(`{}`), nil); err != nil {
		t.Fatalf("Enqueue2: %v", err)
	}

	srv, _ := newStaticServer(t, http.StatusOK, `{}`)
	sender := NewFeishuSenderWithHTTP(srv.URL, srv.Client())

	sw := NewSweeper(q, sender)
	delivered, cleaned, err := sw.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if delivered != 1 {
		t.Errorf("delivered: got %d, want 1", delivered)
	}
	if cleaned != 0 {
		t.Errorf("cleaned: got %d, want 0 (rows are fresh)", cleaned)
	}
	left, _ := q.Count(ctx)
	if left != 1 {
		t.Errorf("Count after RunOnce: got %d, want 1", left)
	}
}

// TestRetryQueue_Sweeper_RunUntilCancel exercises the blocking Run loop
// under ctx cancellation: it must respect ctx.Done() promptly (within
// ~Interval duration).
func TestRetryQueue_Sweeper_RunUntilCancel(t *testing.T) {
	q := newRetryQueue(t)
	srv, _ := newStaticServer(t, http.StatusOK, `{}`)
	sender := NewFeishuSenderWithHTTP(srv.URL, srv.Client())

	sw := NewSweeper(q, sender)
	// Use a tiny interval so we observe at least one tick before cancel.
	sw.Interval = 30 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- sw.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && err.Error() != context.DeadlineExceeded.Error() {
			// Run returns nil on ctx cancel; only fail if the sweeper reported
			// an unrelated error.
			t.Fatalf("Run returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s after ctx cancel — leaked goroutine?")
	}
}

// TestRetryQueue_Sweeper_NilSender — the sweeper should still call Sweep
// (which itself tolerates a nil sender by setting per-row sendErr) but the
// rows that come back failed will not be re-delivered. Cleanup still works.
func TestRetryQueue_Sweeper_NilSender(t *testing.T) {
	q := newRetryQueue(t)
	ctx := context.Background()

	if err := q.Enqueue(ctx, "feishu", []byte(`{}`), nil); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	time.Sleep(InitialBackoff + 50*time.Millisecond)

	sw := NewSweeper(q, nil)
	delivered, _, err := sw.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered with nil sender: got %d, want 0", delivered)
	}
	// Row must still be present (sender was nil → permanent sendErr → retry_count++).
	left, _ := q.Count(ctx)
	if left != 1 {
		t.Errorf("Count: got %d, want 1", left)
	}
}
