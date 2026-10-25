package session

import "encoding/json"

// ProjectionBaseline mirrors SessionProjectionBaseline: the projection values
// at one exact event cursor.
type ProjectionBaseline struct {
	AsOfSeq int64          `json:"asOfSeq"`
	Values  map[string]any `json:"values"`
}

// Baseline folds the durable events into projection values. Values the client
// may omit are omitted here too rather than guessed.
func Baseline(header Header, events []Event) ProjectionBaseline {
	asOfSeq := int64(-1)
	if len(events) > 0 {
		asOfSeq = events[len(events)-1].Seq
	}
	values := map[string]any{}

	entry := summarize(header, events)
	if entry.Title != "" {
		values["title"] = entry.Title
	}
	var lastPromptAt any
	if entry.LastPromptAt != nil {
		lastPromptAt = *entry.LastPromptAt
	}
	values["sessionListMetadata"] = map[string]any{
		"blank":        entry.Blank,
		"lastPromptAt": lastPromptAt,
	}
	// The projection face always exists: a Session with no selection yet still
	// reports the fold's initial view. The web client treats a missing face as
	// "projections are still loading" and never enables the composer.
	var next any
	if entry.ModelSelection != nil {
		next = entry.ModelSelection
	}
	values["modelSelection"] = map[string]any{
		// dsh-go records selections but not the request header that consumes
		// one, so no selection has become "last used" yet.
		"lastUsed": nil,
		"next":     next,
	}
	values["todos"] = todosProjection(events)
	return ProjectionBaseline{AsOfSeq: asOfSeq, Values: values}
}

// SummaryProjections returns the list-row hint block for a cold session.
func SummaryProjections(entry ListEntry) map[string]any {
	values := map[string]any{
		"title": entry.DisplayTitle(),
	}
	var lastPromptAt any
	if entry.LastPromptAt != nil {
		lastPromptAt = *entry.LastPromptAt
	}
	values["sessionListMetadata"] = map[string]any{
		"blank":        entry.Blank,
		"lastPromptAt": lastPromptAt,
	}
	var next any
	if entry.ModelSelection != nil {
		next = entry.ModelSelection
	}
	values["modelSelection"] = map[string]any{
		"lastUsed": nil,
		"next":     next,
	}
	return map[string]any{
		"kind":    "cached",
		"asOfSeq": entry.LastSeq,
		"values":  values,
	}
}

// todosProjection folds the most recent todo/write snapshot. A new turn clears
// the list until the agent writes it again; the finished checklist remains
// visible after turn/end.
func todosProjection(events []Event) any {
	var value any
	for _, event := range events {
		switch event.Type {
		case EventTurnStart:
			value = nil
		case EventTodoWrite:
			var data struct {
				Todos any `json:"todos"`
			}
			if err := json.Unmarshal(event.Data, &data); err == nil {
				value = data.Todos
			}
		}
	}
	return value
}
