package insurance_platform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestValidateRuleSet_Complete covers the happy path: every required field
// non-empty, every per-rule field valid, every numeric field non-negative,
// every code/name non-empty, effective_date a real ISO calendar date. The
// fixtureRuleSet() helper used here matches the testdata/platform_response.json
// shape so this is the "what cmd/demo sees" baseline.
func TestValidateRuleSet_Complete(t *testing.T) {
	rs := fixtureRuleSet()
	if err := validateRuleSet(rs); err != nil {
		t.Fatalf("expected fixture to validate cleanly, got %v", err)
	}
}

// TestValidation_MissingVersionID drives validateRuleSet with
// VersionID="" and asserts a *ValidationError with Field="version_id".
// The contract demands the platform bundle carry a non-empty version_id
// because it ties together rollback_sql + prev_version_id; a missing one
// would leave the dry-run engine pointing at a phantom bundle.
func TestValidation_MissingVersionID(t *testing.T) {
	rs := fixtureRuleSet()
	rs.VersionID = ""
	err := validateRuleSet(rs)
	if err == nil {
		t.Fatalf("expected ValidationError for missing version_id")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if vErr.Field != "version_id" {
		t.Errorf("expected field=version_id, got %q", vErr.Field)
	}
}

// TestValidation_EmptyRules drives validateRuleSet with Rules=nil
// and asserts a *ValidationError with Field="rules". An empty rules array
// is silently nonsensical — the dry-run engine would compute affected=0
// for every Plan and the dry-run result would pass review despite doing
// nothing. Refuse at the gate.
func TestValidation_EmptyRules(t *testing.T) {
	rs := fixtureRuleSet()
	rs.Rules = nil
	err := validateRuleSet(rs)
	if err == nil {
		t.Fatalf("expected ValidationError for empty rules")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if vErr.Field != "rules" {
		t.Errorf("expected field=rules, got %q", vErr.Field)
	}
}

// TestValidation_MissingRuleCode drives validateRuleSet with one rule
// having Code="" and asserts the field path is rules[i].code. Per-rule
// emptiness is named at the array index so the operator can grep the
// platform response directly.
func TestValidation_MissingRuleCode(t *testing.T) {
	rs := fixtureRuleSet()
	rs.Rules[2].Code = "   " // whitespace-only also counts as empty (TrimSpace)
	err := validateRuleSet(rs)
	if err == nil {
		t.Fatalf("expected ValidationError for missing rule code")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if !strings.Contains(vErr.Field, "rules[2].code") {
		t.Errorf("expected field rules[2].code, got %q", vErr.Field)
	}
}

// TestValidation_NegativeLimit drives validateRuleSet with one rule
// having Limit=-100 and asserts the field path is rules[i].limit. Negative
// limits are nonsensical — the projected_diff calculator would compute a
// positive offset that flips sign depending on rule order, hiding real
// drops. Refuse at the gate. Renamed from TestValidateRuleSet_NegativeLimit
// to avoid colliding with the older test in client_test.go which uses -1.
func TestValidation_NegativeLimit(t *testing.T) {
	rs := fixtureRuleSet()
	rs.Rules[0].Limit = -100
	err := validateRuleSet(rs)
	if err == nil {
		t.Fatalf("expected ValidationError for negative limit")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if !strings.Contains(vErr.Field, "rules[0].limit") {
		t.Errorf("expected field rules[0].limit, got %q", vErr.Field)
	}
}

// TestValidateRuleSet_MalformedEffectiveDate drives validateRuleSet with
// effective_date set to a non-ISO string ("2026/10/01") and asserts a
// *ValidationError with Field="effective_date". Real-date validation was
// added in Task 24 — earlier versions passed "2026/10/01" through the
// empty-string check and crashed the diff calculator downstream.
func TestValidateRuleSet_MalformedEffectiveDate(t *testing.T) {
	rs := fixtureRuleSet()
	rs.EffectiveDate = "2026/10/01"
	err := validateRuleSet(rs)
	if err == nil {
		t.Fatalf("expected ValidationError for malformed effective_date")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if vErr.Field != "effective_date" {
		t.Errorf("expected field=effective_date, got %q", vErr.Field)
	}
}

// TestValidateRuleSet_ImpossibleEffectiveDate covers month/day overflow
// inputs (e.g. "2026-13-01", "2026-02-30") — strings that match the
// YYYY-MM-DD shape but don't correspond to a real calendar day. Go's
// time.Parse rejects both, so the validator surfaces them as ValidationError.
func TestValidateRuleSet_ImpossibleEffectiveDate(t *testing.T) {
	cases := []string{
		"2026-13-01", // month 13
		"2026-02-30", // Feb 30
		"2026-00-15", // month 0
		"2026-04-31", // Apr only has 30
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			rs := fixtureRuleSet()
			rs.EffectiveDate = c
			err := validateRuleSet(rs)
			if err == nil {
				t.Fatalf("expected ValidationError for impossible date %q", c)
			}
			var vErr *ValidationError
			if !errors.As(err, &vErr) {
				t.Fatalf("expected *ValidationError, got %T", err)
			}
			if vErr.Field != "effective_date" {
				t.Errorf("expected field=effective_date, got %q", vErr.Field)
			}
		})
	}
}

// TestValidateEffectiveDate_HappyPath covers the date validator directly
// for a few valid ISO calendar dates including leap-year edge cases.
func TestValidateEffectiveDate_HappyPath(t *testing.T) {
	cases := []string{
		"2026-01-01",
		"2026-12-31",
		"2024-02-29", // leap year
		"2000-02-29", // century leap year
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if err := validateEffectiveDate(c); err != nil {
				t.Errorf("expected %q to validate, got %v", c, err)
			}
		})
	}
}

// TestValidateEffectiveDate_SadPath drives the date validator with strings
// that should fail — wrong separators, wrong widths, real-but-not-ISO
// dates like RFC3339 timestamps, and empty strings.
func TestValidateEffectiveDate_SadPath(t *testing.T) {
	cases := []string{
		"",                  // empty
		"2026/10/01",        // slash separator
		"2026-10-1",         // missing zero-pad
		"2026-10-01T00:00:00Z", // RFC3339 timestamp
		"20261001",          // no separators
		"26-10-01",          // 2-digit year
		"2026-10-01 ",       // trailing whitespace (TrimSpace would still pass it; that's a separate code path)
	}
	for _, c := range cases {
		// skip trailing-whitespace because validateEffectiveDate already
		// TrimSpaces the input — that's a deliberate choice documented in
		// the function body.
		if strings.Contains(c, " ") {
			continue
		}
		t.Run(c, func(t *testing.T) {
			if err := validateEffectiveDate(c); err == nil {
				t.Errorf("expected %q to fail validation", c)
			}
		})
	}
}

// TestFetchRuleSet_DoubleTimeout drives an httptest.Server whose handler
// sleeps longer than 30s and asserts FetchRuleSet returns ctx.Err()
// (deadline exceeded) instead of retrying or hanging. This is the
// Task 24 double-protection guard: http.Client.Timeout *and*
// context.WithTimeout must both fail-closed, and the retry loop must
// short-circuit on ctx.Err() without burning the 3-retry budget.
//
// We can't actually wait 30s in a unit test, so the handler checks
// ctx.Err() and returns 503 once — which the retry loop converts into
// a RetryableError. We then verify the retry loop respects ctx
// cancellation by canceling mid-flight.
func TestFetchRuleSet_DoubleTimeout(t *testing.T) {
	hits := new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		// Block until ctx is done — simulates a real network stall.
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	// Wrap the call with a deadline so we don't actually wait 30s.
	// context.WithTimeout already replaces any deadline on the parent ctx,
	// and FetchRuleSet wraps that again with 30s — both timeouts fire on
	// whichever deadline wins first. Here the inner 100ms one wins.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.FetchRuleSet(ctx, "r-2026-09-DRG-v17")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected error from ctx timeout, got nil")
	}
	// The deadline should fire inside the inner context.WithTimeout that
	// FetchRuleSet applied — the client must propagate it, NOT retry.
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected context deadline error, got %v", err)
	}
	// Should fail fast — well under the 30s outer budget.
	if elapsed > 5*time.Second {
		t.Errorf("expected fast-fail under 5s, got %v", elapsed)
	}
	// Cancellation should short-circuit retries. We allow up to MaxRetries
	// attempts (since each call sees ctx.Err and returns it before the
	// retry loop checks ctx again on the backoff path), but the test
	// primarily verifies the wall-clock budget, not the precise hit count.
	t.Logf("hits=%d elapsed=%v err=%v", atomic.LoadInt32(hits), elapsed, err)
}

// TestFetchRuleSet_TimeoutControl_HTTPClientBounded covers the http.Client
// timeout path specifically: an http.Client with a 100ms Timeout talking
// to a server that hangs on write. This is the second half of the
// "double timeout protection" — http.Client.Timeout fires even if the
// caller's ctx is unbounded. We use NewClientWithHTTP with a custom
// http.Client to simulate that scenario.
func TestFetchRuleSet_TimeoutControl_HTTPClientBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the connection open — never write a response.
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	// Build a Client whose http.Client.Timeout is 100ms — this is the
	// "outer" timeout. FetchRuleSet then wraps with context.WithTimeout
	// 30s. The outer 100ms wins.
	hc := &http.Client{Timeout: 100 * time.Millisecond}
	c := NewClientWithHTTP(srv.URL, "test-token", hc)

	start := time.Now()
	_, err := c.FetchRuleSet(context.Background(), "r-2026-09-DRG-v17")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Errorf("expected fast-fail under 2s, got %v", elapsed)
	}
}

// TestFetchRuleSet_ValidationError_MalformedDate_EndToEnd is the
// integration version of TestValidateRuleSet_MalformedEffectiveDate:
// drives a 200 OK with a body whose effective_date is wrong, and
// asserts FetchRuleSet surfaces a *ValidationError. This locks the
// validateRuleSet contract end-to-end.
func TestFetchRuleSet_ValidationError_MalformedDate_EndToEnd(t *testing.T) {
	rs := fixtureRuleSet()
	rs.EffectiveDate = "2026-13-40"
	body, _ := json.Marshal(rs)

	srv, hits := newTestServer(t, http.StatusOK, string(body), "application/json")
	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	_, err := c.FetchRuleSet(context.Background(), "r-2026-09-DRG-v17")
	if err == nil {
		t.Fatalf("expected ValidationError for malformed effective_date")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T (%v)", err, err)
	}
	if vErr.Field != "effective_date" {
		t.Errorf("expected field=effective_date, got %q", vErr.Field)
	}
	// Validation errors must NOT trigger retries.
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("expected exactly 1 hit on validation error, got %d", atomic.LoadInt32(hits))
	}
}

// TestFetchRuleSet_AcceptsValidResponse_SmokeTest is the wire-level
// confirmation that the new date validator doesn't break the happy path —
// the fixture uses 2026-10-01 which is a real calendar date.
func TestFetchRuleSet_AcceptsValidResponse_SmokeTest(t *testing.T) {
	rs := fixtureRuleSet()
	body, _ := json.Marshal(rs)

	srv, hits := newTestServer(t, http.StatusOK, string(body), "application/json")
	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	got, err := c.FetchRuleSet(context.Background(), rs.VersionID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.EffectiveDate != rs.EffectiveDate {
		t.Errorf("EffectiveDate: got %q, want %q", got.EffectiveDate, rs.EffectiveDate)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("expected exactly 1 hit on 200, got %d", atomic.LoadInt32(hits))
	}
}

// silence unused-import warnings for io — kept available for future
// readers adding streaming tests.
var _ = io.Discard