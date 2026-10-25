package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xhd2015/deepseek-harness-go/session"
)

type todoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

func (e *ToolExecutor) todoWrite(inv ToolInvocation, args map[string]any) ToolOutcome {
	raw, ok := args["todos"]
	if !ok {
		return ToolOutcome{Text: "todos must be an array", IsError: true}
	}
	items, ok := raw.([]any)
	if !ok {
		return ToolOutcome{Text: "todos must be an array", IsError: true}
	}
	todos := make([]todoItem, 0, len(items))
	seen := map[string]bool{}
	active := 0
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			return ToolOutcome{Text: "each todo must be an object", IsError: true}
		}
		contentValue, ok := item["content"]
		content, textOK := contentValue.(string)
		if !ok || !textOK {
			return ToolOutcome{Text: "invalid todo: `content` must be a non-empty string", IsError: true}
		}
		content = strings.TrimSpace(content)
		if content == "" {
			return ToolOutcome{Text: "invalid todo: `content` must be a non-empty string", IsError: true}
		}
		statusValue, ok := item["status"]
		status, statusOK := statusValue.(string)
		if !ok || !statusOK {
			return ToolOutcome{Text: "invalid todo: `status` must be pending, in_progress, or completed", IsError: true}
		}
		switch status {
		case "pending", "in_progress", "completed":
		default:
			return ToolOutcome{Text: "invalid todo: `status` must be pending, in_progress, or completed", IsError: true}
		}
		encoded, _ := json.Marshal(content)
		if seen[content] {
			return ToolOutcome{Text: fmt.Sprintf("invalid todos: duplicate content %s", encoded), IsError: true}
		}
		seen[content] = true
		if status == "in_progress" {
			active++
		}
		todos = append(todos, todoItem{Content: content, Status: status})
	}
	if inv.Emit == nil {
		return ToolOutcome{Text: "todo_write requires an owning agent session", IsError: true}
	}
	if err := inv.Emit(session.EventTodoWrite, map[string]any{"todos": todos}); err != nil {
		return ToolOutcome{Text: "failed to write todo list: " + err.Error(), IsError: true}
	}
	counts := map[string]int{"pending": 0, "inProgress": 0, "completed": 0}
	for _, todo := range todos {
		switch todo.Status {
		case "pending":
			counts["pending"]++
		case "in_progress":
			counts["inProgress"]++
		case "completed":
			counts["completed"]++
		}
	}
	return ToolOutcome{
		Text: fmt.Sprintf("Updated todo list: %d pending, %d in progress, %d completed.",
			counts["pending"], counts["inProgress"], counts["completed"]),
		Present: map[string]any{"card": "generic", "title": "Update todo list", "kind": "other", "rawInput": todos},
	}
}
