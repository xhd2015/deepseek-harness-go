// Package modelconfig turns the DeepSeek Harness cordis patch into the model
// catalog dsh-go serves.
//
// The web client builds its model picker from the catalog advertised by the
// backend, so a Go backend that only knows one route would hide every other
// llm-proxy model. Reading the same patch as the Node profile keeps the two
// implementations on one source of truth without copying model lists around.
package modelconfig

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/xhd2015/deepseek-harness-go/llm"
)

// Selection names one model route and, optionally, its reasoning effort.
type Selection struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

// Effort is one reasoning effort the client may pick.
type Effort struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Model is one selectable model.
type Model struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	ContextWindow int      `json:"contextWindow,omitempty"`
	MaxTokens     int      `json:"maxTokens,omitempty"`
	Efforts       []Effort `json:"efforts,omitempty"`
	DefaultEffort string   `json:"defaultEffort,omitempty"`
}

// Provider groups models that share one wire protocol and endpoint.
type Provider struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Protocol string  `json:"protocol"`
	BaseURL  string  `json:"baseURL"`
	APIKey   string  `json:"-"`
	Models   []Model `json:"models"`
}

// Failure reports a provider that could not be loaded.
type Failure struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

// Catalog is the whole model surface served by one dsh-go process.
type Catalog struct {
	Default   Selection  `json:"default"`
	Providers []Provider `json:"providers"`
	Failures  []Failure  `json:"failures"`
}

// Route is a resolved provider/model pair ready for a client call.
type Route struct {
	Selection
	ProviderName  string
	ModelName     string
	Protocol      string
	BaseURL       string
	APIKey        string
	ContextWindow int
	MaxTokens     int
}

// LoadPatch reads a cordis patch file. A missing file or a patch without any
// llm-pi-ai provider yields (nil, nil) so callers can fall back to the
// single-route configuration.
func LoadPatch(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var entries []patchEntry
	if err := yaml.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	catalog := &Catalog{}
	for _, entry := range entries {
		config := derefNode(&entry.Config)
		if config == nil {
			continue
		}
		switch entry.ID {
		case "llm-pi-ai":
			if err := catalog.parseLLMPlugin(config); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
		case "agent-default-model":
			var value defaultModelPatch
			if err := config.Decode(&value); err != nil {
				return nil, fmt.Errorf("parse %s agent-default-model: %w", path, err)
			}
			catalog.Default = Selection{
				Provider:        strings.TrimSpace(value.Provider),
				Model:           strings.TrimSpace(value.Model),
				ReasoningEffort: normalizeEffort(value.ReasoningEffort),
			}
		}
	}
	if len(catalog.Providers) == 0 {
		return nil, nil
	}
	catalog.normalize()
	return catalog, nil
}

type patchEntry struct {
	ID     string    `yaml:"id"`
	Config yaml.Node `yaml:"config"`
}

type defaultModelPatch struct {
	Provider        string `yaml:"provider"`
	Model           string `yaml:"model"`
	ReasoningEffort string `yaml:"reasoningEffort"`
}

type llmPluginPatch struct {
	Providers yaml.Node `yaml:"providers"`
}

type providerPatch struct {
	DisplayName string       `yaml:"displayName"`
	APIKeyEnv   string       `yaml:"apiKeyEnv"`
	APIKey      string       `yaml:"apiKey"`
	API         string       `yaml:"api"`
	BaseURL     string       `yaml:"baseURL"`
	Reasoning   string       `yaml:"reasoning"`
	Models      []modelPatch `yaml:"models"`
}

type modelPatch struct {
	ID               string            `yaml:"id"`
	Name             string            `yaml:"name"`
	Description      string            `yaml:"description"`
	ContextWindow    int               `yaml:"contextWindow"`
	MaxTokens        int               `yaml:"maxTokens"`
	ReasoningEfforts map[string]string `yaml:"reasoningEfforts"`
}

func (c *Catalog) parseLLMPlugin(config *yaml.Node) error {
	providers := mappingValue(config, "providers")
	if providers == nil || providers.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(providers.Content); index += 2 {
		id := strings.TrimSpace(providers.Content[index].Value)
		if id == "" {
			continue
		}
		var patch providerPatch
		if err := providers.Content[index+1].Decode(&patch); err != nil {
			return fmt.Errorf("provider %s: %w", id, err)
		}
		protocol := normalizeProtocol(patch.API)
		if protocol == "" {
			c.Failures = append(c.Failures, Failure{
				ID:      id,
				Name:    firstNonEmpty(patch.DisplayName, id),
				Message: "unsupported protocol " + strings.TrimSpace(patch.API),
			})
			continue
		}
		provider := Provider{
			ID:       id,
			Name:     firstNonEmpty(patch.DisplayName, id),
			Protocol: protocol,
			BaseURL:  strings.TrimRight(strings.TrimSpace(patch.BaseURL), "/"),
			APIKey:   resolveAPIKey(patch),
		}
		if provider.BaseURL == "" {
			provider.BaseURL = llm.DefaultBaseURL
		}
		for _, modelPatch := range patch.Models {
			modelID := strings.TrimSpace(modelPatch.ID)
			if modelID == "" {
				continue
			}
			model := Model{
				ID:            modelID,
				Name:          firstNonEmpty(modelPatch.Name, modelID),
				Description:   modelPatch.Description,
				ContextWindow: modelPatch.ContextWindow,
				MaxTokens:     modelPatch.MaxTokens,
			}
			if normalizeEffort(patch.Reasoning) != "off" {
				for effortID := range modelPatch.ReasoningEfforts {
					model.Efforts = append(model.Efforts, Effort{
						ID:   effortID,
						Name: effortDisplayName(effortID),
					})
				}
				sortEfforts(model.Efforts)
				model.DefaultEffort = normalizeEffort(patch.Reasoning)
				if !hasEffort(model.Efforts, model.DefaultEffort) {
					model.DefaultEffort = ""
				}
			}
			provider.Models = append(provider.Models, model)
		}
		if len(provider.Models) == 0 {
			c.Failures = append(c.Failures, Failure{
				ID:      id,
				Name:    provider.Name,
				Message: "provider has no models",
			})
			continue
		}
		c.Providers = append(c.Providers, provider)
	}
	return nil
}

// normalize makes sure the advertised default is selectable. A patch may
// point at a provider dsh-go could not load (or no patch may set a default at
// all), and the web client cannot start a turn without one.
func (c *Catalog) normalize() {
	selection := c.Default
	if _, err := c.resolve(selection.Provider, selection.Model, selection.ReasoningEffort); err == nil {
		c.Default = selection
		return
	}
	for _, provider := range c.Providers {
		if len(provider.Models) == 0 {
			continue
		}
		model := provider.Models[0]
		c.Default = Selection{
			Provider:        provider.ID,
			Model:           model.ID,
			ReasoningEffort: model.DefaultEffort,
		}
		return
	}
}

// Resolve validates a selection and fills in omitted parts from the default.
func (c *Catalog) Resolve(selection Selection) (Route, error) {
	if c == nil {
		return Route{}, fmt.Errorf("no model catalog configured")
	}
	providerID := strings.TrimSpace(selection.Provider)
	modelID := strings.TrimSpace(selection.Model)
	if providerID == "" {
		providerID = c.Default.Provider
	}
	if modelID == "" {
		if providerID == c.Default.Provider {
			modelID = c.Default.Model
		} else {
			provider := c.provider(providerID)
			if provider == nil {
				return Route{}, fmt.Errorf("unknown provider %s", providerID)
			}
			if len(provider.Models) == 0 {
				return Route{}, fmt.Errorf("provider %s has no models", providerID)
			}
			modelID = provider.Models[0].ID
		}
	}
	return c.resolve(providerID, modelID, selection.ReasoningEffort)
}

func (c *Catalog) provider(id string) *Provider {
	for index := range c.Providers {
		if c.Providers[index].ID == id {
			return &c.Providers[index]
		}
	}
	return nil
}

// HasProvider reports whether the catalog can serve the provider id.
func (c *Catalog) HasProvider(id string) bool { return c != nil && c.provider(id) != nil }

func (c *Catalog) resolve(providerID, modelID, effort string) (Route, error) {
	var (
		provider *Provider
		model    *Model
	)
	for index := range c.Providers {
		if c.Providers[index].ID == providerID {
			provider = &c.Providers[index]
			break
		}
	}
	if provider == nil {
		return Route{}, fmt.Errorf("unknown provider %s", providerID)
	}
	for index := range provider.Models {
		if provider.Models[index].ID == modelID {
			model = &provider.Models[index]
			break
		}
	}
	if model == nil {
		return Route{}, fmt.Errorf("unknown model %s in provider %s", modelID, providerID)
	}
	effort = normalizeEffort(effort)
	if effort == "" {
		effort = model.DefaultEffort
	}
	if effort != "" && len(model.Efforts) > 0 && !hasEffort(model.Efforts, effort) {
		return Route{}, fmt.Errorf("unknown reasoning effort %s for %s/%s", effort, providerID, modelID)
	}
	return Route{
		Selection: Selection{
			Provider:        provider.ID,
			Model:           model.ID,
			ReasoningEffort: effort,
		},
		ProviderName:  provider.Name,
		ModelName:     model.Name,
		Protocol:      provider.Protocol,
		BaseURL:       provider.BaseURL,
		APIKey:        provider.APIKey,
		ContextWindow: model.ContextWindow,
		MaxTokens:     model.MaxTokens,
	}, nil
}

// Single builds a catalog for the legacy single-route configuration.
func Single(provider, model, effort string) *Catalog {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "deepseek"
	}
	model = strings.TrimSpace(model)
	if model == "" {
		model = "deepseek-chat"
	}
	effort = normalizeEffort(effort)
	return &Catalog{
		Default: Selection{Provider: provider, Model: model, ReasoningEffort: effort},
		Providers: []Provider{{
			ID:       provider,
			Name:     provider,
			Protocol: llm.ProtocolOpenAIChat,
			BaseURL:  llm.DefaultBaseURL,
			Models: []Model{{
				ID:            model,
				Name:          model,
				DefaultEffort: effort,
			}},
		}},
	}
}

// Routes counts every servable provider/model pair.
func (c *Catalog) RouteCount() int {
	if c == nil {
		return 0
	}
	count := 0
	for _, provider := range c.Providers {
		count += len(provider.Models)
	}
	return count
}

func resolveAPIKey(patch providerPatch) string {
	if env := strings.TrimSpace(patch.APIKeyEnv); env != "" {
		return os.Getenv(env)
	}
	return strings.TrimSpace(patch.APIKey)
}

func normalizeProtocol(value string) string {
	switch strings.TrimSpace(value) {
	case llm.ProtocolOpenAIChat:
		return llm.ProtocolOpenAIChat
	case llm.ProtocolAnthropicMessages:
		return llm.ProtocolAnthropicMessages
	case llm.ProtocolOpenAIResponses:
		return llm.ProtocolOpenAIResponses
	default:
		return ""
	}
}

func normalizeEffort(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "none" {
		return "off"
	}
	return value
}

func effortDisplayName(id string) string {
	switch id {
	case "xhigh":
		return "Xhigh"
	case "":
		return ""
	default:
		return strings.ToUpper(id[:1]) + id[1:]
	}
}

func sortEfforts(efforts []Effort) {
	rank := func(id string) int {
		switch id {
		case "off":
			return 0
		case "minimal":
			return 1
		case "low":
			return 2
		case "medium":
			return 3
		case "high":
			return 4
		case "xhigh":
			return 5
		case "max":
			return 6
		default:
			return 7
		}
	}
	sort.SliceStable(efforts, func(i, j int) bool {
		left, right := rank(efforts[i].ID), rank(efforts[j].ID)
		if left != right {
			return left < right
		}
		return efforts[i].ID < efforts[j].ID
	})
}

func hasEffort(efforts []Effort, id string) bool {
	if id == "" {
		return true
	}
	for _, effort := range efforts {
		if effort.ID == id {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func derefNode(node *yaml.Node) *yaml.Node {
	for node != nil && (node.Kind == yaml.DocumentNode || node.Kind == yaml.AliasNode) {
		if len(node.Content) == 0 {
			return nil
		}
		if node.Kind == yaml.AliasNode {
			node = node.Alias
			continue
		}
		node = node.Content[0]
	}
	return node
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	node = derefNode(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}
