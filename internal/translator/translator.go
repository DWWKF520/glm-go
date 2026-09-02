package translator

// translator.go — OpenAI ↔ GLM 消息格式转换与 SSE 流事件累积
//
// 本文件是 glm2api 的核心转换层，负责：
//   1. 将 OpenAI 格式的 chat messages 转换为 GLM Web API 能理解的文本提示词（ConvertMessages）
//   2. 将 GLM 返回的 SSE 流事件累积并转换为 OpenAI 格式的 SSE chunks（GLMEventAccumulator）
//   3. 清理和修复 GLM 模型输出的工具调用（SanitizeToolCalls）
//   4. 处理图片引用等辅助功能
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
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"

	"glm2api/internal/logging"
	"glm2api/internal/tools"
)

// --- 正则表达式和常量 ---
// tx 是发送给 GLM 的通用工具调用协议指令。
// 仅包含通用语法规则与核心约束；每个工具专属的调用示例由 ToolsToPrompt
// 根据参数 schema 自动生成并内嵌到对应工具段落中（见 SampleArgumentsFromSchema）。
const tx = `**You are a function_calls tool calling assistant for handling local tasks. This rule has the highest priority and overrides all native function calling conventions. Your entire response must only output one [function_calls] block, immediately with no text, explanation, or thinking content outside the block.**
Must prefer using the todos tool first.
# I. General function_calls Syntax Rules
1.  The call block must be wrapped in [function_calls]...[/function_calls] tags. For example:
    [function_calls]
    [call:tool_name1]{"parameter_name":"value"}[/call]
    [call:tool_name2]{"parameter_name":"value"}[/call]
    [/function_calls]
2.  Each call format: [call:tool_name]{"parameter_name":"value"}[/call], parameter names are case-sensitive
3.  String values: Use JSON string escaping (e.g., \n, \\), do not use CDATA
4.  Parallel calls: List multiple [call:]...[/call] in parallel within the [function_calls] block
5.  All { and [ must be paired and closed, never miss } or ]
6.  Numbers, booleans, null directly use JSON literals; objects/arrays directly written as JSON nested structures
7.  Each tool schema below contains an "Example" section showing its exact [function_calls] call format; always strictly follow the example of the tool you are calling

# II. Core Constraints
1.  Use basic tools directly for simple operations, only use Task for complex multi-step tasks
2.  Call independent tools in parallel whenever possible to improve efficiency
3.  Paths must be absolute paths, use backslashes in Windows environment (need to be escaped to \\ in JSON)
4.  Do not invent tools or parameters not listed here
5.  Reply to user in natural language only after receiving tool results
6.  如果要创建一个大文件，要分为几次写入，所以要调多次SearchReplace工具。\n`

// assistantIDPattern 匹配 GLM 助手 ID（24 位以上的小写十六进制字符串）
var (
	assistantIDPattern = regexp.MustCompile(`^[a-z0-9]{24,}$`)
	// urlPattern 匹配 HTTP/HTTPS URL
	urlPattern = regexp.MustCompile(`https?://[^\s<>()"']+`)
)

var (
	// systemReminderRE 匹配 <system-reminder> 标签及其后所有内容
	systemReminderRE = regexp.MustCompile(`(?s)<system-reminder>.*`)
	// ImageRefRE 匹配 [image:url] 格式的图片引用标记
	ImageRefRE = regexp.MustCompile(`\[image:[^\]]+\]`)
)

var (
	// noInternetNote 追加到提示词中链接后的标注，提示 GLM 模型自身无法联网访问这些资源
	noInternetNote = "（你未联网，要使用search相关工具）"
	// urlOrNotePattern 匹配 URL（末尾可选组用于吞掉已存在的标注，保证重复转换时不会叠加标注）
	urlOrNotePattern = regexp.MustCompile(`https?://[^\s<>()"']+(?:（你未联网，要使用search相关工具）)?`)
)

// AnnotateNoInternet 在文本中的链接后追加 "（你未联网，要使用search相关工具）" 标注。
//
// 用于入站转换：GLM 无法联网，用户消息中的 URL 若不加说明，
// 模型可能尝试直接"访问"或编造其内容；追加标注可明确提示模型这些资源
// 需要通过工具处理或让用户提供内容。已带标注的匹配不再重复追加（幂等）。
func AnnotateNoInternet(text string) string {
	if text == "" || !urlOrNotePattern.MatchString(text) {
		return text
	}
	return urlOrNotePattern.ReplaceAllStringFunc(text, func(m string) string {
		if strings.HasSuffix(m, noInternetNote) {
			return m
		}
		return m + noInternetNote
	})
}

// StripNoInternetNote 清除文本中的 "（你未联网，要使用search相关工具）" 标注（含半角括号变体）。
//
// 用于出站转换：模型偶尔会把提示词中的标注原样复制进工具调用参数
// （如 url、file_path），返回给客户端前必须清除，避免污染真实参数值。
func StripNoInternetNote(s string) string {
	if !strings.Contains(s, "你未联网，要使用search相关工具") {
		return s
	}
	r := strings.NewReplacer(
		"（你未联网，要使用search相关工具）", "",
		"(你未联网，要使用search相关工具)", "",
		"（你未联网，要使用search相关工具)", "",
		"(你未联网，要使用search相关工具）", "",
	)
	return strings.TrimSpace(r.Replace(s))
}

// stripNoInternetNoteArgs 清除参数 map 中所有字符串值里的 "（你未联网，要使用search相关工具）" 标注。
func stripNoInternetNoteArgs(args map[string]any) {
	for k, v := range args {
		if s, ok := v.(string); ok {
			args[k] = StripNoInternetNote(s)
		}
	}
}

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

// buildToolParamTypeMap 从工具定义列表中构建工具名称到参数类型映射
//
// 工具定义格式（OpenAI 格式）：
//
//	{
//	  "type": "function",
//	  "function": {
//	    "name": "tool_name",
//	    "parameters": {
//	      "type": "object",
//	      "properties": {
//	        "param1": { "type": "integer" },
//	        "param2": { "type": "string" }
//	      }
//	    }
//	  }
//	}
//
// 返回格式：map[工具名]map[参数名]参数类型字符串
func buildToolParamTypeMap(toolsList []map[string]any) map[string]map[string]string {
	result := make(map[string]map[string]string)
	if len(toolsList) == 0 {
		return result
	}
	for _, tool := range toolsList {
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			continue
		}
		toolName := strings.TrimSpace(fmt.Sprintf("%v", fn["name"]))
		if toolName == "" {
			continue
		}
		parameters, _ := fn["parameters"].(map[string]any)
		if parameters == nil {
			continue
		}
		properties, _ := parameters["properties"].(map[string]any)
		if properties == nil {
			continue
		}
		paramTypes := make(map[string]string)
		for paramName, paramDef := range properties {
			paramDefMap, _ := paramDef.(map[string]any)
			if paramDefMap == nil {
				continue
			}
			if paramType, ok := paramDefMap["type"].(string); ok {
				paramTypes[paramName] = paramType
			}
		}
		result[toolName] = paramTypes
	}
	return result
}

// extractToolNames 从工具定义列表中提取所有工具名称，用于辅助从 name 字段中分离嵌入的工具名和参数名。
// 返回工具名称字符串切片；输入为空返回 nil。
func extractToolNames(toolsList []map[string]any) []string {
	if len(toolsList) == 0 {
		return nil
	}
	names := make([]string, 0, len(toolsList))
	for _, tool := range toolsList {
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name := strings.TrimSpace(fmt.Sprintf("%v", fn["name"]))
		if name != "" && name != "<nil>" {
			names = append(names, name)
		}
	}
	return names
}

// coerceParamValue 根据参数类型 schema 矫正参数值
//
// 处理模型常见的类型错误：
//   - integer/number 类型：字符串 "5" → 数字 5
//   - boolean 类型：字符串 "true"/"false" → 布尔值
//   - array 类型：字符串（双重序列化的 JSON）→ []any
//   - object 类型：字符串（双重序列化的 JSON）→ map[string]any
//
// 参数：
//   - value: 原始参数值
//     expectedType: 期望的参数类型（如 "integer", "number", "boolean", "array", "object"）
//
// 返回矫正后的值，如果无法转换则返回原值
func coerceParamValue(value any, expectedType string) any {
	switch expectedType {
	case "integer":
		// 字符串 → 整数
		if s, ok := value.(string); ok {
			s = strings.TrimSpace(s)
			if s == "" {
				return value
			}
			// 尝试解析为整数
			if i, err := strconv.ParseInt(s, 10, 64); err == nil {
				return i
			}
			// 尝试解析为浮点数后取整（处理 "5.0" 这种情况）
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return int64(f)
			}
		}
		// 浮点数 → 整数（处理 JSON 解析为 float64 的情况）
		if f, ok := value.(float64); ok {
			return int64(f)
		}
	case "number":
		// 字符串 → 浮点数
		if s, ok := value.(string); ok {
			s = strings.TrimSpace(s)
			if s == "" {
				return value
			}
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return f
			}
		}
	case "boolean":
		// 字符串 → 布尔值
		if s, ok := value.(string); ok {
			s = strings.ToLower(strings.TrimSpace(s))
			switch s {
			case "true", "1", "yes":
				return true
			case "false", "0", "no":
				return false
			}
		}
	case "array":
		// 字符串 → 数组（处理 GLM 双重序列化的 JSON 字符串）
		// 如 todos="[{\"id\":\"1\"}]" → [{"id":"1"}]
		if s, ok := value.(string); ok {
			s = strings.TrimSpace(s)
			if s == "" {
				return value
			}
			var arr []any
			if err := sonic.UnmarshalString(s, &arr); err == nil {
				return arr
			}
		}
	case "object":
		// 字符串 → 对象（处理 GLM 双重序列化的 JSON 字符串）
		if s, ok := value.(string); ok {
			s = strings.TrimSpace(s)
			if s == "" {
				return value
			}
			var obj map[string]any
			if err := sonic.UnmarshalString(s, &obj); err == nil {
				return obj
			}
		}
	}
	return value
}

// coerceParamValueHeuristic 在没有工具 schema 的情况下，对参数值做启发式类型矫正。
// GLM 服务端原生工具（如 WebSearch）可能不在客户端提供的 ToolsList 中，
// 此时无法通过 schema 矫正类型，用启发式处理字符串值：
//   - 纯整数字符串 "5" → int64(5)
//   - 浮点数字符串 "3.14" → float64(3.14)
//   - "true"/"false"（不区分大小写）→ 布尔值
//   - 其他字符串保持不变
//
// 避免过度转换：仅处理明确的数字和布尔字面量，不处理日期、ID 等字符串。
func coerceParamValueHeuristic(value any) any {
	s, ok := value.(string)
	if !ok {
		return value
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return value
	}
	// 整数：纯数字（可选负号），不含小数点
	if isIntegerLiteral(s) {
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i
		}
	}
	// 浮点数：含小数点
	if strings.Contains(s, ".") {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	// 布尔值
	switch strings.ToLower(s) {
	case "true":
		return true
	case "false":
		return false
	}
	return value
}

// isIntegerLiteral 判断字符串是否为纯整数字面量（可选负号开头，后跟数字）。
// 用于启发式类型矫正，避免把 "1e5"、"0x1F"、"1_000" 等误判为整数。
func isIntegerLiteral(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '-' || s[0] == '+' {
		i = 1
	}
	if i >= len(s) {
		return false
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// coerceToolCallArgs 对工具调用参数进行类型矫正（schema 优先 + 启发式兜底）。
// 若工具在 schema 中有定义，按 schema 类型矫正；否则对字符串值做启发式矫正。
// 跳过 "id" 参数（工具调用 ID 不应被转换）。
func coerceToolCallArgs(args map[string]any, toolName string, toolParamTypes map[string]map[string]string) map[string]any {
	if len(args) == 0 {
		return args
	}
	paramTypes, hasSchema := toolParamTypes[toolName]
	for k, v := range args {
		if k == "id" {
			continue
		}
		if hasSchema {
			if expectedType, ok := paramTypes[k]; ok {
				args[k] = coerceParamValue(v, expectedType)
				continue
			}
		}
		// 无 schema 或该参数未定义类型：启发式矫正
		args[k] = coerceParamValueHeuristic(v)
	}
	return args
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
//  5. 根据工具 schema 矫正参数类型（如字符串 "5" → 数字 5）
//
// 返回清理后的参数 map，若工具调用应被丢弃则返回 nil
func SanitizeToolCallPayload(toolName string, arguments any, fallbackURL string, paramTypes map[string]string) map[string]any {
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

	// 清除模型复制进参数的 "（你未联网，要使用search相关工具）" 标注
	stripNoInternetNoteArgs(cleaned)

	// 根据工具 schema 矫正参数类型（如字符串 "5" → 数字 5）
	if len(paramTypes) > 0 {
		for k, v := range cleaned {
			if expectedType, ok := paramTypes[k]; ok {
				if k == "id" {
					continue
				}
				cleaned[k] = coerceParamValue(v, expectedType)
			}
		}
	}

	return cleaned
}

// SanitizeToolCalls 批量清理工具调用列表
//
// 对每个工具调用：
//  1. 提取并验证工具名称（空名称的调用被跳过）
//  2. 调用 SanitizeToolCallPayload 清理参数
//  3. 根据工具 schema 矫正参数类型（如字符串 "5" → 数字 5）
//  4. 检测参数是否被修复（_repaired 标记）
//  5. 生成标准格式的工具调用对象
//
// 参数：
//   - toolCalls: 原始工具调用列表
//   - fallbackURL: 备用 URL（用于修复参数格式错误）
//   - toolsList: 工具定义列表（用于提取参数类型信息进行类型矫正）
//
// 返回清理后的工具调用列表
func SanitizeToolCalls(toolCalls []map[string]any, fallbackURL string, toolsList []map[string]any) []map[string]any {
	// 构建工具名称到参数类型映射
	toolParamTypes := buildToolParamTypeMap(toolsList)

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
		// 获取该工具的参数类型映射
		paramTypes := toolParamTypes[toolName]
		cleanedArguments := SanitizeToolCallPayload(toolName, originalArguments, fallbackURL, paramTypes)
		if cleanedArguments == nil {
			continue
		}

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
//  4. 包装为 GLM 格式的单条 user 消息
//
// 参数：
//   - messages: OpenAI 格式的消息列表
//   - toolsList: OpenAI 格式的工具定义列表
//   - toolChoice: tool_choice 参数（"auto"/"none"/"required" 或指定工具名）
//   - serverSideToolNames: 服务端原生工具名集合（由后端自动执行）
//
// wkf
func ConvertMessages(
	messages []map[string]any,
	toolsList []map[string]any,
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
			role = "context"
		}
		content := message["content"]

		// 用户消息：提取文本并更新最新 URL（用于工具调用的 fallback）
		if role == "user" {
			currentText := ExtractTextContent(content)
			currentURL := ExtractFirstURL(currentText)
			if currentURL != "" {
				latestUserURL = currentURL
			}
			// 在链接和文件路径后追加 "（你未联网，要使用search相关工具）" 标注
			// （必须在提取 URL 之后进行，避免标注污染 fallback URL）
			content = AnnotateNoInternet(currentText)
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
				// 清理工具调用参数（修复模型输出的格式错误，包括类型矫正）
				sanitizedToolCalls := SanitizeToolCalls(rawCallsList, latestUserURL, toolsList)
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
			role = "tool_result" // GLM 没有独立的 tool 角色，统一为 user
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
	if len(toolsList) > 0 {
		transcriptParts = append(transcriptParts,
			tools.ToolsToPrompt(toolsList, serverSideToolNames),
			"# CONVERSATION",
		)
	}
	// 格式化对话历史：每条消息以角色名开头
	for _, item := range processed {
		title := item.role
		switch title {
		case "system":
			title = "System"
			item.content = tx + item.content
		case "context":
			title = "Context"
		case "assistant":
			title = "Assistant"
		case "user":
			title = "User"
		case "tool_result":
			title = "Tool_Result"
		}
		line := title + ": " + item.content
		transcriptParts = append(transcriptParts, strings.TrimSpace(line))
	}

	prompt := strings.TrimSpace(strings.Join(transcriptParts, "\n"))

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
	Model           string           // 模型名称（如 "glm-4-flash"）
	FallbackToolURL string           // 工具调用的 fallback URL（来自用户消息）
	ToolsList       []map[string]any // 工具定义列表（用于参数类型矫正）
	DebugEnabled    bool             // 是否启用调试日志
	Logger          *slog.Logger     // 日志记录器
	ConversationID  string           // GLM 会话 ID（从第一个事件中提取）
	Created         int64            // 响应创建时间戳（Unix 秒）

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
//   - toolsList: 工具定义列表（用于参数类型矫正）
//   - logger: 日志记录器（nil 时使用空 logger）
func NewGLMEventAccumulator(model string, fallbackToolURL string, toolsList []map[string]any, debugEnabled bool, logger *slog.Logger) *GLMEventAccumulator {
	if logger == nil {
		logger = logging.GetLogger("glm2api.null")
	}
	acc := &GLMEventAccumulator{
		Model:                 model,
		FallbackToolURL:       fallbackToolURL,
		ToolsList:             toolsList,
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

// extractEmbeddedArgsFromName 和 argumentsIsEmpty 已移至 tools 包（protocol.go），
// 因为它们是通用的工具调用参数处理逻辑，不依赖 translator 状态。
// 详见 tools.ExtractEmbeddedArgsFromName 和 tools.ArgumentsIsEmpty。

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
//
// wkf
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
				if isIncreasePushPayload(payload) {
					// 增量推送模式：part 内容只是本次新增片段，需合并进已累积的 part
					a.partsByLogicID[logicID] = mergeIncrementalPart(a.partsByLogicID[logicID], part)
				} else {
					a.partsByLogicID[logicID] = part
				}
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
						// 兼容 GLM 服务端将参数嵌入 name 字段的异常格式
						// 如 name = `Read{"file_path":"..."}` 或 `TodoWritetodos[{...}]`
						// 需要分离出真正的工具名和参数
						var embeddedArgs map[string]any
						if actualName, args, ok := tools.ExtractEmbeddedArgsFromName(toolName, extractToolNames(a.ToolsList)...); ok {
							toolName = actualName
							embeddedArgs = args
						}
						// GLM 内部的 open_url 工具在 API 层映射为 read
						if toolName == "finish" {
							continue
						}
						if toolName == "open_url" {
							toolName = "read"
						}
						toolID, _ := toolCallsData["id"].(string)
						toolID = strings.TrimSpace(toolID)
						arguments := toolCallsData["arguments"]
						// 如果 arguments 为空但 name 中嵌入了参数，使用嵌入的参数
						if tools.ArgumentsIsEmpty(arguments) && embeddedArgs != nil {
							arguments = embeddedArgs
							a.Logger.Debug("使用 name 中嵌入的参数作为 arguments", "toolName", toolName)
						}
						// 根据工具 schema 矫正参数类型（如字符串 "5" → 数字 5）；
						// 无 schema 时（如 GLM 服务端原生工具）用启发式矫正
						argsStr := "{}"
						toolParamTypes := buildToolParamTypeMap(a.ToolsList)
						if s, ok := arguments.(string); ok {
							// string 类型：先解析为 map，矫正后再序列化
							var parsed map[string]any
							if err := sonic.UnmarshalString(s, &parsed); err != nil || parsed == nil {
								// JSON 被截断（上游流中断导致缺少闭合括号等）：尝试补全后重新解析，
								// 否则原样透传会绕过类型矫正，且非法 JSON 下发给客户端也无法解析
								parsed = nil
								if repaired := tools.RepairTruncatedJSON(s); repaired != s {
									if err := sonic.UnmarshalString(repaired, &parsed); err == nil && parsed != nil {
										a.Logger.Warn("服务端工具调用 arguments 被截断，已自动补全修复",
											"toolName", toolName, "raw", s, "repaired", repaired)
									}
								}
							}
							if parsed != nil {
								// 清除模型复制进参数的 "（你未联网，要使用search相关工具）" 标注
								stripNoInternetNoteArgs(parsed)
								argsStr = tools.SafeJSONDumpsCompact(coerceToolCallArgs(parsed, toolName, toolParamTypes))
							} else {
								argsStr = StripNoInternetNote(s)
							}
						} else if argsMap, ok := arguments.(map[string]any); ok {
							// map 类型：直接矫正
							// 清除模型复制进参数的 "（你未联网，要使用search相关工具）" 标注
							stripNoInternetNoteArgs(argsMap)
							argsStr = tools.SafeJSONDumpsCompact(coerceToolCallArgs(argsMap, toolName, toolParamTypes))
						} else if arguments != nil {
							// 其他类型（如数字、布尔）：直接序列化
							argsStr = tools.SafeJSONDumpsCompact(arguments)
						}

						// 去重：同一个 toolID 只记录一次
						if toolName != "" && toolID != "" && !a.serverSideToolCallIDs[toolID] {
							a.serverSideToolCallIDs[toolID] = true
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
						a.Logger.Info("Server-side tool call:", "name:", toolName, "args:", argsStr)
						a.toolParser.SetToolCallCompleted()
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
//
// wkf
func (a *GLMEventAccumulator) Finalize(status string, lastError map[string]any) []string {
	// 刷新工具解析器，获取解析出的工具调用
	jsonToolCalls := a.toolParser.Flush()
	a.Logger.Info("finalize: tool parser flush", "jsonToolCalls", jsonToolCalls)
	jsonToolCalls = SanitizeToolCalls(jsonToolCalls, a.FallbackToolURL, a.ToolsList)
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
	return SanitizeToolCalls(toolCalls, a.FallbackToolURL, a.ToolsList)
}

// isIncreasePushPayload 判断事件是否为增量推送模式（meta_data.if_increase_push=true）。
// 该模式下 part 的 text/think 内容只包含本次新增的片段而非全量快照，
// 必须合并到已累积的 part 上，否则 computeDeltas 的 prevLen 截断会破坏流式文本
// （例如把 [function_calls] 标记拆散，导致工具调用解析失败）。
func isIncreasePushPayload(payload map[string]any) bool {
	meta, ok := payload["meta_data"].(map[string]any)
	if !ok {
		return false
	}
	increase, _ := meta["if_increase_push"].(bool)
	return increase
}

// mergeIncrementalPart 将增量片段 part 合并到已累积的 existing part。
// text/think 内容追加到同类型内容的最后一项，其他内容（如 tool_calls）直接追加；
// part 的其余字段（status、meta_data 等）以片段中的最新值为准。
// existing 为 nil 时等价于直接使用 fragment。
func mergeIncrementalPart(existing, fragment map[string]any) map[string]any {
	if existing == nil {
		return fragment
	}

	merged := map[string]any{}
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range fragment {
		merged[k] = v
	}

	existingContent, _ := existing["content"].([]any)
	fragmentContent, _ := fragment["content"].([]any)
	if len(existingContent) == 0 || len(fragmentContent) == 0 {
		merged["content"] = fragmentContent
		return merged
	}

	content := make([]any, len(existingContent))
	copy(content, existingContent)
	for _, fc := range fragmentContent {
		item, ok := fc.(map[string]any)
		if !ok {
			content = append(content, fc)
			continue
		}
		itemType, _ := item["type"].(string)
		var field string
		switch itemType {
		case "text":
			field = "text"
		case "think":
			field = "think"
		default:
			content = append(content, item)
			continue
		}
		// 追加到已累积内容中同类型的最后一项
		appended := false
		for i := len(content) - 1; i >= 0; i-- {
			prev, ok := content[i].(map[string]any)
			if !ok {
				continue
			}
			if t, _ := prev["type"].(string); t == itemType {
				fragText, _ := item[field].(string)
				if prevText, ok := prev[field].(string); ok {
					prev[field] = prevText + fragText
				} else {
					prev[field] = fragText
				}
				appended = true
				break
			}
		}
		if !appended {
			content = append(content, item)
		}
	}
	merged["content"] = content
	return merged
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
			isNew := !slices.Contains(a.knownLogicIDsForText, logicID)
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
			isNew := !slices.Contains(a.knownLogicIDsForReasoning, logicID)
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
