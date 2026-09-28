package insurance_platform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/insurance-rule-rollout/internal/config"
)

// fixtureRuleSet returns the same JSON shape the platform returns in
// production (mirrors testdata/platform_response.json). It is reused by
// every "happy path" test so we do not accidentally diverge from the
// fixture that cmd/demo also serves.
func fixtureRuleSet() RuleSet {
	return RuleSet{
		VersionID:     "r-2026-09-DRG-v17",
		RuleSet:       "DRG-2026-09-NATIONAL",
		EffectiveDate: "2026-10-01",
		PrevVersionID: "r-2026-06-DRG-v16",
		RollbackSQL:   "UPDATE his_dict SET version='r-2026-06-DRG-v16' WHERE version='r-2026-09-DRG-v17';",
		Rules: []Rule{
			{Code: "DRG-A001", Name: "胃食管及腹部手术", Weight: 2.450, Limit: 36800.00, PrevLimit: 35200.00},
			{Code: "DRG-A002", Name: "肝胆胰系统手术", Weight: 3.180, Limit: 49500.00, PrevLimit: 51200.00},
			{Code: "DRG-B014", Name: "肺部感染", Weight: 1.250, Limit: 14800.00, PrevLimit: 15800.00},
			{Code: "DRG-C007", Name: "急性脑血管病", Weight: 2.090, Limit: 27600.00, PrevLimit: 31200.00},
			{Code: "DRG-E022", Name: "新生儿疾病", Weight: 1.080, Limit: 11500.00, PrevLimit: 11500.00},
		},
	}
}

// newTestServer returns an httptest.Server that responds with the supplied
// status / body / headers. The hit count is exposed via the returned
// atomic so tests can verify retry behaviour.
func newTestServer(t *testing.T, status int, body string, contentType string) (*httptest.Server, *int32) {
	t.Helper()
	hits := new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		// Verify the Authorization header is forwarded — silent absence would
		// mask misconfiguration in production.
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("expected Bearer Authorization header, got %q", got)
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// TestFetchRuleSet_HappyPath drives a 200 OK with the fixture body and
// asserts the decoded RuleSet matches. Also asserts the Authorization
// header was forwarded (see newTestServer above).
func TestFetchRuleSet_HappyPath(t *testing.T) {
	want := fixtureRuleSet()
	body, _ := json.Marshal(want)

	srv, hits := newTestServer(t, http.StatusOK, string(body), "application/json")
	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	got, err := c.FetchRuleSet(context.Background(), want.VersionID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.VersionID != want.VersionID {
		t.Errorf("VersionID: got %q, want %q", got.VersionID, want.VersionID)
	}
	if len(got.Rules) != len(want.Rules) {
		t.Errorf("Rules count: got %d, want %d", len(got.Rules), len(want.Rules))
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("expected exactly 1 hit (no retries on 200), got %d", *hits)
	}
}

// TestFetchRuleSet_401_NoRetry confirms that 401 responses are classified
// as permanent (no retry) — re-fetching the token is the operator's job,
// not the client's.
func TestFetchRuleSet_401_NoRetry(t *testing.T) {
	srv, hits := newTestServer(t, http.StatusUnauthorized, `{"error":"invalid_token"}`, "application/json")
	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	_, err := c.FetchRuleSet(context.Background(), "r-2026-09-DRG-v17")
	if err == nil {
		t.Fatalf("expected error on 401")
	}
	var apiErr *PlatformAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *PlatformAPIError, got %T (%v)", err, err)
	}
	if apiErr.Retryable {
		t.Errorf("401 should NOT be marked retryable")
	}
	if apiErr.Status != http.StatusUnauthorized {
		t.Errorf("status: got %d, want %d", apiErr.Status, http.StatusUnauthorized)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("expected exactly 1 hit (no retry on 401), got %d", *hits)
	}
}

// TestFetchRuleSet_403_NoRetry confirms 403 (forbidden) is also permanent.
func TestFetchRuleSet_403_NoRetry(t *testing.T) {
	srv, hits := newTestServer(t, http.StatusForbidden, `{"error":"forbidden"}`, "application/json")
	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	_, err := c.FetchRuleSet(context.Background(), "r-2026-09-DRG-v17")
	if err == nil {
		t.Fatalf("expected error on 403")
	}
	var apiErr *PlatformAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *PlatformAPIError, got %T (%v)", err, err)
	}
	if apiErr.Retryable {
		t.Errorf("403 should NOT be marked retryable")
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("expected exactly 1 hit (no retry on 403), got %d", *hits)
	}
}

// TestFetchRuleSet_404_NoRetry confirms 404 (bundle withdrawn / typo'd
// version_id) is permanent — retrying will not conjure the bundle into
// existence.
func TestFetchRuleSet_404_NoRetry(t *testing.T) {
	srv, hits := newTestServer(t, http.StatusNotFound, `{"error":"version_not_found"}`, "application/json")
	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	_, err := c.FetchRuleSet(context.Background(), "r-does-not-exist")
	if err == nil {
		t.Fatalf("expected error on 404")
	}
	var apiErr *PlatformAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *PlatformAPIError, got %T (%v)", err, err)
	}
	if apiErr.Retryable {
		t.Errorf("404 should NOT be marked retryable")
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("expected exactly 1 hit (no retry on 404), got %d", *hits)
	}
}

// TestFetchRuleSet_500_Retry3TimesThenFail configures the test server to
// return 500 on every request — the client must retry MaxRetries times and
// surface the classified error after the last attempt.
func TestFetchRuleSet_500_Retry3TimesThenFail(t *testing.T) {
	srv, hits := newTestServer(t, http.StatusInternalServerError, `{"error":"db_down"}`, "application/json")
	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	start := time.Now()
	_, err := c.FetchRuleSet(context.Background(), "r-2026-09-DRG-v17")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected error after retries exhausted")
	}
	var apiErr *PlatformAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *PlatformAPIError, got %T (%v)", err, err)
	}
	if !apiErr.Retryable {
		t.Errorf("500 SHOULD be marked retryable (after exhaustion, surfaced as last error)")
	}
	if atomic.LoadInt32(hits) != int32(MaxRetries) {
		t.Errorf("expected %d hits (1 initial + %d retries), got %d", MaxRetries, MaxRetries-1, *hits)
	}
	// Sanity check on backoff total — should be at least 1+2 = 3s for 3
	// attempts (attempt 1 + 1s wait + attempt 2 + 2s wait + attempt 3).
	if elapsed < 3*time.Second {
		t.Errorf("expected at least 3s of backoff, got %v", elapsed)
	}
}

// TestFetchRuleSet_429_Retryable confirms 429 (rate-limited) is treated
// the same as 5xx: retryable with backoff.
func TestFetchRuleSet_429_Retryable(t *testing.T) {
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
	if atomic.LoadInt32(hits) != int32(MaxRetries) {
		t.Errorf("expected %d hits on 429, got %d", MaxRetries, *hits)
	}
}

// TestFetchRuleSet_ValidationError_EmptyVersionID drives a 200 OK with a
// body whose JSON is well-formed but whose version_id is empty. The client
// must surface this as a *ValidationError, not a *PlatformAPIError.
func TestFetchRuleSet_ValidationError_EmptyVersionID(t *testing.T) {
	bad := RuleSet{
		RuleSet:       "DRG-2026-09-NATIONAL",
		EffectiveDate: "2026-10-01",
		Rules:         fixtureRuleSet().Rules,
	}
	body, _ := json.Marshal(bad)

	srv, _ := newTestServer(t, http.StatusOK, string(body), "application/json")
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
}

// TestFetchRuleSet_ValidationError_EmptyRules drives a 200 OK with a
// body whose rules[] is empty. The client must reject this so cmd/demo
// does not silently produce a Plan with zero rules.
func TestFetchRuleSet_ValidationError_EmptyRules(t *testing.T) {
	bad := RuleSet{
		VersionID:     "r-2026-09-DRG-v17",
		RuleSet:       "DRG-2026-09-NATIONAL",
		EffectiveDate: "2026-10-01",
		Rules:         []Rule{}, // intentionally empty
	}
	body, _ := json.Marshal(bad)

	srv, _ := newTestServer(t, http.StatusOK, string(body), "application/json")
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
}

// TestFetchRuleSet_MalformedJSON drives a 200 OK whose body is not valid
// JSON. The client must surface a *ValidationError explaining the parse
// failure — operators must not see a Go json.SyntaxError directly.
func TestFetchRuleSet_MalformedJSON(t *testing.T) {
	srv, _ := newTestServer(t, http.StatusOK, "this is not json {{{", "application/json")
	c := NewClientWithHTTP(srv.URL, "test-token", srv.Client())

	_, err := c.FetchRuleSet(context.Background(), "r-2026-09-DRG-v17")
	if err == nil {
		t.Fatalf("expected ValidationError for malformed JSON")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T (%v)", err, err)
	}
	if vErr.Field != "body" {
		t.Errorf("expected field=body, got %q", vErr.Field)
	}
}

// TestFetchRuleSet_TimeoutBubblesContext verifies that when the context
// is canceled mid-flight, the client surfaces the context error rather
// than the wrapped network error.
func TestFetchRuleSet_TimeoutBubblesContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the client cancels.
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

// TestFetchRuleSet_EmptyVersionID covers the client-side guard against
// empty versionID — saves a round-trip when the caller forgot to fill it.
func TestFetchRuleSet_EmptyVersionID(t *testing.T) {
	c := NewClientWithHTTP("http://example.com", "test-token", http.DefaultClient)

	_, err := c.FetchRuleSet(context.Background(), "  ")
	if err == nil {
		t.Fatalf("expected ValidationError for empty versionID")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T (%v)", err, err)
	}
	if vErr.Field != "version_id" {
		t.Errorf("expected field=version_id, got %q", vErr.Field)
	}
}

// TestFetchRuleSet_InvalidVersionIDPathTraversal defends against caller
// inputs that try to escape the path (e.g. "../admin"). The client must
// reject these before issuing the HTTP request.
func TestFetchRuleSet_InvalidVersionIDPathTraversal(t *testing.T) {
	c := NewClientWithHTTP("http://example.com", "test-token", http.DefaultClient)

	_, err := c.FetchRuleSet(context.Background(), "../admin")
	if err == nil {
		t.Fatalf("expected error for path-traversal versionID")
	}
	if !strings.Contains(err.Error(), "invalid character") {
		t.Errorf("expected invalid character error, got %v", err)
	}
}

// TestNewClient_MissingBaseURL covers the config-injection constructor's
// refusal to operate when the BaseURL is empty.
func TestNewClient_MissingBaseURL(t *testing.T) {
	cfg := &config.Config{
		InsurancePlatform: "",
		InsuranceToken:    "test-token",
	}
	_, err := NewClient(cfg)
	if err == nil {
		t.Fatalf("expected error for empty BaseURL")
	}
	var mc *config.MissingCredentialError
	if !errors.As(err, &mc) {
		t.Fatalf("expected *config.MissingCredentialError, got %T (%v)", err, err)
	}
	if mc.Field != config.EnvKeyInsurancePlatformBaseURL {
		t.Errorf("expected Field=%s, got %q", config.EnvKeyInsurancePlatformBaseURL, mc.Field)
	}
}

// TestNewClient_MissingToken covers the symmetric guard for token absence.
func TestNewClient_MissingToken(t *testing.T) {
	cfg := &config.Config{
		InsurancePlatform: "https://example.com",
		InsuranceToken:    "",
	}
	_, err := NewClient(cfg)
	if err == nil {
		t.Fatalf("expected error for empty token")
	}
	var mc *config.MissingCredentialError
	if !errors.As(err, &mc) {
		t.Fatalf("expected *config.MissingCredentialError, got %T (%v)", err, err)
	}
	if mc.Field != config.EnvKeyInsurancePlatformToken {
		t.Errorf("expected Field=%s, got %q", config.EnvKeyInsurancePlatformToken, mc.Field)
	}
}

// TestNewClient_NilConfig covers the defensive nil check.
func TestNewClient_NilConfig(t *testing.T) {
	_, err := NewClient(nil)
	if err == nil {
		t.Fatalf("expected error for nil config")
	}
	if !strings.Contains(err.Error(), "config is nil") {
		t.Errorf("expected 'config is nil' in error, got %v", err)
	}
}

// TestNewClient_HappyPathFromConfig confirms that when both credentials
// are present, the client is constructed and the BaseURL trailing slash
// is normalised away.
func TestNewClient_HappyPathFromConfig(t *testing.T) {
	cfg := &config.Config{
		InsurancePlatform: "https://example.com/",
		InsuranceToken:    "test-token",
	}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.BaseURL != "https://example.com" {
		t.Errorf("BaseURL trailing slash not trimmed, got %q", c.BaseURL)
	}
	if c.HC == nil {
		t.Errorf("expected HC to be initialised")
	}
}

// TestFetchRuleSet_FromFixtureFileRoundTrip exercises the full path the
// demo binary uses: load the fixture from testdata/, decode it, and verify
// the same JSON shape FetchRuleSet would parse in production. This locks
// the contract between the fixture and the wire format.
func TestFetchRuleSet_FromFixtureFileRoundTrip(t *testing.T) {
	// Resolve the fixture path relative to this test file.
	body, err := os.ReadFile("../../../testdata/platform_response.json")
	if err != nil {
		t.Skipf("fixture not available: %v", err)
	}
	var rs RuleSet
	if err := json.Unmarshal(body, &rs); err != nil {
		t.Fatalf("fixture parse failed: %v", err)
	}
	if err := validateRuleSet(rs); err != nil {
		t.Fatalf("fixture failed validation: %v", err)
	}
	if rs.VersionID == "" || len(rs.Rules) == 0 {
		t.Errorf("fixture is incomplete: version_id=%q rules=%d", rs.VersionID, len(rs.Rules))
	}
}

// TestValidateRuleSet_NegativeLimit covers the per-rule numeric guard —
// a negative limit is silently nonsensical and would crash the projected
// diff calculator downstream.
func TestValidateRuleSet_NegativeLimit(t *testing.T) {
	rs := fixtureRuleSet()
	rs.Rules[0].Limit = -1
	err := validateRuleSet(rs)
	if err == nil {
		t.Fatalf("expected ValidationError for negative limit")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if !strings.Contains(vErr.Field, "limit") {
		t.Errorf("expected limit field error, got %q", vErr.Field)
	}
}

// TestValidateRuleSet_NegativeWeight is the symmetric guard for weight.
func TestValidateRuleSet_NegativeWeight(t *testing.T) {
	rs := fixtureRuleSet()
	rs.Rules[0].Weight = -0.5
	err := validateRuleSet(rs)
	if err == nil {
		t.Fatalf("expected ValidationError for negative weight")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if !strings.Contains(vErr.Field, "weight") {
		t.Errorf("expected weight field error, got %q", vErr.Field)
	}
}

// TestValidateRuleSet_EmptyRuleCode covers the per-rule code guard.
func TestValidateRuleSet_EmptyRuleCode(t *testing.T) {
	rs := fixtureRuleSet()
	rs.Rules[0].Code = "   "
	err := validateRuleSet(rs)
	if err == nil {
		t.Fatalf("expected ValidationError for empty code")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if !strings.Contains(vErr.Field, "code") {
		t.Errorf("expected code field error, got %q", vErr.Field)
	}
}

// TestValidateRuleSet_EmptyEffectiveDate covers the missing-date guard.
func TestValidateRuleSet_EmptyEffectiveDate(t *testing.T) {
	rs := fixtureRuleSet()
	rs.EffectiveDate = ""
	err := validateRuleSet(rs)
	if err == nil {
		t.Fatalf("expected ValidationError for empty effective_date")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if vErr.Field != "effective_date" {
		t.Errorf("expected field=effective_date, got %q", vErr.Field)
	}
}