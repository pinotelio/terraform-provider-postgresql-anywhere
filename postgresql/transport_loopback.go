package postgresql

import (
	"fmt"
	"net"
	"sync"
)

// loopbackTunnel is a local 127.0.0.1 listener that hands each accepted
// connection to a per-connection handler. It is the shared plumbing for
// transports that open one upstream stream per client connection (GCP IAP,
// Azure Bastion), mirroring how the SSM tunnel opens one session per connection.
type loopbackTunnel struct {
	listener  net.Listener
	localHost string
	localPort int
	closeOnce sync.Once
	closed    chan struct{}
}

// startLoopbackTunnel opens a loopback listener and serves each accepted
// connection with handle. localPort 0 lets the OS choose a free port.
func startLoopbackTunnel(localPort int, handle func(local net.Conn, closed <-chan struct{})) (*loopbackTunnel, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		return nil, err
	}
	t := &loopbackTunnel{
		listener:  ln,
		localHost: "127.0.0.1",
		localPort: ln.Addr().(*net.TCPAddr).Port,
		closed:    make(chan struct{}),
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handle(conn, t.closed)
		}
	}()
	return t, nil
}

func (t *loopbackTunnel) close() error {
	var err error
	t.closeOnce.Do(func() {
		close(t.closed)
		err = t.listener.Close()
	})
	return err
}
