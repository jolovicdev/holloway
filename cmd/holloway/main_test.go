package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jolovicdev/holloway/internal/tunnel"
)

func TestTunnelURLConvertsHTTPBaseToWebSocketTunnel(t *testing.T) {
	got, err := buildTunnelURL("https://example.com", "abc", 5)
	if err != nil {
		t.Fatalf("tunnelURL: %v", err)
	}
	want := "wss://example.com/tunnel/abc?replay=5"
	if got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
}

func TestTunnelURLPreservesServerBasePath(t *testing.T) {
	got, err := buildTunnelURL("wss://example.com/holloway/", "abc", 0)
	if err != nil {
		t.Fatalf("tunnelURL: %v", err)
	}
	want := "wss://example.com/holloway/tunnel/abc"
	if got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
}

func TestForwardLocalDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	redirectFollowed := make(chan struct{}, 1)
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectFollowed <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(redirectTarget.Close)

	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+"/metadata", http.StatusFound)
	}))
	t.Cleanup(local.Close)

	resp := forwardLocal(context.Background(), localForwardClient(), serverPort(t, local.URL), tunnel.Request{
		ID:     "req_1",
		Method: http.MethodPost,
		Path:   "/redirect",
	})

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want redirect status %d", resp.StatusCode, http.StatusFound)
	}
	select {
	case <-redirectFollowed:
		t.Fatal("client followed redirect away from localhost target")
	default:
	}
}

func TestForwardLocalRejectsUnsafePathsBeforeSending(t *testing.T) {
	t.Parallel()

	called := false
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(local.Close)

	for _, path := range []string{
		"/../admin",
		"/%2e%2e/admin",
		"//169.254.169.254/latest",
		"http://169.254.169.254/latest",
	} {
		resp := forwardLocal(context.Background(), localForwardClient(), serverPort(t, local.URL), tunnel.Request{
			ID:     "req_1",
			Method: http.MethodPost,
			Path:   path,
		})
		if resp.StatusCode != 0 || resp.Error == "" {
			t.Fatalf("path %q response = %#v, want local validation error", path, resp)
		}
	}

	if called {
		t.Fatal("unsafe path reached local server")
	}
}

func TestForwardLocalDropsUnsafeHeaders(t *testing.T) {
	t.Parallel()

	seen := make(chan http.Header, 1)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("accepted"))
	}))
	t.Cleanup(local.Close)

	resp := forwardLocal(context.Background(), localForwardClient(), serverPort(t, local.URL), tunnel.Request{
		ID:     "req_1",
		Method: http.MethodPost,
		Path:   "/orders",
		Headers: map[string][]string{
			"X-Good":              {"kept"},
			"Connection":          {"keep-alive, X-Connection-Only"},
			"X-Connection-Only":   {"dropped"},
			"Bad\r\nInjected":     {"dropped"},
			"X-Bad-Value":         {"ok\r\nX-Injected: bad"},
			"Proxy-Authorization": {"dropped"},
		},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d error = %q, want 202", resp.StatusCode, resp.Error)
	}
	if resp.Body != "accepted" {
		t.Fatalf("body = %q, want accepted", resp.Body)
	}

	headers := <-seen
	if headers.Get("X-Good") != "kept" {
		t.Fatalf("X-Good = %q, want kept", headers.Get("X-Good"))
	}
	for _, key := range []string{"Connection", "X-Connection-Only", "Bad\r\nInjected", "X-Bad-Value", "Proxy-Authorization"} {
		if headers.Get(key) != "" {
			t.Fatalf("header %q was forwarded as %q", key, headers.Get(key))
		}
	}
}

func serverPort(t *testing.T, rawURL string) int {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("split server host: %v", err)
	}
	value, err := net.LookupPort("tcp", port)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return value
}

func localForwardClient() *http.Client {
	return &http.Client{Timeout: time.Second}
}
