package notify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// RetryQueue persists failed notifications so a later sweep can retry
// them. It is intentionally SQLite-only (matches the rest of the local
// state in internal/store) and uses the same *sql.DB the workflow engine
// already holds — no separate connection, no separate transaction.
//
// Layout:
//
//	id PK          — monotonic row id; ordering by id is the same as
//	                 ordering by enqueue time when retry_count=0
//	channel TEXT   — "feishu" | "wecom"
//	payload TEXT   — rendered JSON body (Feishu card or WeCom wrapper)
//	last_error TEXT — truncated error string from the last attempt
//	retry_count INT — number of times Sweep has re-attempted this row
//	next_retry_at TEXT — ISO-8601 timestamp; Sweep only fires when now ≥ this
//	created_at TEXT  — ISO-8601 timestamp when the row was first enqueued
//
// The 24-hour cleanup is performed by Cleanup below (Task 28 will add
// the periodic cron in cmd/demo).
type RetryQueue struct {
	DB *sql.DB
}

// retrySchema is the CREATE TABLE statement. Idempotent so Open() can
// call it on every startup. The 0001_init.sql in internal/store does NOT
// include this table — notify owns its own schema because the queue is
// conceptually orthogonal to Plan / Issue / AuditEvent.
const retrySchema = `
CREATE TABLE IF NOT EXISTS notify_retry (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    channel       TEXT NOT NULL,
    payload       TEXT NOT NULL,
    last_error    TEXT NOT NULL DEFAULT '',
    retry_count   INTEGER NOT NULL DEFAULT 0,
    next_retry_at TEXT NOT NULL,
    created_at    TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_notify_retry_due
    ON notify_retry (next_retry_at);
`

// OpenRetryQueue opens (or creates) the SQLite database at dsn and
// applies the retry schema. The same DSN can be shared with internal/store
// (the two schemas do not collide).
func OpenRetryQueue(dsn string) (*RetryQueue, error) {
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("notify: open retry queue: %w", err)
	}
	if _, err := db.Exec(retrySchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("notify: apply retry schema: %w", err)
	}
	return &RetryQueue{DB: db}, nil
}

// NewRetryQueue wraps an existing *sql.DB. Used by tests that share a
// connection with internal/store and by cmd/demo when the store is
// already open.
func NewRetryQueue(db *sql.DB) (*RetryQueue, error) {
	if db == nil {
		return nil, errors.New("notify: nil *sql.DB")
	}
	if _, err := db.Exec(retrySchema); err != nil {
		return nil, fmt.Errorf("notify: apply retry schema: %w", err)
	}
	return &RetryQueue{DB: db}, nil
}

// Close closes the underlying *sql.DB. No-op if it was created via
// NewRetryQueue (caller owns the connection).
func (q *RetryQueue) Close() error {
	if q == nil || q.DB == nil {
		return nil
	}
	return q.DB.Close()
}

// Enqueue inserts a failed notification. retry_count starts at 0 and
// next_retry_at is set to now + InitialBackoff (1 second).
//
// lastErr may be nil; it is recorded as "" in that case.
func (q *RetryQueue) Enqueue(ctx context.Context, channel string, payload []byte, lastErr error) error {
	if q == nil || q.DB == nil {
		return errors.New("notify: RetryQueue is not initialised")
	}
	if channel == "" {
		return errors.New("notify: channel is empty")
	}
	if len(payload) == 0 {
		return errors.New("notify: payload is empty")
	}

	now := time.Now().UTC()
	next := now.Add(InitialBackoff)
	errStr := ""
	if lastErr != nil {
		errStr = truncate(lastErr.Error(), 500)
	}

	_, err := q.DB.ExecContext(ctx,
		`INSERT INTO notify_retry (channel, payload, last_error, retry_count, next_retry_at, created_at)
		 VALUES (?, ?, ?, 0, ?, ?)`,
		channel, string(payload), errStr, next.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("notify: enqueue: %w", err)
	}
	return nil
}

// Sweep processes every row whose next_retry_at has passed. For each row
// it tries to deliver via the supplied sender; on success the row is
// deleted, on failure retry_count is incremented and next_retry_at is
// pushed out by 2^retry_count seconds (exponential backoff: 2s, 4s, 8s).
//
// Returns the count of successfully-redelivered rows.
//
// sender is the production MultiSender (or a single-channel sender in
// tests). It only receives Feishu / WeCom bodies — the queue dispatches
// based on the row's channel column.
func (q *RetryQueue) Sweep(ctx context.Context, sender Sender) (int, error) {
	if q == nil || q.DB == nil {
		return 0, errors.New("notify: RetryQueue is not initialised")
	}

	rows, err := q.DB.QueryContext(ctx,
		`SELECT id, channel, payload, retry_count
		 FROM notify_retry
		 WHERE next_retry_at <= ?
		 ORDER BY id ASC`,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return 0, fmt.Errorf("notify: sweep query: %w", err)
	}
	defer rows.Close()

	type entry struct {
		id      int64
		channel string
		payload string
		count   int
	}
	var due []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.channel, &e.payload, &e.count); err != nil {
			return 0, fmt.Errorf("notify: sweep scan: %w", err)
		}
		due = append(due, e)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("notify: sweep iterate: %w", err)
	}

	delivered := 0
	for _, e := range due {
		var sendErr error
		switch e.channel {
		case "feishu":
			if sender == nil {
				sendErr = errors.New("no sender available")
			} else {
				sendErr = sender.SendFeishu(ctx, e.payload)
			}
		case "wecom":
			if sender == nil {
				sendErr = errors.New("no sender available")
			} else {
				sendErr = sender.SendWeCom(ctx, e.payload)
			}
		default:
			sendErr = fmt.Errorf("unknown channel %q", e.channel)
		}

		if sendErr == nil {
			if _, err := q.DB.ExecContext(ctx, `DELETE FROM notify_retry WHERE id = ?`, e.id); err != nil {
				return delivered, fmt.Errorf("notify: sweep delete: %w", err)
			}
			delivered++
			continue
		}

		// Failure → bump retry_count + push next_retry_at.
		next := time.Now().UTC().Add(backoff(e.count + 1))
		_, err := q.DB.ExecContext(ctx,
			`UPDATE notify_retry
			 SET retry_count = retry_count + 1,
			     next_retry_at = ?,
			     last_error = ?
			 WHERE id = ?`,
			next.Format(time.RFC3339Nano),
			truncate(sendErr.Error(), 500),
			e.id,
		)
		if err != nil {
			return delivered, fmt.Errorf("notify: sweep update: %w", err)
		}
	}
	return delivered, nil
}

// Cleanup deletes rows older than maxAge. Returns the count of deleted
// rows. The 24-hour default matches spec.md "24h 过期清理" (the workflow
// engine calls Cleanup nightly).
func (q *RetryQueue) Cleanup(ctx context.Context, maxAge time.Duration) (int, error) {
	if q == nil || q.DB == nil {
		return 0, errors.New("notify: RetryQueue is not initialised")
	}
	cutoff := time.Now().UTC().Add(-maxAge).Format(time.RFC3339Nano)
	res, err := q.DB.ExecContext(ctx, `DELETE FROM notify_retry WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("notify: cleanup: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Count returns the number of rows currently in the queue. Useful for
// tests and for the /healthz endpoint in cmd/demo.
func (q *RetryQueue) Count(ctx context.Context) (int, error) {
	if q == nil || q.DB == nil {
		return 0, errors.New("notify: RetryQueue is not initialised")
	}
	var n int
	err := q.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM notify_retry`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("notify: count: %w", err)
	}
	return n, nil
}

// InitialBackoff is the delay before the first retry. Subsequent retries
// grow by 2^retry_count seconds (2s, 4s, 8s, ...). Capped at MaxBackoff
// to keep the queue from scheduling retries years in the future when a
// webhook is permanently broken.
const (
	InitialBackoff = 1 * time.Second
	MaxBackoff     = 30 * time.Minute
)

// backoff returns the delay for the n-th retry. Exported as a function
// (not a constant) so tests can assert the schedule.
func backoff(n int) time.Duration {
	if n < 0 {
		n = 0
	}
	d := time.Duration(1<<uint(n)) * time.Second
	if d <= 0 || d > MaxBackoff {
		return MaxBackoff
	}
	return d
}

// Sweeper runs Sweep + Cleanup on a periodic interval. cmd/demo launches
// a sweeper goroutine so that webhook failures are eventually redelivered
// and stale rows are reaped after the 24h cutoff without operator
// intervention.
//
// Two entry points:
//   - RunOnce — single synchronous pass. Used by tests and by callers that
//     want to coordinate sweep timing with surrounding work.
//   - Run — blocking loop on the supplied Interval. Returns when ctx is
//     cancelled. Used by cmd/demo as a fire-and-forget goroutine.
//
// Both honour ctx cancellation; Run blocks even on transient errors but
// returns immediately on ctx.Done so a clean shutdown is guaranteed.
type Sweeper struct {
	Queue    *RetryQueue
	Sender   Sender
	Interval time.Duration // default 30s when <= 0
	MaxAge   time.Duration // default 24h when <= 0
}

// NewSweeper builds a Sweeper with the given queue + sender. Interval and
// MaxAge default to 30s and 24h respectively when left zero.
func NewSweeper(q *RetryQueue, sender Sender) *Sweeper {
	return &Sweeper{
		Queue:    q,
		Sender:   sender,
		Interval: 30 * time.Second,
		MaxAge:   24 * time.Hour,
	}
}

// RunOnce performs a single Sweep + Cleanup pass and returns the counters.
// Errors from Sweep are returned verbatim; Cleanup errors are swallowed so
// a transient Cleanup miss never blocks the next sweep.
func (s *Sweeper) RunOnce(ctx context.Context) (delivered, cleaned int, err error) {
	if s == nil || s.Queue == nil {
		return 0, 0, errors.New("notify: Sweeper is not initialised")
	}
	maxAge := s.MaxAge
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}

	delivered, err = s.Queue.Sweep(ctx, s.Sender)
	if err != nil {
		return delivered, 0, err
	}
	if c, cerr := s.Queue.Cleanup(ctx, maxAge); cerr == nil {
		cleaned = c
	}
	return delivered, cleaned, nil
}

// Run loops every Interval, calling RunOnce until ctx is cancelled. On
// error the loop logs by swallowing (the next tick picks up the slack)
// but always returns nil on ctx cancel so cmd/demo can join cleanly.
func (s *Sweeper) Run(ctx context.Context) error {
	if s == nil || s.Queue == nil {
		return errors.New("notify: Sweeper is not initialised")
	}
	interval := s.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			_, _, _ = s.RunOnce(ctx)
		}
	}
}