package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var errStopStream = errors.New("stream done")

// Stream performs one streaming completion. emit, when non-nil, receives each
// canonical DSH chunk in order. A retry is only attempted when no chunk has
// been emitted yet.
func (c *Client) Stream(ctx context.Context, req Request, emit func(Chunk) error) (Completion, error) {
	if req.Model == "" {
		req.Model = c.cfg.Model
	}
	switch c.cfg.Protocol {
	case ProtocolOpenAIChat, ProtocolAnthropicMessages, ProtocolOpenAIResponses:
	default:
		return Completion{}, &Failure{Code: "INVALID_REQUEST", Message: "unknown protocol: " + c.cfg.Protocol}
	}
	if c.cfg.Protocol == ProtocolAnthropicMessages && len(req.Messages) == 0 {
		return Completion{}, &Failure{Code: "INVALID_REQUEST", Message: "messages must not be empty"}
	}
	encoded, err := c.encodeStreamRequest(req)
	if err != nil {
		return Completion{}, err
	}
	var acc streamAccumulator
	emitted := 0
	deliver := func(chunk Chunk) error {
		acc.accept(chunk)
		emitted++
		if emit != nil {
			if err := emit(chunk); err != nil {
				return err
			}
		}
		return nil
	}
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Completion{}, ctx.Err()
			case <-time.After(time.Second):
			}
		}
		before := emitted
		retry, err := c.streamOnce(ctx, encoded, deliver)
		if err == nil {
			completion := acc.completion()
			if !hasVisibleContent(completion) {
				return Completion{}, &Failure{Code: "EMPTY_RESPONSE", Message: "model returned no content"}
			}
			return completion, nil
		}
		if ctx.Err() != nil {
			return acc.completion(), ctx.Err()
		}
		last = err
		if emitted > before || !retry {
			return acc.completion(), err
		}
	}
	return acc.completion(), last
}

func (c *Client) streamOnce(ctx context.Context, encoded []byte, emit func(Chunk) error) (bool, error) {
	path := "/chat/completions"
	switch c.cfg.Protocol {
	case ProtocolAnthropicMessages:
		path = "/messages"
	case ProtocolOpenAIResponses:
		path = "/responses"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return false, &Failure{Code: "INVALID_REQUEST", Message: err.Error()}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	if c.cfg.Protocol == ProtocolAnthropicMessages {
		if c.cfg.APIKey != "" {
			request.Header.Set("x-api-key", c.cfg.APIKey)
		}
		request.Header.Set("anthropic-version", "2023-06-01")
	} else if c.cfg.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	for key, value := range c.cfg.Headers {
		request.Header.Set(key, value)
	}
	response, err := c.cfg.HTTPClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return true, &Failure{Code: "NETWORK", Message: err.Error()}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
		return response.StatusCode >= 500 || response.StatusCode == 429, &Failure{
			Code:       "HTTP_ERROR",
			Message:    errorMessage(payload, response.Status),
			StatusCode: response.StatusCode,
		}
	}
	contentType := response.Header.Get("Content-Type")
	jsonBody := strings.Contains(contentType, "application/json")
	switch c.cfg.Protocol {
	case ProtocolAnthropicMessages:
		if jsonBody {
			return false, decodeAnthropicJSONAnswer(ctx, response.Body, emit)
		}
		return decodeAnthropicStream(ctx, response.Body, emit)
	case ProtocolOpenAIResponses:
		if jsonBody {
			return false, decodeResponsesJSONAnswer(ctx, response.Body, emit)
		}
		return decodeResponsesStream(ctx, response.Body, emit)
	default:
		if jsonBody {
			return false, decodeChatJSONAnswer(ctx, response.Body, emit)
		}
		return decodeChatStream(ctx, response.Body, emit)
	}
}

func hasVisibleContent(c Completion) bool {
	if c.Text != "" || c.Reasoning != "" || len(c.ToolCalls) > 0 {
		return true
	}
	for _, block := range c.Blocks {
		if block.Type == "tool-call" || block.Text != "" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- assembler

type streamAccumulator struct {
	order          []int
	blocks         map[int]*ContentBlock
	text           strings.Builder
	reasoning      strings.Builder
	toolCalls      []ToolCall
	model          string
	finish         string
	usage          Usage
	replay         json.RawMessage
	reasoningBlock bool
}

func (a *streamAccumulator) block(index int, typ string) *ContentBlock {
	if a.blocks == nil {
		a.blocks = map[int]*ContentBlock{}
	}
	block, ok := a.blocks[index]
	if !ok {
		block = &ContentBlock{Type: typ}
		a.blocks[index] = block
		a.order = append(a.order, index)
	}
	if block.Type == "" {
		block.Type = typ
	}
	if block.Type == "reasoning" {
		a.reasoningBlock = true
	}
	return block
}

func (a *streamAccumulator) accept(chunk Chunk) {
	switch chunk.Type {
	case "block-start":
		a.block(chunk.Index, chunk.BlockType)
	case "text-delta":
		a.block(chunk.Index, "text").Text += chunk.Text
		a.text.WriteString(chunk.Text)
	case "reasoning-delta":
		a.block(chunk.Index, "reasoning").Text += chunk.Text
		a.reasoning.WriteString(chunk.Text)
	case "tool-call-delta":
		block := a.block(chunk.Index, "tool-call")
		if chunk.ID != "" {
			block.ID = chunk.ID
		}
		if chunk.HasName {
			block.Name = chunk.Name
		}
		block.Arguments += chunk.ArgumentsDelta
	case "block-end":
		if chunk.Block == nil {
			return
		}
		block := a.block(chunk.Index, chunk.Block.Type)
		if chunk.Block.Type != "" {
			block.Type = chunk.Block.Type
		}
		if chunk.Block.Text != "" {
			block.Text = chunk.Block.Text
		}
		if chunk.Block.ID != "" {
			block.ID = chunk.Block.ID
		}
		if chunk.Block.Name != "" {
			block.Name = chunk.Block.Name
		}
		if chunk.Block.Arguments != "" {
			block.Arguments = chunk.Block.Arguments
		}
		if chunk.Block.Signature != "" {
			block.Signature = chunk.Block.Signature
		}
		if block.Type == "reasoning" {
			a.reasoningBlock = true
		}
	case "usage":
		if chunk.Usage != nil {
			a.usage = *chunk.Usage
		}
	case "finish":
		a.finish = chunk.Reason
		if chunk.Model != "" {
			a.model = chunk.Model
		}
		if len(chunk.ReplayState) > 0 {
			a.replay = append(json.RawMessage(nil), chunk.ReplayState...)
		}
	}
}

func (a *streamAccumulator) completion() Completion {
	result := Completion{
		Text:         a.text.String(),
		Reasoning:    a.reasoning.String(),
		Model:        a.model,
		FinishReason: normalizeFinishReason(a.finish),
		Usage:        a.usage,
		Replay:       append(json.RawMessage(nil), a.replay...),
	}
	// Keep every block the provider opened, including empty text or reasoning
	// blocks. The durable replay envelope carries one entry per block, so
	// dropping an empty block here would shift that alignment.
	all := make([]ContentBlock, 0, len(a.order))
	for _, index := range a.order {
		block := a.blocks[index]
		if block == nil {
			continue
		}
		all = append(all, *block)
	}
	if result.Reasoning != "" && !a.reasoningBlock {
		all = append(all, ContentBlock{Type: "reasoning", Text: result.Reasoning})
	}
	if result.FinishReason == "max-tokens" {
		// A truncated tool call was never fully streamed and cannot be
		// executed, so drop it from both content and replay metadata.
		for _, block := range all {
			if block.Type != "tool-call" {
				result.Blocks = append(result.Blocks, block)
			}
		}
	} else {
		result.Blocks = all
	}
	for _, block := range result.Blocks {
		if block.Type == "tool-call" {
			result.ToolCalls = append(result.ToolCalls, ToolCall{ID: block.ID, Name: block.Name, Arguments: block.Arguments})
		}
	}
	result.Replay = replayForBlocks(a.replay, all, result.FinishReason)
	if result.FinishReason == "" {
		result.FinishReason = "stop"
	}
	return result
}

// replayForBlocks prunes per-block replay entries in step with the emitted
// blocks. An envelope whose entries do not align with the blocks seen on the
// wire is omitted rather than replayed against the wrong content.
func replayForBlocks(replay json.RawMessage, all []ContentBlock, finish string) json.RawMessage {
	if len(replay) == 0 {
		return nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(replay, &envelope); err != nil {
		return append(json.RawMessage(nil), replay...)
	}
	rawBlocks, ok := envelope["blocks"]
	if !ok || string(rawBlocks) == "null" {
		return append(json.RawMessage(nil), replay...)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(rawBlocks, &entries); err != nil {
		return append(json.RawMessage(nil), replay...)
	}
	if len(entries) != len(all) {
		return nil
	}
	if finish != "max-tokens" {
		return append(json.RawMessage(nil), replay...)
	}
	kept := make([]json.RawMessage, 0, len(entries))
	for i, entry := range entries {
		if all[i].Type != "tool-call" {
			kept = append(kept, entry)
		}
	}
	if len(kept) == len(entries) {
		return append(json.RawMessage(nil), replay...)
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil
	}
	envelope["blocks"] = encoded
	out, err := json.Marshal(envelope)
	if err != nil {
		return nil
	}
	return out
}

// --------------------------------------------------------------------- SSE

func readSSE(ctx context.Context, body io.Reader, handle func([]byte) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var data []string
	dispatch := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		return handle([]byte(payload))
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				if errors.Is(err, errStopStream) {
					return nil
				}
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok || field != "data" {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		if len(data) > 0 {
			if err := dispatch(); err != nil {
				if errors.Is(err, errStopStream) {
					return nil
				}
				return err
			}
		}
		data = append(data, value)
	}
	if err := scanner.Err(); err != nil {
		return &Failure{Code: "NETWORK", Message: err.Error()}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dispatch(); err != nil && !errors.Is(err, errStopStream) {
		return err
	}
	return nil
}

// ------------------------------------------------------------- OpenAI chat

type wireChatToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireChatChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content          string             `json:"content"`
			ReasoningContent string             `json:"reasoning_content"`
			Reasoning        string             `json:"reasoning"`
			ToolCalls        []wireChatToolCall `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Content          string             `json:"content"`
			ReasoningContent string             `json:"reasoning_content"`
			Reasoning        string             `json:"reasoning"`
			ToolCalls        []wireChatToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens      int `json:"prompt_tokens"`
		CompletionTokens  int `json:"completion_tokens"`
		TotalTokens       int `json:"total_tokens"`
		PromptCacheHit    int `json:"prompt_cache_hit_tokens"`
		PromptCacheMiss   int `json:"prompt_cache_miss_tokens"`
		CompletionDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
		PromptDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func decodeChatStream(ctx context.Context, body io.Reader, emit func(Chunk) error) (bool, error) {
	state := &chatStreamState{emit: emit}
	err := readSSE(ctx, body, state.event)
	if err != nil {
		return false, err
	}
	if err := state.finish("stop"); err != nil {
		return false, err
	}
	return false, nil
}

type chatStreamState struct {
	emit         func(Chunk) error
	model        string
	nextIndex    int
	current      int
	open         bool
	currentTyp   string
	text         strings.Builder
	reasoning    strings.Builder
	blocks       map[int]*ContentBlock
	toolIndex    map[int]int
	tools        map[int]*ContentBlock
	usage        Usage
	finished     bool
	finishReason string
	stopEmitted  bool
}

func (s *chatStreamState) start(typ string) error {
	if s.blocks == nil {
		s.blocks = map[int]*ContentBlock{}
		s.toolIndex = map[int]int{}
		s.tools = map[int]*ContentBlock{}
	}
	index := s.nextIndex
	s.nextIndex++
	s.current = index
	s.currentTyp = typ
	s.open = true
	block := &ContentBlock{Type: typ}
	s.blocks[index] = block
	return s.emit(Chunk{Type: "block-start", Index: index, BlockType: typ})
}

func (s *chatStreamState) close() error {
	if !s.open {
		return nil
	}
	block := s.blocks[s.current]
	if err := s.emit(Chunk{Type: "block-end", Index: s.current, Block: block}); err != nil {
		return err
	}
	s.open = false
	return nil
}

func (s *chatStreamState) toolBlock(wire int, delta *wireChatToolCall) (int, error) {
	if index, ok := s.toolIndex[wire]; ok {
		if s.current != index {
			if err := s.close(); err != nil {
				return 0, err
			}
			s.current = index
			s.currentTyp = "tool-call"
			s.open = true
		}
		return index, nil
	}
	if err := s.close(); err != nil {
		return 0, err
	}
	index := s.nextIndex
	s.nextIndex++
	s.current = index
	s.currentTyp = "tool-call"
	s.open = true
	block := &ContentBlock{Type: "tool-call", ID: delta.ID, Name: delta.Function.Name}
	s.blocks[index] = block
	s.tools[index] = block
	s.toolIndex[wire] = index
	if err := s.emit(Chunk{Type: "block-start", Index: index, BlockType: "tool-call"}); err != nil {
		return 0, err
	}
	if err := s.emit(Chunk{Type: "tool-call-delta", Index: index, ID: delta.ID, Name: delta.Function.Name, HasName: delta.Function.Name != "", ArgumentsDelta: ""}); err != nil {
		return 0, err
	}
	return index, nil
}

func (s *chatStreamState) event(payload []byte) error {
	if string(payload) == "[DONE]" {
		return errStopStream
	}
	var parsed wireChatChunk
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return &Failure{Code: "BAD_RESPONSE", Message: "malformed stream chunk: " + err.Error()}
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return &Failure{Code: "PROVIDER", Message: parsed.Error.Message}
	}
	if parsed.Model != "" {
		s.model = parsed.Model
	}
	if parsed.Usage != nil {
		s.usage = Usage{
			InputTokens:      parsed.Usage.PromptTokens,
			OutputTokens:     parsed.Usage.CompletionTokens,
			TotalTokens:      parsed.Usage.TotalTokens,
			CacheReadTokens:  parsed.Usage.PromptCacheHit,
			CacheWriteTokens: parsed.Usage.PromptCacheMiss,
		}
		if parsed.Usage.PromptCacheHit == 0 && parsed.Usage.PromptDetails != nil {
			s.usage.CacheReadTokens = parsed.Usage.PromptDetails.CachedTokens
		}
		if parsed.Usage.CompletionDetails != nil {
			s.usage.ReasoningTokens = parsed.Usage.CompletionDetails.ReasoningTokens
		}
	}
	for _, choice := range parsed.Choices {
		reasoning := choice.Delta.ReasoningContent
		if reasoning == "" {
			reasoning = choice.Delta.Reasoning
		}
		if reasoning != "" {
			if !s.open || s.currentTyp != "reasoning" {
				if err := s.close(); err != nil {
					return err
				}
				if err := s.start("reasoning"); err != nil {
					return err
				}
			}
			s.blocks[s.current].Text += reasoning
			s.reasoning.WriteString(reasoning)
			if err := s.emit(Chunk{Type: "reasoning-delta", Index: s.current, Text: reasoning}); err != nil {
				return err
			}
		}
		if choice.Delta.Content != "" {
			if !s.open || s.currentTyp != "text" {
				if err := s.close(); err != nil {
					return err
				}
				if err := s.start("text"); err != nil {
					return err
				}
			}
			s.blocks[s.current].Text += choice.Delta.Content
			s.text.WriteString(choice.Delta.Content)
			if err := s.emit(Chunk{Type: "text-delta", Index: s.current, Text: choice.Delta.Content}); err != nil {
				return err
			}
		}
		for i := range choice.Delta.ToolCalls {
			delta := &choice.Delta.ToolCalls[i]
			index, err := s.toolBlock(delta.Index, delta)
			if err != nil {
				return err
			}
			block := s.blocks[index]
			if delta.ID != "" {
				block.ID = delta.ID
			}
			if delta.Function.Name != "" {
				if block.Name == "" {
					block.Name = delta.Function.Name
				}
			}
			block.Arguments += delta.Function.Arguments
			if err := s.emit(Chunk{
				Type:           "tool-call-delta",
				Index:          index,
				ID:             block.ID,
				Name:           block.Name,
				HasName:        delta.Function.Name != "",
				ArgumentsDelta: delta.Function.Arguments,
			}); err != nil {
				return err
			}
		}
		if choice.FinishReason != "" {
			s.finishReason = choice.FinishReason
		}
	}
	if s.finishReason != "" && parsed.Usage != nil {
		return s.finish(s.finishReason)
	}
	return nil
}

func (s *chatStreamState) finish(reason string) error {
	if s.stopEmitted {
		return nil
	}
	if reason == "" {
		reason = s.finishReason
	}
	if reason == "" {
		reason = "stop"
	}
	if err := s.close(); err != nil {
		return err
	}
	s.stopEmitted = true
	if s.usage != (Usage{}) || len(s.blocks) > 0 {
		usage := s.usage
		if err := s.emit(Chunk{Type: "usage", Usage: &usage}); err != nil {
			return err
		}
	}
	return s.emit(Chunk{Type: "finish", Reason: reason, Model: s.model})
}

func decodeChatJSONAnswer(ctx context.Context, body io.Reader, emit func(Chunk) error) error {
	var parsed struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content          string             `json:"content"`
				ReasoningContent string             `json:"reasoning_content"`
				Reasoning        string             `json:"reasoning"`
				ToolCalls        []wireChatToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.NewDecoder(body).Decode(&parsed); err != nil {
		return &Failure{Code: "BAD_RESPONSE", Message: err.Error()}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(parsed.Choices) == 0 {
		return &Failure{Code: "EMPTY_RESPONSE", Message: "model returned no content"}
	}
	state := &chatStreamState{emit: emit}
	state.model = parsed.Model
	for _, choice := range parsed.Choices {
		payload := map[string]any{}
		_ = payload
		reasoning := choice.Message.ReasoningContent
		if reasoning == "" {
			reasoning = choice.Message.Reasoning
		}
		if reasoning != "" {
			if err := state.close(); err != nil {
				return err
			}
			if err := state.start("reasoning"); err != nil {
				return err
			}
			state.blocks[state.current].Text += reasoning
			state.reasoning.WriteString(reasoning)
			if err := emit(Chunk{Type: "reasoning-delta", Index: state.current, Text: reasoning}); err != nil {
				return err
			}
		}
		if choice.Message.Content != "" {
			if err := state.close(); err != nil {
				return err
			}
			if err := state.start("text"); err != nil {
				return err
			}
			state.blocks[state.current].Text += choice.Message.Content
			state.text.WriteString(choice.Message.Content)
			if err := emit(Chunk{Type: "text-delta", Index: state.current, Text: choice.Message.Content}); err != nil {
				return err
			}
		}
		for i := range choice.Message.ToolCalls {
			call := &choice.Message.ToolCalls[i]
			if err := state.close(); err != nil {
				return err
			}
			if err := state.start("tool-call"); err != nil {
				return err
			}
			state.blocks[state.current].ID = call.ID
			state.blocks[state.current].Name = call.Function.Name
			state.blocks[state.current].Arguments = call.Function.Arguments
			if err := emit(Chunk{Type: "tool-call-delta", Index: state.current, ID: call.ID, Name: call.Function.Name, HasName: call.Function.Name != "", ArgumentsDelta: call.Function.Arguments}); err != nil {
				return err
			}
		}
		if choice.FinishReason != "" {
			state.finishReason = choice.FinishReason
		}
	}
	if len(parsed.Usage) > 0 {
		var usage wireChatChunk
		if err := json.Unmarshal([]byte(fmt.Sprintf(`{"usage":%s}`, parsed.Usage)), &usage); err == nil && usage.Usage != nil {
			state.usage = Usage{
				InputTokens:      usage.Usage.PromptTokens,
				OutputTokens:     usage.Usage.CompletionTokens,
				TotalTokens:      usage.Usage.TotalTokens,
				CacheReadTokens:  usage.Usage.PromptCacheHit,
				CacheWriteTokens: usage.Usage.PromptCacheMiss,
			}
			if usage.Usage.CompletionDetails != nil {
				state.usage.ReasoningTokens = usage.Usage.CompletionDetails.ReasoningTokens
			}
		}
	}
	return state.finish(state.finishReason)
}

// ------------------------------------------------------------- Anthropic

type wireAnthropicEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Model string          `json:"model"`
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		Input     any    `json:"input"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage      *anthropicUsage `json:"usage"`
	StopReason string          `json:"stop_reason"`
	Error      *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicStreamState struct {
	emit       func(Chunk) error
	started    bool
	model      string
	nextIndex  int
	blocks     map[int]*ContentBlock
	wire       map[int]int
	current    int
	open       bool
	currentTyp string
	usage      Usage
	stopReason string
	signatures map[int]string
	finished   bool
}

func decodeAnthropicStream(ctx context.Context, body io.Reader, emit func(Chunk) error) (bool, error) {
	state := &anthropicStreamState{emit: emit, blocks: map[int]*ContentBlock{}, wire: map[int]int{}, signatures: map[int]string{}}
	err := readSSE(ctx, body, state.event)
	if err != nil {
		return false, err
	}
	if !state.started {
		return false, &Failure{Code: "BAD_RESPONSE", Message: "stream ended before message_start"}
	}
	if err := state.finish(); err != nil {
		return false, err
	}
	return false, nil
}

func (s *anthropicStreamState) start(typ string) error {
	index := s.nextIndex
	s.nextIndex++
	s.current = index
	s.currentTyp = typ
	s.open = true
	s.blocks[index] = &ContentBlock{Type: typ}
	return s.emit(Chunk{Type: "block-start", Index: index, BlockType: typ})
}

func (s *anthropicStreamState) close() error {
	if !s.open {
		return nil
	}
	block := s.blocks[s.current]
	if signature := s.signatures[s.current]; signature != "" {
		block.Signature = signature
	}
	if err := s.emit(Chunk{Type: "block-end", Index: s.current, Block: block}); err != nil {
		return err
	}
	s.open = false
	return nil
}

func (s *anthropicStreamState) event(payload []byte) error {
	var event wireAnthropicEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return &Failure{Code: "BAD_RESPONSE", Message: "malformed stream chunk: " + err.Error()}
	}
	switch event.Type {
	case "message_start":
		s.started = true
		if event.Message != nil {
			s.model = event.Message.Model
			mergeAnthropicUsage(&s.usage, event.Message.Usage)
		}
	case "content_block_start":
		if !s.started {
			return &Failure{Code: "BAD_RESPONSE", Message: "content_block_start before message_start"}
		}
		if event.ContentBlock == nil {
			return &Failure{Code: "BAD_RESPONSE", Message: "content_block_start without block"}
		}
		typ := "text"
		switch event.ContentBlock.Type {
		case "thinking", "redacted_thinking", "reasoning":
			typ = "reasoning"
		case "tool_use":
			typ = "tool-call"
		}
		if err := s.close(); err != nil {
			return err
		}
		if err := s.start(typ); err != nil {
			return err
		}
		s.wire[event.Index] = s.current
		block := s.blocks[s.current]
		block.ID = event.ContentBlock.ID
		block.Name = event.ContentBlock.Name
		if event.ContentBlock.Signature != "" {
			s.signatures[s.current] = event.ContentBlock.Signature
		}
		switch typ {
		case "text":
			if event.ContentBlock.Text != "" {
				block.Text += event.ContentBlock.Text
				if err := s.emit(Chunk{Type: "text-delta", Index: s.current, Text: event.ContentBlock.Text}); err != nil {
					return err
				}
			}
		case "reasoning":
			if event.ContentBlock.Thinking != "" {
				block.Text += event.ContentBlock.Thinking
				if err := s.emit(Chunk{Type: "reasoning-delta", Index: s.current, Text: event.ContentBlock.Thinking}); err != nil {
					return err
				}
			}
		case "tool-call":
			if err := s.emit(Chunk{Type: "tool-call-delta", Index: s.current, ID: event.ContentBlock.ID, Name: event.ContentBlock.Name, HasName: event.ContentBlock.Name != "", ArgumentsDelta: ""}); err != nil {
				return err
			}
		}
	case "content_block_delta":
		index, ok := s.wire[event.Index]
		if !ok || event.Delta == nil {
			return &Failure{Code: "BAD_RESPONSE", Message: "delta for unknown content block"}
		}
		if s.current != index {
			if err := s.close(); err != nil {
				return err
			}
			s.current = index
			s.open = true
			s.currentTyp = s.blocks[index].Type
		}
		block := s.blocks[index]
		switch event.Delta.Type {
		case "text_delta":
			block.Text += event.Delta.Text
			if err := s.emit(Chunk{Type: "text-delta", Index: index, Text: event.Delta.Text}); err != nil {
				return err
			}
		case "thinking_delta":
			block.Text += event.Delta.Thinking
			if err := s.emit(Chunk{Type: "reasoning-delta", Index: index, Text: event.Delta.Thinking}); err != nil {
				return err
			}
		case "signature_delta":
			s.signatures[index] += event.Delta.Signature
		case "input_json_delta":
			block.Arguments += event.Delta.PartialJSON
			if err := s.emit(Chunk{Type: "tool-call-delta", Index: index, ID: block.ID, Name: block.Name, ArgumentsDelta: event.Delta.PartialJSON}); err != nil {
				return err
			}
		}
	case "content_block_stop":
		index, ok := s.wire[event.Index]
		if !ok {
			return &Failure{Code: "BAD_RESPONSE", Message: "stop for unknown content block"}
		}
		if s.current != index {
			if err := s.close(); err != nil {
				return err
			}
			s.current = index
			s.open = true
		}
		if err := s.close(); err != nil {
			return err
		}
	case "message_delta":
		if event.Delta != nil && event.Delta.StopReason != "" {
			s.stopReason = event.Delta.StopReason
		}
		if event.StopReason != "" {
			s.stopReason = event.StopReason
		}
		mergeAnthropicUsage(&s.usage, event.Usage)
	case "message_stop":
		return s.finish()
	case "error":
		message := "provider error"
		if event.Error != nil && event.Error.Message != "" {
			message = event.Error.Message
		}
		return &Failure{Code: "PROVIDER", Message: message}
	}
	return nil
}

func (s *anthropicStreamState) finish() error {
	if s.finished {
		return nil
	}
	s.finished = true
	if err := s.close(); err != nil {
		return err
	}
	if len(s.blocks) == 0 {
		return &Failure{Code: "EMPTY_RESPONSE", Message: "model returned no content"}
	}
	if s.stopReason != "max_tokens" {
		for _, block := range s.blocks {
			if block.Type == "tool-call" && block.Arguments != "" && !json.Valid([]byte(block.Arguments)) {
				return &Failure{Code: "BAD_RESPONSE", Message: "malformed tool arguments"}
			}
		}
	}
	replay := s.replayState()
	usage := s.usage
	if err := s.emit(Chunk{Type: "usage", Usage: &usage}); err != nil {
		return err
	}
	reason := s.stopReason
	if reason == "" {
		reason = "stop"
	}
	return s.emit(Chunk{Type: "finish", Reason: reason, ReplayState: replay, Model: s.model})
}

func (s *anthropicStreamState) replayState() json.RawMessage {
	if s.model == "" {
		return nil
	}
	blocks := make([]map[string]any, 0, len(s.blocks))
	for i := 0; i < s.nextIndex; i++ {
		block, ok := s.blocks[i]
		if !ok {
			blocks = append(blocks, map[string]any{"type": "text"})
			continue
		}
		entry := map[string]any{}
		switch block.Type {
		case "reasoning":
			entry["type"] = "reasoning"
			if signature := s.signatures[i]; signature != "" {
				entry["signature"] = signature
			}
		case "tool-call":
			entry["type"] = "tool-call"
		default:
			entry["type"] = "text"
		}
		blocks = append(blocks, entry)
	}
	envelope := map[string]any{
		"response": map[string]any{"kind": "deepseek-messages", "version": 1, "model": s.model},
		"blocks":   blocks,
	}
	encoded, _ := json.Marshal(envelope)
	return encoded
}

func decodeAnthropicJSONAnswer(ctx context.Context, body io.Reader, emit func(Chunk) error) error {
	var parsed struct {
		Model   string `json:"model"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      *anthropicUsage `json:"usage"`
	}
	if err := json.NewDecoder(body).Decode(&parsed); err != nil {
		return &Failure{Code: "BAD_RESPONSE", Message: err.Error()}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(parsed.Content) == 0 {
		return &Failure{Code: "EMPTY_RESPONSE", Message: "model returned no content"}
	}
	state := &anthropicStreamState{emit: emit, blocks: map[int]*ContentBlock{}, wire: map[int]int{}, signatures: map[int]string{}, started: true, model: parsed.Model, stopReason: parsed.StopReason}
	mergeAnthropicUsage(&state.usage, parsed.Usage)
	for _, item := range parsed.Content {
		typ := "text"
		switch item.Type {
		case "thinking", "redacted_thinking":
			typ = "reasoning"
		case "tool_use":
			typ = "tool-call"
		}
		if err := state.close(); err != nil {
			return err
		}
		if err := state.start(typ); err != nil {
			return err
		}
		block := state.blocks[state.current]
		block.ID = item.ID
		block.Name = item.Name
		switch typ {
		case "text":
			block.Text = item.Text
			if err := emit(Chunk{Type: "text-delta", Index: state.current, Text: item.Text}); err != nil {
				return err
			}
		case "reasoning":
			block.Text = item.Thinking
			state.signatures[state.current] = item.Signature
			if err := emit(Chunk{Type: "reasoning-delta", Index: state.current, Text: item.Thinking}); err != nil {
				return err
			}
		case "tool-call":
			if len(item.Input) > 0 && string(item.Input) != "null" {
				block.Arguments = string(item.Input)
			}
			if block.Arguments == "" {
				block.Arguments = "{}"
			}
			if err := emit(Chunk{Type: "tool-call-delta", Index: state.current, ID: item.ID, Name: item.Name, HasName: item.Name != "", ArgumentsDelta: block.Arguments}); err != nil {
				return err
			}
		}
	}
	return state.finish()
}

// -------------------------------------------------------------- Responses

type wireResponsesEvent struct {
	Type        string          `json:"type"`
	OutputIndex int             `json:"output_index"`
	Delta       string          `json:"delta"`
	Arguments   string          `json:"arguments"`
	Item        json.RawMessage `json:"item"`
	Response    json.RawMessage `json:"response"`
	Code        string          `json:"code"`
	Message     string          `json:"message"`
	Input       string          `json:"input"`
}

type responsesItem struct {
	Type             string `json:"type"`
	ID               string `json:"id"`
	CallID           string `json:"call_id"`
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	Input            string `json:"input"`
	EncryptedContent string `json:"encrypted_content"`
	Summary          []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
	Content []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}

type responsesStreamState struct {
	emit      func(Chunk) error
	nextIndex int
	blocks    map[int]*ContentBlock
	slots     map[int]*responsesSlot
	current   int
	open      bool
	usage     Usage
	finish    string
	terminal  bool
	completed json.RawMessage
	toolCount int
	model     string
}

type responsesSlot struct {
	index int
	typ   string
	block *ContentBlock
	wire  responsesItem
}

func decodeResponsesStream(ctx context.Context, body io.Reader, emit func(Chunk) error) (bool, error) {
	state := &responsesStreamState{emit: emit, blocks: map[int]*ContentBlock{}, slots: map[int]*responsesSlot{}}
	err := readSSE(ctx, body, state.event)
	if err != nil {
		return false, err
	}
	if !state.terminal {
		return false, &Failure{Code: "BAD_RESPONSE", Message: "responses stream ended before terminal event"}
	}
	if err := state.finishStream(); err != nil {
		return false, err
	}
	return false, nil
}

func (s *responsesStreamState) start(typ string) error {
	index := s.nextIndex
	s.nextIndex++
	s.current = index
	s.open = true
	block := &ContentBlock{Type: typ}
	s.blocks[index] = block
	return s.emit(Chunk{Type: "block-start", Index: index, BlockType: typ})
}

func (s *responsesStreamState) closeSlot(slot *responsesSlot) error {
	if slot == nil || !s.open || s.current != slot.index {
		return nil
	}
	if err := s.emit(Chunk{Type: "block-end", Index: slot.index, Block: slot.block}); err != nil {
		return err
	}
	s.open = false
	return nil
}

func (s *responsesStreamState) slot(outputIndex int, item *responsesItem) (*responsesSlot, error) {
	if slot, ok := s.slots[outputIndex]; ok {
		return slot, nil
	}
	if s.open {
		if err := s.emit(Chunk{Type: "block-end", Index: s.current, Block: s.blocks[s.current]}); err != nil {
			return nil, err
		}
		s.open = false
	}
	typ := "text"
	switch item.Type {
	case "reasoning":
		typ = "reasoning"
	case "function_call", "custom_tool_call":
		typ = "tool-call"
		s.toolCount++
	}
	if err := s.start(typ); err != nil {
		return nil, err
	}
	block := s.blocks[s.current]
	block.ID = item.CallID
	if block.ID == "" {
		block.ID = item.ID
	}
	block.Name = item.Name
	slot := &responsesSlot{index: s.current, typ: typ, block: block, wire: *item}
	s.slots[outputIndex] = slot
	if typ == "tool-call" {
		s.open = true
		if err := s.emit(Chunk{Type: "tool-call-delta", Index: slot.index, ID: block.ID, Name: block.Name, HasName: block.Name != "", ArgumentsDelta: ""}); err != nil {
			return nil, err
		}
	}
	return slot, nil
}

func (s *responsesStreamState) event(payload []byte) error {
	var event wireResponsesEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return &Failure{Code: "BAD_RESPONSE", Message: "malformed stream chunk: " + err.Error()}
	}
	switch event.Type {
	case "response.created":
		// The model is learned from the terminal response.
	case "response.output_item.added":
		var item responsesItem
		if err := json.Unmarshal(event.Item, &item); err != nil {
			return &Failure{Code: "BAD_RESPONSE", Message: err.Error()}
		}
		_, err := s.slot(event.OutputIndex, &item)
		return err
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		slot := s.slots[event.OutputIndex]
		if slot == nil {
			return nil
		}
		slot.block.Text += event.Delta
		return s.emit(Chunk{Type: "reasoning-delta", Index: slot.index, Text: event.Delta})
	case "response.reasoning_summary_part.done":
		slot := s.slots[event.OutputIndex]
		if slot == nil {
			return nil
		}
		slot.block.Text += "\n\n"
		return s.emit(Chunk{Type: "reasoning-delta", Index: slot.index, Text: "\n\n"})
	case "response.output_text.delta", "response.refusal.delta":
		slot := s.slots[event.OutputIndex]
		if slot == nil {
			return nil
		}
		slot.block.Text += event.Delta
		return s.emit(Chunk{Type: "text-delta", Index: slot.index, Text: event.Delta})
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		slot := s.slots[event.OutputIndex]
		if slot == nil {
			return nil
		}
		slot.block.Arguments += event.Delta
		return s.emit(Chunk{Type: "tool-call-delta", Index: slot.index, ID: slot.block.ID, Name: slot.block.Name, ArgumentsDelta: event.Delta})
	case "response.function_call_arguments.done", "response.custom_tool_call_input.done":
		slot := s.slots[event.OutputIndex]
		if slot == nil {
			return nil
		}
		final := event.Arguments
		if event.Type == "response.custom_tool_call_input.done" {
			final = event.Input
		}
		if final == "" {
			final = slot.block.Arguments
		}
		if strings.HasPrefix(final, slot.block.Arguments) {
			delta := strings.TrimPrefix(final, slot.block.Arguments)
			slot.block.Arguments = final
			if delta != "" {
				return s.emit(Chunk{Type: "tool-call-delta", Index: slot.index, ID: slot.block.ID, Name: slot.block.Name, ArgumentsDelta: delta})
			}
		} else {
			slot.block.Arguments = final
		}
	case "response.output_item.done":
		var item responsesItem
		if err := json.Unmarshal(event.Item, &item); err != nil {
			return &Failure{Code: "BAD_RESPONSE", Message: err.Error()}
		}
		slot := s.slots[event.OutputIndex]
		if slot == nil {
			var err error
			slot, err = s.slot(event.OutputIndex, &item)
			if err != nil {
				return err
			}
		}
		slot.wire = item
		switch item.Type {
		case "reasoning":
			var text strings.Builder
			for _, part := range item.Summary {
				if text.Len() > 0 {
					text.WriteString("\n\n")
				}
				text.WriteString(part.Text)
			}
			for _, part := range item.Content {
				if text.Len() > 0 {
					text.WriteString("\n\n")
				}
				text.WriteString(part.Text)
			}
			if text.Len() > 0 {
				slot.block.Text = text.String()
			}
			slot.block.Signature = string(event.Item)
		case "message":
			var text strings.Builder
			for _, part := range item.Content {
				if part.Type == "output_text" {
					text.WriteString(part.Text)
				} else if part.Type == "refusal" {
					text.WriteString(part.Refusal)
				}
			}
			if text.Len() > 0 {
				slot.block.Text = text.String()
			}
		case "function_call", "custom_tool_call":
			if item.Arguments != "" {
				slot.block.Arguments = item.Arguments
			}
			if slot.block.Arguments == "" {
				slot.block.Arguments = "{}"
			}
			slot.block.ID = item.CallID
			if slot.block.ID == "" {
				slot.block.ID = item.ID
			}
			slot.block.Name = item.Name
		}
		return s.closeSlot(slot)
	case "response.completed", "response.incomplete":
		s.terminal = true
		s.completed = append(json.RawMessage(nil), event.Response...)
		var parsed struct {
			Status            string          `json:"status"`
			Model             string          `json:"model"`
			Output            []responsesItem `json:"output"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
			Usage *struct {
				InputTokens        int `json:"input_tokens"`
				OutputTokens       int `json:"output_tokens"`
				TotalTokens        int `json:"total_tokens"`
				InputTokensDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"input_tokens_details"`
				OutputTokensDetails *struct {
					ReasoningTokens int `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
		}
		if len(event.Response) > 0 {
			if err := json.Unmarshal(event.Response, &parsed); err != nil {
				return &Failure{Code: "BAD_RESPONSE", Message: err.Error()}
			}
		}
		if parsed.Model != "" {
			s.model = parsed.Model
		}
		if parsed.Usage != nil {
			s.usage = Usage{
				InputTokens:     parsed.Usage.InputTokens,
				OutputTokens:    parsed.Usage.OutputTokens,
				TotalTokens:     parsed.Usage.TotalTokens,
				ReasoningTokens: 0,
			}
			if parsed.Usage.InputTokensDetails != nil {
				s.usage.CacheReadTokens = parsed.Usage.InputTokensDetails.CachedTokens
			}
			if parsed.Usage.OutputTokensDetails != nil {
				s.usage.ReasoningTokens = parsed.Usage.OutputTokensDetails.ReasoningTokens
			}
		}
		if parsed.IncompleteDetails != nil && parsed.IncompleteDetails.Reason == "max_output_tokens" {
			s.finish = "max-tokens"
		} else if s.toolCount > 0 {
			s.finish = "tool-calls"
		} else {
			s.finish = "stop"
		}
		// Finalize output items that never received output_item.done.
		for i := range parsed.Output {
			item := &parsed.Output[i]
			outputIndex := i
			slot := s.slots[outputIndex]
			if slot == nil && item.Type != "" {
				continue
			}
			if slot != nil && slot.block != nil {
				if item.Type == "function_call" && item.Arguments != "" {
					slot.block.Arguments = item.Arguments
				}
				if err := s.closeSlot(slot); err != nil {
					return err
				}
			}
		}
	case "response.failed":
		var parsed struct {
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(event.Response, &parsed)
		message := event.Message
		if parsed.Error != nil && parsed.Error.Message != "" {
			message = parsed.Error.Message
		}
		if message == "" {
			message = "response failed"
		}
		return &Failure{Code: "PROVIDER", Message: message}
	case "error":
		message := event.Message
		if message == "" {
			message = "provider error"
		}
		return &Failure{Code: "PROVIDER", Message: message}
	}
	return nil
}

func (s *responsesStreamState) finishStream() error {
	if s.open {
		if err := s.emit(Chunk{Type: "block-end", Index: s.current, Block: s.blocks[s.current]}); err != nil {
			return err
		}
		s.open = false
	}
	if len(s.blocks) == 0 {
		return &Failure{Code: "EMPTY_RESPONSE", Message: "model returned no content"}
	}
	usage := s.usage
	if err := s.emit(Chunk{Type: "usage", Usage: &usage}); err != nil {
		return err
	}
	replay := json.RawMessage(nil)
	if len(s.completed) > 0 {
		envelope, _ := json.Marshal(map[string]any{"response": json.RawMessage(s.completed)})
		replay = envelope
	}
	reason := s.finish
	if reason == "" {
		reason = "stop"
	}
	return s.emit(Chunk{Type: "finish", Reason: reason, ReplayState: replay, Model: s.model})
}

func decodeResponsesJSONAnswer(ctx context.Context, body io.Reader, emit func(Chunk) error) error {
	payload, err := io.ReadAll(io.LimitReader(body, 32*1024*1024))
	if err != nil {
		return &Failure{Code: "NETWORK", Message: err.Error()}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var parsed struct {
		ID     string          `json:"id"`
		Model  string          `json:"model"`
		Status string          `json:"status"`
		Output []responsesItem `json:"output"`
		Usage  json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return &Failure{Code: "BAD_RESPONSE", Message: err.Error()}
	}
	if len(parsed.Output) == 0 {
		return &Failure{Code: "EMPTY_RESPONSE", Message: "model returned no content"}
	}
	envelope, _ := json.Marshal(map[string]any{"response": json.RawMessage(payload)})
	state := &responsesStreamState{emit: emit, blocks: map[int]*ContentBlock{}, slots: map[int]*responsesSlot{}, finish: "stop", terminal: true, completed: envelope, model: parsed.Model}
	for i := range parsed.Output {
		item := &parsed.Output[i]
		slot, err := state.slot(i, item)
		if err != nil {
			return err
		}
		switch item.Type {
		case "reasoning":
			var text strings.Builder
			for _, part := range item.Summary {
				text.WriteString(part.Text)
			}
			for _, part := range item.Content {
				text.WriteString(part.Text)
			}
			slot.block.Text = text.String()
			slot.block.Signature = string(mustMarshal(item))
			if text.Len() > 0 {
				if err := emit(Chunk{Type: "reasoning-delta", Index: slot.index, Text: text.String()}); err != nil {
					return err
				}
			}
		case "message":
			var text strings.Builder
			for _, part := range item.Content {
				if part.Type == "output_text" {
					text.WriteString(part.Text)
				} else {
					text.WriteString(part.Refusal)
				}
			}
			slot.block.Text = text.String()
			if text.Len() > 0 {
				if err := emit(Chunk{Type: "text-delta", Index: slot.index, Text: text.String()}); err != nil {
					return err
				}
			}
		case "function_call", "custom_tool_call":
			slot.block.Arguments = item.Arguments
			if slot.block.Arguments == "" {
				slot.block.Arguments = "{}"
			}
			state.toolCount++
			if err := emit(Chunk{Type: "tool-call-delta", Index: slot.index, ID: slot.block.ID, Name: slot.block.Name, HasName: slot.block.Name != "", ArgumentsDelta: slot.block.Arguments}); err != nil {
				return err
			}
		}
		if err := state.closeSlot(slot); err != nil {
			return err
		}
	}
	return state.finishStream()
}

func mustMarshal(v any) []byte {
	encoded, _ := json.Marshal(v)
	return encoded
}

// -------------------------------------------------------------- requests

func (c *Client) encodeStreamRequest(req Request) ([]byte, error) {
	switch c.cfg.Protocol {
	case ProtocolAnthropicMessages:
		return encodeAnthropicRequest(req), nil
	case ProtocolOpenAIResponses:
		return encodeResponsesRequest(req), nil
	case ProtocolOpenAIChat:
		return encodeChatRequest(req), nil
	default:
		return nil, &Failure{Code: "INVALID_REQUEST", Message: "unknown protocol: " + c.cfg.Protocol}
	}
}

// imageDataURL renders one inline image for OpenAI-style content parts.
func imageDataURL(image Image) string {
	return "data:" + image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(image.Data)
}

func openAIToolContent(message Message) []any {
	parts := make([]any, 0, len(message.Images)+1)
	if message.Content != "" {
		parts = append(parts, map[string]any{"type": "text", "text": message.Content})
	}
	for _, image := range message.Images {
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": imageDataURL(image)},
		})
	}
	return parts
}

func anthropicToolContent(message Message) any {
	if len(message.Images) == 0 {
		return message.Content
	}
	parts := make([]any, 0, len(message.Images)+1)
	if message.Content != "" {
		parts = append(parts, map[string]any{"type": "text", "text": message.Content})
	}
	for _, image := range message.Images {
		parts = append(parts, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": image.MediaType,
				"data":       base64.StdEncoding.EncodeToString(image.Data),
			},
		})
	}
	return parts
}

func responsesToolContent(message Message) []any {
	parts := make([]any, 0, len(message.Images)+1)
	if message.Content != "" {
		parts = append(parts, map[string]any{"type": "input_text", "text": message.Content})
	}
	for _, image := range message.Images {
		parts = append(parts, map[string]any{"type": "input_image", "image_url": imageDataURL(image)})
	}
	return parts
}

func openAIMessages(messages []Message) []any {
	out := make([]any, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case "tool":
			entry := map[string]any{"role": "tool", "tool_call_id": message.ToolCallID}
			if len(message.Images) > 0 {
				entry["content"] = openAIToolContent(message)
			} else {
				entry["content"] = message.Content
			}
			out = append(out, entry)
		case "assistant":
			entry := map[string]any{"role": "assistant"}
			var text strings.Builder
			var reasoning strings.Builder
			var calls []any
			if len(message.Blocks) > 0 {
				for _, block := range message.Blocks {
					switch block.Type {
					case "text":
						text.WriteString(block.Text)
					case "reasoning":
						reasoning.WriteString(block.Text)
					case "tool-call":
						args := block.Arguments
						if args == "" {
							args = "{}"
						}
						calls = append(calls, map[string]any{
							"id": block.ID, "type": "function",
							"function": map[string]any{"name": block.Name, "arguments": args},
						})
					}
				}
			} else {
				text.WriteString(message.Content)
			}
			entry["content"] = text.String()
			if reasoning.Len() > 0 {
				entry["reasoning_content"] = reasoning.String()
			}
			if len(calls) > 0 {
				entry["tool_calls"] = calls
			}
			out = append(out, entry)
		default:
			role := message.Role
			if role == "developer" {
				role = "system"
			}
			out = append(out, map[string]any{"role": role, "content": message.Content})
		}
	}
	return out
}

func chatTools(tools []ToolDef) []any {
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		parameters := tool.Parameters
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": tool.Name, "description": tool.Description, "parameters": parameters,
			},
		})
	}
	return out
}

func encodeChatRequest(req Request) []byte {
	body := map[string]any{
		"model":          req.Model,
		"messages":       openAIMessages(req.Messages),
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if len(req.Tools) > 0 {
		body["tools"] = chatTools(req.Tools)
	}
	if req.ReasoningEffort != "" && req.ReasoningEffort != "off" {
		body["reasoning_effort"] = req.ReasoningEffort
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	encoded, _ := json.Marshal(body)
	return encoded
}

func anthropicMessages(messages []Message) (string, []any) {
	var system []string
	out := make([]any, 0, len(messages))
	for i := 0; i < len(messages); i++ {
		message := messages[i]
		switch message.Role {
		case "system", "developer":
			if message.Content != "" {
				system = append(system, message.Content)
			}
		case "tool":
			blocks := []any{map[string]any{
				"type": "tool_result", "tool_use_id": message.ToolCallID,
				"content": anthropicToolContent(message), "is_error": message.IsError,
			}}
			for i+1 < len(messages) && messages[i+1].Role == "tool" {
				i++
				next := messages[i]
				blocks = append(blocks, map[string]any{
					"type": "tool_result", "tool_use_id": next.ToolCallID,
					"content": anthropicToolContent(next), "is_error": next.IsError,
				})
			}
			out = append(out, map[string]any{"role": "user", "content": blocks})
		case "assistant":
			blocks := anthropicAssistantBlocks(message)
			if len(blocks) == 0 {
				blocks = []any{map[string]any{"type": "text", "text": message.Content}}
			}
			out = append(out, map[string]any{"role": "assistant", "content": blocks})
		default:
			out = append(out, map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": message.Content}}})
		}
	}
	return strings.Join(system, "\n\n"), out
}

func anthropicAssistantBlocks(message Message) []any {
	if len(message.Blocks) == 0 {
		if message.Content == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": message.Content}}
	}
	out := make([]any, 0, len(message.Blocks))
	for _, block := range message.Blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				out = append(out, map[string]any{"type": "text", "text": block.Text})
			}
		case "reasoning":
			if block.Text == "" {
				continue
			}
			entry := map[string]any{"type": "thinking", "thinking": block.Text}
			if block.Signature != "" {
				entry["signature"] = block.Signature
			} else {
				continue
			}
			out = append(out, entry)
		case "tool-call":
			var input any = map[string]any{}
			if block.Arguments != "" && json.Valid([]byte(block.Arguments)) {
				_ = json.Unmarshal([]byte(block.Arguments), &input)
				if _, ok := input.(map[string]any); !ok {
					input = map[string]any{}
				}
			}
			out = append(out, map[string]any{"type": "tool_use", "id": block.ID, "name": block.Name, "input": input})
		}
	}
	return out
}

func encodeAnthropicRequest(req Request) []byte {
	system, messages := anthropicMessages(req.Messages)
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	body := map[string]any{
		"model": req.Model, "max_tokens": maxTokens, "messages": messages, "stream": true,
	}
	if system != "" {
		body["system"] = system
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			parameters := tool.Parameters
			if parameters == nil {
				parameters = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"name": tool.Name, "description": tool.Description, "input_schema": parameters})
		}
		body["tools"] = tools
	}
	thinking := req.ReasoningEffort != "" && req.ReasoningEffort != "off" && req.ReasoningEffort != "none"
	if thinking {
		budget := anthropicThinkingBudget(req.ReasoningEffort)
		if maxTokens <= budget {
			maxTokens = budget + 1024
			body["max_tokens"] = maxTokens
		}
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
	} else if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	encoded, _ := json.Marshal(body)
	return encoded
}

func anthropicThinkingBudget(effort string) int {
	switch effort {
	case "xhigh":
		return 8192
	case "high":
		return 4096
	case "medium":
		return 2048
	default:
		return 1024
	}
}

// responsesReplayItems restores the exact output items of an earlier response:
// encrypted reasoning, assistant text, and function calls in their original
// order. Without them an encrypted-reasoning model rejects the next request.
func responsesReplayItems(replay json.RawMessage) []any {
	if len(replay) == 0 {
		return nil
	}
	var envelope struct {
		Response struct {
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal(replay, &envelope); err != nil {
		return nil
	}
	items := make([]any, 0, len(envelope.Response.Output))
	for _, raw := range envelope.Response.Output {
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		if item["type"] == nil {
			continue
		}
		items = append(items, item)
	}
	return items
}

func encodeResponsesRequest(req Request) []byte {
	input := make([]any, 0, len(req.Messages))
	for _, message := range req.Messages {
		switch message.Role {
		case "tool":
			output := any(message.Content)
			if len(message.Images) > 0 {
				output = responsesToolContent(message)
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": message.ToolCallID, "output": output})
		case "assistant":
			if replay := responsesReplayItems(message.Replay); len(replay) > 0 {
				input = append(input, replay...)
				continue
			}
			var text strings.Builder
			if len(message.Blocks) == 0 {
				text.WriteString(message.Content)
			} else {
				for _, block := range message.Blocks {
					if block.Type == "text" {
						text.WriteString(block.Text)
					}
				}
			}
			if text.Len() > 0 {
				input = append(input, map[string]any{"role": "assistant", "content": text.String()})
			}
			if len(message.Blocks) > 0 {
				for _, block := range message.Blocks {
					if block.Type != "tool-call" {
						continue
					}
					args := block.Arguments
					if args == "" {
						args = "{}"
					}
					input = append(input, map[string]any{"type": "function_call", "call_id": block.ID, "name": block.Name, "arguments": args})
				}
			}
		case "developer":
			input = append(input, map[string]any{"role": "system", "content": message.Content})
		default:
			input = append(input, map[string]any{"role": message.Role, "content": message.Content})
		}
	}
	body := map[string]any{
		"model": req.Model, "input": input, "stream": true, "store": false,
		"include": []string{"reasoning.encrypted_content"},
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			parameters := tool.Parameters
			if parameters == nil {
				parameters = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{
				"type": "function", "name": tool.Name, "description": tool.Description,
				"parameters": parameters, "strict": false,
			})
		}
		body["tools"] = tools
	}
	if req.ReasoningEffort != "" && req.ReasoningEffort != "off" && req.ReasoningEffort != "none" {
		effort := req.ReasoningEffort
		if effort == "xhigh" {
			effort = "high"
		}
		body["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
	}
	if req.MaxTokens > 0 {
		body["max_output_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	encoded, _ := json.Marshal(body)
	return encoded
}

// keep strconv imported for compatibility with older helpers
var _ = strconv.Itoa
