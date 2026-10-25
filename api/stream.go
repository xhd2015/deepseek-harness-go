package api

import (
	"context"
	"encoding/json"
	"io"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// stream adapts a pull function to mux.Stream.
type stream struct {
	next  func(ctx context.Context) ([]byte, error)
	close func()
}

func (s *stream) Next(ctx context.Context) ([]byte, error) { return s.next(ctx) }

func (s *stream) Close() {
	if s.close != nil {
		s.close()
	}
}

// valueStream replays fixed values, then ends.
func valueStream(values ...[]byte) *stream {
	index := 0
	return &stream{next: func(context.Context) ([]byte, error) {
		if index >= len(values) {
			return nil, io.EOF
		}
		value := values[index]
		index++
		return value, nil
	}}
}

// encodeValue marshals one downlink value.
func encodeValue(value any) ([]byte, error) {
	return json.Marshal(value)
}

// eventEntry is SessionEventEntry.
type eventEntry struct {
	Type  string        `json:"type"`
	Event session.Event `json:"event"`
}

// assistantFrameEntry is one live assistant-stream frame.
type assistantFrameEntry struct {
	Type  string                       `json:"type"`
	Frame session.AssistantStreamFrame `json:"frame"`
}

// recordOf wraps one durable event.
func recordOf(event session.Event) eventEntry {
	return eventEntry{Type: "event", Event: event}
}

// recordsOf wraps a window of durable events.
func recordsOf(events []session.Event) []eventEntry {
	records := make([]eventEntry, len(events))
	for index, event := range events {
		records[index] = recordOf(event)
	}
	return records
}

// sessionEventStream follows one session: one snapshot frame, then live events.
type sessionEventStream struct {
	snapshot []byte
	feed     <-chan session.FeedItem
	cancel   func()
	sent     bool
}

func (s *sessionEventStream) Next(ctx context.Context) ([]byte, error) {
	if !s.sent {
		s.sent = true
		return s.snapshot, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case item, ok := <-s.feed:
		if !ok {
			return nil, io.EOF
		}
		if item.Frame != nil {
			return encodeValue(assistantFrameEntry{Type: "assistant-stream", Frame: *item.Frame})
		}
		if item.Event != nil {
			return encodeValue(recordOf(*item.Event))
		}
		return nil, io.EOF
	}
}

func (s *sessionEventStream) Close() {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

// controlStream emits one projection baseline, then stays open until the
// client goes away.
type controlStream struct {
	baseline []byte
	sent     bool
}

func (s *controlStream) Next(ctx context.Context) ([]byte, error) {
	if !s.sent {
		s.sent = true
		return s.baseline, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *controlStream) Close() {}

// eventsStream is the Gateway-internal forwarded-event channel. It announces
// itself once, then forwards live Agent events such as running-state changes.
type eventsStream struct {
	ready  []byte
	feed   <-chan []byte
	cancel func()
	sent   bool
}

func (s *eventsStream) Next(ctx context.Context) ([]byte, error) {
	if !s.sent {
		s.sent = true
		return s.ready, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case frame, ok := <-s.feed:
		if !ok {
			return nil, io.EOF
		}
		return frame, nil
	}
}

func (s *eventsStream) Close() {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}
