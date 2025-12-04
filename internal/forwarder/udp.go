package forwarder

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/easayliu/orris-client/internal/logger"
)

const (
	udpBufferSize  = 65535
	udpSessionTTL  = 60 * time.Second
	udpCleanupTick = 30 * time.Second
)

type UDPForwarder struct {
	listenPort    uint16
	targetAddress string
	targetPort    uint16
	conn          *net.UDPConn
	done          chan struct{}
	wg            sync.WaitGroup

	uploadBytes   int64
	downloadBytes int64

	sessions   map[string]*udpSession
	sessionsMu sync.RWMutex
}

type udpSession struct {
	clientAddr *net.UDPAddr
	targetConn *net.UDPConn
	lastActive time.Time
}

func NewUDPForwarder(listenPort uint16, targetAddress string, targetPort uint16) *UDPForwarder {
	return &UDPForwarder{
		listenPort:    listenPort,
		targetAddress: targetAddress,
		targetPort:    targetPort,
		done:          make(chan struct{}),
		sessions:      make(map[string]*udpSession),
	}
}

func (f *UDPForwarder) Start() error {
	addr := &net.UDPAddr{Port: int(f.listenPort)}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("listen udp on port %d: %w", f.listenPort, err)
	}
	f.conn = conn

	f.wg.Add(2)
	go f.readLoop()
	go f.cleanupLoop()

	return nil
}

func (f *UDPForwarder) Stop() {
	close(f.done)
	if f.conn != nil {
		f.conn.Close()
	}

	f.sessionsMu.Lock()
	for _, sess := range f.sessions {
		sess.targetConn.Close()
	}
	f.sessions = make(map[string]*udpSession)
	f.sessionsMu.Unlock()

	f.wg.Wait()
}

func (f *UDPForwarder) readLoop() {
	defer f.wg.Done()

	buf := make([]byte, udpBufferSize)
	for {
		select {
		case <-f.done:
			return
		default:
		}

		f.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, clientAddr, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-f.done:
				return
			default:
				logger.Error("udp read error", "error", err)
				continue
			}
		}

		atomic.AddInt64(&f.uploadBytes, int64(n))

		sess, err := f.getOrCreateSession(clientAddr)
		if err != nil {
			logger.Error("create udp session failed", "error", err)
			continue
		}

		sess.lastActive = time.Now()
		_, err = sess.targetConn.Write(buf[:n])
		if err != nil {
			logger.Error("udp write to target failed", "error", err)
		}
	}
}

func (f *UDPForwarder) getOrCreateSession(clientAddr *net.UDPAddr) (*udpSession, error) {
	key := clientAddr.String()

	f.sessionsMu.RLock()
	sess, ok := f.sessions[key]
	f.sessionsMu.RUnlock()
	if ok {
		return sess, nil
	}

	f.sessionsMu.Lock()
	defer f.sessionsMu.Unlock()

	if sess, ok := f.sessions[key]; ok {
		return sess, nil
	}

	targetAddr := net.JoinHostPort(f.targetAddress, fmt.Sprintf("%d", f.targetPort))
	raddr, err := net.ResolveUDPAddr("udp", targetAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve target addr: %w", err)
	}

	targetConn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return nil, fmt.Errorf("dial target: %w", err)
	}

	sess = &udpSession{
		clientAddr: clientAddr,
		targetConn: targetConn,
		lastActive: time.Now(),
	}
	f.sessions[key] = sess

	f.wg.Add(1)
	go f.sessionReadLoop(sess, key)

	return sess, nil
}

func (f *UDPForwarder) sessionReadLoop(sess *udpSession, key string) {
	defer f.wg.Done()

	buf := make([]byte, udpBufferSize)
	for {
		select {
		case <-f.done:
			return
		default:
		}

		sess.targetConn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, err := sess.targetConn.Read(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				f.sessionsMu.RLock()
				_, exists := f.sessions[key]
				f.sessionsMu.RUnlock()
				if !exists {
					return
				}
				continue
			}
			return
		}

		atomic.AddInt64(&f.downloadBytes, int64(n))
		sess.lastActive = time.Now()

		_, err = f.conn.WriteToUDP(buf[:n], sess.clientAddr)
		if err != nil {
			logger.Error("udp write to client failed", "error", err)
		}
	}
}

func (f *UDPForwarder) cleanupLoop() {
	defer f.wg.Done()

	ticker := time.NewTicker(udpCleanupTick)
	defer ticker.Stop()

	for {
		select {
		case <-f.done:
			return
		case <-ticker.C:
			f.cleanupSessions()
		}
	}
}

func (f *UDPForwarder) cleanupSessions() {
	now := time.Now()
	var toDelete []string

	f.sessionsMu.RLock()
	for key, sess := range f.sessions {
		if now.Sub(sess.lastActive) > udpSessionTTL {
			toDelete = append(toDelete, key)
		}
	}
	f.sessionsMu.RUnlock()

	if len(toDelete) == 0 {
		return
	}

	f.sessionsMu.Lock()
	for _, key := range toDelete {
		if sess, ok := f.sessions[key]; ok {
			sess.targetConn.Close()
			delete(f.sessions, key)
		}
	}
	f.sessionsMu.Unlock()
}

func (f *UDPForwarder) GetAndResetTraffic() (upload, download int64) {
	upload = atomic.SwapInt64(&f.uploadBytes, 0)
	download = atomic.SwapInt64(&f.downloadBytes, 0)
	return
}
