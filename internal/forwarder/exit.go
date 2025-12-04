package forwarder

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/orris-inc/orris-client/internal/logger"
)

type ExitForwarder struct {
	wsListenPort  uint16
	targetAddress string
	targetPort    uint16
	ruleID        uint
	token         string

	server   *http.Server
	upgrader websocket.Upgrader
	done     chan struct{}
	wg       sync.WaitGroup

	uploadBytes   int64
	downloadBytes int64
}

func NewExitForwarder(wsListenPort uint16, targetAddress string, targetPort uint16, ruleID uint, token string) *ExitForwarder {
	return &ExitForwarder{
		wsListenPort:  wsListenPort,
		targetAddress: targetAddress,
		targetPort:    targetPort,
		ruleID:        ruleID,
		token:         token,
		done:          make(chan struct{}),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  32 * 1024,
			WriteBufferSize: 32 * 1024,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
	}
}

func (f *ExitForwarder) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/tunnel/", f.handleTunnel)

	addr := fmt.Sprintf(":%d", f.wsListenPort)
	f.server = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		if err := f.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			logger.Error("exit ws server error", "error", err)
		}
	}()

	return nil
}

func (f *ExitForwarder) Stop() {
	close(f.done)
	if f.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		f.server.Shutdown(ctx)
	}
	f.wg.Wait()
}

func (f *ExitForwarder) handleTunnel(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if !f.validateToken(auth) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/tunnel/"), "/")
	if len(pathParts) < 1 {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	ruleID, err := strconv.ParseUint(pathParts[0], 10, 64)
	if err != nil || uint(ruleID) != f.ruleID {
		http.Error(w, "Invalid rule ID", http.StatusBadRequest)
		return
	}

	wsConn, err := f.upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Error("exit ws upgrade failed", "error", err)
		return
	}

	f.wg.Add(1)
	go f.handleWsConnection(wsConn)
}

func (f *ExitForwarder) validateToken(auth string) bool {
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	return token == f.token
}

func (f *ExitForwarder) handleWsConnection(wsConn *websocket.Conn) {
	defer f.wg.Done()
	defer wsConn.Close()

	targetAddr := net.JoinHostPort(f.targetAddress, fmt.Sprintf("%d", f.targetPort))
	targetConn, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
	if err != nil {
		logger.Error("exit dial target failed", "target", targetAddr, "error", err)
		return
	}
	defer targetConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go f.tcpToWs(ctx, cancel, targetConn, wsConn)
	f.wsToTcp(ctx, cancel, wsConn, targetConn)
}

func (f *ExitForwarder) wsToTcp(ctx context.Context, cancel context.CancelFunc, wsConn *websocket.Conn, tcpConn net.Conn) {
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

		atomic.AddInt64(&f.uploadBytes, int64(len(data)))

		tcpConn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		if _, err := tcpConn.Write(data); err != nil {
			logger.Error("exit tcp write failed", "error", err)
			return
		}
	}
}

func (f *ExitForwarder) tcpToWs(ctx context.Context, cancel context.CancelFunc, tcpConn net.Conn, wsConn *websocket.Conn) {
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

		atomic.AddInt64(&f.downloadBytes, int64(n))

		wsConn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		if err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
			logger.Error("exit ws write failed", "error", err)
			return
		}
	}
}

func (f *ExitForwarder) GetAndResetTraffic() (upload, download int64) {
	upload = atomic.SwapInt64(&f.uploadBytes, 0)
	download = atomic.SwapInt64(&f.downloadBytes, 0)
	return
}
