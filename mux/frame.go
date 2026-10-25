// Package mux implements the DeepSeek Harness remote stream multiplexing
// protocol: one WebSocket that carries many logical, independently cancelled
// request/response streams.
//
// The wire contract mirrors packages/api/gateway/src/stream-protocol.ts in the
// original TypeScript implementation:
//
//	client → host  {type:"open", streamId, endpoint, payload}
//	               {type:"item", streamId, value?}
//	               {type:"end", streamId}
//	               {type:"cancel", streamId}
//	host → client  {type:"item", streamId, value?}
//	               {type:"error", streamId, error:{code, message, details}}
//	               {type:"end", streamId}
package mux

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Client-to-host frame types.
const (
	FrameOpen   = "open"
	FrameItem   = "item"
	FrameEnd    = "end"
	FrameCancel = "cancel"
)

// Host-to-client frame types.
const (
	FrameError = "error"
)

const (
	maxStreamIDLength = 256
	maxEndpointLength = 512
)

// ClientFrame is one validated client-to-host frame.
type ClientFrame struct {
	// Type is one of FrameOpen, FrameItem, FrameEnd, FrameCancel.
	Type string
	// StreamID identifies the logical stream this frame belongs to.
	StreamID string
	// Endpoint is the host method an open frame asks for; empty otherwise.
	Endpoint string
	// Payload is the raw JSON payload of an open frame; nil otherwise.
	Payload json.RawMessage
	// Value is the raw JSON uplink value of an item frame; nil when absent.
	Value json.RawMessage
}

// ParseClientFrame decodes and validates one client text frame. Every frame
// must carry exactly the keys its type allows, matching the strict parser used
// by the TypeScript gateway.
func ParseClientFrame(text []byte) (ClientFrame, error) {
	fields, err := decodeObject(text)
	if err != nil {
		return ClientFrame{}, err
	}
	typ, err := takeString(fields, "type")
	if err != nil {
		return ClientFrame{}, err
	}
	frame := ClientFrame{Type: typ}
	if frame.StreamID, err = takeString(fields, "streamId"); err != nil {
		return ClientFrame{}, err
	}
	if frame.StreamID == "" {
		return ClientFrame{}, fmt.Errorf("mux: frame streamId must not be empty")
	}
	if len(frame.StreamID) > maxStreamIDLength {
		return ClientFrame{}, fmt.Errorf("mux: frame streamId is too long")
	}
	switch typ {
	case FrameOpen:
		if frame.Endpoint, err = takeString(fields, "endpoint"); err != nil {
			return ClientFrame{}, err
		}
		if frame.Endpoint == "" {
			return ClientFrame{}, fmt.Errorf("mux: open frame endpoint must not be empty")
		}
		if len(frame.Endpoint) > maxEndpointLength {
			return ClientFrame{}, fmt.Errorf("mux: open frame endpoint is too long")
		}
		payload, ok := take(fields, "payload")
		if !ok {
			return ClientFrame{}, fmt.Errorf("mux: open frame requires a payload")
		}
		frame.Payload = payload
	case FrameItem:
		if value, ok := take(fields, "value"); ok {
			frame.Value = value
		}
	case FrameEnd, FrameCancel:
	default:
		return ClientFrame{}, fmt.Errorf("mux: unknown frame type %q", typ)
	}
	if extra := firstKey(fields); extra != "" {
		return ClientFrame{}, fmt.Errorf("mux: unexpected %q frame field %q", typ, extra)
	}
	return frame, nil
}

// Failure is the structured error an `error` frame carries.
type Failure struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// Error implements error so a Failure can travel through ordinary Go error
// returns and still be recognized by AsFailure.
func (f *Failure) Error() string {
	if f == nil {
		return ""
	}
	if f.Message == "" {
		return f.Code
	}
	return f.Code + ": " + f.Message
}

// AsFailure reports whether err carries a wire failure.
func AsFailure(err error) (*Failure, bool) {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure, true
	}
	return nil, false
}

// ServerFrame is one host-to-client frame.
type ServerFrame struct {
	// Type is one of FrameItem, FrameEnd, FrameError.
	Type string
	// StreamID identifies the logical stream this frame belongs to.
	StreamID string
	// Value is the raw JSON value of an item frame.
	Value json.RawMessage
	// Failure is set for error frames.
	Failure *Failure
}

// ItemFrame builds an item frame carrying value.
func ItemFrame(streamID string, value json.RawMessage) ServerFrame {
	return ServerFrame{Type: FrameItem, StreamID: streamID, Value: value}
}

// EndFrame builds a stream end frame.
func EndFrame(streamID string) ServerFrame {
	return ServerFrame{Type: FrameEnd, StreamID: streamID}
}

// ErrorFrame builds a stream failure frame.
func ErrorFrame(streamID string, failure *Failure) ServerFrame {
	return ServerFrame{Type: FrameError, StreamID: streamID, Failure: failure}
}

// Encode renders the frame as JSON text.
func (f ServerFrame) Encode() ([]byte, error) {
	switch f.Type {
	case FrameItem:
		value := f.Value
		if len(value) == 0 {
			value = json.RawMessage("null")
		}
		if !json.Valid(value) {
			return nil, fmt.Errorf("mux: item frame value is not valid JSON")
		}
		return json.Marshal(struct {
			Type     string          `json:"type"`
			StreamID string          `json:"streamId"`
			Value    json.RawMessage `json:"value"`
		}{f.Type, f.StreamID, value})
	case FrameEnd:
		return json.Marshal(struct {
			Type     string `json:"type"`
			StreamID string `json:"streamId"`
		}{f.Type, f.StreamID})
	case FrameError:
		if f.Failure == nil {
			return nil, fmt.Errorf("mux: error frame requires a failure")
		}
		details := f.Failure.Details
		if details == nil {
			// The browser client requires a plain object for details.
			details = map[string]any{}
		}
		return json.Marshal(struct {
			Type     string `json:"type"`
			StreamID string `json:"streamId"`
			Error    struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}{Type: f.Type, StreamID: f.StreamID, Error: struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		}{f.Failure.Code, f.Failure.Message, details}})
	default:
		return nil, fmt.Errorf("mux: cannot encode frame type %q", f.Type)
	}
}

// ParseServerFrame decodes one host text frame. It exists for tests and for
// tools that speak the protocol as a client.
func ParseServerFrame(text []byte) (ServerFrame, error) {
	fields, err := decodeObject(text)
	if err != nil {
		return ServerFrame{}, err
	}
	typ, err := takeString(fields, "type")
	if err != nil {
		return ServerFrame{}, err
	}
	frame := ServerFrame{Type: typ}
	if frame.StreamID, err = takeString(fields, "streamId"); err != nil {
		return ServerFrame{}, err
	}
	switch typ {
	case FrameItem:
		if frame.Value, err = takeRaw(fields, "value"); err != nil {
			return ServerFrame{}, err
		}
	case FrameEnd:
	case FrameError:
		failure, err := takeFailure(fields)
		if err != nil {
			return ServerFrame{}, err
		}
		frame.Failure = failure
	default:
		return ServerFrame{}, fmt.Errorf("mux: unknown frame type %q", typ)
	}
	if extra := firstKey(fields); extra != "" {
		return ServerFrame{}, fmt.Errorf("mux: unexpected %q frame field %q", typ, extra)
	}
	return frame, nil
}

func decodeObject(text []byte) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(text)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("mux: frame must be a JSON object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return nil, fmt.Errorf("mux: malformed frame JSON: %w", err)
	}
	return fields, nil
}

func take(fields map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	value, ok := fields[key]
	if ok {
		delete(fields, key)
	}
	return value, ok
}

func takeString(fields map[string]json.RawMessage, key string) (string, error) {
	raw, ok := take(fields, key)
	if !ok {
		return "", fmt.Errorf("mux: frame field %q is required", key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("mux: frame field %q must be a string", key)
	}
	return value, nil
}

func takeRaw(fields map[string]json.RawMessage, key string) (json.RawMessage, error) {
	raw, ok := take(fields, key)
	if !ok {
		return nil, fmt.Errorf("mux: frame field %q is required", key)
	}
	return raw, nil
}

func takeFailure(fields map[string]json.RawMessage) (*Failure, error) {
	raw, err := takeRaw(fields, "error")
	if err != nil {
		return nil, err
	}
	var wire struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("mux: error frame error must be an object: %w", err)
	}
	if wire.Code == "" {
		return nil, fmt.Errorf("mux: error frame error.code is required")
	}
	return &Failure{Code: wire.Code, Message: wire.Message, Details: wire.Details}, nil
}

func firstKey(fields map[string]json.RawMessage) string {
	if len(fields) == 0 {
		return ""
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys[0]
}

// UplinkError reports the wire failure behind err, defaulting to a generic
// host error when err carries none.
func UplinkError(err error) *Failure {
	if failure, ok := AsFailure(err); ok {
		return failure
	}
	message := "internal error"
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		message = strings.TrimSpace(err.Error())
	}
	return &Failure{Code: "gateway/internal", Message: message}
}
