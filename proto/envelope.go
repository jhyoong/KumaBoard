package proto

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// MaxMessageSize is the largest encoded envelope either side accepts.
const MaxMessageSize = 1 << 20

// ErrMessageTooLarge is returned when an encoded envelope exceeds MaxMessageSize.
var ErrMessageTooLarge = errors.New("proto: message exceeds 1 MiB")

// Envelope wraps every message on the control socket.
type Envelope struct {
	V       int             `json:"v"`
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	TS      time.Time       `json:"ts"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// New builds an envelope with a fresh ULID and the current time.
func New(typ string, payload any) (*Envelope, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("proto: marshal payload: %w", err)
		}
		raw = b
	}
	return &Envelope{
		V:       Version,
		ID:      ulid.Make().String(),
		Type:    typ,
		TS:      time.Now().UTC(),
		Payload: raw,
	}, nil
}

// Reply builds an envelope whose ID echoes the request so the two correlate.
func Reply(to *Envelope, typ string, payload any) (*Envelope, error) {
	e, err := New(typ, payload)
	if err != nil {
		return nil, err
	}
	e.ID = to.ID
	return e, nil
}

// Encode serialises an envelope and enforces MaxMessageSize.
func Encode(e *Envelope) ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("proto: encode: %w", err)
	}
	if len(b) > MaxMessageSize {
		return nil, ErrMessageTooLarge
	}
	return b, nil
}

// Decode parses an envelope and enforces MaxMessageSize.
// Callers must also set the WebSocket read limit to MaxMessageSize so an
// oversized frame is rejected before it is buffered in full.
func Decode(b []byte) (*Envelope, error) {
	if len(b) > MaxMessageSize {
		return nil, ErrMessageTooLarge
	}
	var e Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("proto: decode: %w", err)
	}
	if e.Type == "" {
		return nil, errors.New("proto: missing type")
	}
	return &e, nil
}

// Unmarshal decodes the payload into v.
func (e *Envelope) Unmarshal(v any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(e.Payload, v)
}
