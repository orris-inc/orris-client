package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/orris-inc/orris/sdk/forward"

	"github.com/orris-inc/orris-client/internal/logger"
)

// DataHandler handles data received from tunnel.
type DataHandler interface {
	HandleData(connID uint64, data []byte)
	HandleClose(connID uint64)
}

// EndpointRefresher refreshes the tunnel endpoint when reconnection fails.
// It returns the new endpoint URL and token, or an error if refresh fails.
type EndpointRefresher func() (endpoint, token string, err error)

// Client is a WebSocket tunnel client for Entry agents.
// It connects to an Exit agent and forwards data through the tunnel.
type Client struct {
	endpointMu sync.RWMutex
	endpoint   string
	token      string
	ruleID     string
	conn       *websocket.Conn

	writeMu      sync.Mutex
	handler      DataHandler
	cipher       Cipher // session cipher (created after key exchange)
	sharedSecret string // shared secret for key derivation

	backoff              *Backoff
	heartbeatInterval    time.Duration
	refreshAfterAttempts int // refresh endpoint after this many failed reconnect attempts
	endpointRefresher    EndpointRefresher

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// ClientOption configures Client.
type ClientOption func(*Client)

// WithBackoff sets the backoff configuration for reconnection.
func WithBackoff(b *Backoff) ClientOption {
	return func(c *Client) {
		c.backoff = b
	}
}

// WithHeartbeatInterval sets the heartbeat interval.
func WithHeartbeatInterval(d time.Duration) ClientOption {
	return func(c *Client) {
		c.heartbeatInterval = d
	}
}

// WithEndpointRefresher sets the endpoint refresher callback.
// When reconnection fails after refreshAfterAttempts, the refresher is called
// to get a new endpoint (e.g., when the exit agent restarts with a new port).
func WithEndpointRefresher(refresher EndpointRefresher, refreshAfterAttempts int) ClientOption {
	return func(c *Client) {
		c.endpointRefresher = refresher
		c.refreshAfterAttempts = refreshAfterAttempts
	}
}

// WithSharedSecret sets the shared secret for session key derivation.
// This enables forward-secure encryption with per-connection session keys.
func WithSharedSecret(secret string) ClientOption {
	return func(c *Client) {
		c.sharedSecret = secret
	}
}

// NewClient creates a new tunnel client.
func NewClient(endpoint, token, ruleID string, opts ...ClientOption) *Client {
	c := &Client{
		endpoint:          endpoint,
		token:             token,
		ruleID:            ruleID,
		backoff:           DefaultBackoff(),
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

	// Hold write lock before closing to prevent concurrent write
	c.writeMu.Lock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.writeMu.Unlock()

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

	// Encrypt if cipher is configured
	if c.cipher != nil {
		data, err = c.cipher.Encrypt(data)
		if err != nil {
			return fmt.Errorf("encrypt message: %w", err)
		}
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
	c.endpointMu.RLock()
	endpoint := c.endpoint
	token := c.token
	ruleID := c.ruleID
	c.endpointMu.RUnlock()

	logger.Info("connecting to exit agent", "endpoint", endpoint, "rule_id", ruleID)

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	header := make(map[string][]string)
	header["Authorization"] = []string{"Bearer " + token}

	conn, _, err := dialer.DialContext(c.ctx, endpoint, header)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	// Send tunnel handshake
	handshake := &forward.TunnelHandshake{
		AgentToken: token,
		RuleID:     ruleID,
	}
	handshakeData, err := json.Marshal(handshake)
	if err != nil {
		conn.Close()
		return fmt.Errorf("marshal handshake: %w", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, handshakeData); err != nil {
		conn.Close()
		return fmt.Errorf("send handshake: %w", err)
	}

	// Wait for handshake result
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, resultData, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return fmt.Errorf("read handshake result: %w", err)
	}
	conn.SetReadDeadline(time.Time{}) // Clear deadline

	var result forward.TunnelHandshakeResult
	if err := json.Unmarshal(resultData, &result); err != nil {
		conn.Close()
		return fmt.Errorf("unmarshal handshake result: %w", err)
	}
	if !result.Success {
		conn.Close()
		return fmt.Errorf("handshake failed: %s", result.Error)
	}

	logger.Info("tunnel handshake successful", "entry_agent_id", result.EntryAgentID)

	// Perform key exchange if shared secret is configured
	var cipher Cipher
	if c.sharedSecret != "" {
		var err error
		cipher, err = c.performKeyExchange(conn)
		if err != nil {
			conn.Close()
			return fmt.Errorf("key exchange: %w", err)
		}
		logger.Info("session key established with forward secrecy")
	}

	// Hold write lock when updating connection and cipher
	c.writeMu.Lock()
	oldConn := c.conn
	c.conn = conn
	c.cipher = cipher
	c.writeMu.Unlock()

	// Close old connection if exists (during reconnect)
	if oldConn != nil {
		oldConn.Close()
	}

	logger.Info("connected to exit agent")
	return nil
}

// performKeyExchange performs key exchange with server to establish session key.
func (c *Client) performKeyExchange(conn *websocket.Conn) (Cipher, error) {
	// Generate client nonce
	clientNonce, err := GenerateNonce()
	if err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	// Send client nonce
	keyExMsg := NewKeyExchangeMessage(clientNonce)
	keyExData, err := keyExMsg.Encode()
	if err != nil {
		return nil, fmt.Errorf("encode key exchange: %w", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, keyExData); err != nil {
		return nil, fmt.Errorf("send key exchange: %w", err)
	}

	// Receive server nonce
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, serverData, err := conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("read server key exchange: %w", err)
	}
	conn.SetReadDeadline(time.Time{})

	serverMsg, err := DecodeMessage(bytes.NewReader(serverData))
	if err != nil {
		return nil, fmt.Errorf("decode server key exchange: %w", err)
	}
	if serverMsg.Type != MsgKeyExchange {
		return nil, fmt.Errorf("unexpected message type: %d", serverMsg.Type)
	}
	if len(serverMsg.Payload) != KeyExchangeNonceSize {
		return nil, fmt.Errorf("invalid server nonce size: %d", len(serverMsg.Payload))
	}

	// Derive session key
	sessionKey, err := DeriveSessionKey(c.sharedSecret, clientNonce, serverMsg.Payload)
	if err != nil {
		return nil, fmt.Errorf("derive session key: %w", err)
	}

	// Create cipher
	cipher, err := NewXChaCha20Cipher(sessionKey)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}

	return cipher, nil
}

func (c *Client) reconnect() bool {
	for {
		interval := c.backoff.Next()
		attempt := c.backoff.Attempt()

		logger.Info("reconnecting with backoff",
			"attempt", attempt,
			"interval", interval.Round(time.Millisecond))

		select {
		case <-c.ctx.Done():
			return false
		case <-time.After(interval):
		}

		// Try to refresh endpoint after configured number of failed attempts
		if c.endpointRefresher != nil && c.refreshAfterAttempts > 0 &&
			attempt%c.refreshAfterAttempts == 0 {
			logger.Info("refreshing endpoint after failed reconnect attempts", "attempts", attempt)
			if newEndpoint, newToken, err := c.endpointRefresher(); err != nil {
				logger.Error("endpoint refresh failed", "error", err)
			} else {
				c.endpointMu.Lock()
				if newEndpoint != c.endpoint {
					logger.Info("endpoint updated", "old", c.endpoint, "new", newEndpoint)
					c.endpoint = newEndpoint
					c.token = newToken
				}
				c.endpointMu.Unlock()
			}
		}

		if err := c.connect(); err != nil {
			logger.Error("reconnect failed", "error", err, "attempt", attempt)
			continue
		}

		// Reset backoff on successful reconnection
		c.backoff.Reset()
		logger.Info("reconnected successfully after attempts", "attempts", attempt)
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

		// Decrypt if cipher is configured
		if c.cipher != nil {
			data, err = c.cipher.Decrypt(data)
			if err != nil {
				logger.Error("decrypt message error", "error", err)
				continue
			}
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
