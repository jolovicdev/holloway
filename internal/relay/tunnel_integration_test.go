package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/jolovicdev/holloway/internal/store"
	"github.com/jolovicdev/holloway/internal/tunnel"
)

func TestTunnelDeliversIncomingWebhookOverWebSocket(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	secret := app.createToken(t, "testtoken")
	httpServer := httptest.NewServer(app.Server)
	t.Cleanup(httpServer.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := dialTunnel(ctx, wsURL(httpServer.URL, "/tunnel/testtoken"), secret)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxWebhookBody + 1<<20)
	waitConnected(t, app.hub, "testtoken")

	done := make(chan tunnel.Request, 1)
	go func() {
		var req tunnel.Request
		if err := wsjson.Read(ctx, conn, &req); err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		done <- req
		_ = wsjson.Write(ctx, conn, tunnel.Response{
			Type:       tunnel.MessageResponse,
			ID:         req.ID,
			StatusCode: http.StatusNoContent,
		})
	}()

	resp, err := http.Post(httpServer.URL+"/hook/testtoken/orders", "application/json", strings.NewReader(`{"id":1}`))
	if err != nil {
		t.Fatalf("post webhook: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (local response passed through)", resp.StatusCode)
	}

	select {
	case req := <-done:
		if req.Path != "/orders" {
			t.Fatalf("path = %q, want /orders", req.Path)
		}
		if string(req.Body) != `{"id":1}` {
			t.Fatalf("body = %q", string(req.Body))
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for tunneled request")
	}

	webhooks, err := app.store.LastWebhooks(1)
	if err != nil {
		t.Fatalf("last webhooks: %v", err)
	}
	if len(webhooks) != 1 || webhooks[0].StatusCode != http.StatusNoContent {
		t.Fatalf("stored webhooks = %#v", webhooks)
	}
}

func TestTunnelDrainsPendingWebhookOnConnect(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	secret := app.createToken(t, "testtoken")
	if err := app.store.SaveWebhook(store.Webhook{
		ID:      "wh_pending",
		TokenID: "testtoken",
		Method:  http.MethodPost,
		Path:    "/offline",
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Body:    `{"offline":true}`,
	}); err != nil {
		t.Fatalf("save webhook: %v", err)
	}
	httpServer := httptest.NewServer(app.Server)
	t.Cleanup(httpServer.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := dialTunnel(ctx, wsURL(httpServer.URL, "/tunnel/testtoken"), secret)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxWebhookBody + 1<<20)

	var req tunnel.Request
	if err := wsjson.Read(ctx, conn, &req); err != nil {
		t.Fatalf("read pending request: %v", err)
	}
	if req.Path != "/offline" {
		t.Fatalf("request = %#v", req)
	}
	if err := wsjson.Write(ctx, conn, tunnel.Response{
		Type:       tunnel.MessageResponse,
		ID:         req.ID,
		StatusCode: http.StatusAccepted,
	}); err != nil {
		t.Fatalf("write response: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		pending, err := app.store.PendingWebhooks("testtoken")
		if err != nil {
			t.Fatalf("pending webhooks: %v", err)
		}
		if len(pending) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending webhooks were not drained: %#v", pending)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTunnelReplayDoesNotDuplicatePendingWebhook(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	secret := app.createToken(t, "testtoken")
	if err := app.store.SaveWebhook(store.Webhook{
		ID:      "wh_pending",
		TokenID: "testtoken",
		Method:  http.MethodPost,
		Path:    "/offline",
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Body:    `{"offline":true}`,
	}); err != nil {
		t.Fatalf("save webhook: %v", err)
	}
	httpServer := httptest.NewServer(app.Server)
	t.Cleanup(httpServer.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := dialTunnel(ctx, wsURL(httpServer.URL, "/tunnel/testtoken?replay=1"), secret)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxWebhookBody + 1<<20)

	var req tunnel.Request
	if err := wsjson.Read(ctx, conn, &req); err != nil {
		t.Fatalf("read pending request: %v", err)
	}
	if req.Path != "/offline" || req.Replay {
		t.Fatalf("request = %#v, want pending drain without replay flag", req)
	}
	if err := wsjson.Write(ctx, conn, tunnel.Response{
		Type:       tunnel.MessageResponse,
		ID:         req.ID,
		StatusCode: http.StatusAccepted,
	}); err != nil {
		t.Fatalf("write response: %v", err)
	}

	readCtx, cancelRead := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelRead()
	var duplicate tunnel.Request
	if err := wsjson.Read(readCtx, conn, &duplicate); err == nil {
		t.Fatalf("got duplicate request: %#v", duplicate)
	}
}

func TestTunnelRejectsWebhookTokenWithoutTunnelSecret(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	app.createToken(t, "testtoken")
	if err := app.store.SaveWebhook(store.Webhook{
		ID:      "wh_pending",
		TokenID: "testtoken",
		Method:  http.MethodPost,
		Path:    "/offline",
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Body:    `{"offline":true}`,
	}); err != nil {
		t.Fatalf("save webhook: %v", err)
	}
	httpServer := httptest.NewServer(app.Server)
	t.Cleanup(httpServer.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL(httpServer.URL, "/tunnel/testtoken"), nil)
	if err == nil {
		conn.CloseNow()
		t.Fatal("dial succeeded without tunnel secret")
	}

	pending, err := app.store.PendingWebhooks("testtoken")
	if err != nil {
		t.Fatalf("pending webhooks: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending count = %d, want 1", len(pending))
	}
}

func dialTunnel(ctx context.Context, url string, secret string) (*websocket.Conn, *http.Response, error) {
	return websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + secret}},
	})
}

func wsURL(base, path string) string {
	return "ws" + strings.TrimPrefix(base, "http") + path
}

func waitConnected(t *testing.T, hub *Hub, token string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if hub.Connected(token) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("tunnel did not register")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
