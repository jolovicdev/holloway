package relay

import (
	"context"
	"sync"

	"github.com/jolovicdev/holloway/internal/tunnel"
)

type Client interface {
	Send(context.Context, tunnel.Request) (tunnel.Response, error)
	Close() error
}

type Hub struct {
	mu      sync.RWMutex
	clients map[string]Client
}

func NewHub() *Hub {
	return &Hub{clients: make(map[string]Client)}
}

func (h *Hub) Register(token string, client Client) func() {
	h.mu.Lock()
	old := h.clients[token]
	h.clients[token] = client
	h.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}

	return func() {
		h.mu.Lock()
		if h.clients[token] == client {
			delete(h.clients, token)
		}
		h.mu.Unlock()
	}
}

func (h *Hub) Forward(ctx context.Context, token string, req tunnel.Request) (tunnel.Response, bool) {
	h.mu.RLock()
	client := h.clients[token]
	h.mu.RUnlock()

	if client == nil {
		return tunnel.Response{}, false
	}

	resp, err := client.Send(ctx, req)
	if err != nil {
		return tunnel.Response{}, false
	}
	return resp, true
}

func (h *Hub) Disconnect(token string) {
	h.mu.Lock()
	client := h.clients[token]
	delete(h.clients, token)
	h.mu.Unlock()

	if client != nil {
		_ = client.Close()
	}
}

func (h *Hub) Connected(token string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.clients[token] != nil
}
