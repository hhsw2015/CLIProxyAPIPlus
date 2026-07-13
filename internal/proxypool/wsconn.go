package proxypool

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsConn wraps a WebSocket connection to implement net.Conn.
// Data is tunneled through binary frames.
type wsConn struct {
	ws     *websocket.Conn
	name   string
	target string

	readMu  sync.Mutex
	reader  io.Reader // current frame reader
	writeMu sync.Mutex

	closed chan struct{}
	once   sync.Once

	// Keepalive
	pingDone chan struct{}
}

func newWSConn(ws *websocket.Conn, name, target string) *wsConn {
	c := &wsConn{
		ws:       ws,
		name:     name,
		target:   target,
		closed:   make(chan struct{}),
		pingDone: make(chan struct{}),
	}
	go c.keepAlive()
	return c
}

func (c *wsConn) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	for {
		if c.reader != nil {
			n, err := c.reader.Read(b)
			if n > 0 {
				return n, nil
			}
			if err != io.EOF {
				return 0, err
			}
			c.reader = nil
		}

		mt, r, err := c.ws.NextReader()
		if err != nil {
			return 0, err
		}
		if mt == websocket.TextMessage {
			// Control message from tunnel (e.g. "CLOSE")
			msg, _ := io.ReadAll(r)
			if string(msg) == "CLOSE" {
				return 0, io.EOF
			}
			continue
		}
		c.reader = r
	}
}

func (c *wsConn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ws.WriteMessage(websocket.BinaryMessage, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *wsConn) Close() error {
	c.once.Do(func() {
		close(c.closed)
		c.writeMu.Lock()
		c.ws.WriteMessage(websocket.TextMessage, []byte("CLOSE"))
		c.writeMu.Unlock()
		c.ws.Close()
	})
	return nil
}

func (c *wsConn) keepAlive() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.writeMu.Lock()
			c.ws.WriteMessage(websocket.PingMessage, nil)
			c.writeMu.Unlock()
		case <-c.closed:
			return
		}
	}
}

// net.Conn interface stubs
func (c *wsConn) LocalAddr() net.Addr                { return wsAddr{c.name} }
func (c *wsConn) RemoteAddr() net.Addr               { return wsAddr{c.target} }
func (c *wsConn) SetDeadline(t time.Time) error      { return nil }
func (c *wsConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return nil }

type wsAddr struct{ s string }

func (a wsAddr) Network() string { return "ech-ws" }
func (a wsAddr) String() string  { return a.s }
