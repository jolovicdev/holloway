package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jolovicdev/holloway/internal/dashboard"
	"github.com/jolovicdev/holloway/internal/relay"
	"github.com/jolovicdev/holloway/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Getenv); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string) error {
	flags := flag.NewFlagSet("holloway-server", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	addr := flags.String("addr", envFrom(getenv, "HOLLOWAY_ADDR", ":8080"), "HTTP listen address")
	dbPath := flags.String("db", envFrom(getenv, "HOLLOWAY_DB", "holloway.db"), "SQLite database path")
	templateDir := flags.String("templates", envFrom(getenv, "HOLLOWAY_TEMPLATES", "templates"), "template directory")
	staticDir := flags.String("static", envFrom(getenv, "HOLLOWAY_STATIC", "static"), "static asset directory")
	bootstrapToken := flags.String("bootstrap-token", envFrom(getenv, "HOLLOWAY_BOOTSTRAP_TOKEN", ""), "initial token")
	bootstrapTunnelSecret := flags.String("bootstrap-tunnel-secret", envFrom(getenv, "HOLLOWAY_BOOTSTRAP_TUNNEL_SECRET", ""), "initial token tunnel secret")
	webhookRateLimit := flags.Int("webhook-rate-limit", envIntFrom(getenv, "HOLLOWAY_WEBHOOK_RATE_LIMIT", relay.DefaultWebhookRateLimitPerMinute), "webhook requests per token per minute")
	retentionMaxAge := flags.Duration("retention-max-age", envDurationFrom(getenv, "HOLLOWAY_RETENTION_MAX_AGE", 0), "delete webhooks older than this (e.g. 720h); 0 disables")
	retentionMaxRows := flags.Int("retention-max-rows", envIntFrom(getenv, "HOLLOWAY_RETENTION_MAX_ROWS", 0), "keep at most this many webhooks per token; 0 disables")
	dedup := flags.Bool("dedup", envBoolFrom(getenv, "HOLLOWAY_DEDUP", false), "drop duplicate deliveries (same method, path, and body) and replay the original response")
	allowInsecureAdmin := flags.Bool("allow-insecure-admin", envBoolFrom(getenv, "HOLLOWAY_ALLOW_INSECURE_ADMIN", false), "allow unauthenticated dashboard and token management")
	if err := flags.Parse(args); err != nil {
		return err
	}
	adminPassword := envFrom(getenv, "HOLLOWAY_ADMIN_PASSWORD", "")
	if adminPassword == "" && !*allowInsecureAdmin {
		return errors.New("HOLLOWAY_ADMIN_PASSWORD is required; set HOLLOWAY_ALLOW_INSECURE_ADMIN=true only for local-only use")
	}
	if *bootstrapToken != "" && *bootstrapTunnelSecret == "" {
		return errors.New("HOLLOWAY_BOOTSTRAP_TUNNEL_SECRET is required when HOLLOWAY_BOOTSTRAP_TOKEN is set")
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Printf("close store: %v", err)
		}
	}()

	if *bootstrapToken != "" {
		if err := st.EnsureToken(*bootstrapToken, "Bootstrap token", *bootstrapTunnelSecret); err != nil {
			return err
		}
	}

	startRetentionSweep(ctx, st, *retentionMaxAge, *retentionMaxRows)

	broker := dashboard.NewBroker()
	hub := relay.NewHub()
	server := relay.NewServer(relay.Config{
		Store:              st,
		Hub:                hub,
		AdminPassword:      adminPassword,
		AllowInsecureAdmin: *allowInsecureAdmin,
		Dedup:              *dedup,
		Events:             broker,
		WebhookLimiter:     relay.NewWebhookRateLimiter(*webhookRateLimit, time.Minute),
	})

	dash, err := dashboard.New(st, server, broker, *templateDir, *staticDir)
	if err != nil {
		return err
	}
	server.MountDashboard(dash)

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           server,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
			_ = httpServer.Close()
		}
	}()

	log.Printf("holloway-server listening on %s", *addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

const retentionSweepInterval = time.Hour

func startRetentionSweep(ctx context.Context, st *store.Store, maxAge time.Duration, maxRows int) {
	if maxAge <= 0 && maxRows <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(retentionSweepInterval)
		defer ticker.Stop()
		for {
			sweepRetention(st, maxAge, maxRows)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func sweepRetention(st *store.Store, maxAge time.Duration, maxRows int) {
	if maxAge > 0 {
		if removed, err := st.DeleteWebhooksOlderThan(time.Now().Add(-maxAge)); err != nil {
			log.Printf("retention: delete by age: %v", err)
		} else if removed > 0 {
			log.Printf("retention: removed %d webhooks older than %s", removed, maxAge)
		}
	}
	if maxRows > 0 {
		if removed, err := st.DeleteWebhooksOverCountPerToken(maxRows); err != nil {
			log.Printf("retention: delete by count: %v", err)
		} else if removed > 0 {
			log.Printf("retention: removed %d webhooks over %d per token", removed, maxRows)
		}
	}
}

func envFrom(getenv func(string) string, key, fallback string) string {
	value := getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func envDurationFrom(getenv func(string) string, key string, fallback time.Duration) time.Duration {
	value := getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envIntFrom(getenv func(string) string, key string, fallback int) int {
	value := getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBoolFrom(getenv func(string) string, key string, fallback bool) bool {
	value := getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
