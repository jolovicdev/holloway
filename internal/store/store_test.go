package store

import (
	"testing"
	"time"
)

func TestStorePersistsPendingWebhookThenMarksDelivered(t *testing.T) {
	t.Parallel()

	s, err := Open(t.TempDir() + "/holloway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	if _, err := s.CreateToken("testtoken", "Test token"); err != nil {
		t.Fatalf("create token: %v", err)
	}

	webhook := Webhook{
		ID:      "req_1",
		TokenID: "testtoken",
		Method:  "POST",
		Path:    "/orders?source=test",
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Body:    `{"ok":true}`,
	}
	if err := s.SaveWebhook(webhook); err != nil {
		t.Fatalf("save webhook: %v", err)
	}

	pending, err := s.PendingWebhooks("testtoken")
	if err != nil {
		t.Fatalf("pending webhooks: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending count = %d, want 1", len(pending))
	}
	if pending[0].StatusCode != 0 {
		t.Fatalf("pending status = %d, want 0", pending[0].StatusCode)
	}

	if err := s.MarkDelivered("req_1", 204, "created"); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}

	pending, err = s.PendingWebhooks("testtoken")
	if err != nil {
		t.Fatalf("pending after delivery: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after delivery = %d, want 0", len(pending))
	}

	got, err := s.GetWebhook("req_1")
	if err != nil {
		t.Fatalf("get webhook: %v", err)
	}
	if got.StatusCode != 204 {
		t.Fatalf("delivered status = %d, want 204", got.StatusCode)
	}
	if got.ResponseBody != "created" {
		t.Fatalf("response body = %q, want created", got.ResponseBody)
	}
	if got.DeliveredAt.IsZero() {
		t.Fatal("delivered_at was not set")
	}
}

func TestStoreTokenLifecycle(t *testing.T) {
	t.Parallel()

	s, err := Open(t.TempDir() + "/holloway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	exists, err := s.TokenExists("abc")
	if err != nil {
		t.Fatalf("token exists before create: %v", err)
	}
	if exists {
		t.Fatal("token exists before create")
	}

	secret, err := s.CreateToken("abc", "Local dev")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if secret == "" {
		t.Fatal("create token returned empty tunnel secret")
	}
	exists, err = s.TokenExists("abc")
	if err != nil {
		t.Fatalf("token exists after create: %v", err)
	}
	if !exists {
		t.Fatal("token missing after create")
	}
	matches, err := s.TokenMatchesTunnelSecret("abc", "abc")
	if err != nil {
		t.Fatalf("match webhook token as tunnel secret: %v", err)
	}
	if matches {
		t.Fatal("webhook token authenticated as tunnel secret")
	}
	matches, err = s.TokenMatchesTunnelSecret("abc", secret)
	if err != nil {
		t.Fatalf("match tunnel secret: %v", err)
	}
	if !matches {
		t.Fatal("generated tunnel secret did not authenticate")
	}

	if err := s.DeleteToken("abc"); err != nil {
		t.Fatalf("delete token: %v", err)
	}
	exists, err = s.TokenExists("abc")
	if err != nil {
		t.Fatalf("token exists after delete: %v", err)
	}
	if exists {
		t.Fatal("token still exists after delete")
	}
}

func TestStoreListsWebhooksWithFiltersAndPagination(t *testing.T) {
	t.Parallel()

	s, err := Open(t.TempDir() + "/holloway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	base := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	webhooks := []Webhook{
		{
			ID:         "wh_old",
			TokenID:    "tok_one",
			Method:     "POST",
			Path:       "/orders/old",
			Headers:    map[string][]string{},
			Body:       `{"kind":"old"}`,
			StatusCode: 200,
			ReceivedAt: base.Add(-48 * time.Hour),
		},
		{
			ID:         "wh_pending",
			TokenID:    "tok_one",
			Method:     "POST",
			Path:       "/orders/new",
			Headers:    map[string][]string{},
			Body:       `{"kind":"pending"}`,
			StatusCode: 0,
			ReceivedAt: base,
		},
		{
			ID:         "wh_failed",
			TokenID:    "tok_two",
			Method:     "PUT",
			Path:       "/accounts/new",
			Headers:    map[string][]string{},
			Body:       `{"kind":"failed"}`,
			StatusCode: 502,
			ReceivedAt: base.Add(time.Hour),
		},
	}
	for _, webhook := range webhooks {
		if err := s.SaveWebhook(webhook); err != nil {
			t.Fatalf("save webhook %s: %v", webhook.ID, err)
		}
	}

	page, err := s.ListWebhooks(WebhookQuery{
		Search:       "orders",
		PathContains: "/new",
		Status:       WebhookStatusPending,
		ReceivedFrom: base.Add(-time.Hour),
		ReceivedTo:   base.Add(time.Hour),
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("list filtered webhooks: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("filtered total = %d, want 1", page.Total)
	}
	if len(page.Webhooks) != 1 || page.Webhooks[0].ID != "wh_pending" {
		t.Fatalf("filtered webhooks = %#v, want wh_pending", page.Webhooks)
	}

	page, err = s.ListWebhooks(WebhookQuery{
		Status: WebhookStatusDelivered,
		Limit:  1,
		Offset: 1,
	})
	if err != nil {
		t.Fatalf("list paginated webhooks: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("delivered total = %d, want 2", page.Total)
	}
	if len(page.Webhooks) != 1 || page.Webhooks[0].ID != "wh_old" {
		t.Fatalf("paginated webhooks = %#v, want wh_old", page.Webhooks)
	}
}

func TestStoreOrdersWebhookTimesWithNanoseconds(t *testing.T) {
	t.Parallel()

	s, err := Open(t.TempDir() + "/holloway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	if _, err := s.CreateToken("testtoken", "Test token"); err != nil {
		t.Fatalf("create token: %v", err)
	}

	base := time.Date(2026, 5, 26, 7, 30, 0, 0, time.UTC)
	webhooks := []Webhook{
		{
			ID:         "wh_zero",
			TokenID:    "testtoken",
			Method:     "POST",
			Path:       "/zero",
			Headers:    map[string][]string{},
			ReceivedAt: base,
		},
		{
			ID:         "wh_nano",
			TokenID:    "testtoken",
			Method:     "POST",
			Path:       "/nano",
			Headers:    map[string][]string{},
			ReceivedAt: base.Add(time.Nanosecond),
		},
	}
	for _, webhook := range webhooks {
		if err := s.SaveWebhook(webhook); err != nil {
			t.Fatalf("save webhook %s: %v", webhook.ID, err)
		}
	}

	page, err := s.ListWebhooks(WebhookQuery{Limit: 2})
	if err != nil {
		t.Fatalf("list webhooks: %v", err)
	}
	if len(page.Webhooks) != 2 {
		t.Fatalf("webhook count = %d, want 2", len(page.Webhooks))
	}
	if page.Webhooks[0].ID != "wh_nano" || page.Webhooks[1].ID != "wh_zero" {
		t.Fatalf("webhook order = %s, %s; want wh_nano, wh_zero", page.Webhooks[0].ID, page.Webhooks[1].ID)
	}
}

func TestStoreRetentionDeletes(t *testing.T) {
	t.Parallel()

	s, err := Open(t.TempDir() + "/holloway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	if _, err := s.CreateToken("tok", "Test token"); err != nil {
		t.Fatalf("create token: %v", err)
	}

	now := time.Now().UTC()
	save := func(id string, age time.Duration) {
		if err := s.SaveWebhook(Webhook{
			ID:         id,
			TokenID:    "tok",
			Method:     "POST",
			Path:       "/x",
			Body:       "{}",
			ReceivedAt: now.Add(-age),
		}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}

	save("wh_old", 48*time.Hour)
	save("wh_recent", time.Hour)

	removed, err := s.DeleteWebhooksOlderThan(now.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("delete by age: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed by age = %d, want 1", removed)
	}
	if _, err := s.GetWebhook("wh_old"); err == nil {
		t.Fatal("wh_old should have been deleted")
	}
	if _, err := s.GetWebhook("wh_recent"); err != nil {
		t.Fatalf("wh_recent should remain: %v", err)
	}

	// Cap to the single most recent row per token; wh_recent is older than the
	// two below, so it should be evicted.
	save("wh_a", 30*time.Minute)
	save("wh_b", 10*time.Minute)
	removed, err = s.DeleteWebhooksOverCountPerToken(1)
	if err != nil {
		t.Fatalf("delete by count: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed by count = %d, want 2", removed)
	}
	page, err := s.ListWebhooks(WebhookQuery{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Total != 1 || page.Webhooks[0].ID != "wh_b" {
		t.Fatalf("remaining = %#v, want only wh_b", page.Webhooks)
	}
}

func TestStoreUsesSingleSQLiteConnection(t *testing.T) {
	t.Parallel()

	s, err := Open(t.TempDir() + "/holloway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("max open connections = %d, want 1", got)
	}
}
