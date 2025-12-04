package forwarder

import (
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/easayliu/orris-client/internal/logger"
)

type TCPForwarder struct {
	listenPort    uint16
	targetAddress string
	targetPort    uint16
	listener      net.Listener
	done          chan struct{}
	wg            sync.WaitGroup

	uploadBytes   int64
	downloadBytes int64
}

func NewTCPForwarder(listenPort uint16, targetAddress string, targetPort uint16) *TCPForwarder {
	return &TCPForwarder{
		listenPort:    listenPort,
		targetAddress: targetAddress,
		targetPort:    targetPort,
		done:          make(chan struct{}),
	}
}

func (f *TCPForwarder) Start() error {
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

func (f *TCPForwarder) Stop() {
	close(f.done)
	if f.listener != nil {
		f.listener.Close()
	}
	f.wg.Wait()
}

func (f *TCPForwarder) acceptLoop() {
	defer f.wg.Done()

	for {
		conn, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.done:
				return
			default:
				logger.Error("tcp accept error", "error", err)
				continue
			}
		}

		f.wg.Add(1)
		go f.handleConnection(conn)
	}
}

func (f *TCPForwarder) handleConnection(clientConn net.Conn) {
	defer f.wg.Done()
	defer clientConn.Close()

	targetAddr := net.JoinHostPort(f.targetAddress, fmt.Sprintf("%d", f.targetPort))
	serverConn, err := net.Dial("tcp", targetAddr)
	if err != nil {
		logger.Error("tcp dial target failed", "target", targetAddr, "error", err)
		return
	}
	defer serverConn.Close()

	done := make(chan struct{})

	go func() {
		n, _ := io.Copy(serverConn, &countingReader{r: clientConn, counter: &f.uploadBytes})
		atomic.AddInt64(&f.uploadBytes, 0) // ensure visibility
		_ = n
		close(done)
	}()

	n, _ := io.Copy(clientConn, &countingReader{r: serverConn, counter: &f.downloadBytes})
	_ = n

	<-done
}

func (f *TCPForwarder) GetAndResetTraffic() (upload, download int64) {
	upload = atomic.SwapInt64(&f.uploadBytes, 0)
	download = atomic.SwapInt64(&f.downloadBytes, 0)
	return
}

type countingReader struct {
	r       io.Reader
	counter *int64
}

func (c *countingReader) Read(p []byte) (n int, err error) {
	n, err = c.r.Read(p)
	if n > 0 {
		atomic.AddInt64(c.counter, int64(n))
	}
	return
}
