package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/example/insurance-rule-rollout/internal/config"
)

// MaxRetries is the number of attempts a single-channel sender will make
// before declaring the webhook dead. Feishu / WeCom webhooks tend to flap
// for ~10s during their maintenance windows, so 3 attempts × 1s/2s/4s
// backoff = ~7s total wait before the message goes to the retry queue.
const MaxRetries = 3

// HTTPClient is the minimal interface notify needs from net/http —
// accepting an interface lets tests substitute httptest.Server.Client()
// without exposing *http.Client internals. Matches the pattern used by
// internal/integration/insurance_platform/client.go.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Sender is the outbound interface used by the workflow engine. Both
// single-channel and multi-channel implementations satisfy it so the
// engine can stay channel-agnostic.
type Sender interface {
	// SendFeishu pushes a rendered Feishu card. Returns nil on success.
	// Returns a *WebhookError when the webhook is permanently dead (4xx)
	// or transiently unreachable (5xx / timeout / connection refused).
	SendFeishu(ctx context.Context, cardJSON string) error

	// SendWeCom pushes a rendered WeCom markdown body.
	SendWeCom(ctx context.Context, body string) error
}

// FeishuSender posts a card payload to the inbound Feishu bot webhook.
// The Feishu URL embeds the bot token, so no Authorization header is
// needed — just a JSON POST.
//
// URL format: https://open.feishu.cn/open-apis/bot/v2/hook/<token>
type FeishuSender struct {
	Token string // FEISHU_BOT_TOKEN — appended to the URL
	HC    HTTPClient
	URL   string // pre-computed full URL; tests inject a stub server URL
}

// WeComSender posts a markdown body to the inbound WeCom bot webhook.
// URL format: https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=<key>
type WeComSender struct {
	Key string // WECOM_BOT_KEY — appended as ?key=
	HC  HTTPClient
	URL string
}

// NewFeishuSender builds a FeishuSender from a config.Config. Returns
// an error when the token is empty so misconfiguration fails loudly.
func NewFeishuSender(cfg *config.Config) (*FeishuSender, error) {
	if cfg == nil {
		return nil, fmt.Errorf("notify: config is nil")
	}
	if strings.TrimSpace(cfg.FeishuToken) == "" {
		return nil, &config.MissingCredentialError{
			Field:       config.EnvKeyFeishuToken,
			Description: "飞书 webhook 凭据",
		}
	}
	return &FeishuSender{
		Token: cfg.FeishuToken,
		HC:    &http.Client{Timeout: 30 * time.Second},
		URL:   "https://open.feishu.cn/open-apis/bot/v2/hook/" + cfg.FeishuToken,
	}, nil
}

// NewWeComSender builds a WeComSender from a config.Config.
func NewWeComSender(cfg *config.Config) (*WeComSender, error) {
	if cfg == nil {
		return nil, fmt.Errorf("notify: config is nil")
	}
	if strings.TrimSpace(cfg.WeComKey) == "" {
		return nil, &config.MissingCredentialError{
			Field:       config.EnvKeyWeComKey,
			Description: "企微 webhook 凭据",
		}
	}
	return &WeComSender{
		Key: cfg.WeComKey,
		HC:  &http.Client{Timeout: 30 * time.Second},
		URL: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=" + cfg.WeComKey,
	}, nil
}

// NewFeishuSenderWithHTTP and NewWeComSenderWithHTTP are the
// dependency-injection constructors used by tests.
func NewFeishuSenderWithHTTP(url string, hc HTTPClient) *FeishuSender {
	return &FeishuSender{URL: url, HC: hc}
}

func NewWeComSenderWithHTTP(url string, hc HTTPClient) *WeComSender {
	return &WeComSender{URL: url, HC: hc}
}

// WebhookError describes a single webhook failure. Retryable mirrors the
// insurance_platform classification: 5xx / network / timeout are retryable;
// 4xx (auth, not-found, malformed payload) are permanent failures that
// should not be retried.
//
// Permanent failures go to the retry queue anyway — we never drop a
// notification on the floor — but with a high retry_count so the next
// sweep can garbage-collect them quickly.
type WebhookError struct {
	Channel   string // "feishu" | "wecom"
	URL       string
	Status    int
	Body      string // truncated
	Retryable bool
	Cause     error // nil if no underlying transport error
}

func (e *WebhookError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("notify: %s webhook to %s failed: %v (status=%d body=%s)",
			e.Channel, e.URL, e.Cause, e.Status, e.Body)
	}
	return fmt.Sprintf("notify: %s webhook to %s failed: status=%d body=%s",
		e.Channel, e.URL, e.Status, e.Body)
}

// Unwrap exposes the underlying cause so errors.Is / errors.As work.
func (e *WebhookError) Unwrap() error { return e.Cause }

// SendFeishu POSTs the Feishu card. Returns a *WebhookError on
// non-2xx / transport failure; returns nil on success.
func (s *FeishuSender) SendFeishu(ctx context.Context, cardJSON string) error {
	return postJSON(ctx, s.HC, "feishu", s.URL, []byte(cardJSON))
}

// SendWeCom is the Sender-interface stub. FeishuSender only knows how
// to push Feishu cards; calling SendWeCom on it returns an explicit
// error so callers (e.g. RetryQueue.Sweep) can branch on channel
// support.
func (s *FeishuSender) SendWeCom(ctx context.Context, body string) error {
	return &WebhookError{
		Channel:   "wecom",
		URL:       s.URL,
		Retryable: false,
		Cause:     errors.New("FeishuSender does not support WeCom channel"),
	}
}

// SendFeishu is the Sender-interface stub for WeComSender.
func (s *WeComSender) SendFeishu(ctx context.Context, cardJSON string) error {
	return &WebhookError{
		Channel:   "feishu",
		URL:       s.URL,
		Retryable: false,
		Cause:     errors.New("WeComSender does not support Feishu channel"),
	}
}

// SendWeCom POSTs the WeCom markdown body wrapped in {"msgtype":"markdown","markdown":{"content": body}}.
func (s *WeComSender) SendWeCom(ctx context.Context, body string) error {
	wrapped, err := json.Marshal(struct {
		Msgtype  string `json:"msgtype"`
		Markdown struct {
			Content string `json:"content"`
		} `json:"markdown"`
	}{Msgtype: "markdown", Markdown: struct {
		Content string `json:"content"`
	}{Content: body}})
	if err != nil {
		return &WebhookError{Channel: "wecom", URL: s.URL, Retryable: false, Cause: err}
	}
	return postJSON(ctx, s.HC, "wecom", s.URL, wrapped)
}

// doPostAttempt performs ONE HTTP POST and returns the actual status
// code (0 on transport error), a truncated response body, and the raw
// error from hc.Do. The retry-loop classification lives in postJSON
// and postWebhook.
func doPostAttempt(ctx context.Context, hc HTTPClient, url string, body []byte) (status int, respBody []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, _ = io.ReadAll(io.LimitReader(resp.Body, 2048))
	return resp.StatusCode, respBody, nil
}

// backoffDelay waits the n-th retry interval (capped at MaxBackoff) or
// the context deadline — whichever fires first. Returns whether the
// delay completed (true) or the context expired (false).
func backoffDelay(ctx context.Context, attempt int) bool {
	if attempt <= 0 {
		return ctx.Err() == nil
	}
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	if d <= 0 || d > MaxBackoff {
		d = MaxBackoff
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// postJSON performs the actual HTTP POST with exponential-backoff retry.
// Mirrors the pattern in insurance_platform.FetchRuleSet so error
// classification is consistent across the two HTTP integrations.
func postJSON(ctx context.Context, hc HTTPClient, channel, url string, body []byte) error {
	if hc == nil {
		return &WebhookError{Channel: channel, URL: url, Retryable: false, Cause: errors.New("nil HTTP client")}
	}

	var lastErr error
	for attempt := 0; attempt < MaxRetries; attempt++ {
		if attempt > 0 {
			if !backoffDelay(ctx, attempt) {
				return &WebhookError{Channel: channel, URL: url, Retryable: false, Cause: ctx.Err()}
			}
		}

		status, respBody, err := doPostAttempt(ctx, hc, url, body)
		if err != nil {
			if ctx.Err() != nil {
				return &WebhookError{Channel: channel, URL: url, Retryable: false, Cause: ctx.Err()}
			}
			lastErr = err
			continue
		}

		switch {
		case status >= 200 && status < 300:
			return nil
		case status >= 500 || status == 408 || status == 429:
			lastErr = &WebhookError{
				Channel:   channel,
				URL:       url,
				Status:    status,
				Body:      truncate(string(respBody), 200),
				Retryable: true,
			}
			continue
		default:
			// 4xx other than 408/429 → permanent failure.
			return &WebhookError{
				Channel:   channel,
				URL:       url,
				Status:    status,
				Body:      truncate(string(respBody), 200),
				Retryable: false,
			}
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("exhausted %d retries", MaxRetries)
	}
	// Exhausted retries — surface the last classified error or a generic
	// network error. Always wrap as WebhookError so retry-queue can decide.
	if we, ok := lastErr.(*WebhookError); ok {
		return we
	}
	return &WebhookError{Channel: channel, URL: url, Retryable: true, Cause: lastErr}
}

// postWebhook runs the same 3-attempt exponential-backoff loop as
// postJSON (1s/2s/4s; classified 4xx-permanent / 5xx-transient /
// 408/429-transient) but exposes the actual HTTP status code in the
// return tuple so callers can branch on the server-side response
// without poking inside a *WebhookError.
//
// Return contract:
//   - nil err            → 2xx in [200, 300); status is the real code (200/201/204/...)
//   - 4xx permanent      → *WebhookError{Retryable:false}; status is the real code
//   - 5xx/408/429 exhausted → *WebhookError{Retryable:true}; status is the last seen code
//   - transport / ctx    → *WebhookError{Retryable:true, Status:0}; status is 0
//
// Channel name is omitted on purpose — the Feishu vs WeCom distinction
// lives at the sender level, not in the HTTP plumbing.
func postWebhook(ctx context.Context, hc HTTPClient, url string, payload []byte) (status int, err error) {
	if hc == nil {
		return 0, &WebhookError{Channel: "generic", URL: url, Retryable: false, Cause: errors.New("nil HTTP client")}
	}

	var lastErr error
	var lastStatus int
	for attempt := 0; attempt < MaxRetries; attempt++ {
		if attempt > 0 {
			if !backoffDelay(ctx, attempt) {
				return 0, &WebhookError{Channel: "generic", URL: url, Retryable: false, Cause: ctx.Err()}
			}
		}

		st, respBody, err := doPostAttempt(ctx, hc, url, payload)
		if err != nil {
			if ctx.Err() != nil {
				return 0, &WebhookError{Channel: "generic", URL: url, Retryable: false, Cause: ctx.Err()}
			}
			lastErr = err
			lastStatus = 0
			continue
		}

		switch {
		case st >= 200 && st < 300:
			return st, nil
		case st >= 500 || st == 408 || st == 429:
			lastStatus = st
			lastErr = &WebhookError{
				Channel:   "generic",
				URL:       url,
				Status:    st,
				Body:      truncate(string(respBody), 200),
				Retryable: true,
			}
			continue
		default:
			// 4xx permanent — surface immediately.
			return st, &WebhookError{
				Channel:   "generic",
				URL:       url,
				Status:    st,
				Body:      truncate(string(respBody), 200),
				Retryable: false,
			}
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("exhausted %d retries", MaxRetries)
	}
	if we, ok := lastErr.(*WebhookError); ok {
		return lastStatus, we
	}
	return 0, &WebhookError{Channel: "generic", URL: url, Retryable: true, Cause: lastErr}
}

// truncate returns the first n bytes of s as a string. Used to keep
// error messages bounded when the webhook returns a huge HTML error page.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}

// MultiSender is the production-grade sender: try Feishu first, fall
// back to WeCom, finally enqueue into the local RetryQueue. The
// workflow engine always talks to MultiSender (not to a single channel)
// so misconfigured / flaky webhooks never block a Plan advance.
type MultiSender struct {
	Feishu Sender // optional
	WeCom  Sender // optional
	Queue  *RetryQueue
}

// Send dispatches a NotifyMessage through the configured channels:
//   1. Render the Feishu card.
//   2. Try Feishu → on success, return nil.
//   3. Render the WeCom text.
//   4. Try WeCom → on success, return nil.
//   5. Both dead → enqueue both payloads into the retry queue; return
//      a *NotifyAllFailedError wrapping the last error from each.
//
// nil renderers / nil queue are tolerated so callers can construct
// a MultiSender with just one channel and no queue in tests.
func (m *MultiSender) Send(ctx context.Context, msg NotifyMessage) error {
	if m == nil {
		return fmt.Errorf("notify: MultiSender is nil")
	}

	var lastFeishuErr, lastWeComErr error

	// Feishu first.
	if m.Feishu != nil {
		card, err := RenderFeishuCard(msg)
		if err == nil {
			lastFeishuErr = m.Feishu.SendFeishu(ctx, card)
			if lastFeishuErr == nil {
				return nil
			}
		} else {
			lastFeishuErr = err
		}
	}

	// WeCom fallback.
	if m.WeCom != nil {
		body, err := RenderWeComText(msg)
		if err == nil {
			lastWeComErr = m.WeCom.SendWeCom(ctx, body)
			if lastWeComErr == nil {
				return nil
			}
		} else {
			lastWeComErr = err
		}
	}

	// Both dead — best-effort enqueue. Enqueue itself can fail only on
	// SQLite errors; we still return a NotifyAllFailedError so the
	// caller (workflow engine) can decide whether to abort.
	if m.Queue != nil {
		if m.Feishu != nil {
			card, _ := RenderFeishuCard(msg)
			_ = m.Queue.Enqueue(ctx, "feishu", []byte(card), lastFeishuErr)
		}
		if m.WeCom != nil {
			body, _ := RenderWeComText(msg)
			_ = m.Queue.Enqueue(ctx, "wecom", []byte(body), lastWeComErr)
		}
	}

	return &NotifyAllFailedError{
		Feishu: lastFeishuErr,
		WeCom:  lastWeComErr,
	}
}

// SendFeishu and SendWeCom satisfy Sender by delegating to the
// corresponding single-channel sender. This lets MultiSender stand in
// for either channel in code paths that want to talk to a single
// channel through a uniform interface (e.g. retry-queue sweep).
func (m *MultiSender) SendFeishu(ctx context.Context, cardJSON string) error {
	if m == nil || m.Feishu == nil {
		return fmt.Errorf("notify: MultiSender has no Feishu channel")
	}
	return m.Feishu.SendFeishu(ctx, cardJSON)
}

func (m *MultiSender) SendWeCom(ctx context.Context, body string) error {
	if m == nil || m.WeCom == nil {
		return fmt.Errorf("notify: MultiSender has no WeCom channel")
	}
	return m.WeCom.SendWeCom(ctx, body)
}

// NotifyAllFailedError wraps both per-channel errors. The workflow engine
// can errors.As this to check whether either channel succeeded (always
// false at this point) or extract each cause for logging.
type NotifyAllFailedError struct {
	Feishu error
	WeCom  error
}

func (e *NotifyAllFailedError) Error() string {
	return fmt.Sprintf("notify: all channels failed — feishu=%v wecom=%v", e.Feishu, e.WeCom)
}