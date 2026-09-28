package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// ---------- postWebhook (status-returning helper) ----------

// TestPostWebhook_2xxReturnsStatus verifies postWebhook returns the
// actual HTTP status (not just 200) on success and yields a nil error.
func TestPostWebhook_2xxReturnsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	status, err := postWebhook(context.Background(), srv.Client(), srv.URL, []byte(`{}`))
	if err != nil {
		t.Fatalf("postWebhook: unexpected error: %v", err)
	}
	if status != http.StatusCreated {
		t.Errorf("status: got %d, want %d", status, http.StatusCreated)
	}
}

// TestPostWebhook_4xxReturnsStatusNoRetry verifies a 4xx response is
// surfaced immediately (no retry loop) and the actual status code is
// returned alongside the typed WebhookError.
func TestPostWebhook_4xxReturnsStatusNoRetry(t *testing.T) {
	hits := new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"unknown route"}`)
	}))
	t.Cleanup(srv.Close)

	status, err := postWebhook(context.Background(), srv.Client(), srv.URL, []byte(`{}`))
	if err == nil {
		t.Fatalf("expected WebhookError, got nil")
	}
	var we *WebhookError
	if !errors.As(err, &we) {
		t.Fatalf("expected *WebhookError, got %T", err)
	}
	if we.Retryable {
		t.Errorf("404 must NOT be Retryable")
	}
	if status != http.StatusNotFound {
		t.Errorf("status: got %d, want %d", status, http.StatusNotFound)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("hits: got %d, want 1 (no retry on 4xx)", got)
	}
}

// TestPostWebhook_5xxExhaustsRetriesThenReturnsLastStatus verifies the
// retry loop runs MaxRetries times and surfaces the last seen status
// code when every attempt fails.
func TestPostWebhook_5xxExhaustsRetriesThenReturnsLastStatus(t *testing.T) {
	hits := new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":"bad gateway"}`)
	}))
	t.Cleanup(srv.Close)

	status, err := postWebhook(context.Background(), srv.Client(), srv.URL, []byte(`{}`))
	if err == nil {
		t.Fatalf("expected WebhookError after exhausting retries, got nil")
	}
	var we *WebhookError
	if !errors.As(err, &we) {
		t.Fatalf("expected *WebhookError, got %T", err)
	}
	if !we.Retryable {
		t.Errorf("502 must be Retryable")
	}
	if status != http.StatusBadGateway {
		t.Errorf("status: got %d, want %d", status, http.StatusBadGateway)
	}
	if got := atomic.LoadInt32(hits); got != int32(MaxRetries) {
		t.Errorf("hits: got %d, want %d (full retry loop)", got, MaxRetries)
	}
}

// ---------- MultiSender end-to-end (the 4 scenarios from Task 27) ----------

// Scenario 1 — both channels healthy: Feishu returns 200 immediately,
// WeCom is NOT called, no retry-queue enqueue. The Plan advance can
// proceed with full audit-trail coverage.
func TestMultiSender_Scenario1_BothChannelsOK(t *testing.T) {
	feishuSrv, feishuHits := newStaticServer(t, http.StatusOK, `{"StatusCode":0}`)
	wecomSrv, wecomHits := newStaticServer(t, http.StatusOK, `{"errcode":0}`)

	fs := NewFeishuSenderWithHTTP(feishuSrv.URL, feishuSrv.Client())
	ws := NewWeComSenderWithHTTP(wecomSrv.URL, wecomSrv.Client())

	q, err := newInMemoryQueue(t)
	if err != nil {
		t.Fatalf("newInMemoryQueue: %v", err)
	}
	m := &MultiSender{Feishu: fs, WeCom: ws, Queue: q}

	if err := m.Send(context.Background(), fixtureMessage()); err != nil {
		t.Fatalf("Send must succeed when both channels are healthy: %v", err)
	}
	if got := atomic.LoadInt32(feishuHits); got != 1 {
		t.Errorf("feishu hits: got %d, want 1", got)
	}
	if got := atomic.LoadInt32(wecomHits); got != 0 {
		t.Errorf("wecom must not be called when feishu succeeds: got %d", got)
	}
	if n, _ := q.Count(context.Background()); n != 0 {
		t.Errorf("queue size after success: got %d, want 0", n)
	}
}

// Scenario 2 — Feishu 5xx transient: Feishu exhausts all 3 retries,
// then WeCom returns 200 on its first attempt. No retry-queue enqueue.
// This is the operational norm: Feishu's flap window clears within 7s,
// we slip through WeCom.
func TestMultiSender_Scenario2_Feishu5xxFallsBackToWeCom(t *testing.T) {
	feishuSrv, feishuHits := newStaticServer(t, http.StatusInternalServerError, `{}`)
	wecomSrv, wecomHits := newStaticServer(t, http.StatusOK, `{}`)

	fs := NewFeishuSenderWithHTTP(feishuSrv.URL, feishuSrv.Client())
	ws := NewWeComSenderWithHTTP(wecomSrv.URL, wecomSrv.Client())

	q, err := newInMemoryQueue(t)
	if err != nil {
		t.Fatalf("newInMemoryQueue: %v", err)
	}
	m := &MultiSender{Feishu: fs, WeCom: ws, Queue: q}

	if err := m.Send(context.Background(), fixtureMessage()); err != nil {
		t.Fatalf("Send must succeed via wecom fallback: %v", err)
	}
	if got := atomic.LoadInt32(feishuHits); got != int32(MaxRetries) {
		t.Errorf("feishu hits: got %d, want %d", got, MaxRetries)
	}
	if got := atomic.LoadInt32(wecomHits); got != 1 {
		t.Errorf("wecom hits: got %d, want 1 (first attempt succeeds)", got)
	}
	if n, _ := q.Count(context.Background()); n != 0 {
		t.Errorf("queue size after fallback success: got %d, want 0", n)
	}
}

// Scenario 3 — both channels 5xx: every webhook is dead. MultiSender
// must NOT drop the notification on the floor. Both rendered payloads
// (one per channel) land in the retry queue with retry_count=0 and a
// 1s initial backoff so the next Sweep cycle picks them up.
func TestMultiSender_Scenario3_BothDeadEnqueuesBoth(t *testing.T) {
	feishuSrv, feishuHits := newStaticServer(t, http.StatusInternalServerError, `{}`)
	wecomSrv, wecomHits := newStaticServer(t, http.StatusInternalServerError, `{}`)

	fs := NewFeishuSenderWithHTTP(feishuSrv.URL, feishuSrv.Client())
	ws := NewWeComSenderWithHTTP(wecomSrv.URL, wecomSrv.Client())

	q, err := newInMemoryQueue(t)
	if err != nil {
		t.Fatalf("newInMemoryQueue: %v", err)
	}
	m := &MultiSender{Feishu: fs, WeCom: ws, Queue: q}

	err = m.Send(context.Background(), fixtureMessage())
	if err == nil {
		t.Fatal("expected NotifyAllFailedError when both channels are dead")
	}
	var naf *NotifyAllFailedError
	if !errors.As(err, &naf) {
		t.Fatalf("expected *NotifyAllFailedError, got %T", err)
	}
	if naf.Feishu == nil || naf.WeCom == nil {
		t.Errorf("both per-channel errors must be present: feishu=%v wecom=%v", naf.Feishu, naf.WeCom)
	}
	// Feishu exhausts 3 retries; WeCom exhausts 3 retries.
	if got := atomic.LoadInt32(feishuHits); got != int32(MaxRetries) {
		t.Errorf("feishu hits: got %d, want %d", got, MaxRetries)
	}
	if got := atomic.LoadInt32(wecomHits); got != int32(MaxRetries) {
		t.Errorf("wecom hits: got %d, want %d", got, MaxRetries)
	}
	// Both payloads must be persisted for the next Sweep.
	if n, _ := q.Count(context.Background()); n != 2 {
		t.Errorf("queue size: got %d, want 2 (one per channel)", n)
	}
}

// Scenario 4 — Feishu 4xx permanent (e.g. 404 webhook URL mis-configured):
// MultiSender must SKIP the retry loop entirely for the 4xx channel
// (1 hit only), still try WeCom, and if WeCom also fails, both payloads
// hit the retry queue. The Plan advance proceeds; the operator is
// notified via the NotifyAllFailedError returned.
func TestMultiSender_Scenario4_Feishu4xxNoRetryDirectEnqueue(t *testing.T) {
	feishuSrv, feishuHits := newStaticServer(t, http.StatusNotFound, `{"error":"hook missing"}`)
	wecomSrv, wecomHits := newStaticServer(t, http.StatusServiceUnavailable, `{}`)

	fs := NewFeishuSenderWithHTTP(feishuSrv.URL, feishuSrv.Client())
	ws := NewWeComSenderWithHTTP(wecomSrv.URL, wecomSrv.Client())

	q, err := newInMemoryQueue(t)
	if err != nil {
		t.Fatalf("newInMemoryQueue: %v", err)
	}
	m := &MultiSender{Feishu: fs, WeCom: ws, Queue: q}

	err = m.Send(context.Background(), fixtureMessage())
	if err == nil {
		t.Fatal("expected NotifyAllFailedError when both channels fail")
	}
	// Permanent failure: Feishu is hit once and never retried.
	if got := atomic.LoadInt32(feishuHits); got != 1 {
		t.Errorf("feishu hits: got %d, want 1 (no retry on 4xx)", got)
	}
	// WeCom is retryable 503 — it should still hit MaxRetries times.
	if got := atomic.LoadInt32(wecomHits); got != int32(MaxRetries) {
		t.Errorf("wecom hits: got %d, want %d (503 is retryable)", got, MaxRetries)
	}
	// Both payloads should land in the queue for the next Sweep so the
	// 404 mis-configuration does not drop the Plan notification.
	if n, _ := q.Count(context.Background()); n != 2 {
		t.Errorf("queue size: got %d, want 2 (feishu permanent + wecom retryable)", n)
	}
}
