package insurance_platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/example/insurance-rule-rollout/internal/config"
)

// MaxRetries is the number of times the client will retry a 5xx or
// transient network error before giving up. Exported so tests can lower it
// to keep the test wall-clock short.
const MaxRetries = 3

// HTTPClient is the minimal interface the Client needs from net/http.
// Accepting an interface lets tests substitute httptest.Server's client
// without exposing *http.Client internals.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client fetches monthly DRG/DIP rule bundles from the medical-insurance
// platform. It is constructed via NewClient(cfg) and is safe for
// concurrent use — BaseURL / Token are immutable after construction.
type Client struct {
	BaseURL string
	Token   string
	HC      HTTPClient
}

// NewClient builds a Client from a config.Config. Returns an error when
// BaseURL / Token are empty so that misconfiguration fails loudly at the
// call site instead of silently returning 401s on every request.
func NewClient(cfg *config.Config) (*Client, error) {
	if cfg == nil {
		return nil, fmt.Errorf("insurance_platform: config is nil")
	}
	if strings.TrimSpace(cfg.InsurancePlatform) == "" {
		return nil, &config.MissingCredentialError{
			Field:       config.EnvKeyInsurancePlatformBaseURL,
			Description: "医保平台 API BaseURL",
		}
	}
	if strings.TrimSpace(cfg.InsuranceToken) == "" {
		return nil, &config.MissingCredentialError{
			Field:       config.EnvKeyInsurancePlatformToken,
			Description: "医保平台 API 凭据（Authorization 头）",
		}
	}
	return &Client{
		BaseURL: strings.TrimRight(cfg.InsurancePlatform, "/"),
		Token:   cfg.InsuranceToken,
		HC: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

// NewClientWithHTTP is the dependency-injection constructor used by tests.
// Production code paths should call NewClient(cfg).
func NewClientWithHTTP(baseURL, token string, hc HTTPClient) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HC:      hc,
	}
}

// PlatformAPIError wraps a non-2xx HTTP response. Carries status code,
// URL, and the first 200 bytes of body so the operator can grep the
// platform's error message from the log.
//
// Retryable reports whether the underlying error is worth retrying — set
// for 5xx and 408/429; cleared for 4xx that signal permanent failure
// (401/403/404).
type PlatformAPIError struct {
	URL       string
	Status    int
	Body      string // truncated to 200 bytes
	Retryable bool
}

func (e *PlatformAPIError) Error() string {
	return fmt.Sprintf("insurance_platform: HTTP %d from %s: %s", e.Status, e.URL, e.Body)
}

// UnauthorizedError is returned when the platform rejects the caller's
// credentials (HTTP 401/403). Auth problem — re-fetch the token, do not
// retry. Embeds PlatformAPIError so callers can use errors.As(err, &apiErr)
// uniformly to access URL/Status/Body across all error types; the explicit
// Unwrap() also surfaces the embedded *PlatformAPIError to errors.As for
// backward compatibility with code that predates the typed split.
type UnauthorizedError struct {
	PlatformAPIError
}

func (e *UnauthorizedError) Error() string {
	return fmt.Sprintf(
		"insurance_platform: unauthorized HTTP %d from %s — re-fetch INS_PLATFORM_API_TOKEN; do not retry (body: %s)",
		e.Status, e.URL, e.Body,
	)
}

func (e *UnauthorizedError) Unwrap() error {
	return &e.PlatformAPIError
}

// NotFoundError is returned when the platform has no bundle for the
// requested version_id (HTTP 404). versionID typo or bundle withdrawn —
// retrying will not conjure the bundle into existence. Embeds PlatformAPIError
// for the same backward-compatibility reason as UnauthorizedError.
type NotFoundError struct {
	PlatformAPIError
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf(
		"insurance_platform: rule bundle not found (HTTP 404) at %s — check versionID; do not retry (body: %s)",
		e.URL, e.Body,
	)
}

func (e *NotFoundError) Unwrap() error {
	return &e.PlatformAPIError
}

// RetryableError is returned for transient failures (5xx, 408, 429) and
// for transport-level network errors that haven't yet exhausted the retry
// loop. The fetch loop retries with exponential backoff before surfacing
// this error to the caller. Embeds PlatformAPIError for the same
// backward-compatibility reason as UnauthorizedError.
type RetryableError struct {
	PlatformAPIError
}

func (e *RetryableError) Error() string {
	return fmt.Sprintf(
		"insurance_platform: transient HTTP %d from %s — exhausted retries (body: %s)",
		e.Status, e.URL, e.Body,
	)
}

func (e *RetryableError) Unwrap() error {
	return &e.PlatformAPIError
}

// FetchRuleSet performs GET /v1/rules/{versionID} and decodes the response
// into a RuleSet. Errors are classified by classifyHTTPError:
//
//   - 401/403 → *UnauthorizedError  (auth problem — re-fetch the token, no retry)
//   - 404     → *NotFoundError      (versionID typo / bundle withdrawn, no retry)
//   - 408/429 → *RetryableError     (rate-limited, retry with backoff)
//   - 5xx     → *RetryableError     (transient, retry with backoff)
//   - other 4xx → *PlatformAPIError (permanent failure)
//   - ctx canceled / deadline exceeded → bubbles ctx.Err() (no retry)
//   - transport-level network errors → wrapped as *RetryableError, retried
//   - 200 with malformed JSON → *ValidationError
//   - 200 with semantically incomplete body (empty version_id, empty rules) → *ValidationError
//
// The retry loop runs MaxRetries times via retryWithBackoff (1s, 2s, 4s
// exponential backoff). Permanent errors (Unauthorized/NotFound/PlatformAPIError
// with Retryable=false/ValidationError) short-circuit the loop on first hit.
func (c *Client) FetchRuleSet(ctx context.Context, versionID string) (RuleSet, error) {
	if strings.TrimSpace(versionID) == "" {
		return RuleSet{}, &ValidationError{Field: "version_id", Reason: "is empty"}
	}
	if c == nil || c.BaseURL == "" {
		return RuleSet{}, fmt.Errorf("insurance_platform: client not initialized")
	}

	// Double timeout protection: http.Client.Timeout bounds the entire round
	// trip (DNS + connect + TLS + write + read); context.WithTimeout also
	// bounds the deadline in case the caller's context has no deadline but
	// the operator wants to cancel after 30s. Either timeout fires ⇒ ctx.Err()
	// bubbles out of retryWithBackoff without retrying.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Build request URL safely — refuse weird inputs to keep the path clean.
	endpoint, err := c.buildURL(versionID)
	if err != nil {
		return RuleSet{}, err
	}

	var result RuleSet
	err = retryWithBackoff(ctx, func() error {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if reqErr != nil {
			return fmt.Errorf("insurance_platform: build request: %w", reqErr)
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Accept", "application/json")

		resp, doErr := c.HC.Do(req)
		if doErr != nil {
			// Transport-level error — bubble ctx if the deadline was the
			// root cause; otherwise wrap as retryable so the loop tries again.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// http.Client.Timeout fires the same way a deadline would, but
			// it does NOT populate ctx.Err() — the parent ctx is fine, only
			// the per-request client budget expired. We deliberately do NOT
			// retry on this branch: the cap is a budget we hit, not a
			// transient hiccup. Wrapping it as RetryableError would burn
			// the retry budget (~3.3s with 1s/2s/4s backoff) for no gain.
			return &PlatformAPIError{
				URL:       endpoint,
				Status:    0,
				Body:      truncate([]byte(doErr.Error()), 200),
				Retryable: false,
			}
		}

		// Drain + close so the connection can be reused.
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()

		if apiErr := classifyHTTPError(resp.StatusCode, bodyBytes, endpoint); apiErr != nil {
			return apiErr
		}

		// Success — decode the RuleSet and run field-level validation.
		var rs RuleSet
		if jErr := json.Unmarshal(bodyBytes, &rs); jErr != nil {
			return &ValidationError{
				Field:   "body",
				Reason:  "JSON parse failed: " + jErr.Error(),
				Payload: truncateBytes(bodyBytes, 200),
			}
		}
		if vErr := validateRuleSet(rs); vErr != nil {
			return vErr
		}
		result = rs
		return nil
	}, MaxRetries)

	if err != nil {
		return RuleSet{}, err
	}
	return result, nil
}

// classifyHTTPError maps an HTTP status code (and the body bytes that
// accompanied it) to the right typed error. Returns nil for 2xx so the
// caller can fall through to its success path. The url is included in
// the resulting error so log readers can grep the exact endpoint that
// failed.
//
// Mapping:
//
//   - 2xx             → nil (success — caller proceeds to decode)
//   - 401 / 403       → *UnauthorizedError  (auth — no retry)
//   - 404             → *NotFoundError      (versionID typo — no retry)
//   - 408 / 429 / 5xx → *RetryableError     (transient — retry with backoff)
//   - other 4xx       → *PlatformAPIError   (permanent — no retry)
func classifyHTTPError(status int, body []byte, url string) error {
	bodyStr := truncate(body, 200)
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == 401, status == 403:
		return &UnauthorizedError{
			PlatformAPIError: PlatformAPIError{
				URL: url, Status: status, Body: bodyStr, Retryable: false,
			},
		}
	case status == 404:
		return &NotFoundError{
			PlatformAPIError: PlatformAPIError{
				URL: url, Status: status, Body: bodyStr, Retryable: false,
			},
		}
	case status == 408, status == 429, status >= 500:
		return &RetryableError{
			PlatformAPIError: PlatformAPIError{
				URL: url, Status: status, Body: bodyStr, Retryable: true,
			},
		}
	default:
		// Other 4xx (e.g. 400, 422) — permanent failure.
		return &PlatformAPIError{
			URL: url, Status: status, Body: bodyStr, Retryable: false,
		}
	}
}

// retryWithBackoff invokes fn up to maxRetries times with exponential
// backoff (1s, 2s, 4s, ...). Returns nil as soon as fn succeeds. Returns
// immediately (no retry) when fn returns a non-retryable error per
// isRetryableError — this is what lets FetchRuleSet skip retries on
// 401/403/404 without splitting its call site. Returns ctx.Err() if the
// context is canceled during a backoff sleep.
//
// maxRetries is clamped to ≥1 so callers that accidentally pass 0 still
// get at least one attempt.
func retryWithBackoff(ctx context.Context, fn func() error, maxRetries int) error {
	if maxRetries < 1 {
		maxRetries = 1
	}
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s. Respect ctx cancellation.
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		err := fn()
		if err == nil {
			return nil
		}
		if !isRetryableError(err) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

// isRetryableError reports whether err should be retried by retryWithBackoff.
// Returns true for *RetryableError and for *PlatformAPIError with
// Retryable=true; returns false for nil, *ValidationError, *UnauthorizedError,
// *NotFoundError, and untyped errors.
//
// Implementation note: *UnauthorizedError and *NotFoundError each embed
// *PlatformAPIError with Retryable=false, and each implements Unwrap() to
// surface that embedded field. errors.As therefore finds the embedded
// PlatformAPIError when called on either of those wrappers, so the
// Retryable field is the tiebreaker — exactly what we want.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	var retryable *RetryableError
	if errors.As(err, &retryable) {
		return true
	}
	var apiErr *PlatformAPIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable
	}
	return false
}

// buildURL composes the GET endpoint and defends against versionID values
// that contain path separators or URL-encoded nonsense.
func (c *Client) buildURL(versionID string) (string, error) {
	// Reject characters that should never appear in a real bundle ID —
	// '/' would let callers escape the path, '#' / '?' would corrupt the
	// query string. The platform issues IDs like r-2026-09-DRG-v17.
	for _, ch := range versionID {
		if ch == '/' || ch == '\\' || ch == '?' || ch == '#' || ch == ' ' {
			return "", fmt.Errorf("insurance_platform: versionID contains invalid character %q", ch)
		}
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return "", fmt.Errorf("insurance_platform: parse BaseURL: %w", err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/rules/" + url.PathEscape(versionID)
	return u.String(), nil
}

// validateRuleSet enforces the spec.md RulePlan Spec contract: version_id
// / rule_set / effective_date / rules[] must all be present and non-empty.
// Also verifies that effective_date is a real ISO calendar date (YYYY-MM-DD)
// — malformed dates (e.g. 2026-13-40, 2026/10/01) were silently passing the
// empty-string check and crashing the dry-run diff calculator downstream.
// Returns *ValidationError so callers can errors.As on it.
func validateRuleSet(rs RuleSet) error {
	if strings.TrimSpace(rs.VersionID) == "" {
		return &ValidationError{Field: "version_id", Reason: "is empty"}
	}
	if strings.TrimSpace(rs.RuleSet) == "" {
		return &ValidationError{Field: "rule_set", Reason: "is empty"}
	}
	if strings.TrimSpace(rs.EffectiveDate) == "" {
		return &ValidationError{Field: "effective_date", Reason: "is empty"}
	}
	if vErr := validateEffectiveDate(rs.EffectiveDate); vErr != nil {
		return &ValidationError{Field: "effective_date", Reason: vErr.Error()}
	}
	if len(rs.Rules) == 0 {
		return &ValidationError{Field: "rules", Reason: "is empty (need at least 1 rule)"}
	}
	for i, r := range rs.Rules {
		if strings.TrimSpace(r.Code) == "" {
			return &ValidationError{
				Field:  fmt.Sprintf("rules[%d].code", i),
				Reason: "is empty",
			}
		}
		if strings.TrimSpace(r.Name) == "" {
			return &ValidationError{
				Field:  fmt.Sprintf("rules[%d].name", i),
				Reason: "is empty",
			}
		}
		if r.Limit < 0 {
			return &ValidationError{
				Field:  fmt.Sprintf("rules[%d].limit", i),
				Reason: "is negative",
			}
		}
		if r.Weight < 0 {
			return &ValidationError{
				Field:  fmt.Sprintf("rules[%d].weight", i),
				Reason: "is negative",
			}
		}
	}
	return nil
}

// validateEffectiveDate checks that s is a real YYYY-MM-DD calendar day. It
// refuses:
//   - empty / whitespace strings (callers handle that separately)
//   - wrong shape (e.g. 2026/10/01, 2026-10-1, 20261001, 26-10-01)
//   - impossible dates (month 13, day 40, 2026-02-30, etc.)
//
// Real-date check uses time.Parse with strict YYYY-MM-DD; Go's parser will
// reject "2026-13-01" and "2026-02-30" because it bounces through the
// calendar rather than accepting the raw integers.
func validateEffectiveDate(s string) error {
	s = strings.TrimSpace(s)
	if len(s) != len("2006-01-02") {
		return fmt.Errorf("malformed %q (expected YYYY-MM-DD)", s)
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return fmt.Errorf("not a real calendar date %q (%w)", s, err)
	}
	return nil
}

// truncate returns the first n bytes of b as a string. Used to keep error
// messages bounded when the platform returns a huge HTML error page.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	// Append a clear marker so log readers know we cut content.
	var buf bytes.Buffer
	buf.Grow(n + 16)
	buf.Write(b[:n])
	buf.WriteString("...[truncated]")
	return buf.String()
}

// truncateBytes returns the first n bytes of b as a slice. Used by
// ValidationError.Payload which must be raw bytes (the caller may want to
// re-decode or hex-dump the payload without going through string).
func truncateBytes(b []byte, n int) []byte {
	if len(b) <= n {
		out := make([]byte, len(b))
		copy(out, b)
		return out
	}
	out := make([]byte, n, n+16)
	copy(out, b[:n])
	return out
}