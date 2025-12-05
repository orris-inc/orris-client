package forwarder

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/orris-inc/orris/sdk/forward"

	"github.com/orris-inc/orris-client/internal/logger"
	"github.com/orris-inc/orris-client/internal/tunnel"
)

// Forwarder is an interface for all forwarder types.
type Forwarder interface {
	Start(ctx context.Context) error
	Stop() error
	Traffic() *TrafficCounter
	RuleID() uint
}

// TrafficCounter tracks upload and download bytes.
type TrafficCounter struct {
	uploadBytes   atomic.Int64
	downloadBytes atomic.Int64
}

// AddUpload adds to upload bytes counter.
func (t *TrafficCounter) AddUpload(n int64) {
	t.uploadBytes.Add(n)
}

// AddDownload adds to download bytes counter.
func (t *TrafficCounter) AddDownload(n int64) {
	t.downloadBytes.Add(n)
}

// GetAndReset returns current values and resets counters.
func (t *TrafficCounter) GetAndReset() (upload, download int64) {
	upload = t.uploadBytes.Swap(0)
	download = t.downloadBytes.Swap(0)
	return
}

// DirectForwarder handles direct forwarding (local port -> target).
type DirectForwarder struct {
	rule     *forward.Rule
	listener net.Listener
	traffic  *TrafficCounter

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewDirectForwarder creates a new direct forwarder.
func NewDirectForwarder(rule *forward.Rule) *DirectForwarder {
	return &DirectForwarder{
		rule:    rule,
		traffic: &TrafficCounter{},
	}
}

// Start starts the direct forwarder.
func (f *DirectForwarder) Start(ctx context.Context) error {
	f.ctx, f.cancel = context.WithCancel(ctx)

	addr := fmt.Sprintf(":%d", f.rule.ListenPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	f.listener = listener

	logger.Info("direct forwarder started",
		"rule_id", f.rule.ID,
		"listen_port", f.rule.ListenPort,
		"target", fmt.Sprintf("%s:%d", f.rule.TargetAddress, f.rule.TargetPort))

	f.wg.Add(1)
	go f.acceptLoop()

	return nil
}

// Stop stops the direct forwarder.
func (f *DirectForwarder) Stop() error {
	if f.cancel != nil {
		f.cancel()
	}
	if f.listener != nil {
		f.listener.Close()
	}
	f.wg.Wait()
	logger.Info("direct forwarder stopped", "rule_id", f.rule.ID)
	return nil
}

// Traffic returns the traffic counter.
func (f *DirectForwarder) Traffic() *TrafficCounter {
	return f.traffic
}

// RuleID returns the rule ID.
func (f *DirectForwarder) RuleID() uint {
	return f.rule.ID
}

func (f *DirectForwarder) acceptLoop() {
	defer f.wg.Done()

	for {
		conn, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.ctx.Done():
				return
			default:
				logger.Error("direct accept error", "error", err)
				continue
			}
		}

		f.wg.Add(1)
		go f.handleConn(conn)
	}
}

func (f *DirectForwarder) handleConn(clientConn net.Conn) {
	defer f.wg.Done()
	defer clientConn.Close()

	targetAddr := fmt.Sprintf("%s:%d", f.rule.TargetAddress, f.rule.TargetPort)
	targetConn, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
	if err != nil {
		logger.Error("direct dial target failed", "target", targetAddr, "error", err)
		return
	}
	defer targetConn.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	// Client -> Target (upload)
	go func() {
		defer wg.Done()
		n, _ := io.Copy(targetConn, clientConn)
		f.traffic.AddUpload(n)
		if tc, ok := targetConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	// Target -> Client (download)
	go func() {
		defer wg.Done()
		n, _ := io.Copy(clientConn, targetConn)
		f.traffic.AddDownload(n)
		if tc, ok := clientConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	wg.Wait()
}

// EntryForwarder handles entry forwarding (local port -> WS tunnel -> exit agent).
type EntryForwarder struct {
	rule     *forward.Rule
	listener net.Listener
	traffic  *TrafficCounter

	tunnel tunnel.Sender
	connMu sync.RWMutex
	conns  map[uint64]net.Conn // connID -> client connection

	nextConnID atomic.Uint64
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

// NewEntryForwarder creates a new entry forwarder.
func NewEntryForwarder(rule *forward.Rule, t tunnel.Sender) *EntryForwarder {
	return &EntryForwarder{
		rule:    rule,
		tunnel:  t,
		traffic: &TrafficCounter{},
		conns:   make(map[uint64]net.Conn),
	}
}

// Start starts the entry forwarder.
func (f *EntryForwarder) Start(ctx context.Context) error {
	f.ctx, f.cancel = context.WithCancel(ctx)

	addr := fmt.Sprintf(":%d", f.rule.ListenPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	f.listener = listener

	logger.Info("entry forwarder started",
		"rule_id", f.rule.ID,
		"listen_port", f.rule.ListenPort,
		"exit_agent_id", f.rule.ExitAgentID)

	f.wg.Add(1)
	go f.acceptLoop()

	return nil
}

// Stop stops the entry forwarder.
func (f *EntryForwarder) Stop() error {
	if f.cancel != nil {
		f.cancel()
	}
	if f.listener != nil {
		f.listener.Close()
	}

	f.connMu.Lock()
	for _, conn := range f.conns {
		conn.Close()
	}
	f.conns = make(map[uint64]net.Conn)
	f.connMu.Unlock()

	f.wg.Wait()
	logger.Info("entry forwarder stopped", "rule_id", f.rule.ID)
	return nil
}

// Traffic returns the traffic counter.
func (f *EntryForwarder) Traffic() *TrafficCounter {
	return f.traffic
}

// RuleID returns the rule ID.
func (f *EntryForwarder) RuleID() uint {
	return f.rule.ID
}

// IsTunnelConnected returns true if the tunnel is connected.
func (f *EntryForwarder) IsTunnelConnected() bool {
	if f.tunnel == nil {
		return false
	}
	// Check if the tunnel implements ConnectionChecker
	if checker, ok := f.tunnel.(interface{ IsConnected() bool }); ok {
		return checker.IsConnected()
	}
	// If no way to check, assume connected
	return true
}

// HandleData handles data received from tunnel (exit -> entry -> client).
func (f *EntryForwarder) HandleData(connID uint64, data []byte) {
	f.connMu.RLock()
	conn, ok := f.conns[connID]
	f.connMu.RUnlock()

	if !ok {
		return
	}

	n, err := conn.Write(data)
	if err != nil {
		logger.Debug("entry write to client failed", "conn_id", connID, "error", err)
		f.closeConn(connID)
		return
	}
	f.traffic.AddDownload(int64(n))
}

// HandleClose handles close message from tunnel.
func (f *EntryForwarder) HandleClose(connID uint64) {
	f.closeConn(connID)
}

func (f *EntryForwarder) acceptLoop() {
	defer f.wg.Done()

	for {
		conn, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.ctx.Done():
				return
			default:
				logger.Error("entry accept error", "error", err)
				continue
			}
		}

		f.wg.Add(1)
		go f.handleConn(conn)
	}
}

func (f *EntryForwarder) handleConn(clientConn net.Conn) {
	defer f.wg.Done()

	connID := f.nextConnID.Add(1)

	f.connMu.Lock()
	f.conns[connID] = clientConn
	f.connMu.Unlock()

	defer f.closeConn(connID)

	logger.Debug("entry new connection", "conn_id", connID, "client", clientConn.RemoteAddr())

	if err := f.tunnel.SendMessage(tunnel.NewConnectMessage(connID)); err != nil {
		logger.Error("entry send connect message failed", "conn_id", connID, "error", err)
		return
	}

	buf := make([]byte, 32*1024)
	for {
		select {
		case <-f.ctx.Done():
			return
		default:
		}

		n, err := clientConn.Read(buf)
		if err != nil {
			if err != io.EOF {
				logger.Debug("entry read from client failed", "conn_id", connID, "error", err)
			}
			return
		}

		f.traffic.AddUpload(int64(n))

		if err := f.tunnel.SendMessage(tunnel.NewDataMessage(connID, buf[:n])); err != nil {
			logger.Error("entry send data message failed", "conn_id", connID, "error", err)
			return
		}
	}
}

func (f *EntryForwarder) closeConn(connID uint64) {
	f.connMu.Lock()
	conn, ok := f.conns[connID]
	if ok {
		delete(f.conns, connID)
	}
	f.connMu.Unlock()

	if ok {
		conn.Close()
		f.tunnel.SendMessage(tunnel.NewCloseMessage(connID))
	}
}

// ExitForwarder handles exit forwarding (WS tunnel -> target).
type ExitForwarder struct {
	rule    *forward.Rule
	traffic *TrafficCounter

	tunnel tunnel.Sender
	connMu sync.RWMutex
	conns  map[uint64]net.Conn // connID -> target connection

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewExitForwarder creates a new exit forwarder.
func NewExitForwarder(rule *forward.Rule) *ExitForwarder {
	return &ExitForwarder{
		rule:    rule,
		traffic: &TrafficCounter{},
		conns:   make(map[uint64]net.Conn),
	}
}

// SetSender sets the tunnel sender.
func (f *ExitForwarder) SetSender(s tunnel.Sender) {
	f.tunnel = s
}

// Start starts the exit forwarder.
func (f *ExitForwarder) Start(ctx context.Context) error {
	f.ctx, f.cancel = context.WithCancel(ctx)
	logger.Info("exit forwarder started",
		"rule_id", f.rule.ID,
		"target", fmt.Sprintf("%s:%d", f.rule.TargetAddress, f.rule.TargetPort))
	return nil
}

// Stop stops the exit forwarder.
func (f *ExitForwarder) Stop() error {
	if f.cancel != nil {
		f.cancel()
	}

	f.connMu.Lock()
	for _, conn := range f.conns {
		conn.Close()
	}
	f.conns = make(map[uint64]net.Conn)
	f.connMu.Unlock()

	f.wg.Wait()
	logger.Info("exit forwarder stopped", "rule_id", f.rule.ID)
	return nil
}

// Traffic returns the traffic counter.
func (f *ExitForwarder) Traffic() *TrafficCounter {
	return f.traffic
}

// RuleID returns the rule ID.
func (f *ExitForwarder) RuleID() uint {
	return f.rule.ID
}

// HandleConnect handles connect message from tunnel.
func (f *ExitForwarder) HandleConnect(connID uint64) {
	targetAddr := fmt.Sprintf("%s:%d", f.rule.TargetAddress, f.rule.TargetPort)
	targetConn, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
	if err != nil {
		logger.Error("exit dial target failed", "conn_id", connID, "target", targetAddr, "error", err)
		if f.tunnel != nil {
			f.tunnel.SendMessage(tunnel.NewCloseMessage(connID))
		}
		return
	}

	f.connMu.Lock()
	f.conns[connID] = targetConn
	f.connMu.Unlock()

	logger.Debug("exit target connection established", "conn_id", connID, "target", targetAddr)

	f.wg.Add(1)
	go f.readFromTarget(connID, targetConn)
}

// HandleData handles data message from tunnel (entry -> exit -> target).
func (f *ExitForwarder) HandleData(connID uint64, data []byte) {
	f.connMu.RLock()
	conn, ok := f.conns[connID]
	f.connMu.RUnlock()

	if !ok {
		return
	}

	n, err := conn.Write(data)
	if err != nil {
		logger.Debug("exit write to target failed", "conn_id", connID, "error", err)
		f.closeConn(connID)
		return
	}
	f.traffic.AddUpload(int64(n))
}

// HandleClose handles close message from tunnel.
func (f *ExitForwarder) HandleClose(connID uint64) {
	f.closeConn(connID)
}

func (f *ExitForwarder) readFromTarget(connID uint64, targetConn net.Conn) {
	defer f.wg.Done()
	defer f.closeConn(connID)

	buf := make([]byte, 32*1024)
	for {
		select {
		case <-f.ctx.Done():
			return
		default:
		}

		n, err := targetConn.Read(buf)
		if err != nil {
			if err != io.EOF {
				logger.Debug("exit read from target failed", "conn_id", connID, "error", err)
			}
			return
		}

		f.traffic.AddDownload(int64(n))

		if f.tunnel != nil {
			if err := f.tunnel.SendMessage(tunnel.NewDataMessage(connID, buf[:n])); err != nil {
				logger.Error("exit send data message failed", "conn_id", connID, "error", err)
				return
			}
		}
	}
}

func (f *ExitForwarder) closeConn(connID uint64) {
	f.connMu.Lock()
	conn, ok := f.conns[connID]
	if ok {
		delete(f.conns, connID)
	}
	f.connMu.Unlock()

	if ok {
		conn.Close()
		if f.tunnel != nil {
			f.tunnel.SendMessage(tunnel.NewCloseMessage(connID))
		}
	}
}
