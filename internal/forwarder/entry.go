package forwarder

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/easayliu/orris-client/internal/logger"
)

const (
	wsDialTimeout    = 10 * time.Second
	wsWriteTimeout   = 10 * time.Second
	wsPingInterval   = 30 * time.Second
	wsReconnectDelay = 5 * time.Second
	wsMaxReconnect   = 10
)

type EntryForwarder struct {
	listenPort  uint16
	exitAddress string
	exitWsPort  uint16
	ruleID      uint
	token       string

	listener net.Listener
	done     chan struct{}
	wg       sync.WaitGroup

	uploadBytes   int64
	downloadBytes int64
}

func NewEntryForwarder(listenPort uint16, exitAddress string, exitWsPort uint16, ruleID uint, token string) *EntryForwarder {
	return &EntryForwarder{
		listenPort:  listenPort,
		exitAddress: exitAddress,
		exitWsPort:  exitWsPort,
		ruleID:      ruleID,
		token:       token,
		done:        make(chan struct{}),
	}
}

func (f *EntryForwarder) Start() error {
	addr := fmt.Sprintf(":%d", f.listenPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	f.listener = listener

	f.wg.Add(1)
	go f.acceptLoop()

	return nil
}

func (f *EntryForwarder) Stop() {
	close(f.done)
	if f.listener != nil {
		f.listener.Close()
	}
	f.wg.Wait()
}

func (f *EntryForwarder) acceptLoop() {
	defer f.wg.Done()

	for {
		conn, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.done:
				return
			default:
				logger.Error("entry accept error", "error", err)
				continue
			}
		}

		f.wg.Add(1)
		go f.handleConnection(conn)
	}
}

func (f *EntryForwarder) handleConnection(clientConn net.Conn) {
	defer f.wg.Done()
	defer clientConn.Close()

	wsHost := net.JoinHostPort(f.exitAddress, fmt.Sprintf("%d", f.exitWsPort))
	wsURL := fmt.Sprintf("ws://%s/tunnel/%d", wsHost, f.ruleID)
	header := http.Header{}
	header.Set("Authorization", "Bearer "+f.token)

	dialer := websocket.Dialer{
		HandshakeTimeout: wsDialTimeout,
	}

	wsConn, _, err := dialer.Dial(wsURL, header)
	if err != nil {
		logger.Error("entry ws dial failed", "url", wsURL, "error", err)
		return
	}
	defer wsConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go f.wsToTcp(ctx, cancel, wsConn, clientConn)
	f.tcpToWs(ctx, cancel, clientConn, wsConn)
}

func (f *EntryForwarder) tcpToWs(ctx context.Context, cancel context.CancelFunc, tcpConn net.Conn, wsConn *websocket.Conn) {
	defer cancel()

	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.done:
			return
		default:
		}

		tcpConn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, err := tcpConn.Read(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return
		}

		atomic.AddInt64(&f.uploadBytes, int64(n))

		wsConn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		if err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
			logger.Error("entry ws write failed", "error", err)
			return
		}
	}
}

func (f *EntryForwarder) wsToTcp(ctx context.Context, cancel context.CancelFunc, wsConn *websocket.Conn, tcpConn net.Conn) {
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return
		case <-f.done:
			return
		default:
		}

		wsConn.SetReadDeadline(time.Now().Add(1 * time.Second))
		msgType, data, err := wsConn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return
		}

		if msgType != websocket.BinaryMessage {
			continue
		}

		atomic.AddInt64(&f.downloadBytes, int64(len(data)))

		tcpConn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		if _, err := tcpConn.Write(data); err != nil {
			logger.Error("entry tcp write failed", "error", err)
			return
		}
	}
}

func (f *EntryForwarder) GetAndResetTraffic() (upload, download int64) {
	upload = atomic.SwapInt64(&f.uploadBytes, 0)
	download = atomic.SwapInt64(&f.downloadBytes, 0)
	return
}
