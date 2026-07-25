package translator

import (
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bytedance/sonic"

	"glm2api/internal/logging"
	"glm2api/internal/tools"
)

var (
	assistantIDPattern = regexp.MustCompile(`^[a-z0-9]{24,}$`)
	urlPattern         = regexp.MustCompile(`https?://[^\s<>()"']+`)
)

const LocalFileHint = "(本地文件，不在服务区上，应该用[function_calls]工具)"

var (
	systemReminderRE = regexp.MustCompile(`(?s)<system-reminder>.*`)
	localFilePathRE  = regexp.MustCompile(`[A-Za-z]:[\\\/](?:[^\s<>()"'\n\\\/]+[\\\/])*[^\s<>()"'\n\\\/]+(?:#L\d+(?:-\d+)?)?`)
	ImageRefRE       = regexp.MustCompile(`\[image:[^\]]+\]`)
)

// removeLocalFileHint 递归移除本地文件提示
func removeLocalFileHint(v any) any {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(strings.ReplaceAll(x, LocalFileHint, ""))
	case map[string]any:
		result := make(map[string]any, len(x))
		for k, val := range x {
			result[k] = removeLocalFileHint(val)
		}
		return result
	case []any:
		result := make([]any, len(x))
		for i, val := range x {
			result[i] = removeLocalFileHint(val)
		}
		return result
	default:
		return v
	}
}

// appendLocalFileHints 在本地文件路径后追加提示。
// 模拟 Python 的 (?<!LOCAL_FILE_HINT) 负向后顾断言：若路径已紧随提示则不重复追加。
func appendLocalFileHints(prompt string) string {
	matches := localFilePathRE.FindAllStringIndex(prompt, -1)
	if len(matches) == 0 {
		return prompt
	}
	hintLen := len(LocalFileHint)
	var b strings.Builder
	lastEnd := 0
	for _, loc := range matches {
		start, end := loc[0], loc[1]
		b.WriteString(prompt[lastEnd:start])
		match := prompt[start:end]
		if start >= hintLen && prompt[start-hintLen:start] == LocalFileHint {
			// 路径前已是提示，跳过避免重复
			b.WriteString(match)
		} else {
			b.WriteString(match)
			b.WriteString(LocalFileHint)
		}
		lastEnd = end
	}
	b.WriteString(prompt[lastEnd:])
	return b.String()
}

// ExtractTextContent 从消息内容中提取文本
func ExtractTextContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case map[string]any:
		return tools.SafeJSONDumpsCompact(v)
	case []any:
		var parts []string
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			itemType, _ := m["type"].(string)
			switch itemType {
			case "text":
				if t, ok := m["text"]; ok && t != nil {
					parts = append(parts, fmt.Sprintf("%v", t))
				}
			case "image_url":
				url := ""
				if iu, ok := m["image_url"].(map[string]any); ok {
					url, _ = iu["url"].(string)
				}
				parts = append(parts, "[image:"+url+"]")
			case "file":
				url := ""
				if fu, ok := m["file_url"].(map[string]any); ok {
					url, _ = fu["url"].(string)
				}
				parts = append(parts, "[file:"+url+"]")
			}
		}
		return strings.Join(filterEmpty(parts), "\n")
	default:
		return ""
	}
}

// ExtractFirstURL 提取文本中第一个 URL
func ExtractFirstURL(text string) string {
	loc := urlPattern.FindStringIndex(text)
	if loc == nil {
		return ""
	}
	s := text[loc[0]:loc[1]]
	return strings.TrimRight(s, ".,;:!?)}+")
}

// ExtractRecentUserURL 提取最近用户消息中的 URL
func ExtractRecentUserURL(messages []map[string]any) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		role, _ := msg["role"].(string)
		if strings.TrimSpace(role) != "user" {
			continue
		}
		text := ExtractTextContent(msg["content"])
		url := ExtractFirstURL(text)
		if url != "" {
			return url
		}
	}
	return ""
}

// SanitizeToolCallPayload 清理工具调用负载
func SanitizeToolCallPayload(toolName string, arguments any, fallbackURL string) map[string]any {
	parsedArguments := arguments
	if s, ok := arguments.(string); ok {
		var v any
		if err := sonic.UnmarshalString(s, &v); err == nil {
			parsedArguments = v
		} else {
			return nil
		}
	}

	if parsedArguments == nil {
		parsedArguments = map[string]any{}
	}
	m, ok := parsedArguments.(map[string]any)
	if !ok {
		return nil
	}

	cleaned := make(map[string]any, len(m))
	for k, v := range m {
		cleaned[fmt.Sprintf("%v", k)] = v
	}

	// 处理 {"param_name": "url"} 特殊情况
	if paramVal, hasParam := cleaned["param_name"]; len(cleaned) == 1 && hasParam {
		if pv, ok := paramVal.(string); ok && pv == "url" {
			if fallbackURL != "" {
				cleaned = map[string]any{"url": fallbackURL}
			} else {
				cleaned = map[string]any{}
			}
		} else {
			cleaned = map[string]any{}
		}
	}
	if _, hasParamName := cleaned["param_name"]; hasParamName {
		if _, hasParamValue := cleaned["param_value"]; !hasParamValue && len(cleaned) == 1 {
			cleaned = map[string]any{}
		}
	}

	return cleaned
}

// SanitizeToolCalls 清理工具调用列表
func SanitizeToolCalls(toolCalls []map[string]any, fallbackURL string) []map[string]any {
	var sanitized []map[string]any
	for i, tc := range toolCalls {
		fn, _ := tc["function"].(map[string]any)
		if fn == nil {
			continue
		}
		toolName := strings.TrimSpace(fmt.Sprintf("%v", fn["name"]))
		if toolName == "" {
			continue
		}
		originalArguments := fn["arguments"]
		originalValue := originalArguments
		if s, ok := originalArguments.(string); ok {
			var v any
			if err := sonic.UnmarshalString(s, &v); err == nil {
				originalValue = v
			} else {
				originalValue = s
			}
		}
		cleanedArguments := SanitizeToolCallPayload(toolName, originalArguments, fallbackURL)
		if cleanedArguments == nil {
			continue
		}
		cleanedArguments = removeLocalFileHint(cleanedArguments).(map[string]any)

		repaired := true
		if _, isDict := originalValue.(map[string]any); isDict {
			repaired = tools.SafeJSONDumpsCompact(cleanedArguments) != tools.SafeJSONDumpsCompact(originalValue)
		}

		id, _ := tc["id"].(string)
		if id == "" {
			id = fmt.Sprintf("call_repaired_%d", i)
		}
		sanitized = append(sanitized, map[string]any{
			"id":        id,
			"type":      "function",
			"index":     i,
			"_repaired": repaired,
			"function": map[string]any{
				"name":      toolName,
				"arguments": tools.SafeJSONDumpsCompact(cleanedArguments),
			},
		})
	}
	return sanitized
}

// ConvertMessages 将 OpenAI 消息列表转换为 GLM 格式
func ConvertMessages(
	messages []map[string]any,
	toolsList []map[string]any,
	blockedToolNames map[string]bool,
	toolChoice any,
	serverSideToolNames map[string]bool,
) []map[string]any {
	filteredTools := tools.FilterTools(toolsList, blockedToolNames)
	availableToolNames := map[string]bool{}
	for _, t := range filteredTools {
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name := strings.TrimSpace(fmt.Sprintf("%v", fn["name"]))
		if name != "" {
			availableToolNames[name] = true
		}
	}
	if serverSideToolNames == nil {
		serverSideToolNames = tools.ServerSideToolNames
	}
	toolChoicePolicy := tools.ParseToolChoicePolicy(toolChoice, availableToolNames)

	type processedItem struct {
		role    string
		content string
	}
	var processed []processedItem
	latestUserURL := ExtractRecentUserURL(messages)
	validToolCallIDs := map[string]bool{}
	repairedToolCallIDs := map[string]bool{}
	toolCallIDToName := map[string]string{}

	for _, message := range messages {
		role, _ := message["role"].(string)
		if role == "" {
			role = "user"
		}
		content := message["content"]

		if role == "user" {
			currentText := ExtractTextContent(content)
			currentURL := ExtractFirstURL(currentText)
			if currentURL != "" {
				latestUserURL = currentURL
			}
		}

		if role == "assistant" {
			if _, hasToolCalls := message["tool_calls"]; hasToolCalls {
				var toolBlocks []string
				rawToolCalls, _ := message["tool_calls"].([]any)
				var rawCallsList []map[string]any
				for _, c := range rawToolCalls {
					if m, ok := c.(map[string]any); ok {
						rawCallsList = append(rawCallsList, m)
					}
				}
				sanitizedToolCalls := SanitizeToolCalls(rawCallsList, latestUserURL)
				for _, tc := range sanitizedToolCalls {
					fn, _ := tc["function"].(map[string]any)
					toolName := "unknown"
					if fn != nil {
						if n, ok := fn["name"].(string); ok {
							toolName = n
						}
					}
					if len(availableToolNames) > 0 && !availableToolNames[toolName] {
						continue
					}
					args := "{}"
					if fn != nil {
						if a, ok := fn["arguments"].(string); ok {
							args = a
						}
					}
					toolBlocks = append(toolBlocks, tools.SerializeToolCallBlock(toolName, args))
					toolCallID, _ := tc["id"].(string)
					toolCallID = strings.TrimSpace(toolCallID)
					if toolCallID != "" && !strings.HasPrefix(toolCallID, "call_repaired_") {
						validToolCallIDs[toolCallID] = true
						toolCallIDToName[toolCallID] = toolName
						if repaired, _ := tc["_repaired"].(bool); repaired {
							repairedToolCallIDs[toolCallID] = true
						}
					}
				}
				assistantText := strings.TrimSpace(ExtractTextContent(content))
				block := strings.Join(toolBlocks, "\n")
				if assistantText == "" && block == "" {
					continue
				}
				if assistantText != "" && block != "" {
					content = assistantText + "\n" + block
				} else if assistantText != "" {
					content = assistantText
				} else {
					content = block
				}
			} else if content == nil {
				continue
			}
		} else if role == "tool" {
			toolCallID, _ := message["tool_call_id"].(string)
			toolCallID = strings.TrimSpace(toolCallID)
			if toolCallID != "" && len(validToolCallIDs) > 0 && !validToolCallIDs[toolCallID] {
				continue
			}
			if toolCallID != "" && repairedToolCallIDs[toolCallID] {
				continue
			}
			role = "user"
			toolName, _ := message["name"].(string)
			toolName = strings.TrimSpace(toolName)
			if toolName == "" && toolCallID != "" {
				toolName = toolCallIDToName[toolCallID]
			}
			if toolName == "" {
				toolName = "unknown_tool"
			}
			toolResultText := ExtractTextContent(content)
			callID := toolCallID
			if callID == "" {
				if id, ok := message["tool_call_id"].(string); ok {
					callID = id
				}
				if callID == "" {
					callID = "unknown"
				}
			}
			content = tools.SerializeToolResultBlock(callID, toolName, toolResultText)
		}

		text := ""
		if content != nil {
			text = ExtractTextContent(content)
		}
		if text != "" {
			processed = append(processed, processedItem{role: role, content: text})
		}
	}

	var transcriptParts []string
	if len(filteredTools) > 0 && toolChoicePolicy.Mode != "none" {
		transcriptParts = append(transcriptParts,
			tools.ToolsToPrompt(filteredTools, blockedToolNames, toolChoicePolicy, serverSideToolNames),
			"# CONVERSATION",
		)
	}

	for _, item := range processed {
		title := item.role
		switch title {
		case "system":
			title = "System"
		case "assistant":
			title = "Assistant"
		case "user":
			title = "User"
		case "developer":
			title = "Developer"
		}
		line := title + ": " + item.content
		transcriptParts = append(transcriptParts, strings.TrimSpace(line))
	}

	prompt := strings.TrimSpace(strings.Join(transcriptParts, "\n"))

	// 截取 <system-reminder> 之后的内容
	if loc := systemReminderRE.FindStringIndex(prompt); loc != nil {
		prompt = prompt[loc[0]:]
	}

	// 在本地文件路径后追加提示（跳过已带提示的路径，与 Python 负向后顾断言一致）
	prompt = appendLocalFileHints(prompt)

	return []map[string]any{
		{
			"role": "user",
			"content": []map[string]any{
				{
					"type": "text",
					"text": prompt + "\n\nAssistant: ",
				},
			},
		},
	}
}

// GLMEventAccumulator GLM 事件累加器
type GLMEventAccumulator struct {
	Model            string
	AllowedToolNames map[string]bool
	FallbackToolURL  string
	DebugEnabled     bool
	Logger           *slog.Logger
	ConversationID   string
	Created          int64

	partsByLogicID            map[string]map[string]any
	orderedLogicIDs           []string
	lastFullText              string
	lastFullReasoning         string
	partTextSent              map[string]int
	partReasoningSent         map[string]int
	knownLogicIDsForText      []string
	knownLogicIDsForReasoning []string
	toolParser                *tools.StreamingToolParser
	emittedRole               bool
	renderCacheDirty          bool
	cachedFullText            string
	cachedFullReasoning       string
	cachedPartTexts           map[string]string
	cachedPartReasonings      map[string]string
	serverSideToolCalls       []map[string]any
	serverSideToolCallIDs     map[string]bool
	blockedToolCallIDs        map[string]bool
	blockedToolResultText     string
	deferredVisibleText       string
}

// NewGLMEventAccumulator 创建事件累加器
func NewGLMEventAccumulator(model string, allowedToolNames map[string]bool, fallbackToolURL string, debugEnabled bool, logger *slog.Logger) *GLMEventAccumulator {
	if logger == nil {
		logger = logging.GetLogger("glm2api.null")
	}
	acc := &GLMEventAccumulator{
		Model:                 model,
		AllowedToolNames:      allowedToolNames,
		FallbackToolURL:       fallbackToolURL,
		DebugEnabled:          debugEnabled,
		Logger:                logger,
		Created:               time.Now().Unix(),
		partsByLogicID:        map[string]map[string]any{},
		partTextSent:          map[string]int{},
		partReasoningSent:     map[string]int{},
		toolParser:            tools.NewStreamingToolParser(),
		cachedPartTexts:       map[string]string{},
		cachedPartReasonings:  map[string]string{},
		serverSideToolCallIDs: map[string]bool{},
		blockedToolCallIDs:    map[string]bool{},
		renderCacheDirty:      true,
	}
	// 与 Python __post_init__ 一致：将 allowed_tool_names 同步到流式工具解析器
	acc.toolParser.AllowedToolNames = allowedToolNames
	return acc
}

// insertSorted 将 logic_id 有序插入
func (a *GLMEventAccumulator) insertSorted(logicID string) {
	idx := sort.SearchStrings(a.orderedLogicIDs, logicID)
	if idx < len(a.orderedLogicIDs) && a.orderedLogicIDs[idx] == logicID {
		return
	}
	a.orderedLogicIDs = append(a.orderedLogicIDs, "")
	copy(a.orderedLogicIDs[idx+1:], a.orderedLogicIDs[idx:])
	a.orderedLogicIDs[idx] = logicID
}

// ConsumeEvent 消费一个事件，返回 (chunks, status)
func (a *GLMEventAccumulator) ConsumeEvent(payload map[string]any) ([]string, string) {
	logging.DebugDump(a.Logger, a.DebugEnabled, "GLM SSE 解析事件", payload)
	if a.ConversationID == "" {
		if id, ok := payload["conversation_id"].(string); ok && id != "" {
			a.ConversationID = id
		}
	}

	// 工具调用已解析完成，后续事件不再处理（与 Python 的 is_tool_call_completed 一致）
	if a.toolParser.IsToolCallCompleted() {
		return nil, "tool_call_complete"
	}

	// 处理 parts
	if parts, ok := payload["parts"].([]any); ok {
		for _, p := range parts {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if logicID, ok := part["logic_id"].(string); ok && logicID != "" {
				if _, exists := a.partsByLogicID[logicID]; !exists {
					a.insertSorted(logicID)
				}
				a.partsByLogicID[logicID] = part
				a.renderCacheDirty = true
			}
			// 提取服务端原生 tool_calls
			if contentList, ok := part["content"].([]any); ok {
				for _, c := range contentList {
					content, ok := c.(map[string]any)
					if !ok {
						continue
					}
					contentType, _ := content["type"].(string)
					if contentType == "tool_calls" {
						toolCallsData, _ := content["tool_calls"].(map[string]any)
						toolName, _ := toolCallsData["name"].(string)
						toolName = strings.TrimSpace(toolName)
						if toolName == "open_url" {
							toolName = "read"
						}
						toolID, _ := toolCallsData["id"].(string)
						toolID = strings.TrimSpace(toolID)
						arguments := toolCallsData["arguments"]

						if tools.BlockedNativeToolNames[toolName] {
							if toolID != "" {
								a.blockedToolCallIDs[toolID] = true
							}
							continue
						}
						if a.AllowedToolNames != nil && !a.AllowedToolNames[toolName] {
							continue
						}
						if toolName != "" && toolID != "" && !a.serverSideToolCallIDs[toolID] {
							a.serverSideToolCallIDs[toolID] = true
							argsStr := "{}"
							if s, ok := arguments.(string); ok {
								argsStr = s
							} else {
								argsStr = tools.SafeJSONDumpsCompact(arguments)
							}
							a.serverSideToolCalls = append(a.serverSideToolCalls, map[string]any{
								"id":    toolID,
								"type":  "function",
								"index": len(a.serverSideToolCalls),
								"function": map[string]any{
									"name":      toolName,
									"arguments": argsStr,
								},
							})
						}
					}
					// 转换被屏蔽工具的结果为纯文本
					if contentType == "tool_result" {
						toolResultData, _ := content["tool_calls"].(map[string]any)
						resultToolID, _ := toolResultData["id"].(string)
						resultToolID = strings.TrimSpace(resultToolID)
						if a.blockedToolCallIDs[resultToolID] {
							var resultTextParts []string
							meta, _ := part["meta_data"].(map[string]any)
							toolExtra, _ := meta["tool_result_extra"].(map[string]any)
							searchResults, _ := toolExtra["search_results"].([]any)
							for _, sr := range searchResults {
								srm, _ := sr.(map[string]any)
								text, _ := srm["text"].(string)
								title, _ := srm["title"].(string)
								url, _ := srm["url"].(string)
								if text != "" {
									resultTextParts = append(resultTextParts, text)
								} else if title != "" {
									resultTextParts = append(resultTextParts, fmt.Sprintf("%s (%s)", title, url))
								}
							}
							if len(resultTextParts) > 0 {
								a.blockedToolResultText = strings.Join(resultTextParts, "\n\n")
							}
						}
					}
				}
			}
		}
	}

	textDelta, reasoningDelta := a.computeDeltas()
	a.lastFullText = a.cachedFullText
	a.lastFullReasoning = a.cachedFullReasoning

	var chunks []string
	if reasoningDelta != "" {
		chunks = append(chunks, a.chunkJSON(map[string]any{
			"choices": []map[string]any{
				{
					"index":         0,
					"delta":         map[string]any{"reasoning_content": reasoningDelta},
					"finish_reason": nil,
				},
			},
		}))
	}

	visibleTextDelta := a.toolParser.Consume(textDelta)
	if visibleTextDelta != "" {
		if a.AllowedToolNames != nil {
			a.deferredVisibleText += visibleTextDelta
		} else {
			deltaPayload := map[string]any{"content": visibleTextDelta}
			if !a.emittedRole {
				deltaPayload = map[string]any{"role": "assistant", "content": visibleTextDelta}
				a.emittedRole = true
			}
			chunks = append(chunks, a.chunkJSON(map[string]any{
				"choices": []map[string]any{
					{
						"index":         0,
						"delta":         deltaPayload,
						"finish_reason": nil,
					},
				},
			}))
		}
	}
	logging.DebugDump(a.Logger, a.DebugEnabled, "GLM SSE 生成增量块", chunks)

	status := ""
	if s, ok := payload["status"]; ok && s != nil {
		status = fmt.Sprintf("%v", s)
	}
	return chunks, status
}

// Finalize 完成响应，返回剩余 chunks
func (a *GLMEventAccumulator) Finalize(status string, lastError map[string]any) []string {
	tailText, jsonToolCalls := a.toolParser.Flush()
	jsonToolCalls = SanitizeToolCalls(jsonToolCalls, a.FallbackToolURL)
	if len(jsonToolCalls) == 0 {
		jsonToolCalls = a.extractReasoningToolCalls("")
	}

	// 合并服务端和 JSON 工具调用，重新索引
	allToolCalls := make([]map[string]any, len(a.serverSideToolCalls))
	copy(allToolCalls, a.serverSideToolCalls)
	for _, tc := range jsonToolCalls {
		tcCopy := map[string]any{}
		for k, v := range tc {
			tcCopy[k] = v
		}
		tcCopy["index"] = len(allToolCalls)
		allToolCalls = append(allToolCalls, tcCopy)
	}

	if a.Logger != nil {
		a.Logger.Info("响应收尾",
			"status", status,
			"text_len", len(a.cachedFullText),
			"reasoning_len", len(a.cachedFullReasoning),
			"tool_calls", len(jsonToolCalls),
			"server_tools", len(a.serverSideToolCalls),
		)
	}

	var chunks []string
	finalText := a.deferredVisibleText + tailText
	a.deferredVisibleText = ""

	// 如果没有工具调用但延迟文本看起来像 tool_calls，尝试从中提取
	if len(allToolCalls) == 0 && finalText != "" {
		recoveredClean, recoveredCalls := tools.ParseToolCallsFromText(finalText, a.AllowedToolNames)
		if len(recoveredCalls) > 0 {
			sanitizedRecovered := SanitizeToolCalls(recoveredCalls, a.FallbackToolURL)
			if len(sanitizedRecovered) > 0 {
				for _, tc := range sanitizedRecovered {
					tcCopy := map[string]any{}
					for k, v := range tc {
						tcCopy[k] = v
					}
					tcCopy["index"] = len(allToolCalls)
					allToolCalls = append(allToolCalls, tcCopy)
				}
				finalText = recoveredClean
				if a.Logger != nil {
					a.Logger.Info("finalize: recovered tool call(s) from deferred visible text", "count", len(sanitizedRecovered))
				}
			}
		}
	}

	// 注入被屏蔽工具结果作为参考文本
	if a.blockedToolResultText != "" && len(allToolCalls) == 0 {
		if finalText != "" {
			finalText = "[Reference content fetched by browser]:\n" + a.blockedToolResultText + "\n\n" + finalText
		} else {
			finalText = "[Reference content fetched by browser]:\n" + a.blockedToolResultText
		}
	}

	// 检查是否调用了未声明工具
	if finalText == "" && len(allToolCalls) == 0 && a.AllowedToolNames != nil {
		_, attemptedToolCalls := tools.ParseToolCallsFromText(strings.TrimSpace(a.cachedFullText), nil)
		var unavailableNames []string
		seen := map[string]bool{}
		for _, tc := range attemptedToolCalls {
			fn, _ := tc["function"].(map[string]any)
			if fn == nil {
				continue
			}
			name, _ := fn["name"].(string)
			name = strings.TrimSpace(name)
			if name != "" && !a.AllowedToolNames[name] && !seen[name] {
				unavailableNames = append(unavailableNames, name)
				seen[name] = true
			}
		}
		sort.Strings(unavailableNames)
		if len(unavailableNames) > 0 {
			var allowedNames []string
			for n := range a.AllowedToolNames {
				allowedNames = append(allowedNames, n)
			}
			sort.Strings(allowedNames)
			allowedStr := strings.Join(allowedNames, ", ")
			if allowedStr == "" {
				allowedStr = "(none)"
			}
			var quoted []string
			for _, n := range unavailableNames {
				quoted = append(quoted, "`"+n+"`")
			}
			finalText = "模型尝试调用未声明工具 " + strings.Join(quoted, ", ") + "，已阻止。本轮只允许这些工具：" + allowedStr + "。"
		}
	}

	if finalText != "" && len(allToolCalls) == 0 {
		deltaPayload := map[string]any{"content": finalText}
		if !a.emittedRole {
			deltaPayload = map[string]any{"role": "assistant", "content": finalText}
			a.emittedRole = true
		}
		chunks = append(chunks, a.chunkJSON(map[string]any{
			"choices": []map[string]any{
				{
					"index":         0,
					"delta":         deltaPayload,
					"finish_reason": nil,
				},
			},
		}))
	}

	if status == "intervene" && lastError != nil {
		if interveneText, ok := lastError["intervene_text"].(string); ok && interveneText != "" {
			chunks = append(chunks, a.chunkJSON(map[string]any{
				"choices": []map[string]any{
					{
						"index":         0,
						"delta":         map[string]any{"content": "\n\n" + interveneText},
						"finish_reason": nil,
					},
				},
			}))
		}
	}

	if len(allToolCalls) > 0 {
		if !a.emittedRole {
			chunks = append(chunks, a.chunkJSON(map[string]any{
				"choices": []map[string]any{
					{
						"index":         0,
						"delta":         map[string]any{"role": "assistant"},
						"finish_reason": nil,
					},
				},
			}))
			a.emittedRole = true
		}
		for _, tc := range allToolCalls {
			fn := tc["function"]
			chunks = append(chunks, a.chunkJSON(map[string]any{
				"choices": []map[string]any{
					{
						"index": 0,
						"delta": map[string]any{
							"tool_calls": []map[string]any{
								{
									"index":    tc["index"],
									"id":       tc["id"],
									"type":     "function",
									"function": fn,
								},
							},
						},
						"finish_reason": nil,
					},
				},
			}))
		}
	}

	finishReason := "stop"
	if len(allToolCalls) > 0 {
		finishReason = "tool_calls"
	}
	chunks = append(chunks, a.chunkJSON(map[string]any{
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     1,
			"completion_tokens": 1,
			"total_tokens":      2,
		},
	}))
	chunks = append(chunks, "data: [DONE]\n\n")
	logging.DebugDump(a.Logger, a.DebugEnabled, "GLM SSE finalize 输出", chunks)
	return chunks
}

// BuildResponse 构建非流式响应
func (a *GLMEventAccumulator) BuildResponse() map[string]any {
	fullText, fullReasoning := a.renderFullOutput()
	if fullText == "" && a.lastFullText != "" {
		fullText = a.lastFullText
	}
	if fullReasoning == "" && a.lastFullReasoning != "" {
		fullReasoning = a.lastFullReasoning
	}
	cleanContent, jsonToolCalls := tools.ParseToolCallsFromText(strings.TrimSpace(fullText), a.AllowedToolNames)
	jsonToolCalls = SanitizeToolCalls(jsonToolCalls, a.FallbackToolURL)
	if len(jsonToolCalls) == 0 {
		jsonToolCalls = a.extractReasoningToolCalls(fullReasoning)
	}

	allToolCalls := make([]map[string]any, len(a.serverSideToolCalls))
	copy(allToolCalls, a.serverSideToolCalls)
	for _, tc := range jsonToolCalls {
		tcCopy := map[string]any{}
		for k, v := range tc {
			tcCopy[k] = v
		}
		tcCopy["index"] = len(allToolCalls)
		allToolCalls = append(allToolCalls, tcCopy)
	}

	finalContent := strings.TrimSpace(cleanContent)
	var contentValue any
	if len(allToolCalls) > 0 || finalContent == "" {
		contentValue = nil
	} else {
		contentValue = finalContent
	}

	var reasoningValue any
	if fullReasoning != "" {
		reasoningValue = fullReasoning
	} else {
		reasoningValue = nil
	}

	message := map[string]any{
		"role":              "assistant",
		"content":           contentValue,
		"reasoning_content": reasoningValue,
	}
	if len(allToolCalls) > 0 {
		var toolCallsList []map[string]any
		for _, item := range allToolCalls {
			toolCallsList = append(toolCallsList, map[string]any{
				"id":       item["id"],
				"type":     "function",
				"function": item["function"],
			})
		}
		message["tool_calls"] = toolCallsList
	}

	finishReason := "stop"
	if len(allToolCalls) > 0 {
		finishReason = "tool_calls"
	}

	response := map[string]any{
		"id":      a.ConversationID,
		"object":  "chat.completion",
		"created": a.Created,
		"model":   a.Model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     1,
			"completion_tokens": 1,
			"total_tokens":      2,
		},
	}
	if a.Logger != nil {
		a.Logger.Info("非流式响应构建完成",
			"model", a.Model,
			"text_len", len(finalContent),
			"reasoning_len", len(fullReasoning),
			"tool_calls", len(allToolCalls),
		)
	}
	logging.DebugDump(a.Logger, a.DebugEnabled, "GLM 非流式最终响应", response)
	return response
}

// GetOrderedParts 返回按 logic_id 排序的 parts 列表（用于图片响应构建）
func (a *GLMEventAccumulator) GetOrderedParts() []map[string]any {
	result := make([]map[string]any, 0, len(a.orderedLogicIDs))
	for _, logicID := range a.orderedLogicIDs {
		if part, ok := a.partsByLogicID[logicID]; ok {
			result = append(result, part)
		}
	}
	return result
}

func (a *GLMEventAccumulator) extractReasoningToolCalls(reasoningText string) []map[string]any {
	source := reasoningText
	if source == "" {
		source = a.lastFullReasoning
	}
	if source == "" {
		source = a.cachedFullReasoning
	}
	if source == "" {
		return nil
	}
	_, toolCalls := tools.ParseToolCallsFromText(strings.TrimSpace(source), a.AllowedToolNames)
	return SanitizeToolCalls(toolCalls, a.FallbackToolURL)
}

func (a *GLMEventAccumulator) computeDeltas() (string, string) {
	a.renderFullOutput()
	var textDeltaParts, reasoningDeltaParts []string

	for _, logicID := range a.orderedLogicIDs {
		renderedText := a.cachedPartTexts[logicID]
		renderedReasoning := a.cachedPartReasonings[logicID]

		if renderedText != "" {
			prevLen := a.partTextSent[logicID]
			isNew := !containsString(a.knownLogicIDsForText, logicID)
			if isNew {
				a.knownLogicIDsForText = append(a.knownLogicIDsForText, logicID)
				if len(textDeltaParts) > 0 || len(a.partTextSent) > 0 {
					textDeltaParts = append(textDeltaParts, "\n\n")
				}
				textDeltaParts = append(textDeltaParts, renderedText)
			} else if len(renderedText) > prevLen {
				textDeltaParts = append(textDeltaParts, renderedText[prevLen:])
			}
			a.partTextSent[logicID] = len(renderedText)
		}

		if renderedReasoning != "" {
			prevLen := a.partReasoningSent[logicID]
			isNew := !containsString(a.knownLogicIDsForReasoning, logicID)
			if isNew {
				a.knownLogicIDsForReasoning = append(a.knownLogicIDsForReasoning, logicID)
				if len(reasoningDeltaParts) > 0 || len(a.partReasoningSent) > 0 {
					reasoningDeltaParts = append(reasoningDeltaParts, "\n\n")
				}
				reasoningDeltaParts = append(reasoningDeltaParts, renderedReasoning)
			} else if len(renderedReasoning) > prevLen {
				reasoningDeltaParts = append(reasoningDeltaParts, renderedReasoning[prevLen:])
			}
			a.partReasoningSent[logicID] = len(renderedReasoning)
		}
	}

	return strings.Join(textDeltaParts, ""), strings.Join(reasoningDeltaParts, "")
}

func (a *GLMEventAccumulator) renderFullOutput() (string, string) {
	if !a.renderCacheDirty {
		return a.cachedFullText, a.cachedFullReasoning
	}

	var textParts, reasoningParts []string
	a.cachedPartTexts = map[string]string{}
	a.cachedPartReasonings = map[string]string{}

	for _, logicID := range a.orderedLogicIDs {
		part := a.partsByLogicID[logicID]
		if part == nil {
			continue
		}
		contentItems, ok := part["content"].([]any)
		if !ok {
			continue
		}

		var partText, partReasoning []string
		for _, c := range contentItems {
			content, ok := c.(map[string]any)
			if !ok {
				continue
			}
			itemType, _ := content["type"].(string)
			switch itemType {
			case "text":
				partText = append(partText, fmt.Sprintf("%v", content["text"]))
			case "think":
				partReasoning = append(partReasoning, fmt.Sprintf("%v", content["think"]))
			case "code":
				partText = append(partText, "```python\n"+fmt.Sprintf("%v", content["code"])+"\n```")
			case "execution_output":
				partText = append(partText, fmt.Sprintf("%v", content["content"]))
			case "image":
				images, _ := content["image"].([]any)
				for _, img := range images {
					im, ok := img.(map[string]any)
					if !ok {
						continue
					}
					if imageURL, ok := im["image_url"].(string); ok && imageURL != "" {
						partText = append(partText, "![image]("+imageURL+")")
					}
				}
			}
		}

		renderedText := strings.TrimSpace(strings.Join(filterEmpty(partText), "\n"))
		renderedReasoning := strings.TrimSpace(strings.Join(filterEmpty(partReasoning), "\n"))
		if renderedText != "" {
			textParts = append(textParts, renderedText)
			a.cachedPartTexts[logicID] = renderedText
		}
		if renderedReasoning != "" {
			reasoningParts = append(reasoningParts, renderedReasoning)
			a.cachedPartReasonings[logicID] = renderedReasoning
		}
	}

	a.cachedFullText = strings.Join(textParts, "\n\n")
	a.cachedFullReasoning = strings.Join(reasoningParts, "\n\n")
	a.renderCacheDirty = false
	return a.cachedFullText, a.cachedFullReasoning
}

func (a *GLMEventAccumulator) chunkJSON(patch map[string]any) string {
	payload := map[string]any{
		"id":      a.ConversationID,
		"object":  "chat.completion.chunk",
		"created": a.Created,
		"model":   a.Model,
	}
	for k, v := range patch {
		payload[k] = v
	}
	jsonStr, err := sonic.MarshalString(payload)
	if err != nil {
		return "data: {}\n\n"
	}
	return "data: " + jsonStr + "\n\n"
}

func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func filterEmpty(parts []string) []string {
	var result []string
	for _, p := range parts {
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
