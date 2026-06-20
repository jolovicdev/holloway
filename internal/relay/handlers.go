package relay

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/jolovicdev/holloway/internal/store"
	"github.com/jolovicdev/holloway/internal/tunnel"
)

func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	tokenID, forwardPath, ok := parseTokenPath(r.URL.Path, "/hook/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !s.validToken(w, tokenID) || !s.allowWebhook(w, tokenID) {
		return
	}
	if r.URL.RawQuery != "" {
		forwardPath += "?" + r.URL.RawQuery
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "read webhook body", http.StatusBadRequest)
		return
	}

	receivedAt := time.Now().UTC()
	id, err := newID("wh_")
	if err != nil {
		http.Error(w, "generate webhook id", http.StatusInternalServerError)
		return
	}

	webhook := store.Webhook{
		ID:         id,
		TokenID:    tokenID,
		Method:     r.Method,
		Path:       forwardPath,
		Headers:    cloneHeader(r.Header),
		Body:       string(body),
		ReceivedAt: receivedAt,
	}
	if s.dedup {
		webhook.DedupKey = dedupKey(r.Method, forwardPath, body)
	}
	if err := s.store.SaveWebhook(webhook); err != nil {
		if errors.Is(err, store.ErrDuplicateWebhook) {
			s.respondDuplicate(w, tokenID, webhook.DedupKey)
			return
		}
		http.Error(w, "save webhook", http.StatusInternalServerError)
		return
	}
	s.publish(webhook)

	// Hybrid delivery: when a client is connected, forward live so the provider
	// sees the real local response. Otherwise (offline, or connected but the
	// local app is unreachable) the webhook stays pending and is accepted
	// durably; the drain worker delivers it on reconnect or on a periodic tick.
	// Failing the provider here instead would invite retries that duplicate the
	// pending row.
	if !s.hub.Connected(tokenID) {
		acceptPending(w)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, delivered := s.hub.Forward(ctx, tokenID, tunnel.Request{
		ID:         id,
		Method:     r.Method,
		Path:       forwardPath,
		Headers:    cloneHeader(r.Header),
		Body:       body,
		ReceivedAt: receivedAt,
	})
	if !delivered || resp.StatusCode == 0 {
		acceptPending(w)
		return
	}

	if err := s.store.MarkDelivered(id, resp.StatusCode, resp.Body); err != nil {
		http.Error(w, "mark delivered", http.StatusInternalServerError)
		return
	}
	webhook.StatusCode = resp.StatusCode
	webhook.ResponseBody = resp.Body
	webhook.DeliveredAt = time.Now().UTC()
	s.publish(webhook)

	w.WriteHeader(resp.StatusCode)
	_, _ = io.WriteString(w, resp.Body)
}

func acceptPending(w http.ResponseWriter) {
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, "accepted\n")
}

// respondDuplicate replays the answer the original delivery already got: its
// real status and body if delivered, or 202 if it is still queued. The
// duplicate is never enqueued or forwarded again.
func (s *Server) respondDuplicate(w http.ResponseWriter, tokenID, key string) {
	original, err := s.store.WebhookByDedupKey(tokenID, key)
	if err != nil || original.StatusCode == 0 {
		acceptPending(w)
		return
	}
	w.WriteHeader(original.StatusCode)
	_, _ = io.WriteString(w, original.ResponseBody)
}

func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	tokenID, _, ok := parseTokenPath(r.URL.Path, "/tunnel/")
	if !ok || !s.validTunnel(w, r, tokenID) {
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(maxTunnelResponseMessage)

	client := NewWSClient(conn)
	unregister := s.hub.Register(tokenID, client)
	defer unregister()
	defer client.Close()

	replayLimit, _ := strconv.Atoi(r.URL.Query().Get("replay"))
	drainCtx, cancelDrain := context.WithCancel(context.Background())
	defer cancelDrain()
	go func() {
		client.Wait()
		cancelDrain()
	}()
	go s.drainLoop(drainCtx, tokenID, client, replayLimit)

	client.Wait()
}

func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	tokenID, err := newID("tok_")
	if err != nil {
		http.Error(w, "generate token", http.StatusInternalServerError)
		return
	}
	if name == "" {
		name = tokenID
	}
	tunnelSecret, err := s.store.CreateToken(tokenID, name)
	if err != nil {
		http.Error(w, "create token", http.StatusInternalServerError)
		return
	}

	if acceptsHTML(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Token created</title></head>
<body>
<h1>Token created</h1>
<p>Webhook token: <code>%s</code></p>
<p>Tunnel secret: <code>%s</code></p>
<p>Save the tunnel secret now. It is not stored in plaintext.</p>
<p><a href="/dashboard">Back to dashboard</a></p>
</body>
</html>`, html.EscapeString(tokenID), html.EscapeString(tunnelSecret))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":%q,"name":%q,"tunnel_secret":%q}`+"\n", tokenID, name, tunnelSecret)
}

func (s *Server) handleTokenAction(w http.ResponseWriter, r *http.Request) {
	tokenID := strings.TrimPrefix(r.URL.Path, "/tokens/")
	tokenID = strings.TrimSuffix(tokenID, "/delete")
	if tokenID == "" || !strings.HasSuffix(r.URL.Path, "/delete") || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteToken(tokenID); err != nil {
		http.Error(w, "delete token", http.StatusInternalServerError)
		return
	}
	s.hub.Disconnect(tokenID)
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}
