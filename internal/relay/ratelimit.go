package relay

import (
	"sync"
	"time"
)

const DefaultWebhookRateLimitPerMinute = 300

type WebhookLimiter interface {
	Allow(key string) (bool, time.Duration)
}

type WebhookRateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string]rateWindow
}

type rateWindow struct {
	start    time.Time
	count    int
	lastSeen time.Time
}

func NewWebhookRateLimiter(limit int, window time.Duration) *WebhookRateLimiter {
	if limit <= 0 {
		limit = 1
	}
	if window <= 0 {
		window = time.Minute
	}
	return &WebhookRateLimiter{
		limit:   limit,
		window:  window,
		entries: make(map[string]rateWindow),
	}
}

func (l *WebhookRateLimiter) Allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}

	now := time.Now().UTC()

	l.mu.Lock()
	defer l.mu.Unlock()

	entry := l.entries[key]
	if entry.start.IsZero() || now.Sub(entry.start) >= l.window {
		entry = rateWindow{start: now}
	}
	entry.lastSeen = now

	if entry.count >= l.limit {
		l.entries[key] = entry
		l.pruneLocked(now)
		return false, max(entry.start.Add(l.window).Sub(now), 0)
	}

	entry.count++
	l.entries[key] = entry
	l.pruneLocked(now)
	return true, 0
}

func (l *WebhookRateLimiter) pruneLocked(now time.Time) {
	cutoff := now.Add(-2 * l.window)
	for key, entry := range l.entries {
		if entry.lastSeen.Before(cutoff) {
			delete(l.entries, key)
		}
	}
}
