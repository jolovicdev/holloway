package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jolovicdev/holloway/internal/store"
	"github.com/jolovicdev/holloway/internal/tunnel"
)

func TestHookRejectsUnknownToken(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)

	req := httptest.NewRequest(http.MethodPost, "/hook/badtoken", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHookStoresWebhookBeforeReportingDisconnectedClient(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	app.createToken(t, "testtoken")

	req := httptest.NewRequest(http.MethodPost, "/hook/testtoken/orders?source=test", strings.NewReader(`{"id":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	pending, err := app.store.PendingWebhooks("testtoken")
	if err != nil {
		t.Fatalf("pending webhooks: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending count = %d, want 1", len(pending))
	}
	if pending[0].Path != "/orders?source=test" {
		t.Fatalf("path = %q, want /orders?source=test", pending[0].Path)
	}
	if pending[0].Body != `{"id":1}` {
		t.Fatalf("body = %q", pending[0].Body)
	}
}

func TestHookRateLimitsBeforeSavingWebhook(t *testing.T) {
	t.Parallel()

	app := newTestAppWithConfig(t, Config{
		WebhookLimiter: NewWebhookRateLimiter(1, time.Minute),
	})
	app.createToken(t, "testtoken")

	req := httptest.NewRequest(http.MethodPost, "/hook/testtoken/orders", strings.NewReader(`{"id":1}`))
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("first status = %d, want 502", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/hook/testtoken/orders", strings.NewReader(`{"id":2}`))
	rec = httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429", rec.Code)
	}

	pending, err := app.store.PendingWebhooks("testtoken")
	if err != nil {
		t.Fatalf("pending webhooks: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending count = %d, want 1", len(pending))
	}
	if pending[0].Body != `{"id":1}` {
		t.Fatalf("saved body = %q, want first request only", pending[0].Body)
	}
}

func TestInvalidHookTokenDoesNotAllocateRateLimiterEntry(t *testing.T) {
	t.Parallel()

	limiter := NewWebhookRateLimiter(100, time.Minute)
	app := newTestAppWithConfig(t, Config{
		WebhookLimiter: limiter,
	})

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/hook/missing-token-"+strconv.Itoa(i), strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	}

	if len(limiter.entries) != 0 {
		t.Fatalf("limiter entry count = %d, want 0", len(limiter.entries))
	}
}

func TestHookMarksDeliveredWhenClientReceivesWebhook(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	app.createToken(t, "testtoken")
	sender := &fakeSender{response: tunnel.Response{ID: "ignored", StatusCode: 202, Body: `{"accepted":true}`}}
	unregister := app.hub.Register("testtoken", sender)
	defer unregister()

	req := httptest.NewRequest(http.MethodPost, "/hook/testtoken/orders", strings.NewReader(`{"id":1}`))
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if strings.TrimSpace(string(body)) != "delivered" {
		t.Fatalf("body = %q, want delivered", string(body))
	}

	pending, err := app.store.PendingWebhooks("testtoken")
	if err != nil {
		t.Fatalf("pending webhooks: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending count = %d, want 0", len(pending))
	}

	history, err := app.store.LastWebhooks(1)
	if err != nil {
		t.Fatalf("last webhooks: %v", err)
	}
	if len(history) != 1 || history[0].StatusCode != 202 {
		t.Fatalf("history = %#v, want delivered status 202", history)
	}
	if history[0].ResponseBody != `{"accepted":true}` {
		t.Fatalf("response body = %q, want local response", history[0].ResponseBody)
	}
	if len(sender.seen) != 1 || sender.seen[0].Body == nil {
		t.Fatalf("forwarded requests = %#v", sender.seen)
	}
}

func TestHookKeepsWebhookPendingWhenLocalDeliveryFails(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	app.createToken(t, "testtoken")
	sender := &fakeSender{response: tunnel.Response{ID: "ignored", StatusCode: 0, Error: "connection refused"}}
	unregister := app.hub.Register("testtoken", sender)
	defer unregister()

	req := httptest.NewRequest(http.MethodPost, "/hook/testtoken/orders", strings.NewReader(`{"id":1}`))
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	pending, err := app.store.PendingWebhooks("testtoken")
	if err != nil {
		t.Fatalf("pending webhooks: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending count = %d, want 1", len(pending))
	}
}

func TestReplayWebhookSendsStoredRequest(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	app.createToken(t, "testtoken")
	if err := app.store.SaveWebhook(store.Webhook{
		ID:      "wh_replay",
		TokenID: "testtoken",
		Method:  http.MethodPost,
		Path:    "/orders",
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Body:    `{"id":2}`,
	}); err != nil {
		t.Fatalf("save webhook: %v", err)
	}

	sender := &fakeSender{response: tunnel.Response{ID: "wh_replay", StatusCode: 204, Body: "replayed"}}
	unregister := app.hub.Register("testtoken", sender)
	defer unregister()

	status, err := app.ReplayWebhook(context.Background(), "wh_replay")
	if err != nil {
		t.Fatalf("replay webhook: %v", err)
	}
	if status != 204 {
		t.Fatalf("status = %d, want 204", status)
	}
	if len(sender.seen) != 1 {
		t.Fatalf("forwarded count = %d, want 1", len(sender.seen))
	}
	if !sender.seen[0].Replay {
		t.Fatal("replayed request did not set replay flag")
	}
	if string(sender.seen[0].Body) != `{"id":2}` {
		t.Fatalf("body = %q", string(sender.seen[0].Body))
	}
	got, err := app.store.GetWebhook("wh_replay")
	if err != nil {
		t.Fatalf("get replayed webhook: %v", err)
	}
	if got.ResponseBody != "replayed" {
		t.Fatalf("response body = %q, want replayed", got.ResponseBody)
	}
}

func TestAdminPostRejectsCrossSiteOrigin(t *testing.T) {
	t.Parallel()

	app := newTestAppWithConfig(t, Config{AdminPassword: "secret"})

	req := httptest.NewRequest(http.MethodPost, "https://hooks.example.com/tokens", strings.NewReader("name=bad"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	req.SetBasicAuth("", "secret")
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAdminPostRejectsMissingOriginAndReferer(t *testing.T) {
	t.Parallel()

	app := newTestAppWithConfig(t, Config{AdminPassword: "secret"})

	req := httptest.NewRequest(http.MethodPost, "/tokens", strings.NewReader("name=bad"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("", "secret")
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAdminRoutesRejectEmptyPassword(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)

	req := httptest.NewRequest(http.MethodPost, "/tokens", strings.NewReader("name=bad"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestDeletingTokenDisconnectsActiveTunnel(t *testing.T) {
	t.Parallel()

	app := newTestAppWithConfig(t, Config{AdminPassword: "secret"})
	app.createToken(t, "testtoken")

	sender := &fakeSender{}
	app.hub.Register("testtoken", sender)

	req := httptest.NewRequest(http.MethodPost, "/tokens/testtoken/delete", nil)
	req.Header.Set("Origin", "http://example.com")
	req.SetBasicAuth("", "secret")
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if !sender.closed {
		t.Fatal("deleted token did not close active tunnel")
	}
	if app.hub.Connected("testtoken") {
		t.Fatal("deleted token left tunnel connected")
	}
}

type testApp struct {
	*Server
	store *store.Store
	hub   *Hub
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()

	return newTestAppWithConfig(t, Config{})
}

func newTestAppWithConfig(t *testing.T, config Config) *testApp {
	t.Helper()

	s, err := store.Open(t.TempDir() + "/holloway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
	})

	hub := NewHub()
	config.Store = s
	config.Hub = hub
	server := NewServer(config)
	return &testApp{Server: server, store: s, hub: hub}
}

func (a *testApp) createToken(t *testing.T, token string) string {
	t.Helper()
	secret, err := a.store.CreateToken(token, "Test token")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return secret
}

var _ Client = (*fakeSender)(nil)
