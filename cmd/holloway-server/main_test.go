package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunRejectsMissingAdminPassword(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := run(ctx, []string{
		"-addr", "127.0.0.1:0",
		"-db", t.TempDir() + "/holloway.db",
		"-templates", "../../templates",
		"-static", "../../static",
	}, func(string) string {
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "HOLLOWAY_ADMIN_PASSWORD") {
		t.Fatalf("run error = %v, want missing admin password error", err)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, []string{
			"-addr", "127.0.0.1:0",
			"-db", t.TempDir() + "/holloway.db",
			"-templates", "../../templates",
			"-static", "../../static",
		}, func(key string) string {
			if key == "HOLLOWAY_ADMIN_PASSWORD" {
				return "secret"
			}
			return ""
		})
	}()

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop after context cancel")
	}
}
