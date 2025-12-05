package tunnel

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/easayliu/orris-client/internal/logger"
)

// DataHandler handles data received from tunnel.
type DataHandler interface {
	HandleData(connID uint64, data []byte)
	HandleClose(connID uint64)
}

// Client is a WebSocket tunnel client for Entry agents.
// It connects to an Exit agent and forwards data through the tunnel.
type Client struct {
	endpoint string
	token    string
	conn     *websocket.Conn

	writeMu sync.Mutex
	handler DataHandler

	reconnectInterval time.Duration
	heartbeatInterval time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// ClientOption configures Client.
type ClientOption func(*Client)

// WithReconnectInterval sets the reconnect interval.
func WithReconnectInterval(d time.Duration) ClientOption {
	return func(c *Client) {
		c.reconnectInterval = d
	}
}

// WithHeartbeatInterval sets the heartbeat interval.
func WithHeartbeatInterval(d time.Duration) ClientOption {
	return func(c *Client) {
		c.heartbeatInterval = d
	}
}

// NewClient creates a new tunnel client.
func NewClient(endpoint, token string, opts ...ClientOption) *Client {
	c := &Client{
		endpoint:          endpoint,
		token:             token,
		reconnectInterval: 5 * time.Second,
		heartbeatInterval: 30 * time.Second,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// SetHandler sets the data handler.
func (c *Client) SetHandler(h DataHandler) {
	c.handler = h
}

// Start starts the tunnel client with auto-reconnect.
func (c *Client) Start(ctx context.Context) error {
	c.ctx, c.cancel = context.WithCancel(ctx)

	if err := c.connect(); err != nil {
		return fmt.Errorf("initial connection failed: %w", err)
	}

	c.wg.Add(2)
	go c.readLoop()
	go c.heartbeatLoop()

	return nil
}

// Stop stops the tunnel client.
func (c *Client) Stop() error {
	if c.cancel != nil {
		c.cancel()
	}
	if c.conn != nil {
		c.conn.Close()
	}
	c.wg.Wait()
	logger.Info("tunnel client stopped")
	return nil
}

// IsConnected returns true if the tunnel is connected.
func (c *Client) IsConnected() bool {
	return c.conn != nil
}

// SendMessage sends a message through the tunnel.
func (c *Client) SendMessage(msg *Message) error {
	data, err := msg.Encode()
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.conn == nil {
		return fmt.Errorf("not connected")
	}

	if err := c.conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		return fmt.Errorf("write message: %w", err)
	}

	return nil
}

func (c *Client) connect() error {
	logger.Info("connecting to exit agent", "endpoint", c.endpoint)

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	header := make(map[string][]string)
	header["Authorization"] = []string{"Bearer " + c.token}

	conn, _, err := dialer.DialContext(c.ctx, c.endpoint, header)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	c.conn = conn
	logger.Info("connected to exit agent")
	return nil
}

func (c *Client) reconnect() bool {
	for {
		select {
		case <-c.ctx.Done():
			return false
		case <-time.After(c.reconnectInterval):
		}

		logger.Info("attempting to reconnect...")
		if err := c.connect(); err != nil {
			logger.Error("reconnect failed", "error", err)
			continue
		}
		return true
	}
}

func (c *Client) readLoop() {
	defer c.wg.Done()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		_, data, err := c.conn.ReadMessage()
		if err != nil {
			logger.Error("tunnel read error", "error", err)
			if !c.reconnect() {
				return
			}
			continue
		}

		msg, err := DecodeMessage(bytes.NewReader(data))
		if err != nil {
			logger.Error("decode message error", "error", err)
			continue
		}

		c.handleMessage(msg)
	}
}

func (c *Client) handleMessage(msg *Message) {
	if c.handler == nil {
		return
	}

	switch msg.Type {
	case MsgData:
		c.handler.HandleData(msg.ConnID, msg.Payload)
	case MsgClose:
		c.handler.HandleClose(msg.ConnID)
	case MsgPong:
		logger.Debug("received pong")
	default:
		logger.Warn("unknown message type", "type", msg.Type)
	}
}

func (c *Client) heartbeatLoop() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if err := c.SendMessage(NewPingMessage()); err != nil {
				logger.Error("send ping failed", "error", err)
			}
		}
	}
}
