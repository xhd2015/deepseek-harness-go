package session

// TurnWindow asks the page for at least this many messages and turns.
type TurnWindow struct {
	MinMessages int `json:"minMessages"`
	MinTurns    int `json:"minTurns"`
}

// PageRequest selects a suffix of the durable log.
type PageRequest struct {
	// ThroughSeq is the newest seq the page may include.
	ThroughSeq int64
	// BeforeSeq, when set, is an exclusive upper bound (older pages).
	BeforeSeq *int64
	// MaxMessages bounds the number of message records; zero means unbounded.
	MaxMessages int
	// TurnWindow widens the window to whole turns on request.
	TurnWindow *TurnWindow
}

// Page is one message-aligned slice of the durable log.
type Page struct {
	Records []Event
	HasMore bool
}

// PageEvents slices events to at most maxMessages messages, then widens the
// slice to whole turns. A page never cuts a turn in half, so the client can
// render every record it receives.
func PageEvents(events []Event, req PageRequest) Page {
	candidates := make([]Event, 0, len(events))
	for _, event := range events {
		if event.Seq > req.ThroughSeq {
			continue
		}
		if req.BeforeSeq != nil && event.Seq >= *req.BeforeSeq {
			continue
		}
		candidates = append(candidates, event)
	}
	if len(candidates) == 0 {
		return Page{Records: []Event{}}
	}

	start := 0
	if req.MaxMessages > 0 {
		messages := 0
		index := len(candidates)
		for index > 0 {
			index--
			if isMessageEvent(candidates[index]) {
				messages++
				if messages >= req.MaxMessages {
					break
				}
			}
		}
		start = alignToTurnStart(candidates, index)
	}

	if window := req.TurnWindow; window != nil {
		for start > 0 {
			if countMessages(candidates[start:]) >= window.MinMessages && countTurns(candidates[start:]) >= window.MinTurns {
				break
			}
			// Include the previous whole turn.
			previous := start - 1
			start = alignToTurnStart(candidates, previous)
		}
	}

	return Page{Records: candidates[start:], HasMore: start > 0}
}

// alignToTurnStart moves index back to the first event of its turn, or to the
// first event of the log when the index sits before the first turn.
func alignToTurnStart(events []Event, index int) int {
	for index > 0 && events[index-1].Type != EventTurnEnd {
		index--
	}
	return index
}

func isMessageEvent(event Event) bool {
	if event.SurfaceOp == "" {
		return false
	}
	switch event.Type {
	case EventUserMessage, EventAssistantMessage, "developer/message", "system/message", "tool/result":
		return true
	default:
		return false
	}
}

func countMessages(events []Event) int {
	total := 0
	for _, event := range events {
		if isMessageEvent(event) {
			total++
		}
	}
	return total
}

func countTurns(events []Event) int {
	total := 0
	for _, event := range events {
		if event.Type == EventTurnStart {
			total++
		}
	}
	return total
}
