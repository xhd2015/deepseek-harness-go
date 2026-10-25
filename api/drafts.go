package api

import (
	"encoding/json"
	"sync"
)

// drafts holds the composer text the client saves without submitting a prompt.
// The reference backend keeps these outside the transcript; this backend keeps
// them in memory, so a restart drops unsubmitted text but never the log.
type drafts struct {
	mu    sync.Mutex
	texts map[string]string
}

// get answers session/getDraft.
func (s *Service) getDraft(args json.RawMessage) (any, error) {
	var request struct {
		SessionID string `json:"sessionId"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	if _, err := s.openSession(request.SessionID); err != nil {
		return nil, err
	}
	return map[string]any{"text": s.drafts.get(request.SessionID)}, nil
}

// set answers session/setDraft. Empty text clears the record.
func (s *Service) setDraft(args json.RawMessage) (any, error) {
	var request struct {
		SessionID string `json:"sessionId"`
		Text      string `json:"text"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	if _, err := s.openSession(request.SessionID); err != nil {
		return nil, err
	}
	s.drafts.set(request.SessionID, request.Text)
	return map[string]any{"text": request.Text}, nil
}

func (d *drafts) get(sessionID string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.texts[sessionID]
}

func (d *drafts) set(sessionID, text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if text == "" {
		delete(d.texts, sessionID)
		return
	}
	if d.texts == nil {
		d.texts = map[string]string{}
	}
	d.texts[sessionID] = text
}
