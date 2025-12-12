package forwarder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/orris-inc/orris-client/internal/forward"

	"github.com/orris-inc/orris-client/internal/logger"
	"github.com/orris-inc/orris-client/internal/tunnel"
)

// isClosedError checks if the error is due to closed connection.
func isClosedError(err error) bool {
	return errors.Is(err, net.ErrClosed)
}

// bufferSize is the size of the buffer used for copying data.
// Using 64KB to match WebSocket buffer size for better throughput.
const bufferSize = 64 * 1024

// UDP constants
const (
	udpMaxPacketSize   = 65535
	udpIdleTimeout     = 2 * time.Minute
	udpCleanupInterval = 30 * time.Second
)

// trafficWriter wraps an io.Writer to count bytes written.
// This allows using io.Copy which can leverage splice(2) for zero-copy.
type trafficWriter struct {
	w         io.Writer
	trafficFn func(int64)
}

func (tw *trafficWriter) Write(p []byte) (n int, err error) {
	n, err = tw.w.Write(p)
	if n > 0 && tw.trafficFn != nil {
		tw.trafficFn(int64(n))
	}
	return
}

// copyWithTraffic copies data from src to dst using io.Copy.
// On Linux, this leverages splice(2) for zero-copy when both src and dst are sockets,
// which provides better backpressure propagation in multi-hop forwarding chains.
func copyWithTraffic(dst io.Writer, src io.Reader, trafficFn func(int64)) (int64, error) {
	tw := &trafficWriter{w: dst, trafficFn: trafficFn}
	return io.Copy(tw, src)
}

// bufPool is a pool of buffers used for copying data to reduce GC pressure.
var bufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, bufferSize)
		return &buf
	},
}

// copyBuffer copies data from src to dst using a pooled buffer.
// It calls trafficFn with the number of bytes written after each write.
// It respects context cancellation by checking ctx.Done() between reads.
// NOTE: For multi-hop forwarding chains, consider using copyWithTraffic instead
// for better backpressure propagation via splice(2).
func copyBuffer(ctx context.Context, dst io.Writer, src io.Reader, trafficFn func(int64)) (int64, error) {
	bufp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufp)

	var written int64
	for {
		select {
		case <-ctx.Done():
			return written, ctx.Err()
		default:
		}

		nr, rerr := src.Read(*bufp)
		if nr > 0 {
			nw, werr := dst.Write((*bufp)[:nr])
			if nw > 0 {
				written += int64(nw)
				if trafficFn != nil {
					trafficFn(int64(nw))
				}
			}
			if werr != nil {
				return written, werr
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return written, nil
			}
			return written, rerr
		}
	}
}

// Forwarder is an interface for all forwarder types.
type Forwarder interface {
	Start(ctx context.Context) error
	Stop() error
	Traffic() *TrafficCounter
	RuleID() string
}

// writeQueueSize is the buffer size for async write queue.
// Large enough to absorb bursts without blocking the tunnel read loop.
// 2048 entries should handle most speed test scenarios.
const writeQueueSize = 2048

// maxBatchSize is the maximum number of buffers to batch in a single writev call.
const maxBatchSize = 64

// ErrQueueFull is returned when the write queue is full.
var ErrQueueFull = errors.New("write queue full")

// connState manages async write queue for a connection.
// It completely decouples the tunnel read loop from connection writes.
// The tunnel read loop dispatches data to the queue (non-blocking),
// and an independent goroutine processes the queue.
type connState struct {
	conn      net.Conn
	queue     chan []byte
	closed    atomic.Bool
	closeOnce sync.Once
	trafficFn func(int64)
}

// newConnState creates a new connection state with async write loop.
func newConnState(conn net.Conn, trafficFn func(int64)) *connState {
	cs := &connState{
		conn:      conn,
		queue:     make(chan []byte, writeQueueSize),
		trafficFn: trafficFn,
	}
	go cs.writeLoop()
	return cs
}

// writeLoop processes the write queue with batch optimization.
// It uses writev (via net.Buffers) to combine multiple small writes
// into a single system call for better performance.
func (cs *connState) writeLoop() {
	bufs := make(net.Buffers, 0, maxBatchSize)

	for {
		// Wait for first data
		data, ok := <-cs.queue
		if !ok {
			return
		}
		bufs = append(bufs, data)

		// Non-blocking batch: collect more data if available
	drain:
		for len(bufs) < maxBatchSize {
			select {
			case data, ok := <-cs.queue:
				if !ok {
					break drain
				}
				bufs = append(bufs, data)
			default:
				break drain
			}
		}

		// Batch write using writev
		n, err := bufs.WriteTo(cs.conn)
		if cs.trafficFn != nil && n > 0 {
			cs.trafficFn(n)
		}
		if err != nil {
			cs.closed.Store(true)
			return
		}

		// Reset for next batch
		bufs = bufs[:0]
	}
}

// Write queues data for async write. Non-blocking to prevent head-of-line blocking.
// With a large queue (2048), this should handle most burst scenarios.
func (cs *connState) Write(data []byte) error {
	if cs.closed.Load() {
		return net.ErrClosed
	}

	// Copy data since the original buffer may be reused
	buf := make([]byte, len(data))
	copy(buf, data)

	// Non-blocking send - never block the tunnel read loop
	select {
	case cs.queue <- buf:
		return nil
	default:
		return ErrQueueFull
	}
}

// Close closes the connection and write queue.
func (cs *connState) Close() {
	cs.closeOnce.Do(func() {
		cs.closed.Store(true)
		close(cs.queue)
		cs.conn.Close()
	})
}

// IsClosed returns true if the connection is closed.
func (cs *connState) IsClosed() bool {
	return cs.closed.Load()
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
	rule        *forward.Rule
	tcpListener net.Listener
	udpConn     *net.UDPConn
	traffic     *TrafficCounter

	// UDP client tracking for response routing
	udpClientsMu sync.RWMutex
	udpClients   map[string]*udpClient // client addr -> upstream conn

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewDirectForwarder creates a new direct forwarder.
func NewDirectForwarder(rule *forward.Rule) *DirectForwarder {
	return &DirectForwarder{
		rule:       rule,
		traffic:    &TrafficCounter{},
		udpClients: make(map[string]*udpClient),
	}
}

// Start starts the direct forwarder.
func (f *DirectForwarder) Start(ctx context.Context) error {
	f.ctx, f.cancel = context.WithCancel(ctx)

	protocol := f.rule.Protocol
	if protocol == "" {
		protocol = "tcp"
	}

	switch protocol {
	case "tcp":
		if err := f.startTCP(); err != nil {
			return err
		}
	case "udp":
		if err := f.startUDP(); err != nil {
			return err
		}
	case "both":
		if err := f.startTCP(); err != nil {
			return err
		}
		if err := f.startUDP(); err != nil {
			f.tcpListener.Close()
			return err
		}
	default:
		return fmt.Errorf("unsupported protocol: %s", protocol)
	}

	logger.Info("direct forwarder started",
		"rule_id", f.rule.ID,
		"listen_port", f.rule.ListenPort,
		"target", fmt.Sprintf("%s:%d", f.rule.TargetAddress, f.rule.TargetPort),
		"protocol", protocol)

	return nil
}

// startTCP starts the TCP listener.
func (f *DirectForwarder) startTCP() error {
	addr := fmt.Sprintf(":%d", f.rule.ListenPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("tcp listen on %s: %w", addr, err)
	}
	f.tcpListener = listener

	f.wg.Add(1)
	go f.tcpAcceptLoop()

	return nil
}

// startUDP starts the UDP listener.
func (f *DirectForwarder) startUDP() error {
	addr := fmt.Sprintf(":%d", f.rule.ListenPort)
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("resolve udp addr %s: %w", addr, err)
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("udp listen on %s: %w", addr, err)
	}
	f.udpConn = conn

	f.wg.Add(2)
	go f.udpReadLoop()
	go f.udpCleanupLoop()

	return nil
}

// Stop stops the direct forwarder.
func (f *DirectForwarder) Stop() error {
	if f.cancel != nil {
		f.cancel()
	}
	if f.tcpListener != nil {
		f.tcpListener.Close()
	}
	if f.udpConn != nil {
		f.udpConn.Close()
	}

	// Close all UDP upstream connections
	f.udpClientsMu.Lock()
	for _, client := range f.udpClients {
		if client.upstream != nil {
			client.upstream.Close()
		}
	}
	f.udpClients = make(map[string]*udpClient)
	f.udpClientsMu.Unlock()

	f.wg.Wait()
	logger.Info("direct forwarder stopped", "rule_id", f.rule.ID)
	return nil
}

// Traffic returns the traffic counter.
func (f *DirectForwarder) Traffic() *TrafficCounter {
	return f.traffic
}

// RuleID returns the rule ID.
func (f *DirectForwarder) RuleID() string {
	return f.rule.ID
}

func (f *DirectForwarder) tcpAcceptLoop() {
	defer f.wg.Done()

	for {
		conn, err := f.tcpListener.Accept()
		if err != nil {
			select {
			case <-f.ctx.Done():
				return
			default:
				if !isClosedError(err) {
					logger.Error("direct tcp accept error", "error", err)
				}
				continue
			}
		}

		f.wg.Add(1)
		go f.handleTCPConn(conn)
	}
}

func (f *DirectForwarder) handleTCPConn(clientConn net.Conn) {
	defer f.wg.Done()
	defer clientConn.Close()

	targetAddr := net.JoinHostPort(f.rule.TargetAddress, fmt.Sprintf("%d", f.rule.TargetPort))

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if f.rule.BindIP != "" {
		dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(f.rule.BindIP)}
	}

	targetConn, err := dialer.Dial("tcp", targetAddr)
	if err != nil {
		logger.Error("direct tcp dial target failed", "target", targetAddr, "bind_ip", f.rule.BindIP, "error", err)
		return
	}
	defer targetConn.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	// Client -> Target (upload)
	go func() {
		defer wg.Done()
		copyBuffer(f.ctx, targetConn, clientConn, f.traffic.AddUpload)
		if tc, ok := targetConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	// Target -> Client (download)
	go func() {
		defer wg.Done()
		copyBuffer(f.ctx, clientConn, targetConn, f.traffic.AddDownload)
		if tc, ok := clientConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	wg.Wait()
}

// udpReadLoop reads UDP packets and forwards them to the target.
func (f *DirectForwarder) udpReadLoop() {
	defer f.wg.Done()

	targetAddr := net.JoinHostPort(f.rule.TargetAddress, fmt.Sprintf("%d", f.rule.TargetPort))
	buf := make([]byte, udpMaxPacketSize)

	for {
		select {
		case <-f.ctx.Done():
			return
		default:
		}

		n, clientAddr, err := f.udpConn.ReadFromUDP(buf)
		if err != nil {
			if !isClosedError(err) && f.ctx.Err() == nil {
				logger.Error("direct udp read error", "error", err)
			}
			continue
		}

		f.traffic.AddUpload(int64(n))

		// Get or create upstream connection for this client
		client := f.getOrCreateUDPClient(clientAddr, targetAddr)
		if client == nil {
			continue
		}

		// Forward packet to upstream
		_, err = client.upstream.Write(buf[:n])
		if err != nil {
			logger.Debug("direct udp write to target failed", "client", clientAddr, "error", err)
			f.removeUDPClient(clientAddr.String())
		}
	}
}

// getOrCreateUDPClient gets or creates a UDP client connection.
func (f *DirectForwarder) getOrCreateUDPClient(clientAddr *net.UDPAddr, targetAddr string) *udpClient {
	key := clientAddr.String()

	f.udpClientsMu.RLock()
	client, exists := f.udpClients[key]
	f.udpClientsMu.RUnlock()

	if exists {
		client.lastActive = time.Now()
		return client
	}

	// Create new upstream connection with optional bind IP
	upstreamAddr, err := net.ResolveUDPAddr("udp", targetAddr)
	if err != nil {
		logger.Error("direct resolve upstream addr failed", "target", targetAddr, "error", err)
		return nil
	}

	var localAddr *net.UDPAddr
	if f.rule.BindIP != "" {
		localAddr = &net.UDPAddr{IP: net.ParseIP(f.rule.BindIP)}
	}

	upstream, err := net.DialUDP("udp", localAddr, upstreamAddr)
	if err != nil {
		logger.Error("direct udp dial target failed", "target", targetAddr, "bind_ip", f.rule.BindIP, "error", err)
		return nil
	}

	client = &udpClient{
		clientAddr: clientAddr,
		upstream:   upstream,
		lastActive: time.Now(),
	}

	f.udpClientsMu.Lock()
	f.udpClients[key] = client
	f.udpClientsMu.Unlock()

	// Start goroutine to read responses from upstream
	f.wg.Add(1)
	go f.udpUpstreamReadLoop(client)

	logger.Debug("direct udp client created", "rule_id", f.rule.ID, "client", clientAddr, "target", targetAddr)

	return client
}

// udpUpstreamReadLoop reads responses from upstream and sends to client.
func (f *DirectForwarder) udpUpstreamReadLoop(client *udpClient) {
	defer f.wg.Done()

	buf := make([]byte, udpMaxPacketSize)

	for {
		select {
		case <-f.ctx.Done():
			return
		default:
		}

		// Set read deadline to allow periodic check
		client.upstream.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := client.upstream.Read(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			if err != io.EOF && !isClosedError(err) && f.ctx.Err() == nil {
				logger.Debug("direct udp upstream read error", "client", client.clientAddr, "error", err)
			}
			f.removeUDPClient(client.clientAddr.String())
			return
		}

		client.lastActive = time.Now()
		f.traffic.AddDownload(int64(n))

		// Send response back to client
		_, err = f.udpConn.WriteToUDP(buf[:n], client.clientAddr)
		if err != nil {
			logger.Debug("direct udp write to client failed", "client", client.clientAddr, "error", err)
		}
	}
}

// udpCleanupLoop periodically cleans up idle UDP clients.
func (f *DirectForwarder) udpCleanupLoop() {
	defer f.wg.Done()

	ticker := time.NewTicker(udpCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-f.ctx.Done():
			return
		case <-ticker.C:
			f.cleanupIdleUDPClients()
		}
	}
}

// cleanupIdleUDPClients removes idle UDP clients.
func (f *DirectForwarder) cleanupIdleUDPClients() {
	now := time.Now()

	f.udpClientsMu.Lock()
	defer f.udpClientsMu.Unlock()

	for key, client := range f.udpClients {
		if now.Sub(client.lastActive) > udpIdleTimeout {
			logger.Debug("direct removing idle udp client", "client", client.clientAddr)
			client.upstream.Close()
			delete(f.udpClients, key)
		}
	}
}

// removeUDPClient removes a UDP client by key.
func (f *DirectForwarder) removeUDPClient(key string) {
	f.udpClientsMu.Lock()
	defer f.udpClientsMu.Unlock()

	if client, exists := f.udpClients[key]; exists {
		client.upstream.Close()
		delete(f.udpClients, key)
	}
}

// EntryForwarder handles entry forwarding (local port -> WS tunnel -> exit agent).
type EntryForwarder struct {
	rule        *forward.Rule
	tcpListener net.Listener
	udpConn     *net.UDPConn
	traffic     *TrafficCounter

	tunnel tunnel.Sender
	connMu sync.RWMutex
	conns  map[uint64]*connState // connID -> TCP client connection state (async write)

	// UDP client tracking: clientAddr -> connID mapping
	udpClientsMu sync.RWMutex
	udpClients   map[string]uint64       // clientAddr -> connID
	udpConnIDs   map[uint64]*net.UDPAddr // connID -> clientAddr (for response routing)

	nextConnID atomic.Uint64
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

// NewEntryForwarder creates a new entry forwarder.
func NewEntryForwarder(rule *forward.Rule, t tunnel.Sender) *EntryForwarder {
	return &EntryForwarder{
		rule:       rule,
		tunnel:     t,
		traffic:    &TrafficCounter{},
		conns:      make(map[uint64]*connState),
		udpClients: make(map[string]uint64),
		udpConnIDs: make(map[uint64]*net.UDPAddr),
	}
}

// Start starts the entry forwarder.
func (f *EntryForwarder) Start(ctx context.Context) error {
	f.ctx, f.cancel = context.WithCancel(ctx)

	protocol := f.rule.Protocol
	if protocol == "" {
		protocol = "tcp"
	}

	switch protocol {
	case "tcp":
		if err := f.startTCP(); err != nil {
			return err
		}
	case "udp":
		if err := f.startUDP(); err != nil {
			return err
		}
	case "both":
		if err := f.startTCP(); err != nil {
			return err
		}
		if err := f.startUDP(); err != nil {
			f.tcpListener.Close()
			return err
		}
	default:
		return fmt.Errorf("unsupported protocol: %s", protocol)
	}

	logger.Info("entry forwarder started",
		"rule_id", f.rule.ID,
		"listen_port", f.rule.ListenPort,
		"exit_agent_id", f.rule.ExitAgentID,
		"protocol", protocol)

	return nil
}

// startTCP starts the TCP listener.
func (f *EntryForwarder) startTCP() error {
	addr := fmt.Sprintf(":%d", f.rule.ListenPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("tcp listen on %s: %w", addr, err)
	}
	f.tcpListener = listener

	f.wg.Add(1)
	go f.tcpAcceptLoop()

	return nil
}

// startUDP starts the UDP listener.
func (f *EntryForwarder) startUDP() error {
	addr := fmt.Sprintf(":%d", f.rule.ListenPort)
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("resolve udp addr %s: %w", addr, err)
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("udp listen on %s: %w", addr, err)
	}
	f.udpConn = conn

	f.wg.Add(1)
	go f.udpReadLoop()

	return nil
}

// Stop stops the entry forwarder.
func (f *EntryForwarder) Stop() error {
	if f.cancel != nil {
		f.cancel()
	}
	if f.tcpListener != nil {
		f.tcpListener.Close()
	}
	if f.udpConn != nil {
		f.udpConn.Close()
	}

	// Close all TCP connections
	f.connMu.Lock()
	for _, cs := range f.conns {
		cs.Close()
	}
	f.conns = make(map[uint64]*connState)
	f.connMu.Unlock()

	// Clear UDP client mappings
	f.udpClientsMu.Lock()
	f.udpClients = make(map[string]uint64)
	f.udpConnIDs = make(map[uint64]*net.UDPAddr)
	f.udpClientsMu.Unlock()

	f.wg.Wait()
	logger.Info("entry forwarder stopped", "rule_id", f.rule.ID)
	return nil
}

// Traffic returns the traffic counter.
func (f *EntryForwarder) Traffic() *TrafficCounter {
	return f.traffic
}

// RuleID returns the rule ID.
func (f *EntryForwarder) RuleID() string {
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

// HandleConnect is not used by EntryForwarder (it initiates connections, not receives).
func (f *EntryForwarder) HandleConnect(connID uint64) {
	// Not used - EntryForwarder is the initiator
}

// HandleConnectWithPayload is not used by EntryForwarder.
func (f *EntryForwarder) HandleConnectWithPayload(connID uint64, payload []byte) {
	// Not used - EntryForwarder is the initiator
}

// HandleData handles data received from tunnel (exit -> entry -> client).
// Uses async write queue to prevent blocking the tunnel read loop.
func (f *EntryForwarder) HandleData(connID uint64, data []byte) {
	// Check if this is a UDP connection
	if tunnel.IsUDPConnID(connID) {
		f.handleUDPData(tunnel.GetConnIDValue(connID), data)
		return
	}

	// TCP connection - use async write
	f.connMu.RLock()
	cs, ok := f.conns[connID]
	f.connMu.RUnlock()

	if !ok || cs.IsClosed() {
		return
	}

	if err := cs.Write(data); err != nil {
		if errors.Is(err, ErrQueueFull) {
			// Queue full - client is reading too slow, log but don't close immediately
			// This prevents premature connection closure during speed tests
			logger.Warn("entry write queue full, dropping data", "conn_id", connID, "len", len(data))
		} else {
			logger.Debug("entry async write to tcp client failed", "conn_id", connID, "error", err)
			f.closeTCPConn(connID)
		}
	}
	// Note: traffic is counted in connState.writeLoop after actual write
}

// handleUDPData handles UDP data from tunnel and sends to client.
func (f *EntryForwarder) handleUDPData(connID uint64, data []byte) {
	f.udpClientsMu.RLock()
	clientAddr, ok := f.udpConnIDs[connID]
	f.udpClientsMu.RUnlock()

	if !ok {
		logger.Debug("entry udp unknown connID", "conn_id", connID)
		return
	}

	n, err := f.udpConn.WriteToUDP(data, clientAddr)
	if err != nil {
		logger.Debug("entry write to udp client failed", "conn_id", connID, "error", err)
		return
	}
	f.traffic.AddDownload(int64(n))
}

// HandleClose handles close message from tunnel.
func (f *EntryForwarder) HandleClose(connID uint64) {
	if tunnel.IsUDPConnID(connID) {
		f.closeUDPConn(tunnel.GetConnIDValue(connID))
		return
	}
	f.closeTCPConn(connID)
}

func (f *EntryForwarder) tcpAcceptLoop() {
	defer f.wg.Done()

	for {
		conn, err := f.tcpListener.Accept()
		if err != nil {
			select {
			case <-f.ctx.Done():
				return
			default:
				if !isClosedError(err) {
					logger.Error("entry tcp accept error", "error", err)
				}
				continue
			}
		}

		f.wg.Add(1)
		go f.handleTCPConn(conn)
	}
}

func (f *EntryForwarder) handleTCPConn(clientConn net.Conn) {
	defer f.wg.Done()

	connID := f.nextConnID.Add(1)

	// Create connState with async write queue for download direction
	cs := newConnState(clientConn, f.traffic.AddDownload)

	f.connMu.Lock()
	f.conns[connID] = cs
	f.connMu.Unlock()

	defer f.closeTCPConn(connID)

	logger.Debug("entry new tcp connection", "conn_id", connID, "client", clientConn.RemoteAddr())

	if err := f.tunnel.SendMessage(tunnel.NewConnectMessage(connID)); err != nil {
		logger.Error("entry send connect message failed", "conn_id", connID, "error", err)
		return
	}

	bufp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufp)
	buf := *bufp

	for {
		select {
		case <-f.ctx.Done():
			return
		default:
		}

		n, err := clientConn.Read(buf)
		if err != nil {
			if err != io.EOF && !isClosedError(err) {
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

func (f *EntryForwarder) closeTCPConn(connID uint64) {
	f.connMu.Lock()
	cs, ok := f.conns[connID]
	if ok {
		delete(f.conns, connID)
	}
	f.connMu.Unlock()

	if ok {
		cs.Close()
		f.tunnel.SendMessage(tunnel.NewCloseMessage(connID))
	}
}

// udpReadLoop reads UDP packets from clients and forwards them through tunnel.
func (f *EntryForwarder) udpReadLoop() {
	defer f.wg.Done()

	buf := make([]byte, udpMaxPacketSize)

	for {
		select {
		case <-f.ctx.Done():
			return
		default:
		}

		n, clientAddr, err := f.udpConn.ReadFromUDP(buf)
		if err != nil {
			if !isClosedError(err) && f.ctx.Err() == nil {
				logger.Error("entry udp read error", "error", err)
			}
			continue
		}

		f.traffic.AddUpload(int64(n))

		// Get or create connID for this UDP client
		connID := f.getOrCreateUDPConnID(clientAddr)

		// Send data through tunnel (using UDP-flagged connID)
		if err := f.tunnel.SendMessage(tunnel.NewUDPDataMessage(connID, buf[:n])); err != nil {
			logger.Error("entry send udp data failed", "conn_id", connID, "error", err)
		}
	}
}

// getOrCreateUDPConnID gets or creates a connID for a UDP client.
func (f *EntryForwarder) getOrCreateUDPConnID(clientAddr *net.UDPAddr) uint64 {
	key := clientAddr.String()

	f.udpClientsMu.RLock()
	connID, exists := f.udpClients[key]
	f.udpClientsMu.RUnlock()

	if exists {
		return connID
	}

	// Create new connID
	connID = f.nextConnID.Add(1)

	f.udpClientsMu.Lock()
	f.udpClients[key] = connID
	f.udpConnIDs[connID] = clientAddr
	f.udpClientsMu.Unlock()

	// Send connect message with client address (for Exit to know where to respond)
	if err := f.tunnel.SendMessage(tunnel.NewUDPConnectMessage(connID, key)); err != nil {
		logger.Error("entry send udp connect failed", "conn_id", connID, "error", err)
	}

	logger.Debug("entry udp client registered", "conn_id", connID, "client", key)
	return connID
}

// closeUDPConn removes a UDP client mapping.
func (f *EntryForwarder) closeUDPConn(connID uint64) {
	f.udpClientsMu.Lock()
	defer f.udpClientsMu.Unlock()

	if clientAddr, ok := f.udpConnIDs[connID]; ok {
		delete(f.udpClients, clientAddr.String())
		delete(f.udpConnIDs, connID)
	}
}

// udpExitClient tracks UDP client state on exit side.
type udpExitClient struct {
	clientAddr string       // Entry-side client address (for logging)
	upstream   *net.UDPConn // Connection to target
	lastActive time.Time
}

// ExitForwarder handles exit forwarding (WS tunnel -> target).
type ExitForwarder struct {
	rule    *forward.Rule
	traffic *TrafficCounter

	tunnel tunnel.Sender
	connMu sync.RWMutex
	conns  map[uint64]*connState // connID -> TCP target connection state (async write)

	// UDP connections
	udpConnsMu sync.RWMutex
	udpConns   map[uint64]*udpExitClient // connID -> UDP state

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewExitForwarder creates a new exit forwarder.
func NewExitForwarder(rule *forward.Rule) *ExitForwarder {
	return &ExitForwarder{
		rule:     rule,
		traffic:  &TrafficCounter{},
		conns:    make(map[uint64]*connState),
		udpConns: make(map[uint64]*udpExitClient),
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

	// Close TCP connections
	f.connMu.Lock()
	for _, cs := range f.conns {
		cs.Close()
	}
	f.conns = make(map[uint64]*connState)
	f.connMu.Unlock()

	// Close UDP connections
	f.udpConnsMu.Lock()
	for _, client := range f.udpConns {
		if client.upstream != nil {
			client.upstream.Close()
		}
	}
	f.udpConns = make(map[uint64]*udpExitClient)
	f.udpConnsMu.Unlock()

	f.wg.Wait()
	logger.Info("exit forwarder stopped", "rule_id", f.rule.ID)
	return nil
}

// Traffic returns the traffic counter.
func (f *ExitForwarder) Traffic() *TrafficCounter {
	return f.traffic
}

// RuleID returns the rule ID.
func (f *ExitForwarder) RuleID() string {
	return f.rule.ID
}

// HandleConnect handles TCP connect message from tunnel.
func (f *ExitForwarder) HandleConnect(connID uint64) {
	targetAddr := net.JoinHostPort(f.rule.TargetAddress, fmt.Sprintf("%d", f.rule.TargetPort))

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if f.rule.BindIP != "" {
		dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(f.rule.BindIP)}
	}

	targetConn, err := dialer.Dial("tcp", targetAddr)
	if err != nil {
		logger.Error("exit tcp dial target failed", "conn_id", connID, "target", targetAddr, "bind_ip", f.rule.BindIP, "error", err)
		if f.tunnel != nil {
			f.tunnel.SendMessage(tunnel.NewCloseMessage(connID))
		}
		return
	}

	// Create connState with async write queue for upload direction
	cs := newConnState(targetConn, f.traffic.AddUpload)

	f.connMu.Lock()
	f.conns[connID] = cs
	f.connMu.Unlock()

	logger.Debug("exit tcp target connection established", "conn_id", connID, "target", targetAddr)

	f.wg.Add(1)
	go f.readFromTarget(connID, targetConn)
}

// HandleConnectWithPayload handles UDP connect message with client address payload.
func (f *ExitForwarder) HandleConnectWithPayload(connID uint64, payload []byte) {
	// Check if this is a UDP connection
	if !tunnel.IsUDPConnID(connID) {
		// TCP connect with payload - ignore payload, treat as regular connect
		f.HandleConnect(connID)
		return
	}

	actualID := tunnel.GetConnIDValue(connID)
	clientAddr := string(payload)

	targetAddr := net.JoinHostPort(f.rule.TargetAddress, fmt.Sprintf("%d", f.rule.TargetPort))
	upstreamAddr, err := net.ResolveUDPAddr("udp", targetAddr)
	if err != nil {
		logger.Error("exit resolve udp target failed", "conn_id", actualID, "error", err)
		f.sendUDPClose(actualID)
		return
	}

	var localAddr *net.UDPAddr
	if f.rule.BindIP != "" {
		localAddr = &net.UDPAddr{IP: net.ParseIP(f.rule.BindIP)}
	}

	upstream, err := net.DialUDP("udp", localAddr, upstreamAddr)
	if err != nil {
		logger.Error("exit udp dial target failed", "conn_id", actualID, "target", targetAddr, "bind_ip", f.rule.BindIP, "error", err)
		f.sendUDPClose(actualID)
		return
	}

	client := &udpExitClient{
		clientAddr: clientAddr,
		upstream:   upstream,
		lastActive: time.Now(),
	}

	f.udpConnsMu.Lock()
	f.udpConns[actualID] = client
	f.udpConnsMu.Unlock()

	logger.Debug("exit udp connection established", "conn_id", actualID, "client", clientAddr, "target", targetAddr)

	f.wg.Add(1)
	go f.udpUpstreamReadLoop(actualID, client)
}

// HandleData handles data message from tunnel (entry -> exit -> target).
// Uses async write queue to prevent blocking the tunnel read loop.
func (f *ExitForwarder) HandleData(connID uint64, data []byte) {
	// Check if this is a UDP connection
	if tunnel.IsUDPConnID(connID) {
		f.handleUDPData(tunnel.GetConnIDValue(connID), data)
		return
	}

	// TCP connection - use async write
	f.connMu.RLock()
	cs, ok := f.conns[connID]
	f.connMu.RUnlock()

	if !ok || cs.IsClosed() {
		return
	}

	if err := cs.Write(data); err != nil {
		if errors.Is(err, ErrQueueFull) {
			// Queue full - target is accepting too slow, log but don't close immediately
			logger.Warn("exit write queue full, dropping data", "conn_id", connID, "len", len(data))
		} else {
			logger.Debug("exit async write to tcp target failed", "conn_id", connID, "error", err)
			f.closeTCPConn(connID)
		}
	}
	// Note: traffic is counted in connState.writeLoop after actual write
}

// handleUDPData handles UDP data from tunnel and forwards to target.
func (f *ExitForwarder) handleUDPData(connID uint64, data []byte) {
	f.udpConnsMu.RLock()
	client, ok := f.udpConns[connID]
	f.udpConnsMu.RUnlock()

	if !ok {
		logger.Debug("exit udp unknown connID", "conn_id", connID)
		return
	}

	client.lastActive = time.Now()

	n, err := client.upstream.Write(data)
	if err != nil {
		logger.Debug("exit write to udp target failed", "conn_id", connID, "error", err)
		f.closeUDPConn(connID)
		return
	}
	f.traffic.AddUpload(int64(n))
}

// HandleClose handles close message from tunnel.
func (f *ExitForwarder) HandleClose(connID uint64) {
	if tunnel.IsUDPConnID(connID) {
		f.closeUDPConn(tunnel.GetConnIDValue(connID))
		return
	}
	f.closeTCPConn(connID)
}

func (f *ExitForwarder) readFromTarget(connID uint64, targetConn net.Conn) {
	defer f.wg.Done()
	defer f.closeTCPConn(connID)

	bufp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufp)
	buf := *bufp

	for {
		select {
		case <-f.ctx.Done():
			return
		default:
		}

		n, err := targetConn.Read(buf)
		if err != nil {
			if err != io.EOF && !isClosedError(err) {
				logger.Debug("exit read from tcp target failed", "conn_id", connID, "error", err)
			}
			return
		}

		f.traffic.AddDownload(int64(n))

		if f.tunnel != nil {
			if err := f.tunnel.SendMessage(tunnel.NewDataMessage(connID, buf[:n])); err != nil {
				logger.Error("exit send tcp data message failed", "conn_id", connID, "error", err)
				return
			}
		}
	}
}

// udpUpstreamReadLoop reads responses from UDP target and sends back through tunnel.
func (f *ExitForwarder) udpUpstreamReadLoop(connID uint64, client *udpExitClient) {
	defer f.wg.Done()
	defer f.closeUDPConn(connID)

	buf := make([]byte, udpMaxPacketSize)

	for {
		select {
		case <-f.ctx.Done():
			return
		default:
		}

		// Set read deadline to allow periodic check
		client.upstream.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := client.upstream.Read(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			if err != io.EOF && !isClosedError(err) && f.ctx.Err() == nil {
				logger.Debug("exit udp upstream read error", "conn_id", connID, "error", err)
			}
			return
		}

		client.lastActive = time.Now()
		f.traffic.AddDownload(int64(n))

		// Send response back through tunnel
		if f.tunnel != nil {
			if err := f.tunnel.SendMessage(tunnel.NewUDPDataMessage(connID, buf[:n])); err != nil {
				logger.Error("exit send udp data message failed", "conn_id", connID, "error", err)
				return
			}
		}
	}
}

func (f *ExitForwarder) closeTCPConn(connID uint64) {
	f.connMu.Lock()
	cs, ok := f.conns[connID]
	if ok {
		delete(f.conns, connID)
	}
	f.connMu.Unlock()

	if ok {
		cs.Close()
		if f.tunnel != nil {
			f.tunnel.SendMessage(tunnel.NewCloseMessage(connID))
		}
	}
}

// closeUDPConn closes a UDP connection.
func (f *ExitForwarder) closeUDPConn(connID uint64) {
	f.udpConnsMu.Lock()
	client, ok := f.udpConns[connID]
	if ok {
		delete(f.udpConns, connID)
	}
	f.udpConnsMu.Unlock()

	if ok && client.upstream != nil {
		client.upstream.Close()
	}
}

// sendUDPClose sends a UDP close message through tunnel.
func (f *ExitForwarder) sendUDPClose(connID uint64) {
	if f.tunnel != nil {
		f.tunnel.SendMessage(tunnel.NewUDPCloseMessage(connID))
	}
}

// RelayForwarder handles relay forwarding (WS tunnel inbound -> WS tunnel outbound).
// It bridges data between two tunnel connections in a chain.
type RelayForwarder struct {
	rule    *forward.Rule
	traffic *TrafficCounter

	inbound  tunnel.Sender  // sender to previous hop (set by tunnel.Server)
	outbound *tunnel.Client // client to next hop

	closedMu sync.RWMutex
	closed   map[uint64]bool // track closed connections to avoid loops

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewRelayForwarder creates a new relay forwarder.
func NewRelayForwarder(rule *forward.Rule, outbound *tunnel.Client) *RelayForwarder {
	return &RelayForwarder{
		rule:     rule,
		outbound: outbound,
		traffic:  &TrafficCounter{},
		closed:   make(map[uint64]bool),
	}
}

// SetSender sets the inbound tunnel sender (called by tunnel.Server).
func (f *RelayForwarder) SetSender(s tunnel.Sender) {
	f.inbound = s
}

// Start starts the relay forwarder.
func (f *RelayForwarder) Start(ctx context.Context) error {
	f.ctx, f.cancel = context.WithCancel(ctx)

	// Set outbound handler wrapper for responses from next hop
	f.outbound.SetHandler(&relayOutboundHandler{relay: f})

	logger.Info("relay forwarder started",
		"rule_id", f.rule.ID,
		"next_hop", fmt.Sprintf("%s:%d", f.rule.NextHopAddress, f.rule.NextHopWsPort))
	return nil
}

// Stop stops the relay forwarder.
func (f *RelayForwarder) Stop() error {
	if f.cancel != nil {
		f.cancel()
	}
	f.wg.Wait()
	logger.Info("relay forwarder stopped", "rule_id", f.rule.ID)
	return nil
}

// Traffic returns the traffic counter.
func (f *RelayForwarder) Traffic() *TrafficCounter {
	return f.traffic
}

// RuleID returns the rule ID.
func (f *RelayForwarder) RuleID() string {
	return f.rule.ID
}

// HandleConnect handles connect message from inbound (previous hop -> relay -> next hop).
// Implements tunnel.MessageHandler for tunnel.Server.
func (f *RelayForwarder) HandleConnect(connID uint64) {
	logger.Debug("relay forward connect", "rule_id", f.rule.ID, "conn_id", connID)

	if f.outbound == nil {
		logger.Error("relay outbound not connected", "rule_id", f.rule.ID, "conn_id", connID)
		if f.inbound != nil {
			f.inbound.SendMessage(tunnel.NewCloseMessage(connID))
		}
		return
	}

	if err := f.outbound.SendMessage(tunnel.NewConnectMessage(connID)); err != nil {
		logger.Error("relay forward connect failed", "rule_id", f.rule.ID, "conn_id", connID, "error", err)
		if f.inbound != nil {
			f.inbound.SendMessage(tunnel.NewCloseMessage(connID))
		}
	}
}

// HandleConnectWithPayload handles connect message with payload (for UDP).
// Forwards the message with payload intact to the next hop.
func (f *RelayForwarder) HandleConnectWithPayload(connID uint64, payload []byte) {
	logger.Debug("relay forward connect with payload", "rule_id", f.rule.ID, "conn_id", connID, "payload_len", len(payload))

	if f.outbound == nil {
		logger.Error("relay outbound not connected", "rule_id", f.rule.ID, "conn_id", connID)
		if f.inbound != nil {
			f.inbound.SendMessage(tunnel.NewCloseMessage(connID))
		}
		return
	}

	// Forward connect message with payload intact (preserves UDP flag in connID)
	msg := &tunnel.Message{
		Type:    tunnel.MsgConnect,
		ConnID:  connID,
		Payload: payload,
	}
	if err := f.outbound.SendMessage(msg); err != nil {
		logger.Error("relay forward connect with payload failed", "rule_id", f.rule.ID, "conn_id", connID, "error", err)
		if f.inbound != nil {
			f.inbound.SendMessage(tunnel.NewCloseMessage(connID))
		}
	}
}

// HandleData handles data message from inbound (previous hop -> relay -> next hop).
// Implements tunnel.MessageHandler for tunnel.Server.
func (f *RelayForwarder) HandleData(connID uint64, data []byte) {
	if f.outbound == nil {
		return
	}

	f.traffic.AddUpload(int64(len(data)))

	if err := f.outbound.SendMessage(tunnel.NewDataMessage(connID, data)); err != nil {
		logger.Debug("relay forward data to next hop failed", "rule_id", f.rule.ID, "conn_id", connID, "error", err)
		f.closeConn(connID)
	}
}

// HandleClose handles close message from inbound (previous hop -> relay -> next hop).
// Implements tunnel.MessageHandler for tunnel.Server.
func (f *RelayForwarder) HandleClose(connID uint64) {
	f.closedMu.Lock()
	if f.closed[connID] {
		f.closedMu.Unlock()
		return
	}
	f.closed[connID] = true
	f.closedMu.Unlock()

	logger.Debug("relay forward close to next hop", "rule_id", f.rule.ID, "conn_id", connID)

	if f.outbound != nil {
		f.outbound.SendMessage(tunnel.NewCloseMessage(connID))
	}
}

// handleOutboundData handles data from outbound (next hop -> relay -> previous hop).
func (f *RelayForwarder) handleOutboundData(connID uint64, data []byte) {
	if f.inbound == nil {
		return
	}

	f.traffic.AddDownload(int64(len(data)))

	if err := f.inbound.SendMessage(tunnel.NewDataMessage(connID, data)); err != nil {
		logger.Debug("relay forward data to previous hop failed", "rule_id", f.rule.ID, "conn_id", connID, "error", err)
		f.closeConn(connID)
	}
}

// handleOutboundClose handles close from outbound (next hop -> relay -> previous hop).
func (f *RelayForwarder) handleOutboundClose(connID uint64) {
	f.closedMu.Lock()
	if f.closed[connID] {
		f.closedMu.Unlock()
		return
	}
	f.closed[connID] = true
	f.closedMu.Unlock()

	logger.Debug("relay forward close to previous hop", "rule_id", f.rule.ID, "conn_id", connID)

	if f.inbound != nil {
		f.inbound.SendMessage(tunnel.NewCloseMessage(connID))
	}
}

func (f *RelayForwarder) closeConn(connID uint64) {
	f.closedMu.Lock()
	if f.closed[connID] {
		f.closedMu.Unlock()
		return
	}
	f.closed[connID] = true
	f.closedMu.Unlock()

	if f.inbound != nil {
		f.inbound.SendMessage(tunnel.NewCloseMessage(connID))
	}
	if f.outbound != nil {
		f.outbound.SendMessage(tunnel.NewCloseMessage(connID))
	}
}

// relayOutboundHandler wraps RelayForwarder for outbound DataHandler interface.
type relayOutboundHandler struct {
	relay *RelayForwarder
}

// HandleData implements tunnel.DataHandler for outbound responses.
func (h *relayOutboundHandler) HandleData(connID uint64, data []byte) {
	h.relay.handleOutboundData(connID, data)
}

// HandleClose implements tunnel.DataHandler for outbound responses.
func (h *relayOutboundHandler) HandleClose(connID uint64) {
	h.relay.handleOutboundClose(connID)
}
