package session

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

// SurfaceOp is one DeepSeek Harness surface marker: either the literal
// "append" or a {op:"replace", startSeq, endSeq} range.
type SurfaceOp struct {
	Replace  bool
	StartSeq int64
	EndSeq   int64
}

// SplitSurface is the op DSH writes onto ordinary appended surface events.
var SplitSurface = &SurfaceOp{}

// IsReplace reports whether this op replaces an existing surface range.
func (op *SurfaceOp) IsReplace() bool { return op != nil && op.Replace }

// MarshalJSON emits the exact DSH wire form.
func (op *SurfaceOp) MarshalJSON() ([]byte, error) {
	if op == nil {
		return []byte("null"), nil
	}
	if op.Replace {
		return json.Marshal(map[string]any{
			"op":       "replace",
			"startSeq": op.StartSeq,
			"endSeq":   op.EndSeq,
		})
	}
	return []byte(`"append"`), nil
}

// UnmarshalJSON accepts both DSH surface marker forms.
func (op *SurfaceOp) UnmarshalJSON(raw []byte) error {
	if len(raw) == 0 || string(raw) == "null" {
		*op = SurfaceOp{}
		return nil
	}
	if raw[0] == '"' {
		var kind string
		if err := json.Unmarshal(raw, &kind); err != nil {
			return err
		}
		if kind != "append" {
			return fmt.Errorf("session: unsupported surfaceOp %q", kind)
		}
		*op = SurfaceOp{}
		return nil
	}
	var value struct {
		Op       string `json:"op"`
		StartSeq int64  `json:"startSeq"`
		EndSeq   int64  `json:"endSeq"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if value.Op != "replace" {
		return fmt.Errorf("session: unsupported surfaceOp object %q", value.Op)
	}
	*op = SurfaceOp{Replace: true, StartSeq: value.StartSeq, EndSeq: value.EndSeq}
	return nil
}

// SurfaceType reports whether DSH treats this event type as part of the
// model-visible surface.
func SurfaceType(typ string) bool { return SurfaceEventTypes[typ] }

var headerKeys = map[string]bool{
	"type": true, "version": true, "id": true, "createdAt": true,
	"cwd": true, "parentSession": true, "isSeeded": true, "origin": true,
	"delegationDepth": true, "agentPreset": true,
}

// DecodeHeader parses and validates one physical header line.
func DecodeHeader(raw []byte) (Header, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Header{}, fmt.Errorf("session: malformed header: %w", err)
	}
	for key := range fields {
		if !headerKeys[key] {
			return Header{}, fmt.Errorf("session: unsupported header field %q", key)
		}
	}
	for _, key := range []string{"type", "version", "id", "createdAt", "isSeeded", "delegationDepth"} {
		if _, ok := fields[key]; !ok {
			return Header{}, fmt.Errorf("session: header is missing required field %q", key)
		}
	}
	var header Header
	if err := json.Unmarshal(raw, &header); err != nil {
		return Header{}, fmt.Errorf("session: malformed header: %w", err)
	}
	if header.Type != "session" {
		return Header{}, fmt.Errorf("session: %q is not a session header", header.Type)
	}
	if header.ID == "" {
		return Header{}, fmt.Errorf("session: header has an empty id")
	}
	if header.CreatedAt < 0 {
		return Header{}, fmt.Errorf("session: header createdAt must not be negative")
	}
	if header.DelegationDepth < 0 {
		return Header{}, fmt.Errorf("session: header delegationDepth must not be negative")
	}
	if header.Cwd != "" && !filepath.IsAbs(header.Cwd) {
		return Header{}, fmt.Errorf("session: header cwd %q is not absolute", header.Cwd)
	}
	if header.Origin != "" && header.Origin != "subagent" {
		return Header{}, fmt.Errorf("session: unsupported header origin %q", header.Origin)
	}
	return header, nil
}

// EncodeHeader marshals one physical header line.
func EncodeHeader(header Header) ([]byte, error) {
	return json.Marshal(header)
}

var envelopeKeys = map[string]bool{
	"type": true, "seq": true, "time": true, "data": true,
	"surfaceOp": true, "sourceEventSeqs": true, "ignorable": true,
}

// DecodeEvent parses and validates one physical event envelope.
func DecodeEvent(raw []byte, expectedSeq int64) (Event, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Event{}, fmt.Errorf("session: malformed event: %w", err)
	}
	for key := range fields {
		if !envelopeKeys[key] {
			return Event{}, fmt.Errorf("session: unsupported event envelope field %q", key)
		}
	}
	if _, ok := fields["data"]; !ok {
		return Event{}, fmt.Errorf("session: event at seq %d has no data", expectedSeq)
	}
	var event Event
	if err := json.Unmarshal(raw, &event); err != nil {
		return Event{}, fmt.Errorf("session: malformed event: %w", err)
	}
	if event.Type == "" {
		return Event{}, fmt.Errorf("session: event at seq %d has an empty type", expectedSeq)
	}
	if event.Seq != expectedSeq {
		return Event{}, fmt.Errorf("session: event seq %d is not contiguous (want %d)", event.Seq, expectedSeq)
	}
	if event.Seq < 0 {
		return Event{}, fmt.Errorf("session: event seq must not be negative")
	}
	return event, nil
}
