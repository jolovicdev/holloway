package relay

import (
	"context"
	"errors"
	"testing"

	"github.com/jolovicdev/holloway/internal/tunnel"
)

type fakeSender struct {
	response tunnel.Response
	err      error
	seen     []tunnel.Request
	closed   bool
}

func (f *fakeSender) Send(_ context.Context, req tunnel.Request) (tunnel.Response, error) {
	f.seen = append(f.seen, req)
	if f.err != nil {
		return tunnel.Response{}, f.err
	}
	return f.response, nil
}

func (f *fakeSender) Close() error {
	f.closed = true
	return nil
}

func TestHubReturnsFalseWhenNoClientIsConnected(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	_, ok := hub.Forward(context.Background(), "missing", tunnel.Request{ID: "req_1"})
	if ok {
		t.Fatal("forward reported success without a connected client")
	}
}

func TestHubForwardsToRegisteredClient(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	sender := &fakeSender{response: tunnel.Response{ID: "req_1", StatusCode: 201}}
	unregister := hub.Register("testtoken", sender)
	defer unregister()

	resp, ok := hub.Forward(context.Background(), "testtoken", tunnel.Request{
		ID:     "req_1",
		Method: "POST",
		Path:   "/orders",
	})
	if !ok {
		t.Fatal("forward failed with a connected client")
	}
	if resp.StatusCode != 201 {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if len(sender.seen) != 1 || sender.seen[0].Path != "/orders" {
		t.Fatalf("forwarded requests = %#v", sender.seen)
	}
}

func TestHubReturnsFalseWhenClientSendFails(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	sender := &fakeSender{err: errors.New("connection dropped")}
	unregister := hub.Register("testtoken", sender)
	defer unregister()

	_, ok := hub.Forward(context.Background(), "testtoken", tunnel.Request{ID: "req_1"})
	if ok {
		t.Fatal("forward reported success when client send failed")
	}
}

func TestHubClosesPreviousClientForSameToken(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	oldClient := &fakeSender{}
	newClient := &fakeSender{}
	hub.Register("testtoken", oldClient)
	hub.Register("testtoken", newClient)

	if !oldClient.closed {
		t.Fatal("old client was not closed")
	}
	if newClient.closed {
		t.Fatal("new client was closed")
	}
}

func TestHubDisconnectClosesRegisteredClient(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	client := &fakeSender{}
	hub.Register("testtoken", client)

	hub.Disconnect("testtoken")

	if !client.closed {
		t.Fatal("client was not closed")
	}
	if hub.Connected("testtoken") {
		t.Fatal("client is still connected")
	}
}
