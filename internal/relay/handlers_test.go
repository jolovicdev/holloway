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

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
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
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", rec.Code)
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
	sender := &fakeSender{response: tunnel.Response{ID: "ignored", StatusCode: 201, Body: `{"accepted":true}`}}
	unregister := app.hub.Register("testtoken", sender)
	defer unregister()

	req := httptest.NewRequest(http.MethodPost, "/hook/testtoken/orders", strings.NewReader(`{"id":1}`))
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	// Hybrid live delivery passes the local app's status and body straight back
	// to the provider rather than a hardcoded 200/"delivered".
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != `{"accepted":true}` {
		t.Fatalf("body = %q, want local response passed through", string(body))
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
	if len(history) != 1 || history[0].StatusCode != 201 {
		t.Fatalf("history = %#v, want delivered status 201", history)
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

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	pending, err := app.store.PendingWebhooks("testtoken")
	if err != nil {
		t.Fatalf("pending webhooks: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending count = %d, want 1", len(pending))
	}
}

func TestHookForwardsLocalErrorStatusToProvider(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	app.createToken(t, "testtoken")
	sender := &fakeSender{response: tunnel.Response{ID: "ignored", StatusCode: 500, Body: "boom"}}
	unregister := app.hub.Register("testtoken", sender)
	defer unregister()

	req := httptest.NewRequest(http.MethodPost, "/hook/testtoken/orders", strings.NewReader(`{"id":1}`))
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	// A reachable local app that returns 5xx must reach the provider as 5xx, not
	// be masked as success.
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 passed through", rec.Code)
	}
	history, err := app.store.LastWebhooks(1)
	if err != nil {
		t.Fatalf("last webhooks: %v", err)
	}
	if len(history) != 1 || history[0].StatusCode != 500 {
		t.Fatalf("history = %#v, want delivered status 500", history)
	}
}

func TestHookDeduplicatesPendingDeliveries(t *testing.T) {
	t.Parallel()

	app := newTestAppWithConfig(t, Config{Dedup: true})
	app.createToken(t, "testtoken")

	post := func() int {
		req := httptest.NewRequest(http.MethodPost, "/hook/testtoken/orders", strings.NewReader(`{"id":1}`))
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post(); code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", code)
	}
	if code := post(); code != http.StatusAccepted {
		t.Fatalf("duplicate status = %d, want 202", code)
	}

	pending, err := app.store.PendingWebhooks("testtoken")
	if err != nil {
		t.Fatalf("pending webhooks: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending count = %d, want 1 (duplicate not stored)", len(pending))
	}
}

func TestHookReplaysOriginalResponseToDuplicate(t *testing.T) {
	t.Parallel()

	app := newTestAppWithConfig(t, Config{Dedup: true})
	app.createToken(t, "testtoken")
	sender := &fakeSender{response: tunnel.Response{StatusCode: 201, Body: `{"ok":true}`}}
	unregister := app.hub.Register("testtoken", sender)
	defer unregister()

	post := func() (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/hook/testtoken/orders", strings.NewReader(`{"id":1}`))
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		body, _ := io.ReadAll(rec.Body)
		return rec.Code, string(body)
	}

	if code, body := post(); code != http.StatusCreated || body != `{"ok":true}` {
		t.Fatalf("first delivery = %d %q, want 201 with passthrough body", code, body)
	}
	// The duplicate replays the original's status and body without re-forwarding.
	if code, body := post(); code != http.StatusCreated || body != `{"ok":true}` {
		t.Fatalf("duplicate = %d %q, want replayed 201 body", code, body)
	}

	if len(sender.seen) != 1 {
		t.Fatalf("forwarded %d times, want 1 (duplicate not re-forwarded)", len(sender.seen))
	}
	history, err := app.store.LastWebhooks(10)
	if err != nil {
		t.Fatalf("last webhooks: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("history rows = %d, want 1 (duplicate not stored)", len(history))
	}
}

func TestHookWithoutDedupStoresEveryDelivery(t *testing.T) {
	t.Parallel()

	app := newTestApp(t) // dedup disabled by default
	app.createToken(t, "testtoken")

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/hook/testtoken/orders", strings.NewReader(`{"id":1}`))
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", rec.Code)
		}
	}

	pending, err := app.store.PendingWebhooks("testtoken")
	if err != nil {
		t.Fatalf("pending webhooks: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending count = %d, want 2 (dedup disabled keeps every delivery)", len(pending))
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
