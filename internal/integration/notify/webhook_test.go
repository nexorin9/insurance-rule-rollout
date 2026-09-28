package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ---------- helpers ----------

// fixtureMessage is the canonical triple used by every happy-path test.
// We deliberately pick an actor name without underscores / asterisks /
// brackets so the renderer does NOT have to escape anything for the
// happy-path assertions to match — escape behaviour has its own table-
// driven tests in template_test.go (TestEscapeMarkdown_*).
func fixtureMessage() NotifyMessage {
	return NotifyMessage{
		PlanID:    "r-2026-09-DRG-v17",
		Action:    "APPROVE",
		Actor:     "yangjb（医保办）",
		Comment:   "已核对影响面",
		Timestamp: time.Date(2026, 9, 28, 10, 30, 0, 0, time.UTC),
	}
}

// newStaticServer returns an httptest.Server that replies with the given
// status + body for every request. The hit counter lets tests verify
// retry behaviour.
func newStaticServer(t *testing.T, status int, body string) (*httptest.Server, *int32) {
	t.Helper()
	hits := new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// newFlakyServer returns an httptest.Server that fails the first
// `failCount` requests with status, then succeeds with 200. Used to
// verify exponential backoff actually retries and eventually delivers.
func newFlakyServer(t *testing.T, failStatus int, failCount int) (*httptest.Server, *int32) {
	t.Helper()
	hits := new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		if int(n) <= failCount {
			w.WriteHeader(failStatus)
			_, _ = io.WriteString(w, `{"error":"flaky"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// ---------- templates_test ----------

func TestRenderFeishuCard_HappyPath(t *testing.T) {
	got, err := RenderFeishuCard(fixtureMessage())
	if err != nil {
		t.Fatalf("RenderFeishuCard: %v", err)
	}
	// The template renders JSON — parse and check the structural keys
	// without re-implementing the renderer.
	var payload map[string]any
	if err := json.Unmarshal([]byte(got), &payload); err != nil {
		t.Fatalf("Feishu output is not valid JSON: %v\n%s", err, got)
	}
	if payload["msg_type"] != "interactive" {
		t.Errorf("msg_type: got %v, want interactive", payload["msg_type"])
	}
	card, ok := payload["card"].(map[string]any)
	if !ok {
		t.Fatalf("card missing or wrong shape: %v", payload["card"])
	}
	header, ok := card["header"].(map[string]any)
	if !ok {
		t.Fatalf("card.header missing: %v", card["header"])
	}
	title, ok := header["title"].(map[string]any)
	if !ok {
		t.Fatalf("card.header.title missing: %v", header["title"])
	}
	if got := title["content"]; !strings.Contains(got.(string), "APPROVE") || !strings.Contains(got.(string), "r-2026-09-DRG-v17") {
		t.Errorf("card.header.title missing action/plan id: %v", got)
	}
	if !strings.Contains(got, "yangjb") {
		t.Errorf("Feishu card missing actor: %s", got)
	}
	if !strings.Contains(got, "已核对影响面") {
		t.Errorf("Feishu card missing comment: %s", got)
	}
}

func TestRenderWeComText_HappyPath(t *testing.T) {
	got, err := RenderWeComText(fixtureMessage())
	if err != nil {
		t.Fatalf("RenderWeComText: %v", err)
	}
	for _, want := range []string{"APPROVE", "r-2026-09-DRG-v17", "yangjb", "已核对影响面"} {
		if !strings.Contains(got, want) {
			t.Errorf("WeCom text missing %q:\n%s", want, got)
		}
	}
}

func TestRenderFeishuCard_EmptyFields(t *testing.T) {
	msg := NotifyMessage{PlanID: "p1", Action: "APPLY", Actor: "", Comment: "", Timestamp: time.Now()}
	got, err := RenderFeishuCard(msg)
	if err != nil {
		t.Fatalf("RenderFeishuCard: %v", err)
	}
	if !strings.Contains(got, "APPLY") {
		t.Errorf("Action not rendered: %s", got)
	}
	if !strings.Contains(got, "p1") {
		t.Errorf("PlanID not rendered: %s", got)
	}
}

func TestTemplateError_Is(t *testing.T) {
	// Sanity: TemplateError implements error.
	var err error = &TemplateError{Template: "feishu", Reason: "test"}
	if !strings.Contains(err.Error(), "feishu") {
		t.Errorf("TemplateError.Error: got %q", err.Error())
	}
}

// ---------- FeishuSender ----------

func TestFeishuSender_Success(t *testing.T) {
	srv, hits := newStaticServer(t, http.StatusOK, `{"StatusCode":0,"StatusMessage":"ok"}`)
	s := NewFeishuSenderWithHTTP(srv.URL, srv.Client())
	if err := s.SendFeishu(context.Background(), `{"msg_type":"interactive"}`); err != nil {
		t.Fatalf("SendFeishu: %v", err)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("hits: got %d, want 1 (no retry on success)", *hits)
	}
}

func TestFeishuSender_5xxRetriesThenFails(t *testing.T) {
	srv, hits := newStaticServer(t, http.StatusInternalServerError, `{"error":"boom"}`)
	s := NewFeishuSenderWithHTTP(srv.URL, srv.Client())
	err := s.SendFeishu(context.Background(), `{}`)
	if err == nil {
		t.Fatalf("expected WebhookError, got nil")
	}
	var we *WebhookError
	if !errors.As(err, &we) {
		t.Fatalf("expected *WebhookError, got %T", err)
	}
	if !we.Retryable {
		t.Errorf("5xx must be Retryable, got false")
	}
	if we.Channel != "feishu" {
		t.Errorf("Channel: got %q, want feishu", we.Channel)
	}
	if got := atomic.LoadInt32(hits); got != int32(MaxRetries) {
		t.Errorf("hits: got %d, want %d (3 retries)", got, MaxRetries)
	}
}

func TestFeishuSender_4xxIsPermanent(t *testing.T) {
	srv, hits := newStaticServer(t, http.StatusUnauthorized, `{"error":"bad token"}`)
	s := NewFeishuSenderWithHTTP(srv.URL, srv.Client())
	err := s.SendFeishu(context.Background(), `{}`)
	if err == nil {
		t.Fatalf("expected WebhookError, got nil")
	}
	var we *WebhookError
	if !errors.As(err, &we) {
		t.Fatalf("expected *WebhookError, got %T", err)
	}
	if we.Retryable {
		t.Errorf("401 must NOT be Retryable, got true")
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("hits: got %d, want 1 (no retry on permanent failure)", got)
	}
}

func TestFeishuSender_RetriesThenSucceeds(t *testing.T) {
	// Fail twice, succeed on attempt 3.
	srv, hits := newFlakyServer(t, http.StatusInternalServerError, 2)
	s := NewFeishuSenderWithHTTP(srv.URL, srv.Client())
	if err := s.SendFeishu(context.Background(), `{}`); err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 3 {
		t.Errorf("hits: got %d, want 3 (2 fails + 1 success)", got)
	}
}

func TestFeishuSender_NilClient(t *testing.T) {
	s := &FeishuSender{URL: "http://example.com"}
	err := s.SendFeishu(context.Background(), `{}`)
	if err == nil {
		t.Fatal("expected error for nil client")
	}
	var we *WebhookError
	if !errors.As(err, &we) {
		t.Fatalf("expected *WebhookError, got %T", err)
	}
}

// ---------- WeComSender ----------

func TestWeComSender_Success(t *testing.T) {
	srv, hits := newStaticServer(t, http.StatusOK, `{"errcode":0,"errmsg":"ok"}`)
	s := NewWeComSenderWithHTTP(srv.URL, srv.Client())
	if err := s.SendWeCom(context.Background(), `# hello world`); err != nil {
		t.Fatalf("SendWeCom: %v", err)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("hits: got %d, want 1", *hits)
	}
}

func TestWeComSender_5xxRetriesThenFails(t *testing.T) {
	srv, _ := newStaticServer(t, http.StatusBadGateway, `{}`)
	s := NewWeComSenderWithHTTP(srv.URL, srv.Client())
	err := s.SendWeCom(context.Background(), `body`)
	if err == nil {
		t.Fatal("expected error")
	}
	var we *WebhookError
	if !errors.As(err, &we) {
		t.Fatalf("expected *WebhookError, got %T", err)
	}
	if !we.Retryable {
		t.Errorf("502 must be Retryable")
	}
	if we.Channel != "wecom" {
		t.Errorf("Channel: got %q, want wecom", we.Channel)
	}
}

func TestWeComSender_404IsPermanent(t *testing.T) {
	srv, hits := newStaticServer(t, http.StatusNotFound, `{}`)
	s := NewWeComSenderWithHTTP(srv.URL, srv.Client())
	err := s.SendWeCom(context.Background(), `body`)
	if err == nil {
		t.Fatal("expected error")
	}
	var we *WebhookError
	if !errors.As(err, &we) {
		t.Fatalf("expected *WebhookError, got %T", err)
	}
	if we.Retryable {
		t.Errorf("404 must NOT be Retryable")
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("hits: got %d, want 1 (permanent)", got)
	}
}

// ---------- MultiSender ----------

func TestMultiSender_FeishuSucceeds(t *testing.T) {
	feishuSrv, feishuHits := newStaticServer(t, http.StatusOK, `{}`)
	wecomSrv, wecomHits := newStaticServer(t, http.StatusOK, `{}`)

	fs := NewFeishuSenderWithHTTP(feishuSrv.URL, feishuSrv.Client())
	ws := NewWeComSenderWithHTTP(wecomSrv.URL, wecomSrv.Client())

	inMem, err := newInMemoryQueue(t)
	if err != nil {
		t.Fatalf("newInMemoryQueue: %v", err)
	}
	m := &MultiSender{Feishu: fs, WeCom: ws, Queue: inMem}

	if err := m.Send(context.Background(), fixtureMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if atomic.LoadInt32(feishuHits) != 1 {
		t.Errorf("feishu hits: got %d, want 1", *feishuHits)
	}
	if atomic.LoadInt32(wecomHits) != 0 {
		t.Errorf("wecom must not be called when feishu succeeds: got %d", *wecomHits)
	}
}

func TestMultiSender_FeishuFailsWeComSucceeds(t *testing.T) {
	feishuSrv, feishuHits := newStaticServer(t, http.StatusInternalServerError, `{}`)
	wecomSrv, wecomHits := newStaticServer(t, http.StatusOK, `{}`)
	fs := NewFeishuSenderWithHTTP(feishuSrv.URL, feishuSrv.Client())
	ws := NewWeComSenderWithHTTP(wecomSrv.URL, wecomSrv.Client())

	inMem, err := newInMemoryQueue(t)
	if err != nil {
		t.Fatalf("newInMemoryQueue: %v", err)
	}
	m := &MultiSender{Feishu: fs, WeCom: ws, Queue: inMem}

	if err := m.Send(context.Background(), fixtureMessage()); err != nil {
		t.Fatalf("Send must succeed via wecom fallback: %v", err)
	}
	if atomic.LoadInt32(feishuHits) != int32(MaxRetries) {
		t.Errorf("feishu hits: got %d, want %d (3 retries)", *feishuHits, MaxRetries)
	}
	if atomic.LoadInt32(wecomHits) != 1 {
		t.Errorf("wecom hits: got %d, want 1", *wecomHits)
	}
	// Both payloads should NOT have been enqueued — wecom succeeded.
	if n, _ := inMem.Count(context.Background()); n != 0 {
		t.Errorf("queue size after success: got %d, want 0", n)
	}
}

func TestMultiSender_BothFailEnqueues(t *testing.T) {
	feishuSrv, _ := newStaticServer(t, http.StatusInternalServerError, `{}`)
	wecomSrv, _ := newStaticServer(t, http.StatusInternalServerError, `{}`)
	fs := NewFeishuSenderWithHTTP(feishuSrv.URL, feishuSrv.Client())
	ws := NewWeComSenderWithHTTP(wecomSrv.URL, wecomSrv.Client())

	inMem, err := newInMemoryQueue(t)
	if err != nil {
		t.Fatalf("newInMemoryQueue: %v", err)
	}
	m := &MultiSender{Feishu: fs, WeCom: ws, Queue: inMem}

	err = m.Send(context.Background(), fixtureMessage())
	if err == nil {
		t.Fatal("expected NotifyAllFailedError")
	}
	var naf *NotifyAllFailedError
	if !errors.As(err, &naf) {
		t.Fatalf("expected *NotifyAllFailedError, got %T", err)
	}
	if naf.Feishu == nil || naf.WeCom == nil {
		t.Errorf("both per-channel errors must be present: feishu=%v wecom=%v", naf.Feishu, naf.WeCom)
	}
	// Both payloads must have been enqueued for the next Sweep.
	if n, _ := inMem.Count(context.Background()); n != 2 {
		t.Errorf("queue size: got %d, want 2 (one per channel)", n)
	}
}

func TestMultiSender_NoChannels(t *testing.T) {
	m := &MultiSender{}
	err := m.Send(context.Background(), fixtureMessage())
	if err == nil {
		t.Fatal("expected NotifyAllFailedError with no channels")
	}
	var naf *NotifyAllFailedError
	if !errors.As(err, &naf) {
		t.Fatalf("expected *NotifyAllFailedError, got %T", err)
	}
}

func TestMultiSender_NilReceiver(t *testing.T) {
	var m *MultiSender
	if err := m.Send(context.Background(), fixtureMessage()); err == nil {
		t.Fatal("expected error for nil MultiSender")
	}
}

// ---------- RetryQueue ----------

// newInMemoryQueue builds an in-memory RetryQueue with a unique DSN so
// each test gets its own private SQLite database. Without this,
// file::memory:?cache=shared would leak state across tests (we saw
// Count() return 4 instead of 2 in earlier runs).
func newInMemoryQueue(t *testing.T) (*RetryQueue, error) {
	t.Helper()
	dsn := fmt.Sprintf("file:test-%d-%d?mode=memory&cache=shared", time.Now().UnixNano(), os.Getpid())
	return OpenRetryQueue(dsn)
}

func TestRetryQueue_EnqueueAndCount(t *testing.T) {
	q, err := newInMemoryQueue(t)
	if err != nil {
		t.Fatalf("newInMemoryQueue: %v", err)
	}
	ctx := context.Background()
	if err := q.Enqueue(ctx, "feishu", []byte(`{"x":1}`), nil); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := q.Enqueue(ctx, "wecom", []byte(`{"y":2}`), errors.New("boom")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	n, err := q.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 2 {
		t.Errorf("Count: got %d, want 2", n)
	}
}

func TestRetryQueue_EnqueueRejectsEmpty(t *testing.T) {
	q, err := newInMemoryQueue(t)
	if err != nil {
		t.Fatalf("newInMemoryQueue: %v", err)
	}
	ctx := context.Background()
	if err := q.Enqueue(ctx, "", []byte("x"), nil); err == nil {
		t.Error("empty channel must error")
	}
	if err := q.Enqueue(ctx, "feishu", nil, nil); err == nil {
		t.Error("empty payload must error")
	}
}

func TestRetryQueue_SweepDeliversAndDeletes(t *testing.T) {
	q, err := newInMemoryQueue(t)
	if err != nil {
		t.Fatalf("newInMemoryQueue: %v", err)
	}
	ctx := context.Background()

	srv, _ := newStaticServer(t, http.StatusOK, `{}`)
	sender := NewFeishuSenderWithHTTP(srv.URL, srv.Client())

	if err := q.Enqueue(ctx, "feishu", []byte(`{"x":1}`), errors.New("transient")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Force the row to be due immediately by waiting > InitialBackoff.
	time.Sleep(InitialBackoff + 100*time.Millisecond)

	delivered, err := q.Sweep(ctx, sender)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if delivered != 1 {
		t.Errorf("delivered: got %d, want 1", delivered)
	}
	if n, _ := q.Count(ctx); n != 0 {
		t.Errorf("queue size after sweep: got %d, want 0", n)
	}
}

func TestRetryQueue_SweepBumpsRetryCount(t *testing.T) {
	q, err := newInMemoryQueue(t)
	if err != nil {
		t.Fatalf("newInMemoryQueue: %v", err)
	}
	ctx := context.Background()

	// Permanent-failure webhook: Sweep keeps bumping retry_count
	// but does not delete the row.
	srv, _ := newStaticServer(t, http.StatusUnauthorized, `{}`)
	sender := NewFeishuSenderWithHTTP(srv.URL, srv.Client())

	if err := q.Enqueue(ctx, "feishu", []byte(`{}`), errors.New("transient")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	time.Sleep(InitialBackoff + 100*time.Millisecond)

	delivered, err := q.Sweep(ctx, sender)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if delivered != 0 {
		t.Errorf("permanent failure must yield delivered=0: got %d", delivered)
	}
	// The row must still exist with retry_count >= 1.
	n, _ := q.Count(ctx)
	if n != 1 {
		t.Fatalf("row must survive: got %d, want 1", n)
	}
}

func TestRetryQueue_SweepNoDueRows(t *testing.T) {
	q, err := newInMemoryQueue(t)
	if err != nil {
		t.Fatalf("newInMemoryQueue: %v", err)
	}
	ctx := context.Background()
	srv, _ := newStaticServer(t, http.StatusOK, `{}`)
	sender := NewFeishuSenderWithHTTP(srv.URL, srv.Client())

	if err := q.Enqueue(ctx, "feishu", []byte(`{}`), nil); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Do NOT sleep — the row is not due yet.
	delivered, err := q.Sweep(ctx, sender)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered: got %d, want 0", delivered)
	}
	if n, _ := q.Count(ctx); n != 1 {
		t.Errorf("queue size: got %d, want 1", n)
	}
}

func TestRetryQueue_CleanupRemovesOld(t *testing.T) {
	q, err := newInMemoryQueue(t)
	if err != nil {
		t.Fatalf("newInMemoryQueue: %v", err)
	}
	ctx := context.Background()
	if err := q.Enqueue(ctx, "feishu", []byte(`{}`), nil); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Wait long enough for the row to be older than the cutoff.
	time.Sleep(1100 * time.Millisecond)
	n, err := q.Cleanup(ctx, 1*time.Second)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if n != 1 {
		t.Errorf("Cleanup removed: got %d, want 1", n)
	}
	if cnt, _ := q.Count(ctx); cnt != 0 {
		t.Errorf("queue size after cleanup: got %d, want 0", cnt)
	}
}

func TestRetryQueue_CleanupZeroAge(t *testing.T) {
	q, err := newInMemoryQueue(t)
	if err != nil {
		t.Fatalf("newInMemoryQueue: %v", err)
	}
	ctx := context.Background()
	if err := q.Enqueue(ctx, "feishu", []byte(`{}`), nil); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// 0 cutoff removes all rows.
	n, err := q.Cleanup(ctx, 0)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if n != 1 {
		t.Errorf("Cleanup removed: got %d, want 1", n)
	}
}

func TestRetryQueue_BackoffSchedule(t *testing.T) {
	cases := []struct {
		n    int
		want time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{5, 32 * time.Second},
		{6, 64 * time.Second},
		{12, MaxBackoff}, // 2^12 = 4096s ≈ 68m > 30m cap
	}
	for _, c := range cases {
		got := backoff(c.n)
		if got != c.want {
			t.Errorf("backoff(%d): got %v, want %v", c.n, got, c.want)
		}
	}
}

func TestRetryQueue_NilReceiverGuards(t *testing.T) {
	var q *RetryQueue
	ctx := context.Background()
	if err := q.Enqueue(ctx, "feishu", []byte(`{}`), nil); err == nil {
		t.Error("Enqueue on nil receiver must error")
	}
	if _, err := q.Sweep(ctx, nil); err == nil {
		t.Error("Sweep on nil receiver must error")
	}
	if _, err := q.Cleanup(ctx, time.Hour); err == nil {
		t.Error("Cleanup on nil receiver must error")
	}
	if _, err := q.Count(ctx); err == nil {
		t.Error("Count on nil receiver must error")
	}
}

// ---------- Sender constructors ----------

func TestNewFeishuSender_NilConfig(t *testing.T) {
	if _, err := NewFeishuSender(nil); err == nil {
		t.Error("expected error for nil config")
	}
}

func TestNewWeComSender_NilConfig(t *testing.T) {
	if _, err := NewWeComSender(nil); err == nil {
		t.Error("expected error for nil config")
	}
}

func TestOpenRetryQueue_EmptyDSN_UsesInMemory(t *testing.T) {
	// SQLite driver's convention: empty DSN opens a transient in-memory DB.
	// We document this rather than fighting it — production code passes
	// a real file path from config.StateDBPath.
	q, err := OpenRetryQueue("")
	if err != nil {
		t.Fatalf("OpenRetryQueue(\"\"): unexpected error: %v", err)
	}
	if q == nil {
		t.Fatal("expected non-nil RetryQueue")
	}
	defer q.Close()
	if err := q.Enqueue(context.Background(), "feishu", []byte(`{}`), nil); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if n, _ := q.Count(context.Background()); n != 1 {
		t.Errorf("Count: got %d, want 1", n)
	}
}

func TestNewRetryQueue_NilDB(t *testing.T) {
	if _, err := NewRetryQueue(nil); err == nil {
		t.Error("nil *sql.DB must error")
	}
}