package api

import "encoding/json"

// This backend does not load plugins, and says so.
//
// The Plugins page asks a handful of questions — what is installed, what can be
// enabled, what is available. Answering "nothing" is honest and lets the page
// render an empty state. Answering with a fabricated bundle, or reporting success
// for an install that cannot happen, would be the one outcome worse than an empty
// page: a user who believes a capability is active.

// pluginBundlesNotice explains the empty list, so the page has something to show.
const pluginBundlesNotice = "This backend does not load plugins or MCP servers. " +
	"Tools are built in; agent presets choose among them."

// pluginManagerListBundles answers pluginManager/listBundles.
func (s *Service) pluginManagerListBundles(json.RawMessage) (any, error) {
	return map[string]any{
		"bundles": []any{},
		"notice":  pluginBundlesNotice,
	}, nil
}

// pluginManagerListPlugins answers pluginManager/listPlugins.
func (s *Service) pluginManagerListPlugins(json.RawMessage) (any, error) {
	return map[string]any{
		"plugins": []any{},
		"notice":  pluginBundlesNotice,
	}, nil
}

// pluginManagerRegistries answers pluginManager/registries.
//
// No registries are configured because nothing here installs from one. Returning an
// empty object rather than a null lets a client iterate it without a null check.
func (s *Service) pluginManagerRegistries(json.RawMessage) (any, error) {
	return map[string]any{"registries": []any{}, "notice": pluginBundlesNotice}, nil
}

// pluginManagerRefuse answers the mutating methods.
//
// An install, update, or enable cannot be honoured, so it fails with the reason.
// Returning a success shape would make the page show a plugin that is not there.
func (s *Service) pluginManagerRefuse(args json.RawMessage) (any, error) {
	return nil, fail(CodeUnimplemented, pluginBundlesNotice)
}
