// Package llm is a small streaming client for the three wire protocols used by
// DSH: OpenAI chat completions, Anthropic messages, and OpenAI responses.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is the DeepSeek OpenAI-compatible endpoint.
const DefaultBaseURL = "https://api.deepseek.com/v1"

// Wire protocols supported by the client.
const (
	// ProtocolOpenAIChat is the OpenAI-compatible /chat/completions protocol.
	ProtocolOpenAIChat = "openai-chat"
	// ProtocolAnthropicMessages is the Anthropic /v1/messages protocol, which
	// local llm-proxy deployments commonly expose for DeepSeek models.
	ProtocolAnthropicMessages = "anthropic-messages"
	// ProtocolOpenAIResponses is the OpenAI /v1/responses protocol used by the
	// Codex, AIS gateway, and Grok routes behind llm-proxy.
	ProtocolOpenAIResponses = "openai-responses"
)

// Config configures a Client.
type Config struct {
	BaseURL    string
	APIKey     string
	Model      string
	Protocol   string
	HTTPClient *http.Client
	// Headers are added to every request.
	Headers map[string]string
}

// ToolDef is one model-facing tool definition.
type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ToolCall is one model-requested tool invocation. Arguments is the raw JSON
// string produced by the model.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ContentBlock is one assistant content block. Signature is adapter replay
// metadata for reasoning blocks and is never written into durable content.
type ContentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Signature string `json:"-"`
}

// MarshalJSON emits the exact DSH content-block wire shape.
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	out := map[string]any{"type": b.Type}
	switch b.Type {
	case "text", "reasoning":
		out["text"] = b.Text
	case "tool-call":
		out["id"] = b.ID
		out["name"] = b.Name
		out["arguments"] = b.Arguments
	default:
		if b.Text != "" {
			out["text"] = b.Text
		}
	}
	return json.Marshal(out)
}

// Image is one model-visible inline image. Data is the encoded file content;
// providers receive it as base64 or a data URL.
type Image struct {
	MediaType string
	Data      []byte
	Name      string
}

// Message is one model-visible history message. Blocks carries assistant
// content in order; Content is used for plain text roles. Images carries
// multimodal attachments for user and tool messages.
type Message struct {
	Role       string
	Content    string
	Blocks     []ContentBlock
	Images     []Image
	ToolCallID string
	IsError    bool
	Provider   string
	Model      string
	Replay     json.RawMessage
}

// Request is one completion request.
type Request struct {
	Model           string
	Messages        []Message
	Tools           []ToolDef
	ReasoningEffort string
	MaxTokens       int
	Temperature     *float64
}

// Usage is token accounting for one completion.
type Usage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
	TotalTokens      int `json:"totalTokens"`
	ReasoningTokens  int `json:"reasoningTokens,omitempty"`
}

// Completion is a finished model answer assembled from its stream.
type Completion struct {
	Text         string
	Reasoning    string
	Blocks       []ContentBlock
	ToolCalls    []ToolCall
	Model        string
	FinishReason string
	Usage        Usage
	Replay       json.RawMessage
}

// Chunk is one exact DSH StreamChunk. Only the fields owned by the chunk type
// are serialized.
type Chunk struct {
	Type           string
	Index          int
	BlockType      string
	Text           string
	ID             string
	Name           string
	HasName        bool
	ArgumentsDelta string
	Block          *ContentBlock
	Usage          *Usage
	Reason         string
	ReplayState    json.RawMessage
	Model          string
}

// MarshalJSON emits the exact DSH StreamChunk wire shape.
func (c Chunk) MarshalJSON() ([]byte, error) {
	out := map[string]any{"type": c.Type}
	switch c.Type {
	case "block-start":
		out["index"] = c.Index
		out["blockType"] = c.BlockType
	case "text-delta":
		out["index"] = c.Index
		out["text"] = c.Text
	case "reasoning-delta":
		out["index"] = c.Index
		out["text"] = c.Text
	case "tool-call-delta":
		out["index"] = c.Index
		out["id"] = c.ID
		if c.HasName {
			out["name"] = c.Name
		}
		out["argumentsDelta"] = c.ArgumentsDelta
	case "block-end":
		out["index"] = c.Index
		out["block"] = c.Block
	case "usage":
		out["usage"] = c.Usage
	case "finish":
		out["reason"] = map[string]any{"kind": normalizeFinishReason(c.Reason)}
		if len(c.ReplayState) > 0 {
			out["replayState"] = json.RawMessage(c.ReplayState)
		}
	default:
		return nil, fmt.Errorf("unknown chunk type %q", c.Type)
	}
	return json.Marshal(out)
}

// normalizeFinishReason maps provider-specific stop reasons onto the kinds the
// DSH stream contract defines.
func normalizeFinishReason(reason string) string {
	switch reason {
	case "tool_calls", "tool_use", "function_call", "tool-calls":
		return "tool-calls"
	case "length", "max_tokens", "max_output_tokens", "max-tokens":
		return "max-tokens"
	case "aborted":
		return "aborted"
	case "error":
		return "error"
	default:
		return "stop"
	}
}

// Failure describes a failed call in the shape the session log records.
type Failure struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	StatusCode int    `json:"-"`
}

// Error implements error.
func (f *Failure) Error() string {
	if f == nil {
		return ""
	}
	if f.StatusCode != 0 {
		return fmt.Sprintf("%s (http %d)", f.Message, f.StatusCode)
	}
	return f.Message
}

// Client calls an OpenAI-compatible chat completions endpoint.
type Client struct {
	cfg Config
}

// New builds a client, applying defaults.
func New(cfg Config) *Client {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Protocol == "" {
		cfg.Protocol = ProtocolOpenAIChat
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Minute}
	}
	return &Client{cfg: cfg}
}

// Model returns the configured default model.
func (c *Client) Model() string { return c.cfg.Model }

// Complete consumes one streaming completion and returns the whole answer.
func (c *Client) Complete(ctx context.Context, req Request) (Completion, error) {
	return c.Stream(ctx, req, nil)
}

func errorMessage(payload []byte, status string) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload, &parsed); err == nil {
		if parsed.Error.Message != "" {
			return parsed.Error.Message
		}
		if parsed.Message != "" {
			return parsed.Message
		}
	}
	text := strings.TrimSpace(string(payload))
	if text == "" {
		return status
	}
	if len(text) > 400 {
		text = text[:400]
	}
	return text
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type anthropicEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Model string          `json:"model"`
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content_block"`
	Delta *struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		Thinking   string `json:"thinking"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthropicUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func mergeAnthropicUsage(target *Usage, source *anthropicUsage) {
	if source == nil {
		return
	}
	if source.InputTokens != 0 {
		target.InputTokens = source.InputTokens
	}
	if source.OutputTokens != 0 {
		target.OutputTokens = source.OutputTokens
	}
	if source.CacheReadInputTokens != 0 {
		target.CacheReadTokens = source.CacheReadInputTokens
	}
	if source.CacheCreationInputTokens != 0 {
		target.CacheWriteTokens = source.CacheCreationInputTokens
	}
	if target.InputTokens != 0 || target.OutputTokens != 0 {
		target.TotalTokens = target.InputTokens + target.OutputTokens
	}
}
