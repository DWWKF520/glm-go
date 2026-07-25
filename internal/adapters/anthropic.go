package adapters

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// safeJSON 紧凑 JSON 序列化
func safeJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// AnthropicToOpenAI 将 Anthropic Messages 请求体转换为 OpenAI chat/completions 格式
func AnthropicToOpenAI(payload map[string]any) map[string]any {
	var messages []map[string]any

	// system
	system := payload["system"]
	if system != nil {
		if s, ok := system.(string); ok && s != "" {
			messages = append(messages, map[string]any{"role": "system", "content": s})
		} else if blocks, ok := system.([]any); ok {
			var textParts []string
			for _, b := range blocks {
				bm, ok := b.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := bm["type"].(string); t == "text" {
					if text, _ := bm["text"].(string); text != "" {
						textParts = append(textParts, text)
					}
				}
			}
			if len(textParts) > 0 {
				messages = append(messages, map[string]any{"role": "system", "content": strings.Join(textParts, "\n")})
			}
		}
	}

	// messages
	if msgs, ok := payload["messages"].([]any); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			if role == "" {
				role = "user"
			}
			content := msg["content"]

			if s, ok := content.(string); ok {
				messages = append(messages, map[string]any{"role": role, "content": s})
				continue
			}

			contentList, ok := content.([]any)
			if !ok {
				messages = append(messages, map[string]any{"role": role, "content": ""})
				continue
			}

			var openaiContentParts []map[string]any
			var toolCalls []map[string]any
			var toolResults []map[string]any

			for _, b := range contentList {
				block, ok := b.(map[string]any)
				if !ok {
					continue
				}
				blockType, _ := block["type"].(string)

				switch blockType {
				case "text":
					text, _ := block["text"].(string)
					openaiContentParts = append(openaiContentParts, map[string]any{"type": "text", "text": text})
				case "thinking":
					if thinking, _ := block["thinking"].(string); thinking != "" {
						openaiContentParts = append(openaiContentParts, map[string]any{"type": "text", "text": thinking})
					}
				case "image":
					source, _ := block["source"].(map[string]any)
					if source != nil {
						mediaType, _ := source["media_type"].(string)
						if mediaType == "" {
							mediaType = "image/png"
						}
						data, _ := source["data"].(string)
						sourceType, _ := source["type"].(string)
						if sourceType == "base64" && data != "" {
							openaiContentParts = append(openaiContentParts, map[string]any{
								"type":      "image_url",
								"image_url": map[string]any{"url": fmt.Sprintf("data:%s;base64,%s", mediaType, data)},
							})
						} else if sourceType == "url" {
							if u, _ := source["url"].(string); u != "" {
								openaiContentParts = append(openaiContentParts, map[string]any{
									"type":      "image_url",
									"image_url": map[string]any{"url": u},
								})
							}
						}
					}
				case "tool_use":
					id, _ := block["id"].(string)
					if id == "" {
						id = "call_" + uuid.New().String()[:24]
					}
					name, _ := block["name"].(string)
					input := block["input"]
					if input == nil {
						input = map[string]any{}
					}
					inputBytes, _ := json.Marshal(input)
					toolCalls = append(toolCalls, map[string]any{
						"id":   id,
						"type": "function",
						"function": map[string]any{
							"name":      name,
							"arguments": string(inputBytes),
						},
					})
				case "tool_result":
					resultContent := block["content"]
					resultText := ""
					if s, ok := resultContent.(string); ok {
						resultText = s
					} else if list, ok := resultContent.([]any); ok {
						var parts []string
						for _, rc := range list {
							rcm, ok := rc.(map[string]any)
							if !ok {
								continue
							}
							if t, _ := rcm["type"].(string); t == "text" {
								if text, _ := rcm["text"].(string); text != "" {
									parts = append(parts, text)
								}
							}
						}
						resultText = strings.Join(parts, "\n")
					}
					toolUseID, _ := block["tool_use_id"].(string)
					toolResults = append(toolResults, map[string]any{
						"role":         "tool",
						"tool_call_id": toolUseID,
						"content":      resultText,
					})
				}
			}

			if len(toolResults) > 0 {
				messages = append(messages, toolResults...)
			} else if len(toolCalls) > 0 {
				textContent := ""
				if len(openaiContentParts) > 0 {
					var parts []string
					for _, p := range openaiContentParts {
						if t, _ := p["type"].(string); t == "text" {
							if text, _ := p["text"].(string); text != "" {
								parts = append(parts, text)
							}
						}
					}
					textContent = strings.Join(parts, "\n")
				}
				msgOut := map[string]any{
					"role":      "assistant",
					"content":   nil,
					"tool_calls": toolCalls,
				}
				if textContent != "" {
					msgOut["content"] = textContent
				}
				messages = append(messages, msgOut)
			} else if len(openaiContentParts) == 1 {
				if t, _ := openaiContentParts[0]["type"].(string); t == "text" {
					messages = append(messages, map[string]any{"role": role, "content": openaiContentParts[0]["text"]})
				} else {
					messages = append(messages, map[string]any{"role": role, "content": openaiContentParts})
				}
			} else if len(openaiContentParts) > 0 {
				messages = append(messages, map[string]any{"role": role, "content": openaiContentParts})
			} else {
				messages = append(messages, map[string]any{"role": role, "content": ""})
			}
		}
	}

	// 构建输出
	model, _ := payload["model"].(string)
	if model == "" {
		model = "glm-4"
	}
	result := map[string]any{
		"model":    model,
		"messages": messages,
		"stream":   payload["stream"],
	}
	if result["stream"] == nil {
		result["stream"] = false
	}
	if mt := payload["max_tokens"]; mt != nil {
		result["max_tokens"] = mt
	}
	if t := payload["temperature"]; t != nil {
		result["temperature"] = t
	}
	if tp := payload["top_p"]; tp != nil {
		result["top_p"] = tp
	}
	if ss := payload["stop_sequences"]; ss != nil {
		result["stop"] = ss
	}

	// tools
	if anthropicTools, ok := payload["tools"].([]any); ok && len(anthropicTools) > 0 {
		var openaiTools []map[string]any
		for _, t := range anthropicTools {
			tool, ok := t.(map[string]any)
			if !ok {
				continue
			}
			name, _ := tool["name"].(string)
			description, _ := tool["description"].(string)
			parameters := tool["input_schema"]
			if parameters == nil {
				parameters = map[string]any{}
			}
			openaiTools = append(openaiTools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": description,
					"parameters":  parameters,
				},
			})
		}
		if len(openaiTools) > 0 {
			result["tools"] = openaiTools
		}
	}

	if toolChoice, ok := payload["tool_choice"].(map[string]any); ok {
		choiceType, _ := toolChoice["type"].(string)
		choiceType = strings.ToLower(strings.TrimSpace(choiceType))
		switch choiceType {
		case "auto":
			result["tool_choice"] = "auto"
		case "any":
			result["tool_choice"] = "required"
		case "tool":
			if name, _ := toolChoice["name"].(string); name != "" {
				result["tool_choice"] = map[string]any{
					"type":     "function",
					"function": map[string]any{"name": name},
				}
			}
		}
	}

	// thinking
	if thinking, ok := payload["thinking"].(map[string]any); ok {
		if t, _ := thinking["type"].(string); t == "enabled" {
			if budget, ok := thinking["budget_tokens"]; ok {
				result["reasoning_effort"] = budget
			} else {
				result["reasoning_effort"] = "medium"
			}
		}
	}

	return result
}

// OpenAIToAnthropicResponse 将 OpenAI chat/completions 响应转换为 Anthropic Messages 格式
func OpenAIToAnthropicResponse(result map[string]any, model string) map[string]any {
	var content []map[string]any
	stopReason := "end_turn"

	if choices, ok := result["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			message, _ := choice["message"].(map[string]any)
			if message != nil {
				if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" {
					content = append(content, map[string]any{
						"type":     "thinking",
						"thinking": reasoning,
					})
				}
				if text, ok := message["content"].(string); ok && text != "" {
					content = append(content, map[string]any{"type": "text", "text": text})
				}
				if toolCalls, ok := message["tool_calls"].([]any); ok {
					stopReason = "tool_use"
					for _, tc := range toolCalls {
						tcm, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						fn, _ := tcm["function"].(map[string]any)
						var input any = map[string]any{}
						if fn != nil {
							if args, ok := fn["arguments"].(string); ok {
								var v any
								if err := json.Unmarshal([]byte(args), &v); err == nil {
									input = v
								}
							}
						}
						id, _ := tcm["id"].(string)
						if id == "" {
							id = "toolu_" + uuid.New().String()[:24]
						}
						name := ""
						if fn != nil {
							name, _ = fn["name"].(string)
						}
						content = append(content, map[string]any{
							"type":  "tool_use",
							"id":    id,
							"name":  name,
							"input": input,
						})
					}
				}
			}
			if finishReason, _ := choice["finish_reason"].(string); finishReason == "length" {
				stopReason = "max_tokens"
			}
		}
	}

	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}

	usage, _ := result["usage"].(map[string]any)
	inputTokens := 0
	outputTokens := 0
	if usage != nil {
		if it, ok := usage["prompt_tokens"].(float64); ok {
			inputTokens = int(it)
		}
		if ot, ok := usage["completion_tokens"].(float64); ok {
			outputTokens = int(ot)
		}
	}

	return map[string]any{
		"id":            "msg_" + uuid.New().String()[:24],
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         model,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}
}

// AnthropicStreamAccumulator 将 OpenAI 流式 chunks 转换为 Anthropic SSE 事件
type AnthropicStreamAccumulator struct {
	model           string
	messageID       string
	created         int64
	started         bool
	contentIndex    int
	currentBlockType string
	inputTokens     int
	outputTokens    int
	stopReason      string
	pendingToolCalls map[int]map[string]any
	blockOpen       bool
	finished        bool
}

// NewAnthropicStreamAccumulator 创建 Anthropic 流式累加器
func NewAnthropicStreamAccumulator(model string) *AnthropicStreamAccumulator {
	return &AnthropicStreamAccumulator{
		model:            model,
		messageID:        "msg_" + uuid.New().String()[:24],
		created:          time.Now().Unix(),
		stopReason:       "end_turn",
		pendingToolCalls: map[int]map[string]any{},
	}
}

// StartMessage 发送 message_start 事件
func (a *AnthropicStreamAccumulator) StartMessage() string {
	a.started = true
	msg := map[string]any{
		"id":            a.messageID,
		"type":          "message",
		"role":          "assistant",
		"content":       []any{},
		"model":         a.model,
		"stop_reason":   nil,
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": a.inputTokens, "output_tokens": 0},
	}
	return a.sse("message_start", map[string]any{"type": "message_start", "message": msg})
}

// FeedChunk 处理原始 SSE chunk（已解码），返回 Anthropic SSE 事件
func (a *AnthropicStreamAccumulator) FeedChunk(chunk []byte) []string {
	text := string(chunk)
	var events []string
	for _, line := range strings.Split(text, "\n\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "data: [DONE]" {
			events = append(events, a.finish()...)
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			var data map[string]any
			if err := json.Unmarshal([]byte(line[6:]), &data); err != nil {
				continue
			}
			events = append(events, a.processOpenAIChunk(data)...)
		}
	}
	return events
}

func (a *AnthropicStreamAccumulator) processOpenAIChunk(data map[string]any) []string {
	var events []string
	if !a.started {
		events = append(events, a.StartMessage())
	}

	choices, ok := data["choices"].([]any)
	if !ok || len(choices) == 0 {
		if usage, ok := data["usage"].(map[string]any); ok {
			if it, ok := usage["prompt_tokens"].(float64); ok {
				a.inputTokens = int(it)
			}
			if ot, ok := usage["completion_tokens"].(float64); ok {
				a.outputTokens = int(ot)
			}
		}
		return events
	}

	choice, ok := choices[0].(map[string]any)
	if !ok {
		return events
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return events
	}
	finishReason, _ := choice["finish_reason"].(string)

	// reasoning_content -> thinking block
	if reasoning, ok := delta["reasoning_content"].(string); ok && reasoning != "" {
		if a.currentBlockType != "thinking" {
			if a.blockOpen {
				events = append(events, a.contentBlockStop())
			}
			events = append(events, a.contentBlockStart("thinking", map[string]any{}))
			a.currentBlockType = "thinking"
		}
		events = append(events, a.sse("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": a.contentIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning},
		}))
	}

	// text content
	if content, ok := delta["content"].(string); ok && content != "" {
		if a.currentBlockType != "text" {
			if a.blockOpen {
				events = append(events, a.contentBlockStop())
			}
			events = append(events, a.contentBlockStart("text", map[string]any{"text": ""}))
			a.currentBlockType = "text"
		}
		events = append(events, a.sse("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": a.contentIndex,
			"delta": map[string]any{"type": "text_delta", "text": content},
		}))
	}

	// tool_calls
	if toolCalls, ok := delta["tool_calls"].([]any); ok {
		for _, tc := range toolCalls {
			tcm, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			tcIndex := 0
			if i, ok := tcm["index"].(float64); ok {
				tcIndex = int(i)
			}
			fn, _ := tcm["function"].(map[string]any)
			if fn == nil {
				continue
			}

			if _, exists := a.pendingToolCalls[tcIndex]; !exists {
				if a.blockOpen {
					events = append(events, a.contentBlockStop())
				}
				toolID, _ := tcm["id"].(string)
				if toolID == "" {
					toolID = "toolu_" + uuid.New().String()[:24]
				}
				toolName, _ := fn["name"].(string)
				a.pendingToolCalls[tcIndex] = map[string]any{
					"id":        toolID,
					"name":      toolName,
					"arguments": "",
				}
				events = append(events, a.contentBlockStart("tool_use", map[string]any{
					"id":    toolID,
					"name":  toolName,
					"input": map[string]any{},
				}))
				a.currentBlockType = "tool_use"
				a.stopReason = "tool_use"
			}

			argsDelta, _ := fn["arguments"].(string)
			if argsDelta != "" {
				tcData := a.pendingToolCalls[tcIndex]
				if existing, ok := tcData["arguments"].(string); ok {
					tcData["arguments"] = existing + argsDelta
				}
				events = append(events, a.sse("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": a.contentIndex,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": argsDelta},
				}))
			}
		}
	}

	if finishReason != "" {
		if finishReason == "length" {
			a.stopReason = "max_tokens"
		} else if finishReason == "tool_calls" {
			a.stopReason = "tool_use"
		}
	}

	if usage, ok := data["usage"].(map[string]any); ok {
		if it, ok := usage["prompt_tokens"].(float64); ok {
			a.inputTokens = int(it)
		}
		if ot, ok := usage["completion_tokens"].(float64); ok {
			a.outputTokens = int(ot)
		}
	}

	return events
}

func (a *AnthropicStreamAccumulator) finish() []string {
	if a.finished {
		return nil
	}
	a.finished = true
	var events []string
	if a.blockOpen {
		events = append(events, a.contentBlockStop())
	}
	events = append(events, a.sse("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": a.stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": a.outputTokens},
	}))
	events = append(events, a.sse("message_stop", map[string]any{"type": "message_stop"}))
	return events
}

func (a *AnthropicStreamAccumulator) contentBlockStart(blockType string, initial map[string]any) string {
	block := map[string]any{"type": blockType}
	for k, v := range initial {
		block[k] = v
	}
	a.blockOpen = true
	return a.sse("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         a.contentIndex,
		"content_block": block,
	})
}

func (a *AnthropicStreamAccumulator) contentBlockStop() string {
	event := a.sse("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": a.contentIndex,
	})
	a.contentIndex++
	a.blockOpen = false
	return event
}

func (a *AnthropicStreamAccumulator) sse(eventType string, data map[string]any) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, safeJSON(data))
}
