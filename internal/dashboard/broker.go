package dashboard

import (
	"sync"

	"github.com/jolovicdev/holloway/internal/store"
)

type Broker struct {
	mu      sync.Mutex
	clients map[chan store.Webhook]struct{}
}

func NewBroker() *Broker {
	return &Broker{clients: make(map[chan store.Webhook]struct{})}
}

func (b *Broker) Publish(webhook store.Webhook) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- webhook:
		default:
		}
	}
}

func (b *Broker) Subscribe() (<-chan store.Webhook, func()) {
	ch := make(chan store.Webhook, 16)

	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		delete(b.clients, ch)
		close(ch)
		b.mu.Unlock()
	}
}
