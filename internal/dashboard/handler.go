package dashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jolovicdev/holloway/internal/relay"
	"github.com/jolovicdev/holloway/internal/store"
)

type Replayer interface {
	ReplayWebhook(context.Context, string) (int, error)
}

type Handler struct {
	store    *store.Store
	replayer Replayer
	broker   *Broker
	static   http.Handler
	views    *template.Template
}

func New(store *store.Store, replayer Replayer, broker *Broker, templateDir, staticDir string) (*Handler, error) {
	views, err := template.New("").Funcs(template.FuncMap{
		"statusText": statusText,
		"shortID":    shortID,
		"timeText":   timeText,
		"join":       strings.Join,
	}).ParseGlob(filepath.Join(templateDir, "*.html"))
	if err != nil {
		return nil, err
	}

	return &Handler{
		store:    store,
		replayer: replayer,
		broker:   broker,
		static:   http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir))),
		views:    views,
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/static/"):
		h.static.ServeHTTP(w, r)
	case r.URL.Path == "/dashboard":
		h.showDashboard(w, r)
	case r.URL.Path == "/dashboard/events":
		h.events(w, r)
	case strings.HasPrefix(r.URL.Path, "/dashboard/webhooks/"):
		h.showWebhook(w, r)
	case strings.HasPrefix(r.URL.Path, "/dashboard/replay/"):
		h.replayWebhook(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) showDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query, filters := parseWebhookQuery(r)
	page, err := h.store.ListWebhooks(query)
	if err != nil {
		http.Error(w, "load webhooks", http.StatusInternalServerError)
		return
	}
	tokens, err := h.store.ListTokens()
	if err != nil {
		http.Error(w, "load tokens", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pagination := newPagination(r, page.Total, page.Limit, page.Offset)
	_ = h.views.ExecuteTemplate(w, "dashboard.html", dashboardView{
		Webhooks:    page.Webhooks,
		Tokens:      tokens,
		Filters:     filters,
		Pagination:  pagination,
		LiveEnabled: !filters.Active && pagination.Page == 1,
	})
}

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", http.StatusInternalServerError)
		return
	}

	events, unsubscribe := h.broker.Subscribe()
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case webhook := <-events:
			data, err := json.Marshal(webhookSummary(webhook))
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: webhook\ndata: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (h *Handler) showWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/dashboard/webhooks/")
	webhook, err := h.store.GetWebhook(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "load webhook", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.views.ExecuteTemplate(w, "inspector.html", webhook)
}

func (h *Handler) replayWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/dashboard/replay/")
	status, err := h.replayer.ReplayWebhook(r.Context(), id)
	if err != nil {
		if errors.Is(err, relay.ErrNoClient) {
			http.Error(w, "No client connected", http.StatusBadGateway)
			return
		}
		if errors.Is(err, relay.ErrDeliveryFailed) {
			http.Error(w, "Local delivery failed", http.StatusBadGateway)
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Replay failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<span class="text-emerald-700">Replayed. Local status %d.</span>`, status)
}

type summary struct {
	ID         string `json:"id"`
	ShortID    string `json:"short_id"`
	TokenID    string `json:"token_id"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     string `json:"status"`
	StatusCode int    `json:"status_code"`
	ReceivedAt string `json:"received_at"`
}

func webhookSummary(webhook store.Webhook) summary {
	return summary{
		ID:         webhook.ID,
		ShortID:    shortID(webhook.ID),
		TokenID:    webhook.TokenID,
		Method:     webhook.Method,
		Path:       webhook.Path,
		Status:     statusText(webhook.StatusCode),
		StatusCode: webhook.StatusCode,
		ReceivedAt: timeText(webhook.ReceivedAt),
	}
}

func statusText(statusCode int) string {
	if statusCode == 0 {
		return "pending"
	}
	return strconv.Itoa(statusCode)
}

func shortID(id string) string {
	if len(id) <= 10 {
		return id
	}
	return id[:10]
}

func timeText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("Jan 02 15:04:05")
}

type dashboardView struct {
	Webhooks    []store.Webhook
	Tokens      []store.Token
	Filters     webhookFilters
	Pagination  pagination
	LiveEnabled bool
}

type webhookFilters struct {
	Search string
	Path   string
	Status string
	From   string
	To     string
	Active bool
}

type pagination struct {
	Page       int
	PageSize   int
	Total      int
	From       int
	To         int
	HasPrev    bool
	HasNext    bool
	PrevURL    string
	NextURL    string
	ResultText string
}

const dashboardPageSize = 50

func parseWebhookQuery(r *http.Request) (store.WebhookQuery, webhookFilters) {
	values := r.URL.Query()
	filters := webhookFilters{
		Search: strings.TrimSpace(values.Get("q")),
		Path:   strings.TrimSpace(values.Get("path")),
		Status: strings.TrimSpace(values.Get("status")),
		From:   strings.TrimSpace(values.Get("from")),
		To:     strings.TrimSpace(values.Get("to")),
	}
	filters.Active = filters.Search != "" || filters.Path != "" || filters.Status != "" || filters.From != "" || filters.To != ""

	page := 1
	if parsed, err := strconv.Atoi(values.Get("page")); err == nil && parsed > 0 {
		page = parsed
	}

	query := store.WebhookQuery{
		Search:       filters.Search,
		PathContains: filters.Path,
		Status:       parseStatusFilter(filters.Status),
		Limit:        dashboardPageSize,
		Offset:       (page - 1) * dashboardPageSize,
	}
	if from, ok := parseDate(filters.From); ok {
		query.ReceivedFrom = from
	}
	if to, ok := parseDate(filters.To); ok {
		query.ReceivedTo = to.AddDate(0, 0, 1)
	}
	return query, filters
}

func parseStatusFilter(value string) store.WebhookStatusFilter {
	switch value {
	case string(store.WebhookStatusPending):
		return store.WebhookStatusPending
	case string(store.WebhookStatusDelivered):
		return store.WebhookStatusDelivered
	case string(store.WebhookStatus2xx):
		return store.WebhookStatus2xx
	case string(store.WebhookStatus3xx):
		return store.WebhookStatus3xx
	case string(store.WebhookStatus4xx):
		return store.WebhookStatus4xx
	case string(store.WebhookStatus5xx):
		return store.WebhookStatus5xx
	default:
		return ""
	}
}

func parseDate(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.ParseInLocation("2006-01-02", value, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func newPagination(r *http.Request, total, limit, offset int) pagination {
	if limit <= 0 {
		limit = dashboardPageSize
	}
	page := offset/limit + 1
	resultFrom := 0
	resultTo := 0
	if total > 0 && offset < total {
		resultFrom = offset + 1
		resultTo = min(offset+limit, total)
	}

	p := pagination{
		Page:       page,
		PageSize:   limit,
		Total:      total,
		From:       resultFrom,
		To:         resultTo,
		HasPrev:    page > 1,
		HasNext:    offset+limit < total,
		ResultText: "No webhooks",
	}
	if total > 0 {
		p.ResultText = fmt.Sprintf("Showing %d-%d of %d", resultFrom, resultTo, total)
	}
	if p.HasPrev {
		p.PrevURL = pageURL(r, page-1)
	}
	if p.HasNext {
		p.NextURL = pageURL(r, page+1)
	}
	return p
}

func pageURL(r *http.Request, page int) string {
	values := r.URL.Query()
	if page <= 1 {
		values.Del("page")
	} else {
		values.Set("page", strconv.Itoa(page))
	}
	if encoded := values.Encode(); encoded != "" {
		return r.URL.Path + "?" + encoded
	}
	return r.URL.Path
}
