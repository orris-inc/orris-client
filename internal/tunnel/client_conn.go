package tunnel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/orris-inc/orris-client/internal/forward"
	"github.com/orris-inc/orris-client/internal/logger"
)

// connectWithRetry attempts to connect with retries and endpoint refresh.
func (c *Client) connectWithRetry() error {
	err := c.connect()
	if err == nil {
		return nil
	}

	// If no retry configured, fail immediately
	if c.initialRetryMax <= 0 {
		return err
	}

	logger.Warn("initial connection failed, will retry", "error", err, "max_retries", c.initialRetryMax)

	for attempt := 1; attempt <= c.initialRetryMax; attempt++ {
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		default:
		}

		// Try to refresh endpoint before retry
		if c.endpointRefresher != nil && c.refreshAfterAttempts > 0 &&
			attempt%c.refreshAfterAttempts == 0 {
			logger.Info("refreshing endpoint before retry", "attempt", attempt)
			if newEndpoint, newToken, refreshErr := c.endpointRefresher(); refreshErr != nil {
				logger.Error("endpoint refresh failed", "error", refreshErr)
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

		interval := c.backoff.Next()
		logger.Info("retrying initial connection",
			"attempt", attempt,
			"max_retries", c.initialRetryMax,
			"interval", interval.Round(time.Millisecond))

		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-time.After(interval):
		}

		err = c.connect()
		if err == nil {
			c.backoff.Reset()
			logger.Info("initial connection succeeded after retries", "attempts", attempt)
			return nil
		}

		logger.Warn("initial connection retry failed", "attempt", attempt, "error", err)
	}

	return fmt.Errorf("max retries (%d) exceeded: %w", c.initialRetryMax, err)
}

// connect establishes a WebSocket connection and performs handshake and key exchange.
func (c *Client) connect() error {
	c.endpointMu.RLock()
	endpoint := c.endpoint
	token := c.token
	ruleID := c.ruleID
	c.endpointMu.RUnlock()

	logger.Info("connecting to exit agent", "endpoint", endpoint, "rule_id", ruleID)

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   64 * 1024,
		WriteBufferSize:  64 * 1024,
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

	// Log token info for debugging (only prefix for security)
	tokenDbg := token
	if len(tokenDbg) > 15 {
		tokenDbg = tokenDbg[:15] + "..."
	}
	tokenParts := strings.SplitN(token, "_", 3)
	logger.Debug("sending tunnel handshake",
		"rule_id", ruleID,
		"token_prefix", tokenDbg,
		"token_len", len(token),
		"token_parts", len(tokenParts))

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

// reconnect attempts to reconnect with exponential backoff.
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
