package adapters

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ResponsesToOpenAI 将 OpenAI Responses API 请求体转换为 chat/completions 格式
func ResponsesToOpenAI(payload map[string]any) map[string]any {
	var messages []map[string]any

	// instructions -> system
	if instructions, ok := payload["instructions"].(string); ok && instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}

	// input
	inputData := payload["input"]
	if s, ok := inputData.(string); ok {
		messages = append(messages, map[string]any{"role": "user", "content": s})
	} else if list, ok := inputData.([]any); ok {
		for _, item := range list {
			itemMap, ok := item.(map[string]any)
			if !ok {
				continue
			}
			itemType, _ := itemMap["type"].(string)

			if itemType == "message" || (itemType == "" && hasKey(itemMap, "content")) {
				appendResponseMessage(&messages, itemMap)
			} else if itemType == "function_call_output" {
				callID, _ := itemMap["call_id"].(string)
				toolName := ""
				// 在历史消息中查找对应 tool_call 的 name
				for i := len(messages) - 1; i >= 0; i-- {
					if messages[i]["role"] == "assistant" {
						if tcs, ok := messages[i]["tool_calls"].([]map[string]any); ok {
							for _, tc := range tcs {
								if tc["id"] == callID {
									if fn, ok := tc["function"].(map[string]any); ok {
										toolName, _ = fn["name"].(string)
									}
									break
								}
							}
							if toolName != "" {
								break
							}
						}
					}
				}
				msg := map[string]any{
					"role":         "tool",
					"tool_call_id": callID,
					"content":      fmt.Sprintf("%v", itemMap["output"]),
				}
				if toolName != "" {
					msg["name"] = toolName
				}
				messages = append(messages, msg)
			} else if itemType == "function_call" {
				args := "{}"
				if a, ok := itemMap["arguments"].(string); ok {
					args = a
				} else if itemMap["arguments"] != nil {
					b, _ := json.Marshal(itemMap["arguments"])
					args = string(b)
				}
				callID, _ := itemMap["call_id"].(string)
				if callID == "" {
					callID = "call_" + uuid.New().String()[:24]
				}
				name, _ := itemMap["name"].(string)
				tcEntry := map[string]any{
					"id":   callID,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": args,
					},
				}
				if len(messages) > 0 && messages[len(messages)-1]["role"] == "assistant" {
					if tcs, ok := messages[len(messages)-1]["tool_calls"].([]map[string]any); ok {
						messages[len(messages)-1]["tool_calls"] = append(tcs, tcEntry)
					} else {
						messages[len(messages)-1]["tool_calls"] = []map[string]any{tcEntry}
					}
				} else {
					messages = append(messages, map[string]any{
						"role":       "assistant",
						"content":    nil,
						"tool_calls": []map[string]any{tcEntry},
					})
				}
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
	if t := payload["max_output_tokens"]; t != nil {
		result["max_tokens"] = t
	}
	if t := payload["temperature"]; t != nil {
		result["temperature"] = t
	}
	if tp := payload["top_p"]; tp != nil {
		result["top_p"] = tp
	}

	// tools
	if respTools, ok := payload["tools"].([]any); ok && len(respTools) > 0 {
		var openaiTools []map[string]any
		for _, t := range respTools {
			tool, ok := t.(map[string]any)
			if !ok {
				continue
			}
			toolType, _ := tool["type"].(string)
			if toolType == "function" {
				fn := map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        tool["name"],
						"description": tool["description"],
						"parameters":  tool["parameters"],
					},
				}
				if tool["strict"] != nil {
					fn["function"].(map[string]any)["strict"] = tool["strict"]
				}
				openaiTools = append(openaiTools, fn)
			} else if strings.HasPrefix(toolType, "web_search") {
				result["web_search"] = true
			}
		}
		if len(openaiTools) > 0 {
			result["tools"] = openaiTools
		}
	}
	if tc := payload["tool_choice"]; tc != nil {
		result["tool_choice"] = tc
	}

	// reasoning
	if reasoning, ok := payload["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort != "" {
			result["reasoning_effort"] = effort
		}
	}

	return result
}

func hasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

func appendResponseMessage(messages *[]map[string]any, item map[string]any) {
	role, _ := item["role"].(string)
	if role == "" {
		role = "user"
	}
	converted := responseContentToOpenAI(item["content"])
	if converted != nil {
		*messages = append(*messages, map[string]any{"role": role, "content": converted})
	}
}

func responseContentToOpenAI(content any) any {
	if s, ok := content.(string); ok {
		return s
	}
	list, ok := content.([]any)
	if !ok {
		return nil
	}
	var openaiParts []map[string]any
	for _, p := range list {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}
		converted := responsePartToOpenAI(part)
		if converted != nil {
			openaiParts = append(openaiParts, converted)
		}
	}
	if len(openaiParts) == 1 {
		if t, _ := openaiParts[0]["type"].(string); t == "text" {
			return openaiParts[0]["text"]
		}
	}
	if len(openaiParts) > 0 {
		return openaiParts
	}
	return nil
}

func responsePartToOpenAI(part map[string]any) map[string]any {
	partType, _ := part["type"].(string)
	switch partType {
	case "input_text", "output_text", "text":
		return map[string]any{"type": "text", "text": part["text"]}
	case "input_image", "image_url":
		var imageURL any
		if u, ok := part["image_url"]; ok {
			imageURL = u
		} else if u, ok := part["url"]; ok {
			imageURL = u
		}
		if m, ok := imageURL.(map[string]any); ok {
			imageURL = m["url"]
		}
		if imageURL == nil {
			return nil
		}
		converted := map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": fmt.Sprintf("%v", imageURL)},
		}
		if detail, ok := part["detail"]; ok {
			converted["image_url"].(map[string]any)["detail"] = detail
		}
		return converted
	case "input_file", "file":
		var fileURL any
		if u, ok := part["file_url"]; ok {
			fileURL = u
		}
		if m, ok := fileURL.(map[string]any); ok {
			fileURL = m["url"]
		}
		if fileURL == nil {
			return nil
		}
		return map[string]any{
			"type":     "file",
			"file_url": map[string]any{"url": fmt.Sprintf("%v", fileURL)},
		}
	}
	return nil
}

// OpenAIToResponsesResponse 将 OpenAI chat/completions 响应转换为 Responses API 格式
func OpenAIToResponsesResponse(result map[string]any, model string) map[string]any {
	responseID := "resp_" + uuid.New().String()[:24]
	created := time.Now().Unix()
	var output []map[string]any
	var outputTextParts []string
	status := "completed"
	var incompleteDetails any

	if choices, ok := result["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			message, _ := choice["message"].(map[string]any)
			if message != nil {
				var msgContent []map[string]any
				if text, ok := message["content"].(string); ok && text != "" {
					outputTextParts = append(outputTextParts, text)
					msgContent = append(msgContent, map[string]any{
						"type":        "output_text",
						"text":        text,
						"annotations": []any{},
					})
				}
				if len(msgContent) > 0 {
					output = append(output, map[string]any{
						"type":   "message",
						"id":     "msg_" + uuid.New().String()[:24],
						"status": "completed",
						"role":   "assistant",
						"content": msgContent,
					})
				}
				if toolCalls, ok := message["tool_calls"].([]any); ok {
					for _, tc := range toolCalls {
						tcm, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						fn, _ := tcm["function"].(map[string]any)
						argsStr := "{}"
						if fn != nil {
							if args, ok := fn["arguments"].(string); ok {
								argsStr = args
							}
						}
						callID, _ := tcm["id"].(string)
						if callID == "" {
							callID = "call_" + uuid.New().String()[:24]
						}
						name := ""
						if fn != nil {
							name, _ = fn["name"].(string)
						}
						output = append(output, map[string]any{
							"type":      "function_call",
							"id":        "fc_" + uuid.New().String()[:24],
							"call_id":   callID,
							"name":      name,
							"arguments": argsStr,
							"status":    "completed",
						})
					}
				}
			}
			if finishReason, _ := choice["finish_reason"].(string); finishReason == "length" {
				status = "incomplete"
				incompleteDetails = map[string]any{"reason": "max_output_tokens"}
			}
		}
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
		"id":                  responseID,
		"object":              "response",
		"created_at":          created,
		"status":              status,
		"error":               nil,
		"incomplete_details":  incompleteDetails,
		"instructions":        nil,
		"model":               model,
		"output":              output,
		"output_text":         strings.Join(outputTextParts, ""),
		"parallel_tool_calls": true,
		"previous_response_id": nil,
		"store":               false,
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
			"total_tokens":  inputTokens + outputTokens,
		},
	}
}

// ResponsesStreamAccumulator 将 OpenAI 流式 chunks 转换为 Responses SSE 事件
type ResponsesStreamAccumulator struct {
	model              string
	responseID         string
	created            int64
	started            bool
	outputIndex        int
	contentIndex       int
	currentType        string
	inputTokens        int
	outputTokens       int
	textBuffer         string
	fullText           string
	currentMsgID       string
	currentFCID        string
	pendingToolCalls   map[int]map[string]string
	messageStarted     bool
	contentPartStarted bool
	completedOutput    []map[string]any
	finished           bool
	sequenceNumber     int
	pendingSSE         string
}

// NewResponsesStreamAccumulator 创建 Responses 流式累加器
func NewResponsesStreamAccumulator(model string) *ResponsesStreamAccumulator {
	return &ResponsesStreamAccumulator{
		model:            model,
		responseID:       "resp_" + uuid.New().String()[:24],
		created:          time.Now().Unix(),
		pendingToolCalls: map[int]map[string]string{},
	}
}

func (a *ResponsesStreamAccumulator) baseResponse(status string) map[string]any {
	var usage any
	if status == "completed" {
		usage = map[string]any{
			"input_tokens":         a.inputTokens,
			"output_tokens":        a.outputTokens,
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			"total_tokens":         a.inputTokens + a.outputTokens,
		}
	}
	completedAt := any(nil)
	if status == "completed" {
		completedAt = time.Now().Unix()
	}
	output := make([]any, 0, len(a.completedOutput))
	for _, o := range a.completedOutput {
		output = append(output, o)
	}
	return map[string]any{
		"id":                  a.responseID,
		"object":              "response",
		"created_at":          a.created,
		"status":              status,
		"completed_at":        completedAt,
		"error":               nil,
		"incomplete_details":  nil,
		"instructions":        nil,
		"max_output_tokens":   nil,
		"model":               a.model,
		"output":              output,
		"parallel_tool_calls": true,
		"previous_response_id": nil,
		"reasoning":           map[string]any{"effort": nil, "summary": nil},
		"store":               false,
		"temperature":         1,
		"text":                map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":         "auto",
		"tools":               []any{},
		"top_p":               1,
		"truncation":          "disabled",
		"usage":               usage,
		"user":                nil,
		"metadata":            map[string]any{},
	}
}

// StartResponse 发送 response.created 和 response.in_progress 事件
func (a *ResponsesStreamAccumulator) StartResponse() []string {
	a.started = true
	return []string{
		a.sse("response.created", a.baseResponse("in_progress")),
		a.sse("response.in_progress", a.baseResponse("in_progress")),
	}
}

// FeedChunk 处理原始 SSE chunk，返回 Responses SSE 事件
func (a *ResponsesStreamAccumulator) FeedChunk(chunk []byte) []string {
	text := a.pendingSSE + string(chunk)
	a.pendingSSE = ""
	var events []string
	blocks := strings.Split(text, "\n\n")
	if !strings.HasSuffix(text, "\n\n") {
		a.pendingSSE = blocks[len(blocks)-1]
		blocks = blocks[:len(blocks)-1]
	}
	for _, line := range blocks {
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

func (a *ResponsesStreamAccumulator) processOpenAIChunk(data map[string]any) []string {
	var events []string
	if !a.started {
		events = append(events, a.StartResponse()...)
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

	// 跳过 reasoning_content - Responses API 没有 thinking block
	content, _ := delta["content"].(string)

	if content != "" {
		if !a.messageStarted {
			events = append(events, a.startMessageOutput()...)
		}
		if !a.contentPartStarted {
			events = append(events, a.startContentPart()...)
		}
		events = append(events, a.sse("response.output_text.delta", map[string]any{
			"type":           "response.output_text.delta",
			"item_id":        a.currentMsgID,
			"output_index":   a.outputIndex,
			"content_index":  a.contentIndex,
			"delta":          content,
		}))
		a.textBuffer += content
		a.fullText += content
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
				if a.contentPartStarted {
					events = append(events, a.endContentPart()...)
				}
				if a.messageStarted {
					events = append(events, a.endMessageOutput()...)
				}

				fcID := "fc_" + uuid.New().String()[:24]
				callID, _ := tcm["id"].(string)
				if callID == "" {
					callID = "call_" + uuid.New().String()[:24]
				}
				toolName, _ := fn["name"].(string)
				a.pendingToolCalls[tcIndex] = map[string]string{
					"id":           fcID,
					"call_id":      callID,
					"name":         toolName,
					"arguments":    "",
					"output_index": fmt.Sprintf("%d", a.outputIndex),
				}
				a.currentFCID = fcID
				a.currentType = "function_call"

				fcItem := map[string]any{
					"type":      "function_call",
					"id":        fcID,
					"call_id":   callID,
					"name":      toolName,
					"arguments": "",
					"status":    "in_progress",
				}
				events = append(events, a.sse("response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"output_index": a.outputIndex,
					"item":         fcItem,
				}))
				a.outputIndex++
			}

			argsDelta, _ := fn["arguments"].(string)
			if argsDelta != "" {
				tcData := a.pendingToolCalls[tcIndex]
				tcData["arguments"] += argsDelta
				outIdx, _ := strconv.Atoi(tcData["output_index"])
				events = append(events, a.sse("response.function_call_arguments.delta", map[string]any{
					"type":         "response.function_call_arguments.delta",
					"item_id":      tcData["id"],
					"output_index": outIdx,
					"delta":        argsDelta,
				}))
			}
		}
	}

	finishReason, _ := choice["finish_reason"].(string)
	if usage, ok := data["usage"].(map[string]any); ok {
		if it, ok := usage["prompt_tokens"].(float64); ok {
			a.inputTokens = int(it)
		}
		if ot, ok := usage["completion_tokens"].(float64); ok {
			a.outputTokens = int(ot)
		}
	}

	if finishReason != "" {
		events = append(events, a.finish()...)
	}

	return events
}

func (a *ResponsesStreamAccumulator) startMessageOutput() []string {
	a.currentMsgID = "msg_" + uuid.New().String()[:24]
	a.messageStarted = true
	a.currentType = "text"
	msgItem := map[string]any{
		"type":   "message",
		"id":     a.currentMsgID,
		"status": "in_progress",
		"role":   "assistant",
		"content": []any{},
	}
	return []string{a.sse("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": a.outputIndex,
		"item":         msgItem,
	})}
}

func (a *ResponsesStreamAccumulator) startContentPart() []string {
	a.contentPartStarted = true
	part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
	return []string{a.sse("response.content_part.added", map[string]any{
		"type":          "response.content_part.added",
		"item_id":       a.currentMsgID,
		"output_index":  a.outputIndex,
		"content_index": a.contentIndex,
		"part":          part,
	})}
}

func (a *ResponsesStreamAccumulator) endContentPart() []string {
	events := []string{
		a.sse("response.output_text.done", map[string]any{
			"type":          "response.output_text.done",
			"item_id":       a.currentMsgID,
			"output_index":  a.outputIndex,
			"content_index": a.contentIndex,
			"text":          a.textBuffer,
		}),
		a.sse("response.content_part.done", map[string]any{
			"type":          "response.content_part.done",
			"item_id":       a.currentMsgID,
			"output_index":  a.outputIndex,
			"content_index": a.contentIndex,
			"part":          map[string]any{"type": "output_text", "text": a.textBuffer, "annotations": []any{}},
		}),
	}
	a.contentPartStarted = false
	a.contentIndex++
	a.textBuffer = ""
	return events
}

func (a *ResponsesStreamAccumulator) endMessageOutput() []string {
	var content []any
	if a.fullText != "" {
		content = append(content, map[string]any{"type": "output_text", "text": a.fullText, "annotations": []any{}})
	}
	msgDone := map[string]any{
		"type":   "message",
		"id":     a.currentMsgID,
		"status": "completed",
		"role":   "assistant",
		"content": content,
	}
	a.completedOutput = append(a.completedOutput, msgDone)
	events := []string{a.sse("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": a.outputIndex,
		"item":         msgDone,
	})}
	a.messageStarted = false
	a.outputIndex++
	a.contentIndex = 0
	return events
}

func (a *ResponsesStreamAccumulator) finish() []string {
	if a.finished {
		return nil
	}
	a.finished = true
	var events []string
	if a.contentPartStarted {
		events = append(events, a.endContentPart()...)
	}
	if a.messageStarted {
		events = append(events, a.endMessageOutput()...)
	}

	for _, tcData := range a.pendingToolCalls {
		outIdx, _ := strconv.Atoi(tcData["output_index"])
		events = append(events, a.sse("response.function_call_arguments.done", map[string]any{
			"type":         "response.function_call_arguments.done",
			"item_id":      tcData["id"],
			"output_index": outIdx,
			"arguments":    tcData["arguments"],
		}))
		fcDone := map[string]any{
			"type":      "function_call",
			"id":        tcData["id"],
			"call_id":   tcData["call_id"],
			"name":      tcData["name"],
			"arguments": tcData["arguments"],
			"status":    "completed",
		}
		a.completedOutput = append(a.completedOutput, fcDone)
		events = append(events, a.sse("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": outIdx,
			"item":         fcDone,
		}))
	}
	a.pendingToolCalls = map[int]map[string]string{}

	events = append(events, a.sse("response.completed", a.baseResponse("completed")))
	events = append(events, "data: [DONE]\n\n")
	return events
}

func (a *ResponsesStreamAccumulator) sse(eventType string, data map[string]any) string {
	var eventPayload map[string]any
	if obj, ok := data["object"].(string); ok && obj == "response" {
		eventPayload = map[string]any{
			"type":        eventType,
			"response":    data,
			"response_id": a.responseID,
		}
	} else {
		eventPayload = map[string]any{}
		for k, v := range data {
			eventPayload[k] = v
		}
		eventPayload["type"] = eventType
		if _, ok := eventPayload["response_id"]; !ok {
			eventPayload["response_id"] = a.responseID
		}
	}
	eventPayload["model"] = a.model
	eventPayload["sequence_number"] = a.sequenceNumber
	a.sequenceNumber++
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, safeJSON(eventPayload))
}
