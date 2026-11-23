package api

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// A waterfall is a question the host asks the client and waits on.
//
// Plain `emit` frames are one-way: a notification nobody can answer. An approval
// is the opposite — the host is blocked until the page decides — so it needs the
// waterfall shape DSH's protocol defines: a downlink `waterfall` frame carrying an
// `eventId`, and an uplink `$events/result` frame naming that same id. Without the
// uplink an approval would be asked and never answered.

// remoteEventResult is the client's answer to one waterfall delivery.
type remoteEventResult struct {
	ClientID string `json:"clientId"`
	EventID  string `json:"eventId"`
	Outcome  struct {
		Kind  string          `json:"kind"`
		Value json.RawMessage `json:"value"`
	} `json:"outcome"`
}

// waterfallGate carries pending questions between the host and the page.
type waterfallGate struct {
	mu      sync.Mutex
	pending map[string]chan json.RawMessage
	nextID  int64
}

// newWaterfallGate builds an empty gate.
func newWaterfallGate() *waterfallGate {
	return &waterfallGate{pending: map[string]chan json.RawMessage{}}
}

// ask registers a question and returns its id and the channel the answer arrives on.
//
// The id is minted here rather than by the caller: it is the correlation token both
// frames carry, and one place to mint it means one place to get wrong.
func (g *waterfallGate) ask() (string, chan json.RawMessage) {
	answer := make(chan json.RawMessage, 1)
	g.mu.Lock()
	g.nextID++
	id := fmt.Sprintf("remote-event-%d", g.nextID)
	g.pending[id] = answer
	g.mu.Unlock()
	return id, answer
}

// settle delivers one answer, reporting whether the question was still open.
func (g *waterfallGate) settle(eventID string, value json.RawMessage) bool {
	g.mu.Lock()
	answer, ok := g.pending[eventID]
	if ok {
		delete(g.pending, eventID)
	}
	g.mu.Unlock()
	if !ok {
		return false
	}
	answer <- value
	return true
}

// cancel drops a question that will never be answered.
func (g *waterfallGate) cancel(eventID string) {
	g.mu.Lock()
	delete(g.pending, eventID)
	g.mu.Unlock()
}

// requestWaterfall publishes one waterfall frame and waits for the client's answer.
//
// A deployment with no client attached gets no listener, so the frame is published
// to nobody and the wait would never end. That is what the timeout is for: the
// caller fails closed rather than hanging a turn on a page that is not there.
func (s *Service) requestWaterfall(event, agentID string, request map[string]any) (json.RawMessage, bool) {
	if s.events == nil {
		return nil, false
	}
	eventID, answer := s.waterfalls.ask()
	frame, err := encodeValue(map[string]any{
		"type":    "waterfall",
		"event":   event,
		"eventId": eventID,
		"agentId": agentID,
		"request": request,
	})
	if err != nil {
		s.waterfalls.cancel(eventID)
		s.logf("encode waterfall %q: %v", event, err)
		return nil, false
	}
	s.events.publish(frame)
	window := s.cfg.EventWait
	if window <= 0 {
		window = defaultEventWait
	}
	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case value := <-answer:
		return value, true
	case <-timer.C:
		// Nobody answered. Drop the registration so a late reply is rejected
		// instead of settling a question the host has already given up on.
		s.waterfalls.cancel(eventID)
		return nil, false
	}
}

// eventResult answers $events/result: the client's decision on one waterfall.
func (s *Service) eventResult(args json.RawMessage) (any, error) {
	var result remoteEventResult
	if err := decodeEitherShape(args, &result); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	if result.EventID == "" {
		return nil, fail(CodeBadRequest, "an eventId is required")
	}
	switch result.Outcome.Kind {
	case "result":
		// A listener that answered. Settling an unknown id means the question was
		// already settled or never existed; reporting success would let a stale page
		// believe it decided something.
		if !s.waterfalls.settle(result.EventID, result.Outcome.Value) {
			return nil, fail(CodeNotFound, "no waterfall is waiting for "+result.EventID)
		}
		return map[string]any{"ok": true}, nil
	case "next":
		// The listener declined to answer: the host keeps the question open for the
		// next listener rather than treating silence as consent.
		return map[string]any{"ok": true}, nil
	case "rejected":
		s.waterfalls.cancel(result.EventID)
		return map[string]any{"ok": true}, nil
	default:
		return nil, fail(CodeBadRequest, "unknown waterfall outcome "+result.Outcome.Kind)
	}
}

// defaultEventWait bounds a waterfall wait when the deployment sets no window.
const defaultEventWait = 5 * time.Minute
