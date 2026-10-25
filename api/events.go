package api

import "sync"

// eventBuffer is how many forwarded events may queue for one browser before
// the stream is closed and the client reconnects with a fresh baseline.
const eventBuffer = 64

// eventHub fans one Gateway event channel out to every open browser.
type eventHub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func newEventHub() *eventHub {
	return &eventHub{subs: map[chan []byte]struct{}{}}
}

// subscribe registers one forwarded-event stream.
func (h *eventHub) subscribe() chan []byte {
	feed := make(chan []byte, eventBuffer)
	h.mu.Lock()
	h.subs[feed] = struct{}{}
	h.mu.Unlock()
	return feed
}

// unsubscribe removes and closes one feed. It is safe to call more than once.
func (h *eventHub) unsubscribe(feed chan []byte) {
	h.mu.Lock()
	if _, ok := h.subs[feed]; ok {
		delete(h.subs, feed)
		close(feed)
	}
	h.mu.Unlock()
}

// publish delivers one encoded frame to every subscriber. A subscriber that
// cannot keep up is closed, which makes its stream end so the browser
// reconnects and reloads a baseline instead of waiting behind stale state.
func (h *eventHub) publish(frame []byte) {
	h.mu.Lock()
	for feed := range h.subs {
		select {
		case feed <- frame:
		default:
			delete(h.subs, feed)
			close(feed)
		}
	}
	h.mu.Unlock()
}
