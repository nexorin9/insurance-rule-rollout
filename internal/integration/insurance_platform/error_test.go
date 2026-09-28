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

	"github.com/example/insurance-rule-rollout/internal/config"
)

// TestClassifyHTTPError_401_Unauthorized covers the 401 → *UnauthorizedError
// branch of classifyHTTPError / FetchRuleSet. The client must NOT retry —
// token re-fetch is the operator's responsibility, not the client's. Verifies
// that:
//   - error type is *UnauthorizedError
//   - status field is 401
//   - the embedded *PlatformAPIError is also reachable via errors.As (backward compat)
//   - exactly 1 HTTP hit (no retry)
//   - response time stays under 2s (no exponential backoff)
func TestClassifyHTTPError_401_Unauthorized(t *testing.T) {
	hits := new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid_token"}`)
	}))
	t.Cleanup(srv.Close)
	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	start := time.Now()
	_, err := c.FetchRuleSet(context.Background(), "r-2026-09-DRG-v17")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected error on 401")
	}
	var unauthorized *UnauthorizedError
	if !errors.As(err, &unauthorized) {
		t.Fatalf("expected *UnauthorizedError, got %T (%v)", err, err)
	}
	if unauthorized.Status != http.StatusUnauthorized {
		t.Errorf("Status: got %d, want %d", unauthorized.Status, http.StatusUnauthorized)
	}
	if unauthorized.Retryable {
		t.Errorf("401 must NOT be retryable")
	}
	if !strings.Contains(err.Error(), "re-fetch") {
		t.Errorf("expected message to mention token re-fetch, got %q", err.Error())
	}
	// Backward compatibility — the embedded PlatformAPIError must still
	// be reachable for callers that predate the typed split.
	var apiErr *PlatformAPIError
	if !errors.As(err, &apiErr) {
		t.Errorf("expected embedded *PlatformAPIError to be reachable via errors.As")
	} else if apiErr.Status != http.StatusUnauthorized {
		t.Errorf("embedded PlatformAPIError.Status: got %d, want %d", apiErr.Status, http.StatusUnauthorized)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("expected exactly 1 hit (no retry), got %d", *hits)
	}
	if elapsed >= 2*time.Second {
		t.Errorf("expected sub-2s response (no retry), got %v", elapsed)
	}
}

// TestClassifyHTTPError_403_Unauthorized covers the 403 → *UnauthorizedError
// branch. Same shape as the 401 test but verifies that 403 also routes to
// the unauthorized family (no retry, token re-fetch prompt).
func TestClassifyHTTPError_403_Unauthorized(t *testing.T) {
	hits := new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"forbidden"}`)
	}))
	t.Cleanup(srv.Close)
	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	_, err := c.FetchRuleSet(context.Background(), "r-2026-09-DRG-v17")

	if err == nil {
		t.Fatalf("expected error on 403")
	}
	var unauthorized *UnauthorizedError
	if !errors.As(err, &unauthorized) {
		t.Fatalf("expected *UnauthorizedError, got %T (%v)", err, err)
	}
	if unauthorized.Status != http.StatusForbidden {
		t.Errorf("Status: got %d, want %d", unauthorized.Status, http.StatusForbidden)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("expected exactly 1 hit (no retry), got %d", *hits)
	}
}

// TestClassifyHTTPError_404_NotFound covers the 404 → *NotFoundError branch.
// versionID typo / bundle withdrawn — retrying will not conjure the bundle
// into existence, so the client must NOT retry.
func TestClassifyHTTPError_404_NotFound(t *testing.T) {
	hits := new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"version_not_found"}`)
	}))
	t.Cleanup(srv.Close)
	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	_, err := c.FetchRuleSet(context.Background(), "r-does-not-exist")

	if err == nil {
		t.Fatalf("expected error on 404")
	}
	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected *NotFoundError, got %T (%v)", err, err)
	}
	if notFound.Status != http.StatusNotFound {
		t.Errorf("Status: got %d, want %d", notFound.Status, http.StatusNotFound)
	}
	if notFound.Retryable {
		t.Errorf("404 must NOT be retryable")
	}
	if !strings.Contains(err.Error(), "versionID") {
		t.Errorf("expected message to mention versionID, got %q", err.Error())
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("expected exactly 1 hit (no retry), got %d", *hits)
	}
}

// TestClassifyHTTPError_500_Retryable covers the 500 → *RetryableError
// branch with retry exhaustion. Verifies that:
//   - error type is *RetryableError after retries exhaust
//   - exactly MaxRetries (3) HTTP hits
//   - elapsed time is at least 1s + 2s = 3s of backoff
func TestClassifyHTTPError_500_Retryable(t *testing.T) {
	hits := new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"db_down"}`)
	}))
	t.Cleanup(srv.Close)
	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	start := time.Now()
	_, err := c.FetchRuleSet(context.Background(), "r-2026-09-DRG-v17")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected error after retries exhausted")
	}
	var retryable *RetryableError
	if !errors.As(err, &retryable) {
		t.Fatalf("expected *RetryableError, got %T (%v)", err, err)
	}
	if retryable.Status != http.StatusInternalServerError {
		t.Errorf("Status: got %d, want %d", retryable.Status, http.StatusInternalServerError)
	}
	if !retryable.Retryable {
		t.Errorf("500 must be retryable")
	}
	if !strings.Contains(err.Error(), "transient") {
		t.Errorf("expected message to mention transient, got %q", err.Error())
	}
	if atomic.LoadInt32(hits) != int32(MaxRetries) {
		t.Errorf("expected %d hits, got %d", MaxRetries, *hits)
	}
	// 3 attempts + 1s + 2s backoff = ≥3s wall-clock.
	if elapsed < 3*time.Second {
		t.Errorf("expected at least 3s of backoff, got %v", elapsed)
	}
}

// TestClassifyHTTPError_429_Retryable confirms 429 (rate-limited) is also
// retried like 5xx. This guards the 408/429 branch of classifyHTTPError.
func TestClassifyHTTPError_429_Retryable(t *testing.T) {
	hits := new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate_limited"}`)
	}))
	t.Cleanup(srv.Close)
	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	_, err := c.FetchRuleSet(context.Background(), "r-2026-09-DRG-v17")

	if err == nil {
		t.Fatalf("expected error after 429 retries")
	}
	var retryable *RetryableError
	if !errors.As(err, &retryable) {
		t.Fatalf("expected *RetryableError, got %T (%v)", err, err)
	}
	if atomic.LoadInt32(hits) != int32(MaxRetries) {
		t.Errorf("expected %d hits on 429, got %d", MaxRetries, *hits)
	}
}

// TestClassifyHTTPError_Timeout_BubblesContext covers the ctx-cancel path.
// When the context is canceled mid-flight, FetchRuleSet must surface
// context.DeadlineExceeded (not a RetryableError) and must not retry.
func TestClassifyHTTPError_Timeout_BubblesContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the client cancels — simulates a hung upstream.
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := c.FetchRuleSet(ctx, "r-2026-09-DRG-v17")
	if err == nil {
		t.Fatalf("expected context deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %T (%v)", err, err)
	}
}

// TestClassifyHTTPError_ValidationError_EmptyVersionID covers the JSON body
// validation: empty version_id in 200 OK → *ValidationError. The retry loop
// must NOT retry on ValidationError (it is not retryable).
func TestClassifyHTTPError_ValidationError_EmptyVersionID(t *testing.T) {
	hits := new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// 200 OK but version_id is missing.
		_, _ = io.WriteString(w, `{"rule_set":"DRG","effective_date":"2026-10-01","rules":[{"code":"DRG-A001","name":"x","weight":1.0,"limit":1000.0}]}`)
	}))
	t.Cleanup(srv.Close)
	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	_, err := c.FetchRuleSet(context.Background(), "r-2026-09-DRG-v17")
	if err == nil {
		t.Fatalf("expected ValidationError for empty version_id")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T (%v)", err, err)
	}
	if vErr.Field != "version_id" {
		t.Errorf("expected field=version_id, got %q", vErr.Field)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("expected 1 hit (no retry on ValidationError), got %d", *hits)
	}
}

// TestClassifyHTTPError_ValidationError_EmptyRules covers the JSON body
// validation: empty rules[] → *ValidationError. Same shape as the
// empty-version_id test but with a different invalid field.
func TestClassifyHTTPError_ValidationError_EmptyRules(t *testing.T) {
	hits := new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// 200 OK but rules[] is empty.
		_, _ = io.WriteString(w, `{"version_id":"r-2026-09-DRG-v17","rule_set":"DRG","effective_date":"2026-10-01","rules":[]}`)
	}))
	t.Cleanup(srv.Close)
	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	_, err := c.FetchRuleSet(context.Background(), "r-2026-09-DRG-v17")
	if err == nil {
		t.Fatalf("expected ValidationError for empty rules")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T (%v)", err, err)
	}
	if vErr.Field != "rules" {
		t.Errorf("expected field=rules, got %q", vErr.Field)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("expected 1 hit (no retry on ValidationError), got %d", *hits)
	}
}

// TestRetryWithBackoff_NoRetriesStopsImmediately verifies that retryWithBackoff
// returns the first non-retryable error without retrying. This is the path
// that lets FetchRuleSet skip retries on 401/403/404/ValidationError.
func TestRetryWithBackoff_NoRetriesStopsImmediately(t *testing.T) {
	hits := new(int32)
	err := retryWithBackoff(context.Background(), func() error {
		atomic.AddInt32(hits, 1)
		// Return a non-retryable error — must short-circuit the loop.
		return &UnauthorizedError{
			PlatformAPIError: PlatformAPIError{Status: 401, Retryable: false},
		}
	}, MaxRetries)
	if err == nil {
		t.Fatalf("expected error")
	}
	var unauthorized *UnauthorizedError
	if !errors.As(err, &unauthorized) {
		t.Fatalf("expected *UnauthorizedError, got %T", err)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("expected 1 hit (no retry on permanent error), got %d", *hits)
	}
}

// TestRetryWithBackoff_RetriesUntilSuccess verifies that retryWithBackoff
// keeps retrying on retryable errors and returns nil as soon as fn succeeds.
func TestRetryWithBackoff_RetriesUntilSuccess(t *testing.T) {
	hits := new(int32)
	err := retryWithBackoff(context.Background(), func() error {
		n := atomic.AddInt32(hits, 1)
		if n < 2 {
			return &RetryableError{} // fail once, then succeed on attempt 2
		}
		return nil
	}, MaxRetries)
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if atomic.LoadInt32(hits) != 2 {
		t.Errorf("expected 2 hits (1 retry), got %d", *hits)
	}
}

// TestRetryWithBackoff_ExhaustsRetries verifies that retryWithBackoff
// returns the last retryable error after maxRetries attempts.
func TestRetryWithBackoff_ExhaustsRetries(t *testing.T) {
	hits := new(int32)
	err := retryWithBackoff(context.Background(), func() error {
		atomic.AddInt32(hits, 1)
		return &RetryableError{}
	}, 3)
	if err == nil {
		t.Fatalf("expected error after retries exhausted")
	}
	var retryable *RetryableError
	if !errors.As(err, &retryable) {
		t.Fatalf("expected *RetryableError, got %T", err)
	}
	if atomic.LoadInt32(hits) != 3 {
		t.Errorf("expected 3 hits, got %d", *hits)
	}
}

// TestRetryWithBackoff_ContextCancelMidBackoff verifies that the ctx.Done()
// branch in the backoff sleep returns ctx.Err() rather than continuing
// retry. This is the path that protects against stuck-on-retry behavior
// when the caller's deadline passes.
func TestRetryWithBackoff_ContextCancelMidBackoff(t *testing.T) {
	hits := new(int32)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := retryWithBackoff(ctx, func() error {
		atomic.AddInt32(hits, 1)
		return &RetryableError{} // always retryable so the loop enters backoff
	}, MaxRetries)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %T (%v)", err, err)
	}
	if atomic.LoadInt32(hits) < 1 {
		t.Errorf("expected at least 1 hit before cancel, got %d", *hits)
	}
}

// TestIsRetryableError_TableDriven verifies the type-table of isRetryableError.
// This is the contract that lets retryWithBackoff classify errors correctly.
// Crucially, UnauthorizedError and NotFoundError must NOT be retryable even
// though they embed PlatformAPIError — the Retryable=false field on the
// embedded PlatformAPIError is the tiebreaker.
func TestIsRetryableError_TableDriven(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"RetryableError", &RetryableError{}, true},
		{"PlatformAPIError retryable=true", &PlatformAPIError{Retryable: true}, true},
		{"PlatformAPIError retryable=false", &PlatformAPIError{Retryable: false}, false},
		{"UnauthorizedError", &UnauthorizedError{}, false},
		{"NotFoundError", &NotFoundError{}, false},
		{"ValidationError", &ValidationError{}, false},
		{"plain errors.New", errors.New("x"), false},
		{"wrapped RetryableError", errors.Join(errors.New("outer"), &RetryableError{}), true},
		{"wrapped PlatformAPIError retryable=true", errors.Join(errors.New("outer"), &PlatformAPIError{Retryable: true}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableError(tt.err); got != tt.want {
				t.Errorf("isRetryableError(%T) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestClassifyHTTPError_Other4xx_PlatformAPI covers the default branch:
// 4xx codes that aren't 401/403/404 must surface as the umbrella
// *PlatformAPIError (not UnauthorizedError/NotFoundError). 422 is a
// realistic example (semantically invalid request).
func TestClassifyHTTPError_Other4xx_PlatformAPI(t *testing.T) {
	hits := new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":"malformed_rule_payload"}`)
	}))
	t.Cleanup(srv.Close)
	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	_, err := c.FetchRuleSet(context.Background(), "r-2026-09-DRG-v17")
	if err == nil {
		t.Fatalf("expected error on 422")
	}
	var apiErr *PlatformAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *PlatformAPIError, got %T (%v)", err, err)
	}
	if apiErr.Retryable {
		t.Errorf("422 must NOT be retryable")
	}
	// Must NOT also be UnauthorizedError / NotFoundError.
	var unauthorized *UnauthorizedError
	if errors.As(err, &unauthorized) {
		t.Errorf("422 must not be classified as UnauthorizedError")
	}
	var notFound *NotFoundError
	if errors.As(err, &notFound) {
		t.Errorf("422 must not be classified as NotFoundError")
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("expected 1 hit (no retry on permanent 422), got %d", *hits)
	}
}

// TestClassifyHTTPError_200_Nil verifies the success branch — classifyHTTPError
// returns nil for 2xx so the caller can fall through to its decode path.
// This is exercised internally by FetchRuleSet but is good to have as an
// explicit unit-level contract for the helper.
func TestClassifyHTTPError_200_Nil(t *testing.T) {
	body := []byte(`{"version_id":"r-2026-09-DRG-v17","rule_set":"DRG","effective_date":"2026-10-01","rules":[{"code":"DRG-A001","name":"x","weight":1.0,"limit":1000.0}]}`)
	if err := classifyHTTPError(200, body, "http://example.com"); err != nil {
		t.Errorf("expected nil for 200, got %v", err)
	}
}

// TestNewClient_AfterRefactor sanity-checks the constructor post-refactor.
func TestNewClient_AfterRefactor(t *testing.T) {
	cfg := &config.Config{
		InsurancePlatform: "https://example.com",
		InsuranceToken:    "test-token",
	}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c == nil {
		t.Errorf("expected non-nil client")
	}
	if c.BaseURL != "https://example.com" {
		t.Errorf("BaseURL: got %q, want %q", c.BaseURL, "https://example.com")
	}
}

// guard import of encoding/json used elsewhere in the package.
var _ = json.Marshal