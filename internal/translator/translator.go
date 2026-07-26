package translator

// translator.go — OpenAI ↔ GLM 消息格式转换与 SSE 流事件累积
//
// 本文件是 glm2api 的核心转换层，负责：
//   1. 将 OpenAI 格式的 chat messages 转换为 GLM Web API 能理解的文本提示词（ConvertMessages）
//   2. 将 GLM 返回的 SSE 流事件累积并转换为 OpenAI 格式的 SSE chunks（GLMEventAccumulator）
//   3. 清理和修复 GLM 模型输出的工具调用（SanitizeToolCalls）
//   4. 处理本地文件路径提示、图片引用等辅助功能
//
// 数据流向：
//   入站（OpenAI → GLM）：
//     OpenAI messages → ConvertMessages → GLM 文本提示词
//
//   出站（GLM → OpenAI）：
//     GLM SSE events → GLMEventAccumulator.ConsumeEvent → OpenAI SSE chunks
//     GLM SSE events → GLMEventAccumulator.Finalize → 最终 OpenAI SSE chunks
//     GLM 非流式响应 → GLMEventAccumulator.BuildResponse → OpenAI chat.completion

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

// --- 正则表达式和常量 ---

// assistantIDPattern 匹配 GLM 助手 ID（24 位以上的小写十六进制字符串）
var (
	assistantIDPattern = regexp.MustCompile(`^[a-z0-9]{24,}$`)
	// urlPattern 匹配 HTTP/HTTPS URL
	urlPattern = regexp.MustCompile(`https?://[^\s<>()"']+`)
)

// LocalFileHint 追加在本地文件路径后的提示文本，告知 GLM 该文件在本地不在服务端
const LocalFileHint = "(本地文件，不在服务区上，应该用[function_calls]工具)"

var (
	// systemReminderRE 匹配 <system-reminder> 标签及其后所有内容
	systemReminderRE = regexp.MustCompile(`(?s)<system-reminder>.*`)
	// localFilePathRE 匹配 Windows 本地文件路径（如 C:\path\to\file.txt 或 C:/path/to/file.txt）
	// 可选支持行号标记（如 #L10 或 #L10-20）
	localFilePathRE = regexp.MustCompile(`[A-Za-z]:[\\\/](?:[^\s<>()"'\n\\\/]+[\\\/])*[^\s<>()"'\n\\\/]+(?:#L\d+(?:-\d+)?)?`)
	// ImageRefRE 匹配 [image:url] 格式的图片引用标记
	ImageRefRE = regexp.MustCompile(`\[image:[^\]]+\]`)
)

// removeLocalFileHint 递归移除值中的本地文件提示文本
// 支持 string、map[string]any、[]any 三种类型的递归处理
// 对字符串类型同时执行 TrimSpace 去除首尾空白
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

// appendLocalFileHints 在文本中每个本地文件路径后追加 LocalFileHint 提示
// 模拟 Python 的 (?<!LOCAL_FILE_HINT) 负向后顾断言：若路径已紧随提示则不重复追加
//
// 处理流程：
//  1. 用 localFilePathRE 找到所有本地文件路径
//  2. 对每个路径检查其前方是否已有提示文本
//  3. 若无提示则追加，若有则跳过
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

// ExtractTextContent 从 OpenAI 消息的 content 字段中提取纯文本
//
// 支持三种 content 格式：
//   - string：直接返回
//   - map[string]any：序列化为 JSON 字符串返回
//   - []any（多模态内容列表）：按类型分别处理：
//   - "text" → 提取文本
//   - "image_url" → 转换为 [image:url] 格式
//   - "file" → 转换为 [file:url] 格式
//
// 多个内容片段用换行符连接
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

// ExtractFirstURL 从文本中提取第一个 URL
// 自动去除 URL 尾部常见的标点符号（如句号、逗号、括号等）
func ExtractFirstURL(text string) string {
	loc := urlPattern.FindStringIndex(text)
	if loc == nil {
		return ""
	}
	s := text[loc[0]:loc[1]]
	return strings.TrimRight(s, ".,;:!?)}+")
}

// ExtractRecentUserURL 从消息列表中提取最近一条用户消息中的 URL
// 从后往前遍历消息列表，找到第一个包含 URL 的用户消息并返回该 URL
// 用于工具调用的 fallbackURL 参数（当模型未能正确填充 URL 参数时使用）
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

// SanitizeToolCallPayload 清理单个工具调用的参数
//
// 处理逻辑：
//  1. 如果 arguments 是 JSON 字符串，先解析为对象
//  2. 如果 arguments 不是 map 类型，返回 nil（跳过该工具调用）
//  3. 修复参数名（通过 key 的 fmt.Sprintf 标准化）
//  4. 处理 GLM 模型常见的参数格式错误：
//     - {"param_name": "url"} → 用 fallbackURL 替代
//     - {"param_name": ..., "param_value": ...} → 清理无效参数
//
// 返回清理后的参数 map，若工具调用应被丢弃则返回 nil
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

	// 处理 GLM 模型输出的 {"param_name": "url"} 特殊格式
	// 模型有时无法正确生成工具调用参数，而是输出描述性的 param_name
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

// SanitizeToolCalls 批量清理工具调用列表
//
// 对每个工具调用：
//  1. 提取并验证工具名称（空名称的调用被跳过）
//  2. 调用 SanitizeToolCallPayload 清理参数
//  3. 移除参数中的本地文件提示（removeLocalFileHint）
//  4. 检测参数是否被修复（_repaired 标记）
//  5. 生成标准格式的工具调用对象
//
// 返回清理后的工具调用列表
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

		// 比较清理前后的 JSON 表示，判断参数是否被修复
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

// ConvertMessages 将 OpenAI 格式的消息列表转换为 GLM Web API 的文本提示词格式
//
// 这是入站转换的核心函数。GLM Web API 不接受结构化的消息列表，而是需要一个
// 纯文本提示词，其中包含工具定义、对话历史和角色标记。
//
// 转换流程：
//  1. 构建可用工具名称集合，解析 tool_choice 策略
//  2. 遍历消息列表，逐条转换：
//     - user 消息：提取文本，记录最新 URL
//     - assistant 消息：将 tool_calls 转换为 [function_calls] 格式
//     - tool 消息：转换为 ```tool_result``` 格式
//  3. 拼接工具定义 + 对话历史为完整提示词
//  4. 在本地文件路径后追加提示
//  5. 包装为 GLM 格式的单条 user 消息
//
// 参数：
//   - messages: OpenAI 格式的消息列表
//   - toolsList: OpenAI 格式的工具定义列表
//   - toolChoice: tool_choice 参数（"auto"/"none"/"required" 或指定工具名）
//   - serverSideToolNames: 服务端原生工具名集合（由后端自动执行）
func ConvertMessages(
	messages []map[string]any,
	toolsList []map[string]any,
	toolChoice any,
	serverSideToolNames map[string]bool,
) []map[string]any {
	// 构建可用工具名称集合
	availableToolNames := map[string]bool{}
	for _, t := range toolsList {
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
	validToolCallIDs := map[string]bool{}    // 记录有效的工具调用 ID
	repairedToolCallIDs := map[string]bool{} // 记录被修复的工具调用 ID
	toolCallIDToName := map[string]string{}  // 工具调用 ID → 工具名称映射

	for _, message := range messages {
		role, _ := message["role"].(string)
		if role == "" {
			role = "user"
		}
		content := message["content"]

		// 用户消息：提取文本并更新最新 URL（用于工具调用的 fallback）
		if role == "user" {
			currentText := ExtractTextContent(content)
			currentURL := ExtractFirstURL(currentText)
			if currentURL != "" {
				latestUserURL = currentURL
			}
		}

		// 助手消息：将 OpenAI tool_calls 格式转换为 GLM [function_calls] 格式
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
				// 清理工具调用参数（修复模型输出的格式错误）
				sanitizedToolCalls := SanitizeToolCalls(rawCallsList, latestUserURL)
				for _, tc := range sanitizedToolCalls {
					fn, _ := tc["function"].(map[string]any)
					toolName := "unknown"
					if fn != nil {
						if n, ok := fn["name"].(string); ok {
							toolName = n
						}
					}
					// 跳过不在可用工具列表中的调用
					if len(availableToolNames) > 0 && !availableToolNames[toolName] {
						continue
					}
					args := "{}"
					if fn != nil {
						if a, ok := fn["arguments"].(string); ok {
							args = a
						}
					}
					// 序列化为 [function_calls] 块格式
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
				// 合并助手文本和工具调用块
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
			// 工具结果消息：转换为 GLM 的 ```tool_result``` 块格式
			toolCallID, _ := message["tool_call_id"].(string)
			toolCallID = strings.TrimSpace(toolCallID)
			// 跳过无效或被修复的工具调用对应的结果
			if toolCallID != "" && len(validToolCallIDs) > 0 && !validToolCallIDs[toolCallID] {
				continue
			}
			if toolCallID != "" && repairedToolCallIDs[toolCallID] {
				continue
			}
			role = "user" // GLM 没有独立的 tool 角色，统一为 user
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

	// 拼接完整提示词：工具定义 + 对话历史
	var transcriptParts []string
	if len(toolsList) > 0 && toolChoicePolicy.Mode != "none" {
		transcriptParts = append(transcriptParts,
			tools.ToolsToPrompt(toolsList, toolChoicePolicy, serverSideToolNames),
			"# CONVERSATION",
		)
	}

	// 格式化对话历史：每条消息以角色名开头
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

	// 在本地文件路径后追加提示（跳过已带提示的路径，与 Python 负向后顾断言一致）
	prompt = appendLocalFileHints(prompt)

	// 包装为 GLM 格式的单条 user 消息，末尾追加 "Assistant:" 引导模型生成
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

// GLMEventAccumulator GLM SSE 事件累加器
//
// 将 GLM Web API 返回的 SSE 流事件累积并转换为 OpenAI chat.completion.chunk 格式。
// GLM 的 SSE 事件包含多个 part（通过 logic_id 标识），可能以乱序到达，
// 本累加器负责按 logic_id 排序、计算增量、解析工具调用。
//
// 核心职责：
//   - 按 logic_id 有序管理 parts
//   - 计算文本和推理内容的增量（delta）
//   - 通过 StreamingToolParser 解析内嵌的工具调用
//   - 提取 GLM 服务端原生工具调用（如 read）
//   - 生成符合 OpenAI SSE 规范的 JSON chunks
type GLMEventAccumulator struct {
	// 公开字段
	Model           string       // 模型名称（如 "glm-4-flash"）
	FallbackToolURL string       // 工具调用的 fallback URL（来自用户消息）
	DebugEnabled    bool         // 是否启用调试日志
	Logger          *slog.Logger // 日志记录器
	ConversationID  string       // GLM 会话 ID（从第一个事件中提取）
	Created         int64        // 响应创建时间戳（Unix 秒）

	// parts 管理
	partsByLogicID            map[string]map[string]any // logic_id → part 数据
	orderedLogicIDs           []string                  // 按字母序排列的 logic_id 列表
	lastFullText              string                    // 上一次完整渲染的文本
	lastFullReasoning         string                    // 上一次完整渲染的推理内容
	partTextSent              map[string]int            // 每个 part 已发送的文本长度
	partReasoningSent         map[string]int            // 每个 part 已发送的推理长度
	knownLogicIDsForText      []string                  // 已知的文本 part ID 列表
	knownLogicIDsForReasoning []string                  // 已知的推理 part ID 列表

	// 工具调用解析
	toolParser *tools.StreamingToolParser // 流式工具调用解析器

	// 输出控制
	emittedRole          bool              // 是否已发送 role 字段（OpenAI SSE 要求 role 只发送一次）
	renderCacheDirty     bool              // 渲染缓存是否需要刷新
	cachedFullText       string            // 缓存的完整文本
	cachedFullReasoning  string            // 缓存的完整推理内容
	cachedPartTexts      map[string]string // 每个 part 的渲染文本缓存
	cachedPartReasonings map[string]string // 每个 part 的渲染推理缓存

	// 服务端工具调用
	serverSideToolCalls   []map[string]any // GLM 服务端原生工具调用列表
	serverSideToolCallIDs map[string]bool  // 已记录的服务端工具调用 ID（去重用）
}

// NewGLMEventAccumulator 创建 GLM 事件累加器
//
// 参数：
//   - model: 模型名称
//   - fallbackToolURL: 工具调用的 fallback URL（通常来自用户消息中的 URL）
//   - debugEnabled: 是否启用调试日志
//   - logger: 日志记录器（nil 时使用空 logger）
func NewGLMEventAccumulator(model string, fallbackToolURL string, debugEnabled bool, logger *slog.Logger) *GLMEventAccumulator {
	if logger == nil {
		logger = logging.GetLogger("glm2api.null")
	}
	acc := &GLMEventAccumulator{
		Model:                 model,
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
		renderCacheDirty:      true,
	}
	return acc
}

// insertSorted 将 logicID 按字母序插入 orderedLogicIDs 列表（保持有序且不重复）
// 使用二分查找定位插入位置
func (a *GLMEventAccumulator) insertSorted(logicID string) {
	idx := sort.SearchStrings(a.orderedLogicIDs, logicID)
	if idx < len(a.orderedLogicIDs) && a.orderedLogicIDs[idx] == logicID {
		return // 已存在，跳过
	}
	a.orderedLogicIDs = append(a.orderedLogicIDs, "")
	copy(a.orderedLogicIDs[idx+1:], a.orderedLogicIDs[idx:])
	a.orderedLogicIDs[idx] = logicID
}

// ConsumeEvent 消费一个 GLM SSE 事件，返回增量 chunks 和当前状态
//
// 处理流程：
//  1. 提取 conversation_id（首次事件）
//  2. 如果工具调用已完成，直接返回 "tool_call_complete" 状态
//  3. 处理事件中的 parts：
//     - 按 logic_id 有序存储
//     - 提取服务端原生工具调用（如 read/open_url）
//  4. 计算文本和推理内容的增量
//  5. 通过 toolParser 过滤工具调用标记，提取可见文本
//  6. 生成 OpenAI SSE chunk
//
// 返回值：
//   - []string: SSE 格式的 JSON chunks
//   - string: 状态（"processing"、"finish"、"intervene"、"tool_call_complete"、""）
func (a *GLMEventAccumulator) ConsumeEvent(payload map[string]any) ([]string, string) {
	logging.DebugDump(a.Logger, a.DebugEnabled, "GLM SSE 解析事件", payload)
	// 从第一个事件中提取 conversation_id
	if a.ConversationID == "" {
		if id, ok := payload["conversation_id"].(string); ok && id != "" {
			a.ConversationID = id
		}
	}

	// 工具调用已解析完成，后续事件不再处理（与 Python 的 is_tool_call_completed 一致）
	if a.toolParser.IsToolCallCompleted() {
		return nil, "tool_call_complete"
	}

	// 处理事件中的 parts
	if parts, ok := payload["parts"].([]any); ok {
		for _, p := range parts {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			// 按 logic_id 有序存储 part，标记渲染缓存需要刷新
			if logicID, ok := part["logic_id"].(string); ok && logicID != "" {
				if _, exists := a.partsByLogicID[logicID]; !exists {
					a.insertSorted(logicID)
				}
				a.partsByLogicID[logicID] = part
				a.renderCacheDirty = true
			}
			// 提取 GLM 服务端原生工具调用（后端自动执行的工具）
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
						// GLM 内部的 open_url 工具在 API 层映射为 read
						if toolName == "open_url" {
							toolName = "read"
						}
						toolID, _ := toolCallsData["id"].(string)
						toolID = strings.TrimSpace(toolID)
						arguments := toolCallsData["arguments"]

						// 去重：同一个 toolID 只记录一次
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
				}
			}
		}
	}

	// 计算增量文本和推理内容
	textDelta, reasoningDelta := a.computeDeltas()
	a.lastFullText = a.cachedFullText
	a.lastFullReasoning = a.cachedFullReasoning

	var chunks []string

	// 生成推理内容增量 chunk（reasoning_content 字段）
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

	// 将文本增量通过 toolParser 过滤（去除工具调用标记），得到可见文本
	visibleTextDelta := a.toolParser.Consume(textDelta)
	if visibleTextDelta != "" {
		deltaPayload := map[string]any{"content": visibleTextDelta}
		if !a.emittedRole {
			// 首次输出时需要包含 role 字段
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
	logging.DebugDump(a.Logger, a.DebugEnabled, "GLM SSE 生成增量块", chunks)

	// 提取事件状态（"processing"、"finish"、"intervene" 等）
	status := ""
	if s, ok := payload["status"]; ok && s != nil {
		status = fmt.Sprintf("%v", s)
	}
	return chunks, status
}

// Finalize 完成响应，输出剩余的所有 chunks
//
// 在流结束或收到终止状态时调用，执行以下收尾工作：
//  1. 刷新 toolParser 获取剩余文本和工具调用
//  2. 合并服务端工具调用和 JSON 工具调用，重新编号索引
//  3. 尝试从剩余文本中恢复遗漏的工具调用
//  4. 输出最终文本内容
//  5. 处理 GLM 的 intervene（干预）状态
//  6. 输出所有工具调用 chunks
//  7. 输出 finish_reason 和 usage，以及 [DONE] 标记
func (a *GLMEventAccumulator) Finalize(status string, lastError map[string]any) []string {
	// 刷新工具解析器，获取剩余文本和解析出的工具调用
	tailText, jsonToolCalls := a.toolParser.Flush()
	jsonToolCalls = SanitizeToolCalls(jsonToolCalls, a.FallbackToolURL)
	// 如果 JSON 解析没有工具调用，尝试从推理内容中提取
	if len(jsonToolCalls) == 0 {
		jsonToolCalls = a.extractReasoningToolCalls("")
	}

	// 合并服务端工具调用和 JSON 工具调用，重新索引
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
	finalText := tailText

	// 如果没有工具调用但剩余文本包含工具调用标记，尝试从中提取
	// 这处理了模型输出工具调用但未被 StreamingToolParser 完全解析的情况
	if len(allToolCalls) == 0 && finalText != "" {
		recoveredClean, recoveredCalls := tools.ParseToolCallsFromText(finalText)
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

	// 输出最终可见文本（仅当没有工具调用时）
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

	// 处理 GLM 的 intervene（干预）状态：输出干预文本
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

	// 输出所有工具调用 chunks
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

	// 输出 finish_reason、usage 和 [DONE] 标记
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

// GetOrderedParts 返回按 logic_id 排序的 parts 列表
// 用于图片响应构建，确保图片按 GLM 生成的逻辑顺序返回
func (a *GLMEventAccumulator) GetOrderedParts() []map[string]any {
	result := make([]map[string]any, 0, len(a.orderedLogicIDs))
	for _, logicID := range a.orderedLogicIDs {
		if part, ok := a.partsByLogicID[logicID]; ok {
			result = append(result, part)
		}
	}
	return result
}

// extractReasoningToolCalls 从推理内容中尝试提取工具调用
// 优先使用传入的 reasoningText，为空时依次回退到 lastFullReasoning 和 cachedFullReasoning
// 用于处理模型将工具调用输出在推理内容（think）中的情况
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
	_, toolCalls := tools.ParseToolCallsFromText(strings.TrimSpace(source))
	return SanitizeToolCalls(toolCalls, a.FallbackToolURL)
}

// computeDeltas 计算本次事件相对于上次的文本和推理内容增量
//
// 增量计算逻辑：
//   - 新 part：输出完整内容（前面加 \n\n 分隔）
//   - 已有 part：输出新增部分（截取 prevLen 之后的内容）
//   - 按 logic_id 有序遍历，确保输出顺序一致
//
// 返回值：(文本增量, 推理增量)
func (a *GLMEventAccumulator) computeDeltas() (string, string) {
	a.renderFullOutput()
	var textDeltaParts, reasoningDeltaParts []string

	for _, logicID := range a.orderedLogicIDs {
		renderedText := a.cachedPartTexts[logicID]
		renderedReasoning := a.cachedPartReasonings[logicID]

		// 计算文本增量
		if renderedText != "" {
			prevLen := a.partTextSent[logicID]
			isNew := !containsString(a.knownLogicIDsForText, logicID)
			if isNew {
				// 新 part：输出完整内容
				a.knownLogicIDsForText = append(a.knownLogicIDsForText, logicID)
				if len(textDeltaParts) > 0 || len(a.partTextSent) > 0 {
					textDeltaParts = append(textDeltaParts, "\n\n")
				}
				textDeltaParts = append(textDeltaParts, renderedText)
			} else if len(renderedText) > prevLen {
				// 已有 part：只输出新增部分
				textDeltaParts = append(textDeltaParts, renderedText[prevLen:])
			}
			a.partTextSent[logicID] = len(renderedText)
		}

		// 计算推理增量（逻辑与文本相同）
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

// renderFullOutput 将所有 parts 渲染为完整的文本和推理内容
// 使用缓存机制避免重复渲染，仅在 renderCacheDirty 为 true 时重新计算
//
// GLM part 的 content 类型：
//   - "text": 普通文本 → 直接输出
//   - "think": 推理内容 → 输出到 reasoning
//   - "code": 代码块 → 包装为 ```python ... ``` 格式
//   - "execution_output": 执行输出 → 直接输出
//   - "image": 图片 → 转换为 markdown 图片格式
//
// 返回值：(完整文本, 完整推理内容)
func (a *GLMEventAccumulator) renderFullOutput() (string, string) {
	if !a.renderCacheDirty {
		return a.cachedFullText, a.cachedFullReasoning
	}

	var textParts, reasoningParts []string
	a.cachedPartTexts = map[string]string{}
	a.cachedPartReasonings = map[string]string{}

	// 按 logic_id 有序遍历所有 parts
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

// chunkJSON 将 patch 中的字段合并到标准 chunk 模板中，序列化为 SSE 格式
// 标准模板包含 id、object、created、model 字段
// 返回格式："data: {json}\n\n"
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

// containsString 检查字符串切片中是否包含指定值
func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// filterEmpty 过滤掉字符串切片中的空字符串
func filterEmpty(parts []string) []string {
	var result []string
	for _, p := range parts {
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
