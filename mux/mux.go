package mux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// DefaultHeartbeatInterval matches gateway.websocketHeartbeatIntervalMs.
	DefaultHeartbeatInterval = 2 * time.Second
	// DefaultMaxMissedHeartbeats matches MAX_MISSED_HEARTBEATS.
	DefaultMaxMissedHeartbeats = 2
	// DefaultInboxBytes matches gateway.streamInboxBytes.
	DefaultInboxBytes = 262_144

	writeTimeout   = 10 * time.Second
	controlTimeout = 2 * time.Second
	shutdownGrace  = 3 * time.Second
)

// Uplink carries values the client sends for one open stream. The channel is
// closed once the client ends the stream or the stream is cancelled.
type Uplink interface {
	Values() <-chan []byte
}

// Stream is one host-side logical stream. Next returns the next downlink
// value; returning io.EOF ends the stream normally. Streams are polled from a
// single goroutine, so Next needs no internal locking.
type Stream interface {
	Next(ctx context.Context) ([]byte, error)
	// Close releases the stream after cancellation or socket loss.
	Close()
}

// Opener starts the stream an open frame asks for. Returning an error fails
// the stream with an error frame; unknown endpoints should return a *Failure
// with a stable code.
type Opener func(ctx context.Context, endpoint string, payload []byte, uplink Uplink) (Stream, error)

// Server serves the multiplexing endpoint.
type Server struct {
	// Opener dispatches open frames; it is required.
	Opener Opener
	// Logf receives connection diagnostics; nil discards them.
	Logf func(format string, args ...any)
	// HeartbeatInterval defaults to DefaultHeartbeatInterval.
	HeartbeatInterval time.Duration
	// MaxMissedHeartbeats defaults to DefaultMaxMissedHeartbeats.
	MaxMissedHeartbeats int
	// InboxBytes defaults to DefaultInboxBytes.
	InboxBytes int
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Server) heartbeatInterval() time.Duration {
	if s.HeartbeatInterval > 0 {
		return s.HeartbeatInterval
	}
	return DefaultHeartbeatInterval
}

func (s *Server) maxMissedHeartbeats() int {
	if s.MaxMissedHeartbeats > 0 {
		return s.MaxMissedHeartbeats
	}
	return DefaultMaxMissedHeartbeats
}

func (s *Server) inboxBytes() int {
	if s.InboxBytes > 0 {
		return s.InboxBytes
	}
	return DefaultInboxBytes
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	// Admission is decided by the HTTP layer (loopback bind or token check).
	CheckOrigin: func(*http.Request) bool { return true },
}

// ServeHTTP upgrades one request into a multiplexing socket.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.Opener == nil {
		http.Error(w, "mux opener is not configured", http.StatusInternalServerError)
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an HTTP response.
		s.logf("upgrade failed: %v", err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	c := &connection{
		server:  s,
		ws:      ws,
		ctx:     ctx,
		cancel:  cancel,
		streams: map[string]*streamState{},
	}
	ws.SetPongHandler(func(string) error {
		c.missed.Store(0)
		return nil
	})
	go c.heartbeat()
	c.readFrames()
	c.shutdown()
}

// connection owns one physical socket and every logical stream on it.
type connection struct {
	server *Server
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	missed atomic.Int32

	writeMu sync.Mutex

	streamMu sync.Mutex
	streams  map[string]*streamState

	wg       sync.WaitGroup
	closeOne sync.Once
}

type streamState struct {
	id      string
	cancel  context.CancelFunc
	uplink  *uplinkState
	settled chan struct{}
}

func (c *connection) readFrames() {
	for {
		kind, data, err := c.ws.ReadMessage()
		if err != nil {
			if !errors.Is(err, io.EOF) && !isClosed(err) {
				c.server.logf("read failed: %v", err)
			}
			return
		}
		if kind != websocket.TextMessage {
			c.violation("binary frames are not supported")
			return
		}
		frame, err := ParseClientFrame(data)
		if err != nil {
			c.violation(err.Error())
			return
		}
		if err := c.dispatch(frame); err != nil {
			c.violation(err.Error())
			return
		}
	}
}

func (c *connection) dispatch(frame ClientFrame) error {
	switch frame.Type {
	case FrameOpen:
		return c.openStream(frame)
	case FrameItem:
		if st := c.lookup(frame.StreamID); st != nil && frame.Value != nil {
			st.uplink.push(frame.Value)
		}
		return nil
	case FrameEnd:
		if st := c.lookup(frame.StreamID); st != nil {
			st.uplink.close()
		}
		return nil
	case FrameCancel:
		if st := c.lookup(frame.StreamID); st != nil {
			st.cancel()
			st.uplink.close()
		}
		return nil
	default:
		return fmt.Errorf("unsupported frame type %q", frame.Type)
	}
}

func (c *connection) lookup(id string) *streamState {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	return c.streams[id]
}

func (c *connection) openStream(frame ClientFrame) error {
	c.streamMu.Lock()
	if _, exists := c.streams[frame.StreamID]; exists {
		c.streamMu.Unlock()
		return fmt.Errorf("duplicate remote stream id %q", frame.StreamID)
	}
	ctx, cancel := context.WithCancel(c.ctx)
	st := &streamState{
		id:      frame.StreamID,
		cancel:  cancel,
		uplink:  newUplink(c.server.inboxBytes()),
		settled: make(chan struct{}),
	}
	c.streams[frame.StreamID] = st
	c.streamMu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer close(st.settled)
		defer c.removeStream(frame.StreamID, st)

		src, err := c.server.Opener(ctx, frame.Endpoint, frame.Payload, st.uplink)
		if err != nil {
			c.failStream(frame.StreamID, err)
			return
		}
		defer src.Close()
		c.pump(ctx, frame.StreamID, src)
	}()
	return nil
}

func (c *connection) pump(ctx context.Context, id string, src Stream) {
	for {
		value, err := src.Next(ctx)
		if err == nil {
			value = defaultJSON(value)
			if sendErr := c.send(ItemFrame(id, value)); sendErr != nil {
				c.server.logf("write failed: %v", sendErr)
				c.cancel()
				return
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			if sendErr := c.send(EndFrame(id)); sendErr != nil {
				c.server.logf("write failed: %v", sendErr)
			}
			return
		}
		if ctx.Err() != nil {
			// The client cancelled; a cancelled stream sends nothing.
			return
		}
		c.failStream(id, err)
		return
	}
}

// failStream reports a stream failure. The TypeScript host sends only the
// error frame; the end frame keeps simple clients from waiting forever.
func (c *connection) failStream(id string, err error) {
	_ = c.send(ErrorFrame(id, UplinkError(err)))
	_ = c.send(EndFrame(id))
}

func (c *connection) removeStream(id string, st *streamState) {
	c.streamMu.Lock()
	current, ok := c.streams[id]
	if ok && current == st {
		delete(c.streams, id)
	}
	c.streamMu.Unlock()
	st.cancel()
	st.uplink.close()
}

func (c *connection) send(frame ServerFrame) error {
	data, err := frame.Encode()
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ws.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return c.ws.WriteMessage(websocket.TextMessage, data)
}

func (c *connection) heartbeat() {
	interval := c.server.heartbeatInterval()
	maxMissed := c.server.maxMissedHeartbeats()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if int(c.missed.Load()) >= maxMissed {
				c.violation("heartbeat timeout")
				return
			}
			c.missed.Add(1)
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(controlTimeout)); err != nil {
				c.violation("heartbeat write failed")
				return
			}
		}
	}
}

// violation closes the socket after a protocol error, mirroring the gateway
// policy that only a malformed frame or a duplicate stream id is fatal.
func (c *connection) violation(reason string) {
	_ = c.ws.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseProtocolError, truncate(reason, 120)),
		time.Now().Add(controlTimeout),
	)
	c.cancel()
	c.closeWebSocket()
}

func (c *connection) closeWebSocket() {
	c.closeOne.Do(func() {
		_ = c.ws.Close()
	})
}

// shutdown stops every logical stream before closing the socket so that no
// pump writes into a closed connection.
func (c *connection) shutdown() {
	c.cancel()
	settled := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(settled)
	}()
	select {
	case <-settled:
	case <-time.After(shutdownGrace):
		c.server.logf("closing socket with streams still running")
	}
	c.closeWebSocket()
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

func isClosed(err error) bool {
	return errors.Is(err, websocket.ErrCloseSent) || errors.Is(err, net.ErrClosed)
}

func defaultJSON(value []byte) []byte {
	if len(value) == 0 {
		return []byte("null")
	}
	return value
}

// uplinkState buffers client values for one stream.
type uplinkState struct {
	mu     sync.Mutex
	closed bool
	limit  int
	queued int
	ch     chan []byte
}

func newUplink(limit int) *uplinkState {
	return &uplinkState{limit: limit, ch: make(chan []byte, 64)}
}

func (u *uplinkState) Values() <-chan []byte { return u.ch }

func (u *uplinkState) push(value []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return
	}
	if u.limit > 0 && u.queued+len(value) > u.limit {
		return
	}
	select {
	case u.ch <- value:
		u.queued += len(value)
	default:
	}
}

func (u *uplinkState) close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return
	}
	u.closed = true
	close(u.ch)
}

// Logf is the default server logger.
func Logf(format string, args ...any) {
	log.Printf("[mux] "+format, args...)
}
