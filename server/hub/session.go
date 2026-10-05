package hub

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/jhyoong/KumaBoard/proto"
)

// Session is one live agent connection.
type Session struct {
	ID         string
	DeviceID   int64
	DeviceName string

	conn      *websocket.Conn
	writeMu   sync.Mutex
	closeOnce sync.Once
	closed    chan struct{}
	pongs     atomic.Uint64 // pongs received; compared by count, not wall-clock time
	log       *slog.Logger
}

// Send writes one envelope. Safe for concurrent use.
func (s *Session) Send(env *proto.Envelope) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeEnvelope(ctx, s.conn, env)
}

func (s *Session) close(code websocket.StatusCode, reason string) {
	s.closeOnce.Do(func() {
		close(s.closed)
		// Run in a goroutine because conn.Close performs the full close
		// handshake (write close frame, wait for peer response) and will
		// block if a concurrent Read is in progress on the same conn.
		go s.conn.Close(code, reason)
	})
}

// Closed is closed when the session ends.
func (s *Session) Closed() <-chan struct{} { return s.closed }

// pinger sends a ping every interval and closes the session if no pong
// arrives within pongTimeout of that ping. afterFunc schedules the deadline
// check (time.AfterFunc outside tests).
func (s *Session) pinger(ctx context.Context, interval, pongTimeout time.Duration, afterFunc func(time.Duration, func())) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case <-t.C:
			// Snapshot the pong count before sending: any pong counted
			// after this point was received after the ping went out.
			seen := s.pongs.Load()
			env, _ := proto.New(proto.TypePing, nil)
			if err := s.Send(env); err != nil {
				s.close(websocket.StatusGoingAway, "ping write failed")
				return
			}
			afterFunc(pongTimeout, func() {
				if s.pongs.Load() == seen {
					s.log.Warn("pong timeout", "device", s.DeviceName)
					s.close(websocket.StatusGoingAway, "pong timeout")
				}
			})
		}
	}
}

func (s *Session) readLoop(ctx context.Context, h *Hub) {
	for {
		env, err := readEnvelope(ctx, s.conn)
		if err != nil {
			return
		}
		h.handleMessage(ctx, s, env)
	}
}

func readEnvelope(ctx context.Context, conn *websocket.Conn) (*proto.Envelope, error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	return proto.Decode(data)
}

func writeEnvelope(ctx context.Context, conn *websocket.Conn, env *proto.Envelope) error {
	b, err := proto.Encode(env)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}
