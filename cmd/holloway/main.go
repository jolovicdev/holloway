package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/jolovicdev/holloway/internal/tunnel"
)

const (
	maxTunnelMessage     = 64 << 20
	maxLocalResponseBody = 1 << 20
)

func main() {
	log.SetFlags(0)

	if len(os.Args) < 2 || os.Args[1] != "connect" {
		fmt.Fprintln(os.Stderr, "usage: holloway connect --server wss://example.com --token xyz --secret abc --port 3000 [--replay N]")
		os.Exit(2)
	}

	flags := flag.NewFlagSet("connect", flag.ExitOnError)
	server := flags.String("server", "", "server URL")
	token := flags.String("token", "", "webhook token")
	secret := flags.String("secret", "", "tunnel secret")
	port := flags.Int("port", 3000, "local HTTP port")
	replay := flags.Int("replay", 0, "replay the last N webhooks after connecting")
	_ = flags.Parse(os.Args[2:])

	if *server == "" || *token == "" || *secret == "" {
		fmt.Fprintln(os.Stderr, "--server, --token, and --secret are required")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := connectLoop(ctx, *server, *token, *secret, *port, *replay); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func connectLoop(ctx context.Context, server, token, secret string, port int, replay int) error {
	backoff := time.Second
	for {
		connected, err := connectOnce(ctx, server, token, secret, port, replay)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if connected {
			backoff = time.Second
		}
		if err != nil {
			log.Printf("error: %v", err)
		}

		log.Printf("reconnecting in %s", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func connectOnce(ctx context.Context, server, token, secret string, port int, replay int) (bool, error) {
	url, err := buildTunnelURL(server, token, replay)
	if err != nil {
		return false, err
	}

	dialCtx, cancelDial := context.WithTimeout(ctx, 10*time.Second)
	defer cancelDial()

	conn, _, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + secret}},
	})
	if err != nil {
		return false, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxTunnelMessage)

	log.Printf("connected: %s -> localhost:%d", url, port)

	client := &http.Client{Timeout: 25 * time.Second}
	for {
		var req tunnel.Request
		if err := wsjson.Read(ctx, conn, &req); err != nil {
			return true, err
		}
		if req.Type != "" && req.Type != tunnel.MessageRequest {
			continue
		}

		resp := forwardLocal(ctx, client, port, req)
		if err := wsjson.Write(ctx, conn, resp.WithType()); err != nil {
			return true, err
		}
		status := strconv.Itoa(resp.StatusCode)
		if resp.Error != "" {
			status = resp.Error
		}
		replayLabel := ""
		if req.Replay {
			replayLabel = " replay"
		}
		log.Printf("%s %s -> %s%s", req.Method, req.Path, status, replayLabel)
	}
}

func forwardLocal(ctx context.Context, client *http.Client, port int, req tunnel.Request) tunnel.Response {
	path, err := safeForwardPath(req.Path)
	if err != nil {
		return tunnel.Response{ID: req.ID, StatusCode: 0, Error: err.Error()}
	}
	localURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, localURL, bytes.NewReader(req.Body))
	if err != nil {
		return tunnel.Response{ID: req.ID, StatusCode: 0, Error: err.Error()}
	}
	for key, values := range safeForwardHeaders(req.Headers) {
		httpReq.Header[key] = values
	}

	localClient := noRedirectClient(client)
	httpResp, err := localClient.Do(httpReq)
	if err != nil {
		return tunnel.Response{ID: req.ID, StatusCode: 0, Error: err.Error()}
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, maxLocalResponseBody))
	if err != nil {
		return tunnel.Response{ID: req.ID, StatusCode: 0, Error: err.Error()}
	}

	return tunnel.Response{
		ID:         req.ID,
		StatusCode: httpResp.StatusCode,
		Body:       string(body),
	}
}

func safeForwardPath(rawPath string) (string, error) {
	if rawPath == "" {
		return "/", nil
	}
	if hasControlByte(rawPath) {
		return "", fmt.Errorf("unsafe request path")
	}
	if parsed, err := url.Parse(rawPath); err == nil && (parsed.IsAbs() || parsed.Host != "") {
		return "", fmt.Errorf("unsafe request path")
	}

	if !strings.HasPrefix(rawPath, "/") {
		rawPath = "/" + rawPath
	}
	if strings.HasPrefix(rawPath, "//") {
		return "", fmt.Errorf("unsafe request path")
	}

	parsed, err := url.ParseRequestURI(rawPath)
	if err != nil {
		return "", fmt.Errorf("unsafe request path: %w", err)
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("unsafe request path")
		}
	}
	return parsed.RequestURI(), nil
}

func safeForwardHeaders(headers map[string][]string) http.Header {
	skip := map[string]struct{}{
		"connection":          {},
		"host":                {},
		"keep-alive":          {},
		"proxy-authenticate":  {},
		"proxy-authorization": {},
		"te":                  {},
		"trailer":             {},
		"transfer-encoding":   {},
		"upgrade":             {},
	}
	for key, values := range headers {
		if strings.EqualFold(key, "Connection") {
			for _, value := range values {
				for _, token := range strings.Split(value, ",") {
					token = strings.ToLower(strings.TrimSpace(token))
					if token != "" {
						skip[token] = struct{}{}
					}
				}
			}
		}
	}

	clean := make(http.Header)
	for key, values := range headers {
		if !validHeaderFieldName(key) {
			continue
		}
		if _, blocked := skip[strings.ToLower(key)]; blocked {
			continue
		}
		for _, value := range values {
			if !validHeaderFieldValue(value) {
				continue
			}
			clean.Add(key, value)
		}
	}
	return clean
}

func noRedirectClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &clone
}

func validHeaderFieldName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isTokenChar(name[i]) {
			return false
		}
	}
	return true
}

func validHeaderFieldValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] == '\t' {
			continue
		}
		if value[i] < 0x20 || value[i] == 0x7f {
			return false
		}
	}
	return true
}

func isTokenChar(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	case c >= 'a' && c <= 'z':
		return true
	}
	switch c {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	default:
		return false
	}
}

func hasControlByte(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] == 0x7f {
			return true
		}
	}
	return false
}

func buildTunnelURL(server string, token string, replay int) (string, error) {
	parsed, err := url.Parse(server)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported server scheme %q", parsed.Scheme)
	}

	base := strings.TrimRight(parsed.Path, "/")
	if base == "" || base == "/" {
		parsed.Path = "/tunnel/" + token
	} else {
		parsed.Path = base + "/tunnel/" + token
	}
	query := parsed.Query()
	if replay > 0 {
		query.Set("replay", strconv.Itoa(replay))
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
