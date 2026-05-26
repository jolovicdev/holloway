package dashboard

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jolovicdev/holloway/internal/store"
)

func TestDashboardFiltersWebhookList(t *testing.T) {
	t.Parallel()

	s, err := store.Open(t.TempDir() + "/holloway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	if _, err := s.CreateToken("tok_one", "One"); err != nil {
		t.Fatalf("create token: %v", err)
	}
	receivedAt := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	webhooks := []store.Webhook{
		{
			ID:         "wh_pending",
			TokenID:    "tok_one",
			Method:     http.MethodPost,
			Path:       "/orders/new",
			Headers:    map[string][]string{},
			Body:       `{"ok":true}`,
			ReceivedAt: receivedAt,
		},
		{
			ID:         "wh_delivered",
			TokenID:    "tok_one",
			Method:     http.MethodPost,
			Path:       "/accounts/new",
			Headers:    map[string][]string{},
			Body:       `{"ok":true}`,
			StatusCode: 200,
			ReceivedAt: receivedAt,
		},
	}
	for _, webhook := range webhooks {
		if err := s.SaveWebhook(webhook); err != nil {
			t.Fatalf("save webhook %s: %v", webhook.ID, err)
		}
	}

	templateDir, err := filepath.Abs("../../templates")
	if err != nil {
		t.Fatalf("template dir: %v", err)
	}
	staticDir, err := filepath.Abs("../../static")
	if err != nil {
		t.Fatalf("static dir: %v", err)
	}
	handler, err := New(s, fakeReplayer{}, NewBroker(), templateDir, staticDir)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/dashboard?path=/orders&status=pending&from=2026-05-26&to=2026-05-26", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	bodyBytes, _ := io.ReadAll(rec.Body)
	body := string(bodyBytes)

	for _, want := range []string{`name="q"`, `name="path"`, `name="status"`, `name="from"`, `name="to"`, `/orders/new`} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard body missing %q", want)
		}
	}
	if strings.Contains(body, "/accounts/new") {
		t.Fatal("dashboard included webhook outside filters")
	}
}

func TestDashboardUsesLocalAssets(t *testing.T) {
	t.Parallel()

	s, err := store.Open(t.TempDir() + "/holloway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	templateDir, err := filepath.Abs("../../templates")
	if err != nil {
		t.Fatalf("template dir: %v", err)
	}
	staticDir, err := filepath.Abs("../../static")
	if err != nil {
		t.Fatalf("static dir: %v", err)
	}
	handler, err := New(s, fakeReplayer{}, NewBroker(), templateDir, staticDir)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	bodyBytes, _ := io.ReadAll(rec.Body)
	body := string(bodyBytes)

	for _, want := range []string{`href="/static/tailwind.css"`, `src="/static/htmx.min.js"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard body missing %q", want)
		}
	}
	if strings.Contains(body, "https://") {
		t.Fatal("dashboard includes a remote asset URL")
	}
}

func TestDashboardServesLocalAssets(t *testing.T) {
	t.Parallel()

	s, err := store.Open(t.TempDir() + "/holloway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	templateDir, err := filepath.Abs("../../templates")
	if err != nil {
		t.Fatalf("template dir: %v", err)
	}
	staticDir, err := filepath.Abs("../../static")
	if err != nil {
		t.Fatalf("static dir: %v", err)
	}
	handler, err := New(s, fakeReplayer{}, NewBroker(), templateDir, staticDir)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	for _, path := range []string{"/static/tailwind.css", "/static/htmx.min.js"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, rec.Code)
		}
		bodyBytes, _ := io.ReadAll(rec.Body)
		if len(bodyBytes) == 0 {
			t.Fatalf("%s returned an empty body", path)
		}
	}
}

type fakeReplayer struct{}

func (fakeReplayer) ReplayWebhook(context.Context, string) (int, error) {
	return 0, nil
}
