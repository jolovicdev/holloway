package relay

import (
	"errors"
	"net/http"
	"time"

	"github.com/jolovicdev/holloway/internal/store"
)

const maxWebhookBody = 32 << 20
const maxTunnelResponseMessage = 2 << 20

const periodicDrainInterval = 10 * time.Second

// periodicDrainMinAge keeps the periodic drain from racing a live forward still
// in flight for the same row: the live path saves the webhook as pending before
// forwarding (30s timeout), so anything still pending past this age is genuinely
// stuck rather than mid-delivery. Reconnect drains pending immediately
// regardless of age; this is only the staying-connected safety net.
const periodicDrainMinAge = time.Minute

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
	Dedup              bool
	Events             EventPublisher
	WebhookLimiter     WebhookLimiter
}

type Server struct {
	store              *store.Store
	hub                *Hub
	adminPassword      string
	allowInsecureAdmin bool
	dedup              bool
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
		dedup:              config.Dedup,
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

func (s *Server) publish(webhook store.Webhook) {
	if s.events != nil {
		s.events.Publish(webhook)
	}
}
