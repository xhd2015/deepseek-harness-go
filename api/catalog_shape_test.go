package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestModelCatalogPassesAgentProValidation reproduces the checks agent-pro runs on
// session/modelCatalog and session/refreshModelCatalog.
//
// A shape that "looks right" is not enough: agent-pro rejects a catalog whose
// default selection is incomplete, whose groups are absent, or that repeats a
// provider or model id — and a rejected catalog leaves its operator with no model
// picker at all.
func TestModelCatalogPassesAgentProValidation(t *testing.T) {
	f := newFixture(t)
	for _, method := range []string{"session/modelCatalog", "session/refreshModelCatalog"} {
		value := f.invoke(t, method, map[string]any{}).mustSucceed(t)
		var catalog struct {
			Default           map[string]string `json:"default"`
			RoutableProviders []string          `json:"routableProviders"`
			Groups            []struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				Models []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"models"`
			} `json:"groups"`
			Failures []map[string]any `json:"failures"`
		}
		if err := json.Unmarshal(value, &catalog); err != nil {
			t.Fatalf("%s: %v", method, err)
		}

		// normalizeModelSelection: both halves present.
		if strings.TrimSpace(catalog.Default["provider"]) == "" ||
			strings.TrimSpace(catalog.Default["model"]) == "" {
			t.Fatalf("%s: default selection is incomplete: %v", method, catalog.Default)
		}
		// `groups == nil` is rejected outright, so an empty catalog must still send
		// an empty array rather than omitting the field.
		if !jsonHasGroups(t, value) {
			t.Fatalf("%s: groups is missing or null", method)
		}
		seenProviders := map[string]bool{}
		for _, group := range catalog.Groups {
			if strings.TrimSpace(group.ID) == "" || seenProviders[group.ID] {
				t.Fatalf("%s: invalid or repeated provider group %q", method, group.ID)
			}
			seenProviders[group.ID] = true
			seenModels := map[string]bool{}
			for _, model := range group.Models {
				if strings.TrimSpace(model.ID) == "" || seenModels[model.ID] {
					t.Fatalf("%s: invalid or repeated model %q in %q", method, model.ID, group.ID)
				}
				seenModels[model.ID] = true
			}
		}
		// failures is always an array, never null: an operator reading the reply
		// must be able to iterate it.
		if !jsonHasFailures(t, value) {
			t.Fatalf("%s: failures is missing or null", method)
		}
	}
}

// jsonHasGroups reports whether groups is present as an array.
func jsonHasGroups(t *testing.T, value []byte) bool {
	t.Helper()
	var probe struct {
		Groups json.RawMessage `json:"groups"`
	}
	if err := json.Unmarshal(value, &probe); err != nil {
		return false
	}
	return len(probe.Groups) > 0 && probe.Groups[0] == '['
}

// jsonHasFailures reports whether failures is present as an array.
func jsonHasFailures(t *testing.T, value []byte) bool {
	t.Helper()
	var probe struct {
		Failures json.RawMessage `json:"failures"`
	}
	if err := json.Unmarshal(value, &probe); err != nil {
		return false
	}
	return len(probe.Failures) > 0 && probe.Failures[0] == '['
}
