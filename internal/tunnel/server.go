package tunnel

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/easayliu/orris-client/internal/logger"
)

// MessageHandler handles messages from tunnel clients.
type MessageHandler interface {
	HandleConnect(connID uint64)
	HandleData(connID uint64, data []byte)
	HandleClose(connID uint64)
}

// Sender sends messages through the tunnel.
type Sender interface {
	SendMessage(msg *Message) error
}

// Server is a WebSocket tunnel server for Exit agents.
// It accepts connections from Entry agents and forwards data to targets.
type Server struct {
	port uint16

	listener net.Listener
	server   *http.Server
	upgrader websocket.Upgrader

	handlerMu sync.RWMutex
	handlers  map[string]MessageHandler // ruleID -> handler

	connMu   sync.RWMutex
	conns    map[*websocket.Conn]struct{}
	connLock map[*websocket.Conn]*sync.Mutex // per-connection write lock

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewServer creates a new tunnel server.
func NewServer(port uint16) *Server {
	return &Server{
		port:     port,
		handlers: make(map[string]MessageHandler),
		conns:    make(map[*websocket.Conn]struct{}),
		connLock: make(map[*websocket.Conn]*sync.Mutex),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}
}

// AddHandler adds a message handler for a rule.
func (s *Server) AddHandler(ruleID string, handler MessageHandler) {
	s.handlerMu.Lock()
	s.handlers[ruleID] = handler
	s.handlerMu.Unlock()
}

// RemoveHandler removes a message handler.
func (s *Server) RemoveHandler(ruleID string) {
	s.handlerMu.Lock()
	delete(s.handlers, ruleID)
	s.handlerMu.Unlock()
}

// Start starts the tunnel server.
// If port is 0, a random available port will be used.
func (s *Server) Start(ctx context.Context) error {
	s.ctx, s.cancel = context.WithCancel(ctx)

	// Create listener first to get the actual port (supports port 0 for random)
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	s.listener = listener

	// Update port with the actual port (important when port was 0)
	s.port = uint16(listener.Addr().(*net.TCPAddr).Port)

	mux := http.NewServeMux()
	mux.HandleFunc("/tunnel", s.handleTunnel)

	s.server = &http.Server{
		Handler: mux,
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		logger.Info("tunnel server started", "port", s.port)
		if err := s.server.Serve(s.listener); err != http.ErrServerClosed {
			logger.Error("tunnel server error", "error", err)
		}
	}()

	return nil
}

// Port returns the actual listening port.
func (s *Server) Port() uint16 {
	return s.port
}

// Stop stops the tunnel server.
func (s *Server) Stop() error {
	if s.cancel != nil {
		s.cancel()
	}

	s.connMu.Lock()
	for conn, mu := range s.connLock {
		// Hold write lock before closing to prevent concurrent write
		mu.Lock()
		conn.Close()
		mu.Unlock()
	}
	s.conns = make(map[*websocket.Conn]struct{})
	s.connLock = make(map[*websocket.Conn]*sync.Mutex)
	s.connMu.Unlock()

	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.server.Shutdown(ctx)
	}

	s.wg.Wait()
	logger.Info("tunnel server stopped")
	return nil
}

func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	if !s.validateToken(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Error("tunnel upgrade failed", "error", err)
		return
	}

	// Create per-connection write lock
	writeMu := &sync.Mutex{}

	s.connMu.Lock()
	s.conns[conn] = struct{}{}
	s.connLock[conn] = writeMu
	s.connMu.Unlock()

	logger.Info("entry agent connected", "remote", r.RemoteAddr)

	sender := &connSender{conn: conn, mu: writeMu}

	s.handlerMu.RLock()
	for _, h := range s.handlers {
		if sh, ok := h.(interface{ SetSender(Sender) }); ok {
			sh.SetSender(sender)
		}
	}
	s.handlerMu.RUnlock()

	defer func() {
		s.connMu.Lock()
		delete(s.conns, conn)
		delete(s.connLock, conn)
		s.connMu.Unlock()
		conn.Close()
		logger.Info("entry agent disconnected", "remote", r.RemoteAddr)
	}()

	s.readLoop(conn, sender)
}

func (s *Server) validateToken(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	// Trust non-empty token (only legitimate Entry agents can obtain token from API)
	return token != ""
}

func (s *Server) readLoop(conn *websocket.Conn, sender *connSender) {
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				logger.Error("tunnel read error", "error", err)
			}
			return
		}

		msg, err := DecodeMessage(bytes.NewReader(data))
		if err != nil {
			logger.Error("decode message error", "error", err)
			continue
		}

		s.handleMessage(sender, msg)
	}
}

func (s *Server) handleMessage(sender *connSender, msg *Message) {
	s.handlerMu.RLock()
	var handler MessageHandler
	for _, h := range s.handlers {
		handler = h
		break
	}
	s.handlerMu.RUnlock()

	if handler == nil {
		logger.Warn("no handler available")
		return
	}

	switch msg.Type {
	case MsgConnect:
		handler.HandleConnect(msg.ConnID)
	case MsgData:
		handler.HandleData(msg.ConnID, msg.Payload)
	case MsgClose:
		handler.HandleClose(msg.ConnID)
	case MsgPing:
		// Use the same sender to share the write lock
		sender.SendMessage(NewPongMessage())
	default:
		logger.Warn("unknown message type", "type", msg.Type)
	}
}

// connSender implements Sender for a WebSocket connection.
// All connSender instances for the same connection share the same mutex.
type connSender struct {
	conn *websocket.Conn
	mu   *sync.Mutex // shared per-connection write lock
}

func (s *connSender) SendMessage(msg *Message) error {
	data, err := msg.Encode()
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		return fmt.Errorf("write message: %w", err)
	}

	return nil
}
