package api

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/xhd2015/deepseek-harness-go/mux"
	"github.com/xhd2015/deepseek-harness-go/workspace"
)

// CodeWorkspaceNotFound reports a SessionCreateRequest naming an unregistered
// Workspace.
const CodeWorkspaceNotFound = "workspace/not-found"

// CodeWorkspaceInvalidPath reports a workspace/create path this backend cannot
// adopt.
const CodeWorkspaceInvalidPath = "workspace/invalid-path"

// workspaceView renders one WorkspaceView. Sessions are accounted to the
// Workspace by working directory, the only grouping this backend stores.
func (s *Service) workspaceView(item *workspace.Workspace) map[string]any {
	return map[string]any{
		"workspaceId": item.ID,
		"path":        item.Path,
		"title":       item.Title,
		"sessionIds":  s.workspaceSessionIDs(item),
		"createdAt":   item.CreatedAt.Format(time.RFC3339),
		"updatedAt":   item.UpdatedAt.Format(time.RFC3339),
	}
}

func (s *Service) workspaceSessionIDs(item *workspace.Workspace) []string {
	entries, err := s.cfg.Store.List()
	if err != nil {
		s.logf("list sessions for workspace %s: %v", item.ID, err)
		return []string{}
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if filepath.Clean(entry.Header.Cwd) == item.Path {
			ids = append(ids, entry.Header.ID)
		}
	}
	return ids
}

func (s *Service) workspaceViews() []map[string]any {
	items := s.Workspaces.List()
	views := make([]map[string]any, len(items))
	for index, item := range items {
		views[index] = s.workspaceView(item)
	}
	return views
}

// baselineFrame is WorkspaceFollowFrame `baseline`.
func (s *Service) workspaceBaseline() ([]byte, error) {
	return encodeValue(map[string]any{
		"type": "baseline",
		"value": map[string]any{
			"items":              s.workspaceViews(),
			"archivedSessionIds": []string{},
			"pinnedSessionIds":   []string{},
		},
	})
}

// initializeDefaultWorkspace answers workspace/initializeDefault: the default
// Workspace exists from startup, because this backend registers the directory
// it serves.
func (s *Service) initializeDefaultWorkspace(json.RawMessage) (any, error) {
	path := s.defaultCwd()
	if path == "" {
		return nil, fail(CodeInternal, "no default workspace directory")
	}
	item, _ := s.Workspaces.Add(path)
	return map[string]any{"workspace": s.workspaceView(item)}, nil
}

// createWorkspace answers workspace/create.
func (s *Service) createWorkspace(args json.RawMessage) (any, error) {
	var request struct {
		Path string `json:"path"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	path := strings.TrimSpace(request.Path)
	if path == "" || !filepath.IsAbs(path) {
		return nil, &mux.Failure{
			Code:    CodeWorkspaceInvalidPath,
			Message: "workspace path must be an absolute directory",
			Details: map[string]any{"path": path},
		}
	}
	item, created := s.Workspaces.Add(path)
	return map[string]any{"workspace": s.workspaceView(item), "created": created}, nil
}

// followWorkspaces answers workspace/follow: one baseline, then the Workspaces
// that appeared since.
func (s *Service) followWorkspaces(ctx context.Context) (mux.Stream, error) {
	changes, stop := s.Workspaces.Subscribe()
	baseline, err := s.workspaceBaseline()
	if err != nil {
		stop()
		return nil, fail(CodeInternal, err.Error())
	}
	return &workspaceStream{baseline: baseline, changes: changes, stop: stop, service: s}, nil
}

// workspaceStream emits the baseline once, then one `replace` frame per change
// with the complete registry, which the client accepts as a superseding
// increment.
type workspaceStream struct {
	baseline []byte
	changes  <-chan struct{}
	stop     func()
	service  *Service
	sent     bool
}

func (s *workspaceStream) Next(ctx context.Context) ([]byte, error) {
	if !s.sent {
		s.sent = true
		return s.baseline, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.changes:
		return encodeValue(map[string]any{
			"type": "replace",
			"value": map[string]any{
				"items":              s.service.workspaceViews(),
				"archivedSessionIds": []string{},
				"pinnedSessionIds":   []string{},
			},
		})
	}
}

func (s *workspaceStream) Close() {
	if s.stop != nil {
		s.stop()
		s.stop = nil
	}
}
