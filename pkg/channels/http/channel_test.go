package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

// mockExecutor is a test double for channels.AgentExecutor.
type mockExecutor struct {
	response string
	err      error
	called   chan struct{}
}

func newMockExecutor(response string) *mockExecutor {
	return &mockExecutor{response: response, called: make(chan struct{}, 1)}
}

func (m *mockExecutor) ProcessDirectWithChannel(
	_ context.Context, _, _, _, _ string,
) (string, error) {
	select {
	case m.called <- struct{}{}:
	default:
	}
	return m.response, m.err
}

// testExecutor is a local alias for AgentExecutor used in tests.
type testExecutor interface {
	ProcessDirectWithChannel(ctx context.Context, content, sessionKey, channel, chatID string) (string, error)
}

// newTestChannel creates a started HTTP channel with the given executor.
func newTestChannel(t *testing.T, exec testExecutor) *Channel {
	t.Helper()
	b := bus.NewMessageBus()
	t.Cleanup(b.Close)

	cfg := &config.HTTPSettings{}
	ch, err := NewHTTPChannel(cfg, b)
	if err != nil {
		t.Fatalf("NewHTTPChannel: %v", err)
	}
	if exec != nil {
		ch.SetExecutor(exec)
	}
	if err := ch.Start(context.Background()); err != nil {
		t.Fatalf("channel.Start: %v", err)
	}
	t.Cleanup(func() { _ = ch.Stop(context.Background()) })
	return ch
}

func postJSON(t *testing.T, ch *Channel, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(nethttp.MethodPost, webhookBase, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	ch.ServeHTTP(rr, req)
	return rr
}

func getStatus(t *testing.T, ch *Channel, reqID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(nethttp.MethodGet, fmt.Sprintf("%s/status/%s", webhookBase, reqID), nil)
	rr := httptest.NewRecorder()
	ch.ServeHTTP(rr, req)
	return rr
}

// TestPostReturns202 verifies the immediate 202 Accepted response.
func TestPostReturns202(t *testing.T) {
	exec := newMockExecutor("done")
	ch := newTestChannel(t, exec)

	rr := postJSON(t, ch, webhookRequest{Message: "hello"})

	if rr.Code != nethttp.StatusAccepted {
		t.Fatalf("expected 202, got %d", rr.Code)
	}

	var resp webhookResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK {
		t.Error("expected ok=true")
	}
	if resp.RequestID == "" {
		t.Error("expected non-empty request_id")
	}
	if resp.Status != "accepted" {
		t.Errorf("expected status=accepted, got %q", resp.Status)
	}
}

// TestPostMissingMessage returns 400 for empty message.
func TestPostMissingMessage(t *testing.T) {
	ch := newTestChannel(t, newMockExecutor("ok"))

	rr := postJSON(t, ch, webhookRequest{Message: ""})

	if rr.Code != nethttp.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// TestPostInvalidJSON returns 400 for malformed JSON.
func TestPostInvalidJSON(t *testing.T) {
	ch := newTestChannel(t, newMockExecutor("ok"))

	req := httptest.NewRequest(nethttp.MethodPost, webhookBase, strings.NewReader("not json"))
	rr := httptest.NewRecorder()
	ch.ServeHTTP(rr, req)

	if rr.Code != nethttp.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// TestStatusPending verifies the status endpoint returns pending before the agent completes.
func TestStatusPending(t *testing.T) {
	block := make(chan struct{})

	ch := newTestChannel(t, nil)
	// Override with a slow executor
	blockingExec := &blockExecutor{block: block, response: "result"}
	ch.SetExecutor(blockingExec)

	rr := postJSON(t, ch, webhookRequest{Message: "slow task"})
	if rr.Code != nethttp.StatusAccepted {
		t.Fatalf("expected 202, got %d", rr.Code)
	}

	var resp webhookResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)

	// Poll status immediately — should be pending
	statusRR := getStatus(t, ch, resp.RequestID)
	if statusRR.Code != nethttp.StatusOK {
		t.Fatalf("expected 200 on status, got %d", statusRR.Code)
	}

	var sr statusResponse
	_ = json.NewDecoder(statusRR.Body).Decode(&sr)
	if sr.Status != "pending" {
		t.Errorf("expected pending, got %q", sr.Status)
	}

	// Unblock the executor
	close(block)
}

// TestStatusDone verifies the status endpoint returns done after agent completes.
func TestStatusDone(t *testing.T) {
	exec := newMockExecutor("the result")
	ch := newTestChannel(t, exec)

	rr := postJSON(t, ch, webhookRequest{Message: "quick task"})
	var resp webhookResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)

	// Wait for the executor to be called
	select {
	case <-exec.called:
	case <-time.After(5 * time.Second):
		t.Fatal("executor not called within timeout")
	}
	// Give the goroutine a moment to store the result
	time.Sleep(50 * time.Millisecond)

	statusRR := getStatus(t, ch, resp.RequestID)
	var sr statusResponse
	_ = json.NewDecoder(statusRR.Body).Decode(&sr)

	if sr.Status != "done" {
		t.Errorf("expected done, got %q", sr.Status)
	}
	if sr.Result != "the result" {
		t.Errorf("expected result %q, got %q", "the result", sr.Result)
	}
}

// TestStatusNotFound returns 404 for unknown request ID.
func TestStatusNotFound(t *testing.T) {
	ch := newTestChannel(t, newMockExecutor("ok"))

	rr := getStatus(t, ch, "nonexistent-id")
	if rr.Code != nethttp.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

// TestSessionIDContinuity verifies session_id is used as part of the session key.
func TestSessionIDContinuity(t *testing.T) {
	var capturedSessionKey string
	exec := newCapturingExecutor("ok")
	ch := newTestChannel(t, exec)

	_ = postJSON(t, ch, webhookRequest{Message: "task", SessionID: "my-session-123"})

	// Wait briefly for processing
	select {
	case capturedSessionKey = <-exec.sessionKeys:
	case <-time.After(5 * time.Second):
		t.Fatal("executor not called")
	}

	if !strings.Contains(capturedSessionKey, "my-session-123") {
		t.Errorf("expected session key to contain 'my-session-123', got %q", capturedSessionKey)
	}
}

// TestWebhookPath verifies the registered path ends with a trailing slash.
func TestWebhookPath(t *testing.T) {
	b := bus.NewMessageBus()
	defer b.Close()
	ch, _ := NewHTTPChannel(&config.HTTPSettings{}, b)
	path := ch.WebhookPath()
	if !strings.HasSuffix(path, "/") {
		t.Errorf("WebhookPath should end with '/' for prefix matching, got %q", path)
	}
}

// TestUnknownRoute returns 404 for unrecognised paths.
func TestUnknownRoute(t *testing.T) {
	ch := newTestChannel(t, newMockExecutor("ok"))

	req := httptest.NewRequest(nethttp.MethodGet, "/webhook/http/unknown", nil)
	rr := httptest.NewRecorder()
	ch.ServeHTTP(rr, req)

	if rr.Code != nethttp.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

// TestNoExecutorReturnsError verifies that requests without an executor set
// result in an "error" status rather than a panic or hang.
func TestNoExecutorReturnsError(t *testing.T) {
	ch := newTestChannel(t, nil) // no executor

	rr := postJSON(t, ch, webhookRequest{Message: "task"})
	var resp webhookResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)

	// Give the goroutine time to store the error result
	time.Sleep(100 * time.Millisecond)

	statusRR := getStatus(t, ch, resp.RequestID)
	var sr statusResponse
	_ = json.NewDecoder(statusRR.Body).Decode(&sr)

	if sr.Status != "error" {
		t.Errorf("expected error status, got %q", sr.Status)
	}
}

// --- helpers ---

// blockExecutor blocks until the provided channel is closed.
type blockExecutor struct {
	block    chan struct{}
	response string
}

func (b *blockExecutor) ProcessDirectWithChannel(ctx context.Context, _, _, _, _ string) (string, error) {
	select {
	case <-b.block:
	case <-ctx.Done():
	}
	return b.response, nil
}

// capturingExecutor records the session key passed to ProcessDirectWithChannel.
type capturingExecutor struct {
	response    string
	sessionKeys chan string
}

func newCapturingExecutor(response string) *capturingExecutor {
	return &capturingExecutor{response: response, sessionKeys: make(chan string, 10)}
}

func (c *capturingExecutor) ProcessDirectWithChannel(_ context.Context, _, sessionKey, _, _ string) (string, error) {
	c.sessionKeys <- sessionKey
	return c.response, nil
}
