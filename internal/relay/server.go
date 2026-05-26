package relay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/jolovicdev/holloway/internal/store"
	"github.com/jolovicdev/holloway/internal/tunnel"
)

const maxWebhookBody = 32 << 20
const maxTunnelResponseMessage = 2 << 20

var ErrNoClient = errors.New("no client connected")
var ErrDeliveryFailed = errors.New("delivery failed")

type EventPublisher interface {
	Publish(store.Webhook)
}

type Config struct {
	Store              *store.Store
	Hub                *Hub
	AdminPassword      string
	AllowInsecureAdmin bool
	Events             EventPublisher
	WebhookLimiter     WebhookLimiter
}

type Server struct {
	store              *store.Store
	hub                *Hub
	adminPassword      string
	allowInsecureAdmin bool
	events             EventPublisher
	webhookLimiter     WebhookLimiter
	mux                *http.ServeMux
}

func NewServer(config Config) *Server {
	webhookLimiter := config.WebhookLimiter
	if webhookLimiter == nil {
		webhookLimiter = NewWebhookRateLimiter(DefaultWebhookRateLimitPerMinute, time.Minute)
	}
	server := &Server{
		store:              config.Store,
		hub:                config.Hub,
		adminPassword:      config.AdminPassword,
		allowInsecureAdmin: config.AllowInsecureAdmin,
		events:             config.Events,
		webhookLimiter:     webhookLimiter,
		mux:                http.NewServeMux(),
	}
	if server.hub == nil {
		server.hub = NewHub()
	}
	server.routes()
	return server
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/hook/", s.handleHook)
	s.mux.HandleFunc("/tunnel/", s.handleTunnel)
	s.mux.HandleFunc("/tokens", s.requireAdmin(s.handleTokens))
	s.mux.HandleFunc("/tokens/", s.requireAdmin(s.handleTokenAction))
}

func (s *Server) MountDashboard(handler http.Handler) {
	wrapped := s.requireAdminHandler(handler)
	s.mux.Handle("/dashboard", wrapped)
	s.mux.Handle("/dashboard/", wrapped)
	s.mux.Handle("/static/", wrapped)
}

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
	if err := s.store.SaveWebhook(webhook); err != nil {
		http.Error(w, "save webhook", http.StatusInternalServerError)
		return
	}
	s.publish(webhook)

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
	if !delivered {
		http.Error(w, ErrNoClient.Error(), http.StatusBadGateway)
		return
	}
	if resp.StatusCode == 0 {
		http.Error(w, ErrDeliveryFailed.Error(), http.StatusBadGateway)
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

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("delivered\n"))
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
	go s.drainOnConnect(drainCtx, tokenID, client, replayLimit)

	client.Wait()
}

func (s *Server) drainOnConnect(ctx context.Context, tokenID string, client Client, replayLimit int) {
	pending, err := s.store.PendingWebhooks(tokenID)
	delivered := make(map[string]struct{}, len(pending))
	if err == nil {
		for _, webhook := range s.deliverStored(ctx, client, pending, false) {
			delivered[webhook.ID] = struct{}{}
		}
	}

	if replayLimit > 0 {
		replays, err := s.store.LastWebhooksForTokenExcluding(tokenID, replayLimit, delivered)
		if err == nil {
			s.deliverStored(ctx, client, replays, true)
		}
	}
}

func (s *Server) deliverStored(ctx context.Context, client Client, webhooks []store.Webhook, replay bool) []store.Webhook {
	delivered := make([]store.Webhook, 0, len(webhooks))
	for _, webhook := range webhooks {
		requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		resp, ok := sendStored(requestCtx, client, webhook, replay)
		cancel()
		if !ok {
			return delivered
		}
		if resp.StatusCode == 0 {
			return delivered
		}
		if err := s.store.MarkDelivered(webhook.ID, resp.StatusCode, resp.Body); err == nil {
			webhook.StatusCode = resp.StatusCode
			webhook.ResponseBody = resp.Body
			webhook.DeliveredAt = time.Now().UTC()
			delivered = append(delivered, webhook)
			s.publish(webhook)
		}
	}
	return delivered
}

func (s *Server) ReplayWebhook(ctx context.Context, id string) (int, error) {
	webhook, err := s.store.GetWebhook(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		return 0, fmt.Errorf("get webhook: %w", err)
	}

	requestCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, ok := s.hub.Forward(requestCtx, webhook.TokenID, tunnel.Request{
		ID:         webhook.ID,
		Method:     webhook.Method,
		Path:       webhook.Path,
		Headers:    webhook.Headers,
		Body:       []byte(webhook.Body),
		Replay:     true,
		ReceivedAt: webhook.ReceivedAt,
	})
	if !ok {
		return 0, ErrNoClient
	}
	if resp.StatusCode == 0 {
		return 0, ErrDeliveryFailed
	}
	if err := s.store.MarkDelivered(webhook.ID, resp.StatusCode, resp.Body); err != nil {
		return 0, err
	}
	webhook.StatusCode = resp.StatusCode
	webhook.ResponseBody = resp.Body
	webhook.DeliveredAt = time.Now().UTC()
	s.publish(webhook)
	return resp.StatusCode, nil
}

func sendStored(ctx context.Context, client Client, webhook store.Webhook, replay bool) (tunnel.Response, bool) {
	resp, err := client.Send(ctx, tunnel.Request{
		ID:         webhook.ID,
		Method:     webhook.Method,
		Path:       webhook.Path,
		Headers:    webhook.Headers,
		Body:       []byte(webhook.Body),
		Replay:     replay,
		ReceivedAt: webhook.ReceivedAt,
	})
	return resp, err == nil
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

func (s *Server) validToken(w http.ResponseWriter, tokenID string) bool {
	exists, err := s.store.TokenExists(tokenID)
	if err != nil {
		http.Error(w, "check token", http.StatusInternalServerError)
		return false
	}
	if !exists {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) validTunnel(w http.ResponseWriter, r *http.Request, tokenID string) bool {
	secret := bearerToken(r.Header.Get("Authorization"))
	if secret == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}

	matches, err := s.store.TokenMatchesTunnelSecret(tokenID, secret)
	if err != nil {
		http.Error(w, "check tunnel token", http.StatusInternalServerError)
		return false
	}
	if !matches {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) allowWebhook(w http.ResponseWriter, tokenID string) bool {
	if s.webhookLimiter == nil {
		return true
	}

	allowed, retryAfter := s.webhookLimiter.Allow(tokenID)
	if allowed {
		return true
	}
	if retryAfter > 0 {
		seconds := int((retryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
	}
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
	return false
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOriginForUnsafeMethod(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if s.adminPassword == "" {
			if s.allowInsecureAdmin {
				next(w, r)
				return
			}
			http.Error(w, "admin auth not configured", http.StatusServiceUnavailable)
			return
		}
		_, password, ok := r.BasicAuth()
		if !ok || !constantTimeEqual(password, s.adminPassword) {
			w.Header().Set("WWW-Authenticate", `Basic realm="Holloway"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func bearerToken(header string) string {
	scheme, value, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}

func (s *Server) requireAdminHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requireAdmin(next.ServeHTTP)(w, r)
	})
}

func sameOriginForUnsafeMethod(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		return originMatchesHost(origin, r.Host)
	}
	if referer := r.Header.Get("Referer"); referer != "" {
		return originMatchesHost(referer, r.Host)
	}
	return false
}

func originMatchesHost(rawOrigin, host string) bool {
	parsed, err := url.Parse(rawOrigin)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, host)
}

func constantTimeEqual(got, want string) bool {
	gotHash := sha256.Sum256([]byte(got))
	wantHash := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotHash[:], wantHash[:]) == 1
}

func (s *Server) publish(webhook store.Webhook) {
	if s.events != nil {
		s.events.Publish(webhook)
	}
}

func parseTokenPath(path, prefix string) (token string, forwardPath string, ok bool) {
	rest := strings.TrimPrefix(path, prefix)
	if rest == path || rest == "" {
		return "", "", false
	}
	token, suffix, _ := strings.Cut(rest, "/")
	if token == "" {
		return "", "", false
	}
	if suffix == "" {
		return token, "/", true
	}
	return token, "/" + suffix, true
}

func cloneHeader(header http.Header) map[string][]string {
	clone := make(map[string][]string, len(header))
	for key, values := range header {
		clone[key] = append([]string(nil), values...)
	}
	return clone
}

func newID(prefix string) (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(bytes[:]), nil
}

func acceptsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
