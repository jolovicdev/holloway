package relay

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/jolovicdev/holloway/internal/tunnel"
)

var errClientClosed = errors.New("client connection closed")

type WSClient struct {
	conn *websocket.Conn

	seq     atomic.Uint64
	mu      sync.Mutex
	pending map[string]chan tunnel.Response
	closed  chan struct{}
	once    sync.Once
}

func NewWSClient(conn *websocket.Conn) *WSClient {
	client := &WSClient{
		conn:    conn,
		pending: make(map[string]chan tunnel.Response),
		closed:  make(chan struct{}),
	}
	go client.readLoop()
	return client
}

func (c *WSClient) Send(ctx context.Context, req tunnel.Request) (tunnel.Response, error) {
	req = req.WithType()
	// Route responses by a per-send correlation ID rather than the webhook ID:
	// the same webhook can be in flight twice (e.g. periodic drain and a manual
	// replay), and a shared key would collide in the pending map.
	req.ID = strconv.FormatUint(c.seq.Add(1), 10)
	ch := make(chan tunnel.Response, 1)

	c.mu.Lock()
	select {
	case <-c.closed:
		c.mu.Unlock()
		return tunnel.Response{}, errClientClosed
	default:
		c.pending[req.ID] = ch
	}
	c.mu.Unlock()

	defer c.removePending(req.ID)

	err := wsjson.Write(ctx, c.conn, req)
	if err != nil {
		return tunnel.Response{}, err
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return tunnel.Response{}, errClientClosed
		}
		return resp, nil
	case <-c.closed:
		return tunnel.Response{}, errClientClosed
	case <-ctx.Done():
		_ = c.Close()
		return tunnel.Response{}, ctx.Err()
	}
}

func (c *WSClient) Wait() {
	<-c.closed
}

func (c *WSClient) Close() error {
	c.closePending()
	return c.conn.CloseNow()
}

func (c *WSClient) readLoop() {
	defer c.closePending()

	for {
		var resp tunnel.Response
		if err := wsjson.Read(context.Background(), c.conn, &resp); err != nil {
			return
		}
		if resp.Type != "" && resp.Type != tunnel.MessageResponse {
			continue
		}

		c.mu.Lock()
		ch := c.pending[resp.ID]
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- resp:
			default:
			}
		}
	}
}

func (c *WSClient) removePending(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *WSClient) closePending() {
	c.once.Do(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		close(c.closed)
		for id := range c.pending {
			delete(c.pending, id)
		}
	})
}
