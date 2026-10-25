package api

import (
	"encoding/json"

	"github.com/xhd2015/deepseek-harness-go/modelconfig"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// modelCatalog answers session/modelCatalog with every route dsh-go can serve.
// The web client uses it to label and populate the composer model picker, so
// it must answer rather than report "unimplemented".
func (s *Service) modelCatalog(json.RawMessage) (any, error) {
	catalog := s.catalog()
	groups := make([]map[string]any, 0, len(catalog.Providers))
	for _, provider := range catalog.Providers {
		models := make([]map[string]any, 0, len(provider.Models))
		for _, model := range provider.Models {
			entry := map[string]any{
				"id":   model.ID,
				"name": model.Name,
			}
			if model.Description != "" {
				entry["description"] = model.Description
			}
			if len(model.Efforts) > 0 {
				efforts := make([]map[string]any, 0, len(model.Efforts))
				for _, effort := range model.Efforts {
					option := map[string]any{"id": effort.ID, "name": effort.Name}
					if effort.Description != "" {
						option["description"] = effort.Description
					}
					efforts = append(efforts, option)
				}
				reasoning := map[string]any{"efforts": efforts}
				if model.DefaultEffort != "" {
					reasoning["defaultEffort"] = model.DefaultEffort
				}
				entry["reasoning"] = reasoning
			}
			models = append(models, entry)
		}
		groups = append(groups, map[string]any{
			"id":     provider.ID,
			"name":   provider.Name,
			"models": models,
		})
	}
	providers := make([]string, 0, len(catalog.Providers))
	for _, provider := range catalog.Providers {
		providers = append(providers, provider.ID)
	}
	failures := catalog.Failures
	if failures == nil {
		failures = []modelconfig.Failure{}
	}
	return map[string]any{
		"default":           sessionSelection(catalog.Default),
		"routableProviders": providers,
		"groups":            groups,
		"failures":          failures,
	}, nil
}

// selectModel answers session/selectModel. The choice is recorded as a Session
// event so it survives reloads and drives the next turn.
func (s *Service) selectModel(args json.RawMessage) (any, error) {
	var request struct {
		SessionID       string `json:"sessionId"`
		Provider        string `json:"provider"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoningEffort"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	opened, err := s.openSession(request.SessionID)
	if err != nil {
		return nil, err
	}
	catalog := s.catalog()
	selection := currentSelection(opened, catalog.Default)
	switching := (request.Provider != "" && request.Provider != selection.Provider) ||
		(request.Model != "" && request.Model != selection.Model)
	if request.Provider != "" {
		selection.Provider = request.Provider
	}
	if request.Model != "" {
		selection.Model = request.Model
	}
	if request.ReasoningEffort != "" {
		selection.ReasoningEffort = request.ReasoningEffort
	} else if switching {
		selection.ReasoningEffort = ""
	}
	route, err := catalog.Resolve(selection)
	if err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	selected := route.Selection
	if _, err := opened.Append(session.EventModelSelection, session.ModelSelection{
		Provider:        selected.Provider,
		Model:           selected.Model,
		ReasoningEffort: selected.ReasoningEffort,
	}); err != nil {
		return nil, fail(CodeInternal, err.Error())
	}
	return map[string]any{"selected": sessionSelection(selected)}, nil
}

// defaultSelection is the model route every Session starts on.
func (s *Service) defaultSelection() session.ModelSelection {
	return sessionSelection(s.catalog().Default)
}

func (s *Service) catalog() *modelconfig.Catalog {
	if s.cfg.Catalog != nil {
		return s.cfg.Catalog
	}
	if s.cfg.Runner == nil {
		return modelconfig.Single("deepseek", "deepseek-chat", "")
	}
	return modelconfig.Single(
		s.cfg.Runner.Provider(),
		s.cfg.Runner.Model(),
		s.cfg.Runner.ReasoningEffort(),
	)
}

func sessionSelection(selection modelconfig.Selection) session.ModelSelection {
	return session.ModelSelection{
		Provider:        selection.Provider,
		Model:           selection.Model,
		ReasoningEffort: selection.ReasoningEffort,
	}
}

func currentSelection(sess *session.Session, fallback modelconfig.Selection) modelconfig.Selection {
	var (
		selection modelconfig.Selection
		seq       int64 = -1
	)
	for _, event := range sess.Events() {
		if event.Type != session.EventModelSelection || event.Seq <= seq {
			continue
		}
		var stored session.ModelSelection
		if err := json.Unmarshal(event.Data, &stored); err != nil {
			continue
		}
		selection = modelconfig.Selection{
			Provider:        stored.Provider,
			Model:           stored.Model,
			ReasoningEffort: stored.ReasoningEffort,
		}
		seq = event.Seq
	}
	if seq < 0 {
		return fallback
	}
	return selection
}
