package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const writeTimeout = 10 * time.Second

// conn is one WebSocket. Frames go out through a single writer goroutine in
// the order they were queued; queueing never blocks, and a client that
// cannot keep up is disconnected (it can resume, the session keeps the
// frames).
type conn struct {
	ws   *websocket.Conn
	out  chan []byte
	done chan struct{}
	once sync.Once

	code   websocket.StatusCode
	reason string
}

func newConn(ws *websocket.Conn, queue int) *conn {
	return &conn{ws: ws, out: make(chan []byte, queue), done: make(chan struct{})}
}

// send queues a frame; when the queue is full the client is too slow.
func (c *conn) send(frame []byte) {
	select {
	case <-c.done:
	case c.out <- frame:
	default:
		c.kick(closeSlowConsumer, "slow consumer")
	}
}

// kick closes the connection with code after the frames already queued
// (except for a slow consumer, whose queue is the problem). The first call wins.
func (c *conn) kick(code websocket.StatusCode, reason string) {
	c.once.Do(func() {
		c.code, c.reason = code, reason
		close(c.done)
	})
}

// closeCode is the code the server closed with, 0 while open.
func (c *conn) closeCode() websocket.StatusCode {
	select {
	case <-c.done:
		return c.code
	default:
		return 0
	}
}

// writeLoop runs until the connection is kicked or a write fails.
func (c *conn) writeLoop(ctx context.Context) {
	for {
		select {
		case f := <-c.out:
			if err := c.write(ctx, f); err != nil {
				c.kick(closeUnknownError, "write failed")
				_ = c.ws.CloseNow()
				return
			}
		case <-c.done:
			if c.code != closeSlowConsumer {
				c.flush(ctx)
			}
			if c.ws.Close(c.code, c.reason) != nil {
				// e.g. a code that may not be sent, echoed from the client
				// (1005); make sure the socket is gone anyway.
				_ = c.ws.CloseNow()
			}
			return
		}
	}
}

func (c *conn) flush(ctx context.Context) {
	for {
		select {
		case f := <-c.out:
			if c.write(ctx, f) != nil {
				return
			}
		default:
			return
		}
	}
}

func (c *conn) write(ctx context.Context, f []byte) error {
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return c.ws.Write(wctx, websocket.MessageText, f)
}
