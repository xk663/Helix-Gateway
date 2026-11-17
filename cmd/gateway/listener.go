package main

import (
	"net"
	"net/http"
	"sync"
	"time"
)

type idleWriteListener struct {
	net.Listener
	timeout time.Duration
}

func (l idleWriteListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &idleWriteConn{Conn: conn, timeout: l.timeout}, nil
}

type idleWriteConn struct {
	net.Conn
	timeout time.Duration
	mu      sync.Mutex
}

// Bound every socket write, including buffered HTTP output, and clear the
// deadline before waiting for upstream data or another request.
func (c *idleWriteConn) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	defer c.Conn.SetWriteDeadline(time.Time{})
	return c.Conn.Write(data)
}

func (c *idleWriteConn) CloseWrite() error {
	if tcp, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return tcp.CloseWrite()
	}
	return http.ErrNotSupported
}
