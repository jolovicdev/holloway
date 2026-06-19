package relay

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jolovicdev/holloway/internal/store"
	"github.com/jolovicdev/holloway/internal/tunnel"
)

func (s *Server) drainLoop(ctx context.Context, tokenID string, client Client, replayLimit int) {
	s.drainOnConnect(ctx, tokenID, client, replayLimit)

	ticker := time.NewTicker(periodicDrainInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pending, err := s.store.PendingWebhooks(tokenID)
			if err != nil {
				continue
			}
			if stale := pendingOlderThan(pending, periodicDrainMinAge); len(stale) > 0 {
				s.deliverStored(ctx, client, stale, false)
			}
		}
	}
}

func pendingOlderThan(webhooks []store.Webhook, minAge time.Duration) []store.Webhook {
	cutoff := time.Now().Add(-minAge)
	stale := webhooks[:0]
	for _, webhook := range webhooks {
		if webhook.ReceivedAt.Before(cutoff) {
			stale = append(stale, webhook)
		}
	}
	return stale
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
