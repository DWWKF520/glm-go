package tools

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/bytedance/sonic"

	"github.com/google/uuid"
)

var (
	// ToolCodeFencePattern 匹配完整的 ```tool_code ... ``` 块
	ToolCodeFencePattern = regexp.MustCompile("(?is)```tool_code[ \t]*\n(?P<body>[\\s\\S]*?)\n?```")
	// ToolResultFencePattern 匹配完整的 ```tool_result ... ``` 块
	ToolResultFencePattern = regexp.MustCompile("(?is)```tool_result[ \t]*\n[\\s\\S]*?\n?```")
	// FunctionCallsFencePattern 匹配 [function_calls]...[/function_calls] 块
	FunctionCallsFencePattern = regexp.MustCompile(`(?is)\[function_calls\](?P<body>[\s\S]*?)(?:\[/function_calls\]|$)`)
	// CallItemStartPattern 匹配 [call:name] 项目起始
	CallItemStartPattern = regexp.MustCompile(`(?i)\[call\s*[:=]?\s*(?P<name>[a-zA-Z0-9_:-]+)\]`)
	// ToolCodeStartPattern 流式检测起始标记
	ToolCodeStartPattern = regexp.MustCompile("(?i)```tool_code[ \t]*\n")
	// ToolResultStartPattern 流式检测起始标记
	ToolResultStartPattern = regexp.MustCompile("(?i)```tool_result[ \t]*\n")
	// 任何 ``` 代码块
	AnyFencePattern = regexp.MustCompile("(?is)```[^\n]*\n[\\s\\S]*?```")
	// 裸 JSON 工具调用开始
	BareToolCallsOpenPattern = regexp.MustCompile(`(?i)\{\s*"tool_calls"\s*:`)
	// FunctionCallsStartPattern 流式检测 [function_calls] 开始标记
	FunctionCallsStartPattern = regexp.MustCompile(`(?i)\[function_calls\]`)
	// FunctionCallsEndPattern 流式检测 [/function_calls] 结束标记
	FunctionCallsEndPattern = regexp.MustCompile(`(?i)\[/function_calls\]`)

	toolCodeMarker         = "```tool_code"
	toolResultMarker       = "```tool_result"
	functionCallsMarker    = "[function_calls]"
	functionCallsEndMarker = "[/function_calls]"
	fenceCloseNewline      = "\n```"
	fenceCloseBare         = "```"
)

var toolParserLogger = slog.Default().With("logger", "glm2api.tool_parser")

// FindFenceClose 从 text 的 start 位置开始，查找最早的 ``` 代码块闭合标记。
// 返回两个值：闭合标记在原文中的起始位置，以及闭合标记的字节长度。
// 长度为 4 表示 "\n```" 形式（换行+反引号），长度为 3 表示裸 "```" 形式。
// 如果找不到任何闭合标记，返回 (-1, 0)。
// 该函数优先匹配 "\n```" 形式，因为在实际文本中这种格式更规范。
func FindFenceClose(text string, start int) (int, int) {
	posNewline := indexFrom(text, fenceCloseNewline, start)
	posBare := indexFrom(text, fenceCloseBare, start)
	if posNewline == -1 && posBare == -1 {
		return -1, 0
	}
	if posNewline == -1 {
		return posBare, len(fenceCloseBare)
	}
	if posBare == -1 {
		return posNewline, len(fenceCloseNewline)
	}
	if posNewline <= posBare {
		return posNewline, len(fenceCloseNewline)
	}
	return posBare, len(fenceCloseBare)
}

// indexFrom 是 strings.Index 的增强版本，支持从指定偏移位置开始搜索。
// 如果 start 小于 0 或大于字符串长度，直接返回 -1。
// 找到子串后，返回的结果是基于原字符串的绝对位置（而非子串内的相对位置）。
func indexFrom(s, substr string, start int) int {
	if start < 0 || start > len(s) {
		return -1
	}
	if start > 0 {
		s = s[start:]
	}
	idx := strings.Index(s, substr)
	if idx < 0 {
		return -1
	}
	return start + idx
}

// BuildToolCall 构建一个符合 OpenAI 格式的工具调用对象。
// 参数 name 是工具名称，arguments 是参数键值对，index 是调用序号（用于流式响应中标识第几个调用）。
// 返回的 map 包含：唯一 ID（call_ + UUID 前24位）、类型 "function"、序号、函数名和序列化后的参数 JSON。
func BuildToolCall(name string, arguments map[string]any, index int) map[string]any {
	argsBytes, _ := sonic.MarshalString(arguments)
	return map[string]any{
		"id":    "call_" + uuid.New().String()[:24],
		"type":  "function",
		"index": index,
		"function": map[string]any{
			"name":      name,
			"arguments": argsBytes,
		},
	}
}

// ExtractCallsFromPayload 从 JSON 负载对象中提取工具调用列表。
// payload 应为包含 "tool_calls" 数组的 map，每个元素包含 "name" 和 "arguments" 字段。
// startIndex 指定第一个调用的序号起点，后续调用序号递增。
// 返回符合 OpenAI 格式的工具调用对象切片；如果 payload 格式不正确或无有效调用，返回 nil。
func ExtractCallsFromPayload(payload any, startIndex int) []map[string]any {
	m, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	calls, ok := m["tool_calls"].([]any)
	if !ok {
		return nil
	}
	var toolCalls []map[string]any
	for _, c := range calls {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		name := strings.TrimSpace(fmt.Sprintf("%v", call["name"]))
		if name == "" {
			continue
		}
		arguments := NormalizeArguments(call["arguments"])
		toolCalls = append(toolCalls, BuildToolCall(name, arguments, startIndex+len(toolCalls)))
	}
	return toolCalls
}

// TryParseJSON 尝试将文本解析为 JSON 对象。
// 首先尝试直接解析，如果失败则尝试修复尾随逗号（LLM 输出常见问题）后再次解析。
// 解析成功返回任意类型的 JSON 值，失败返回 nil。
func TryParseJSON(text string) any {
	stripped := strings.TrimSpace(text)
	if stripped == "" {
		return nil
	}
	var v any
	if err := sonic.UnmarshalString(stripped, &v); err == nil {
		return v
	}
	// 修复尾随逗号
	repaired := trailingCommaRE.ReplaceAllString(stripped, "$1")
	if err := sonic.UnmarshalString(repaired, &v); err == nil {
		return v
	}
	return nil
}

// trailingCommaRE 匹配 JSON 中 } 或 ] 前的尾随逗号，用于 FixCommonJsonErrors 修复 LLM 输出
var trailingCommaRE = regexp.MustCompile(`,\s*([\]}])`)

// ExtractFirstJSONObject 从 text 的 start 位置开始，提取第一个花括号平衡的 {...} JSON 对象。
// 使用深度计数器跟踪大括号嵌套，正确处理字符串内的转义字符和引号。
// 返回两个值：完整的 JSON 对象文本，以及其在原文中的 [start, end) 范围。
// 如果找不到完整的 JSON 对象，返回 ("", [-1, -1])。
func ExtractFirstJSONObject(text string, start int) (string, [2]int) {
	pos := indexFrom(text, "{", start)
	if pos == -1 {
		return "", [2]int{-1, -1}
	}
	depth := 0
	inString := false
	escape := false
	for i := pos; i < len(text); i++ {
		c := text[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' {
			escape = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return text[pos : i+1], [2]int{pos, i + 1}
			}
		}
	}
	return "", [2]int{-1, -1}
}

// ExtractCallArgsBalanced 从 [call:name]...[/call] 格式的调用项中提取平衡的 JSON 参数。
// 从 text 开头开始，找到第一个 '{' 并跟踪大括号深度，提取完整的 JSON 对象。
// 返回两个值：JSON 参数字符串，以及参数结束后的文本位置。
// 如果找不到有效 JSON，返回 ("", -1)。
func ExtractCallArgsBalanced(text string) (string, int) {
	pos := indexFrom(text, "{", 0)
	if pos == -1 {
		return "", -1
	}
	depth := 0
	inString := false
	escape := false
	for i := pos; i < len(text); i++ {
		c := text[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' {
			escape = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return text[pos : i+1], i + 1
			}
		}
	}
	return "", -1
}

// FixCommonJsonErrors 修复 LLM 输出中常见的 JSON 格式错误。
// 目前主要修复：移除 } 或 ] 前的尾随逗号（例如 {"a":1,} → {"a":1}）。
// 如果输入为空，直接返回原值。
func FixCommonJsonErrors(text string) string {
	if text == "" {
		return text
	}
	// 移除 } 或 ] 前的尾随逗号
	fixed := trailingCommaRE.ReplaceAllString(text, "$1")
	return fixed
}

// RemoveSpans 从文本中移除指定的字符范围，并清理结果中的多余空行和工具结果块。
// spans 是需要移除的 [start, end) 范围列表，会按起始位置排序后依次移除。
// 如果 spans 为空，仍会移除所有 ```tool_result 块并压缩连续空行。
// trimOuterWhitespace 为 true 时，还会去除结果首尾的空白字符。
func RemoveSpans(text string, spans [][2]int, trimOuterWhitespace bool) string {
	if len(spans) == 0 {
		cleaned := ToolResultFencePattern.ReplaceAllString(text, "")
		cleaned = multiNewlineRE.ReplaceAllString(cleaned, "\n\n")
		if trimOuterWhitespace {
			return strings.TrimSpace(cleaned)
		}
		return cleaned
	}

	// 按起始位置排序
	sortedSpans := make([][2]int, len(spans))
	copy(sortedSpans, spans)
	// 简单插入排序
	for i := 1; i < len(sortedSpans); i++ {
		for j := i; j > 0 && sortedSpans[j-1][0] > sortedSpans[j][0]; j-- {
			sortedSpans[j-1], sortedSpans[j] = sortedSpans[j], sortedSpans[j-1]
		}
	}

	var parts []string
	cursor := 0
	for _, span := range sortedSpans {
		if span[0] < cursor {
			continue
		}
		parts = append(parts, text[cursor:span[0]])
		cursor = span[1]
	}
	parts = append(parts, text[cursor:])
	cleaned := strings.Join(parts, "")
	cleaned = ToolResultFencePattern.ReplaceAllString(cleaned, "")
	cleaned = multiNewlineRE.ReplaceAllString(cleaned, "\n\n")
	if trimOuterWhitespace {
		return strings.TrimSpace(cleaned)
	}
	return cleaned
}

// multiNewlineRE 匹配连续三个或更多换行符，用于 RemoveSpans 压缩多余空行
var multiNewlineRE = regexp.MustCompile(`\n{3,}`)

// ExtractFencedBlocks 从文本中提取所有用 ```tool_code 代码块或 [function_calls] 标签包裹的工具调用。
// 支持两种格式：[function_calls][call:name]{...}[/call][/function_calls] 和 ```tool_code\n{...}\n```。
// 返回两个值：需要从原文中移除的字符范围列表，以及解析出的工具调用对象列表。
// 该函数会尝试解析 JSON，如果解析失败则尝试提取第一个平衡的 JSON 对象作为回退。
func ExtractFencedBlocks(text string) ([][2]int, []map[string]any) {
	var spans [][2]int
	var toolCalls []map[string]any

	// 首先尝试解析 [function_calls]...[/function_calls] 块
	fcMatches := FunctionCallsFencePattern.FindAllStringSubmatchIndex(text, -1)
	for _, m := range fcMatches {
		bodyStart, bodyEnd := m[2], m[3]
		body := text[bodyStart:bodyEnd]

		cursor := 0
		for cursor < len(body) {
			callStart := CallItemStartPattern.FindAllStringSubmatchIndex(body[cursor:], 1)
			if callStart == nil {
				break
			}
			// callStart[0][2]=name起始, callStart[0][3]=name结束
			nameStart := cursor + callStart[0][2]
			nameEnd := cursor + callStart[0][3]
			name := strings.TrimSpace(body[nameStart:nameEnd])
			argsStartPos := cursor + callStart[0][1]

			argsStr, argsEnd := ExtractCallArgsBalanced(body[argsStartPos:])
			if argsStr == "" {
				// 没有找到 JSON，尝试找 [/call] 作为回退
				closeTag := strings.Index(body[argsStartPos:], "[/call]")
				if closeTag == -1 {
					break
				}
				cursor = argsStartPos + closeTag + len("[/call]")
				continue
			}

			if name == "" {
				cursor = argsStartPos + argsEnd
				continue
			}

			argsDict := TryParseJSON(argsStr)
			if argsDict == nil {
				fixedArgs := FixCommonJsonErrors(argsStr)
				argsDict = TryParseJSON(fixedArgs)
			}
			if argsDict == nil {
				argsDict = map[string]any{}
			}
			argsMap, ok := argsDict.(map[string]any)
			if !ok {
				argsMap = map[string]any{"value": argsDict}
			}

			tc := BuildToolCall(name, argsMap, len(toolCalls))
			toolCalls = append(toolCalls, tc)

			// 查找 [/call] 结束标签
			endPos := argsStartPos + argsEnd
			closeTagIdx := strings.Index(body[endPos:], "[/call]")
			if closeTagIdx != -1 {
				cursor = endPos + closeTagIdx + len("[/call]")
			} else {
				cursor = endPos
			}
		}

		// 始终消费整个 fence
		spans = append(spans, [2]int{m[0], m[1]})
	}

	// 然后解析 ```tool_code 块（旧格式支持）
	matches := ToolCodeFencePattern.FindAllStringSubmatchIndex(text, -1)
	for _, m := range matches {
		// m[0]=整体起始, m[1]=整体结束, m[2]=body起始, m[3]=body结束
		bodyStart, bodyEnd := m[2], m[3]
		body := text[bodyStart:bodyEnd]
		payload := TryParseJSON(body)

		// 回退：尝试提取第一个平衡的 JSON 对象
		if payload == nil {
			objText, _ := ExtractFirstJSONObject(body, 0)
			if objText != "" {
				payload = TryParseJSON(objText)
			}
		}

		if payload != nil {
			blockCalls := ExtractCallsFromPayload(payload, len(toolCalls))
			if len(blockCalls) > 0 {
				toolCalls = append(toolCalls, blockCalls...)
			}
		}
		spans = append(spans, [2]int{m[0], m[1]})
	}
	return spans, toolCalls
}

// ExtractBareJSONBlocks 作为回退策略，扫描文本中裸露的 {"tool_calls":...} JSON 对象。
// 跳过所有在 ``` 代码块内部的匹配，只处理代码块外的裸 JSON。
// 找到后提取完整的 JSON 对象，解析并构建工具调用。
// 返回需要移除的字符范围列表，以及解析出的工具调用列表。
func ExtractBareJSONBlocks(text string) ([][2]int, []map[string]any) {
	fenceMatches := AnyFencePattern.FindAllStringIndex(text, -1)
	var fenceSpans [][2]int
	for _, m := range fenceMatches {
		fenceSpans = append(fenceSpans, [2]int{m[0], m[1]})
	}

	var spans [][2]int
	var toolCalls []map[string]any
	cursor := 0
	for cursor < len(text) {
		loc := BareToolCallsOpenPattern.FindStringIndex(text[cursor:])
		if loc == nil {
			break
		}
		matchStart := cursor + loc[0]
		// 跳过任何在 ``` 代码块内的匹配
		inside := false
		for _, fs := range fenceSpans {
			if matchStart >= fs[0] && matchStart < fs[1] {
				inside = true
				break
			}
		}
		if inside {
			cursor = matchStart + 1
			continue
		}

		objText, span := ExtractFirstJSONObject(text, matchStart)
		if objText == "" {
			cursor = matchStart + 1
			continue
		}
		payload := TryParseJSON(objText)
		if payload == nil {
			cursor = span[1]
			continue
		}
		blockCalls := ExtractCallsFromPayload(payload, len(toolCalls))
		if len(blockCalls) > 0 {
			toolCalls = append(toolCalls, blockCalls...)
			spans = append(spans, span)
		}
		cursor = span[1]
	}
	return spans, toolCalls
}

// SalvageIncompleteFencedBlocks 从不完整的 ```tool_code 块中挽救工具调用。
// "不完整"指有 ```tool_code 开始标记但没有 ``` 闭合标记的情况（常见于流式响应被截断）。
// 从不完整块的剩余文本中提取第一个平衡的 JSON 对象，解析其中的工具调用。
// existingCount 参数用于计算正确的调用序号偏移。
// 返回需要移除的范围列表和挽救出的工具调用列表。
func SalvageIncompleteFencedBlocks(text string, existingCount int) ([][2]int, []map[string]any) {
	var spans [][2]int
	var toolCalls []map[string]any

	matches := ToolCodeStartPattern.FindAllStringIndex(text, -1)
	for _, m := range matches {
		bodyStart := m[1]
		// 跳过完整块（有闭合标记）
		_, closeLen := FindFenceClose(text, bodyStart)
		if closeLen > 0 {
			continue
		}
		// 不完整块 - 尝试从 body 提取平衡的 JSON 对象
		body := text[bodyStart:]
		objText, objSpan := ExtractFirstJSONObject(body, 0)
		if objText == "" {
			continue
		}
		payload := TryParseJSON(objText)
		if payload == nil {
			continue
		}
		blockCalls := ExtractCallsFromPayload(payload, existingCount+len(toolCalls))
		if len(blockCalls) > 0 {
			toolCalls = append(toolCalls, blockCalls...)
			endPos := bodyStart + len(body)
			if objSpan[1] > 0 {
				endPos = bodyStart + objSpan[1]
			}
			spans = append(spans, [2]int{m[0], endPos})
		}
	}
	return spans, toolCalls
}

// ParseToolCallsFromText 从完整文本中解析工具调用，是主要的文本解析入口函数。
// 按优先级尝试三种策略：
// 1. 提取 ```tool_code 和 [function_calls] 块中的工具调用
// 2. 如果没有找到，尝试挽救不完整的 ```tool_code 块
// 3. 如果仍然没有，回退到裸 JSON 对象检测
// 返回清理后的文本（移除所有工具调用块）和工具调用列表。
// 如果文本中检测到工具相关标记但无法解析，会记录警告日志。
func ParseToolCallsFromText(text string) (string, []map[string]any) {
	if text == "" {
		return "", nil
	}
	spans, toolCalls := ExtractFencedBlocks(text)

	if len(toolCalls) == 0 {
		// 尝试挽救不完整块
		incSpans, incCalls := SalvageIncompleteFencedBlocks(text, 0)
		if len(incCalls) > 0 {
			toolParserLogger.Debug("parse_tool_calls_from_text: salvaged tool calls from incomplete block(s)", "count", len(incCalls))
			allSpans := append(spans, incSpans...)
			return RemoveSpans(text, allSpans, true), incCalls
		}
		// 回退到裸 JSON 检测
		bareSpans, bareCalls := ExtractBareJSONBlocks(text)
		if len(bareCalls) > 0 {
			toolParserLogger.Debug("parse_tool_calls_from_text: bare-JSON fallback recovered tool calls", "count", len(bareCalls), "text_len", len(text))
			return RemoveSpans(text, bareSpans, true), bareCalls
		}

		// 仅当文本看起来像是要发出工具调用时才发出警告
		lowered := strings.ToLower(text)
		looksTooly := strings.Contains(lowered, "tool_calls") ||
			strings.Contains(lowered, "tool_code") ||
			strings.Contains(lowered, "tool_result") ||
			strings.Contains(lowered, "```tool") ||
			BareToolCallsOpenPattern.MatchString(text)
		if looksTooly {
			toolParserLogger.Warn("parse_tool_calls_from_text: tool-like marker found but no tool calls parsed",
				"text_len", len(text), "text_start", truncateForLog(text, 80))
		} else {
			toolParserLogger.Debug("parse_tool_calls_from_text: no tool calls found (non-tool content)", "text_len", len(text))
		}
	}
	return RemoveSpans(text, spans, true), toolCalls
}

// truncateForLog 将文本截断到指定最大长度，用于日志输出避免过长。
// 如果文本长度未超过限制，返回原文本。
func truncateForLog(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen]
}

// FindPartialMarker 检查文本尾部是否存在部分的标记（```tool_code、```tool_result、[function_calls] 或 [/function_calls]）。
// "部分"指标记被截断，例如 "```tool_co" 或 "[/function_ca"。
// 从最长可能的标记开始，逐字符减少进行后缀匹配，返回部分标记在文本中的起始位置。
// 找不到任何部分标记返回 -1。用于流式解析中判断是否需要等待更多数据。
func FindPartialMarker(text string) int {
	if text == "" {
		return -1
	}
	lowered := strings.ToLower(text)
	for _, marker := range []string{toolCodeMarker, toolResultMarker, functionCallsMarker, functionCallsEndMarker} {
		maxOverlap := len(marker)
		if len(text) < maxOverlap {
			maxOverlap = len(text)
		}
		for size := maxOverlap; size > 0; size-- {
			if strings.HasSuffix(lowered, strings.ToLower(marker[:size])) {
				return len(text) - size
			}
		}
	}
	return -1
}

// StreamingToolParser 是流式文本解析器，用于在流式响应中逐步识别和提取工具调用。
// 工作流程：通过 Consume 方法逐块输入文本，内部调用 SplitStreamText 进行实时解析。
// 一旦识别到完整的工具调用，_toolCallCompleted 标记为 true，后续输入不再产生可见文本。
// PendingText 缓存尚未完成解析的文本（例如正在等待闭合标记的代码块）。
type StreamingToolParser struct {
	PendingText        string
	ToolCalls          []map[string]any
	_toolCallCompleted bool
}

// NewStreamingToolParser 创建并返回一个新的流式工具调用解析器实例。
func NewStreamingToolParser() *StreamingToolParser {
	return &StreamingToolParser{}
}

// Consume 消费一个文本块，返回该块中可见的文本部分（不含工具调用标记）。
// 如果之前已经完成了一个工具调用，后续块直接返回空字符串。
// 内部将新块追加到 PendingText，调用 SplitStreamText 进行解析，
// 将解析出的工具调用追加到 ToolCalls，未完成的部分保留在 PendingText 等待下一次输入。
func (p *StreamingToolParser) Consume(chunk string) string {
	if chunk == "" {
		return ""
	}
	if p._toolCallCompleted {
		return ""
	}
	p.PendingText += chunk
	visible, remainder, parsedCalls := SplitStreamText(p.PendingText, false)
	p.PendingText = remainder
	p.ToolCalls = append(p.ToolCalls, parsedCalls...)
	if len(parsedCalls) > 0 {
		p._toolCallCompleted = true
	}
	return visible
}

// Flush 刷新解析器中剩余的所有文本，在流式响应结束时调用。
// 返回最终的可见文本和完整的工具调用列表。
// 如果还有未完成的工具调用，直接返回已收集的结果。
// 如果剩余文本中仍有未完成的代码块，尝试从不完整块中挽救工具调用。
// 如果挽救失败，将剩余文本作为可见文本返回给用户。
func (p *StreamingToolParser) Flush() (string, []map[string]any) {
	if p._toolCallCompleted {
		return "", p.ToolCalls
	}
	visible, remainder, parsedCalls := SplitStreamText(p.PendingText, true)
	p.ToolCalls = append(p.ToolCalls, parsedCalls...)
	if len(parsedCalls) > 0 {
		p._toolCallCompleted = true
	}

	tail := ""
	if remainder != "" && !p._toolCallCompleted {
		salvaged := SalvageIncompleteBlock(remainder, p.ToolCalls)
		if salvaged != nil {
			p.PendingText = ""
			p._toolCallCompleted = true
		} else {
			tail = remainder
			p.PendingText = ""
		}
	} else {
		p.PendingText = ""
	}
	return strings.TrimSpace(visible + tail), p.ToolCalls
}

// IsToolCallCompleted 返回解析器是否已经识别到至少一个完整的工具调用。
// 一旦返回 true，后续 Consume 调用将不再产生可见文本。
func (p *StreamingToolParser) IsToolCallCompleted() bool {
	return p._toolCallCompleted
}
func (p *StreamingToolParser) SetToolCallCompleted() {
	p._toolCallCompleted = true
}

// SplitStreamText 将文本拆分为三部分：可见文本、剩余未完成文本、和已解析的工具调用。
// 这是流式解析的核心函数，按以下逻辑处理：
// 1. 扫描 text 中的 ```tool_code、```tool_result 和 [function_calls] 标记
// 2. tool_result 块被直接跳过（跳过整个块）
// 3. tool_code 块：如果找到闭合标记则解析 JSON 提取工具调用；如果未闭合且 final=true 则尝试挽救
// 4. [function_calls] 块：查找 [/function_calls] 结束标记，提取其中的 [call:name]{json}[/call] 工具调用
// 5. 如果 final=false 且检测到部分标记（如 "```tool_co" 或 "[/function_ca"），将其放入 remainder 等待更多数据
// 返回 (可见文本部分, 需要缓存的剩余文本, 本次解析出的工具调用列表)
func SplitStreamText(text string, final bool) (string, string, []map[string]any) {
	var visibleParts []string
	var toolCalls []map[string]any
	cursor := 0

	for cursor < len(text) {
		// 找到下一个 tool_code / tool_result / [function_calls] 标记
		startLoc := findFrom(cursor, text, ToolCodeStartPattern)
		resultLoc := findFrom(cursor, text, ToolResultStartPattern)
		fcStartLoc := findFrom(cursor, text, FunctionCallsStartPattern)

		nextMarkerPos := -1
		nextMarkerKind := ""
		if startLoc != -1 {
			nextMarkerPos = startLoc
			nextMarkerKind = "tool_code"
		}
		if resultLoc != -1 && (nextMarkerPos == -1 || resultLoc < nextMarkerPos) {
			nextMarkerPos = resultLoc
			nextMarkerKind = "tool_result"
		}
		if fcStartLoc != -1 && (nextMarkerPos == -1 || fcStartLoc < nextMarkerPos) {
			nextMarkerPos = fcStartLoc
			nextMarkerKind = "function_calls"
		}

		if nextMarkerPos == -1 {
			// 没有更多标记 - 输出到尾部部分标记为止
			var partialPos int
			if !final {
				partialPos = FindPartialMarker(text[cursor:])
			} else {
				partialPos = -1
			}
			if partialPos != -1 {
				visibleParts = append(visibleParts, text[cursor:cursor+partialPos])
				remainder := text[cursor+partialPos:]
				return strings.Join(visibleParts, ""), remainder, toolCalls
			}
			visibleParts = append(visibleParts, text[cursor:])
			return strings.Join(visibleParts, ""), "", toolCalls
		}

		// 输出标记前的文本
		if nextMarkerPos > cursor {
			visibleParts = append(visibleParts, text[cursor:nextMarkerPos])
		}

		if nextMarkerKind == "function_calls" {
			// [function_calls] 块：查找 [/function_calls] 结束标记
			fcEndLoc := findFrom(nextMarkerPos, text, FunctionCallsEndPattern)
			if fcEndLoc == -1 {
				// 不完整的 [function_calls] 块
				if final {
					// 最终模式：尝试从不完整块中提取工具调用
					fcBody := text[nextMarkerPos:]
					fcCalls := extractFunctionCallsFromText(fcBody, len(toolCalls))
					if len(fcCalls) > 0 {
						toolCalls = append(toolCalls, fcCalls...)
						cursor = len(text)
						continue
					}
					visibleParts = append(visibleParts, fcBody)
					return strings.Join(visibleParts, ""), "", toolCalls
				}
				// 保留整个块等待更多数据
				remainder := text[nextMarkerPos:]
				return strings.Join(visibleParts, ""), remainder, toolCalls
			}
			// 完整的 [function_calls] 块
			fcBlock := text[nextMarkerPos : fcEndLoc+len(functionCallsEndMarker)]
			fcCalls := extractFunctionCallsFromText(fcBlock, len(toolCalls))
			if len(fcCalls) > 0 {
				toolCalls = append(toolCalls, fcCalls...)
			}
			cursor = fcEndLoc + len(functionCallsEndMarker)
			continue
		}

		// 找到标记的结束位置
		var markerEnd int
		if nextMarkerKind == "tool_code" {
			loc := ToolCodeStartPattern.FindStringIndex(text[cursor:])
			markerEnd = cursor + loc[1]
		} else {
			loc := ToolResultStartPattern.FindStringIndex(text[cursor:])
			markerEnd = cursor + loc[1]
		}

		if nextMarkerKind == "tool_result" {
			// 查找 tool_result 块的闭合标记
			closePos, closeLen := FindFenceClose(text, markerEnd)
			if closePos == -1 {
				remainder := text[nextMarkerPos:]
				return strings.Join(visibleParts, ""), remainder, toolCalls
			}
			cursor = closePos + closeLen
			continue
		}

		// tool_code 块：查找闭合标记
		closePos, closeLen := FindFenceClose(text, markerEnd)
		if closePos == -1 {
			// 不完整块
			if final {
				body := text[markerEnd:]
				payload := TryParseJSON(body)
				if payload == nil {
					payload = map[string]any{}
				}
				salvaged := ExtractCallsFromPayload(payload, len(toolCalls))
				if len(salvaged) > 0 {
					toolCalls = append(toolCalls, salvaged...)
					cursor = len(text)
					continue
				}
				// 挽救失败 - 输出部分块作为可见文本
				visibleParts = append(visibleParts, text[nextMarkerPos:])
				return strings.Join(visibleParts, ""), "", toolCalls
			}
			// 保留整个块等待更多数据
			remainder := text[nextMarkerPos:]
			return strings.Join(visibleParts, ""), remainder, toolCalls
		}

		bodyEnd := closePos
		blockEnd := closePos + closeLen
		body := text[markerEnd:bodyEnd]
		payload := TryParseJSON(body)
		if payload != nil {
			blockCalls := ExtractCallsFromPayload(payload, len(toolCalls))
			if len(blockCalls) > 0 {
				toolCalls = append(toolCalls, blockCalls...)
			}
			cursor = blockEnd
			continue
		}

		// 解析失败 - 输出块作为可见文本
		visibleParts = append(visibleParts, text[nextMarkerPos:blockEnd])
		cursor = blockEnd
	}

	return strings.Join(visibleParts, ""), "", toolCalls
}

// extractFunctionCallsFromText 从 [function_calls]...[/function_calls] 文本块中提取工具调用。
// 解析其中的 [call:name]{json}[/call] 项，构建标准的工具调用对象。
// startIndex 指定第一个调用的序号起点。
// 返回解析出的工具调用列表；如果没有有效调用，返回 nil。
func extractFunctionCallsFromText(text string, startIndex int) []map[string]any {
	m := FunctionCallsFencePattern.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	body := m[1]

	var calls []map[string]any
	cursor := 0
	for cursor < len(body) {
		callStart := CallItemStartPattern.FindAllStringSubmatchIndex(body[cursor:], 1)
		if callStart == nil {
			break
		}
		nameStart := cursor + callStart[0][2]
		nameEnd := cursor + callStart[0][3]
		name := strings.TrimSpace(body[nameStart:nameEnd])
		argsStartPos := cursor + callStart[0][1]

		argsStr, argsEnd := ExtractCallArgsBalanced(body[argsStartPos:])
		if argsStr == "" {
			closeTag := strings.Index(body[argsStartPos:], "[/call]")
			if closeTag == -1 {
				break
			}
			cursor = argsStartPos + closeTag + len("[/call]")
			continue
		}

		if name == "" {
			cursor = argsStartPos + argsEnd
			continue
		}

		argsDict := TryParseJSON(argsStr)
		if argsDict == nil {
			fixedArgs := FixCommonJsonErrors(argsStr)
			argsDict = TryParseJSON(fixedArgs)
		}
		if argsDict == nil {
			argsDict = map[string]any{}
		}
		argsMap, ok := argsDict.(map[string]any)
		if !ok {
			argsMap = map[string]any{"value": argsDict}
		}

		tc := BuildToolCall(name, argsMap, startIndex+len(calls))
		calls = append(calls, tc)

		endPos := argsStartPos + argsEnd
		closeTagIdx := strings.Index(body[endPos:], "[/call]")
		if closeTagIdx != -1 {
			cursor = endPos + closeTagIdx + len("[/call]")
		} else {
			cursor = endPos
		}
	}
	return calls
}

// SalvageIncompleteBlock 尝试从不完整的剩余文本中挽救工具调用。
// 通常在流式响应结束时（Flush）或 final=true 的 SplitStreamText 中调用。
// 支持三种格式的挽救：
// 1. 有 ```tool_code 开始标记但没有闭合标记的块：提取标记后的 JSON
// 2. 有 [function_calls] 开始标记但没有 [/function_calls] 结束标记的块：提取 [call:name]{json}[/call]
// 3. 裸 JSON 对象（无代码块包裹）
// 返回挽救出的工具调用列表，如果无法挽救返回 nil。
func SalvageIncompleteBlock(text string, existingToolCalls []map[string]any) []map[string]any {
	// 先尝试 [function_calls] 格式
	fcLoc := FunctionCallsStartPattern.FindStringIndex(text)
	if fcLoc != nil {
		fcBody := text[fcLoc[0]:]
		fcCalls := extractFunctionCallsFromText(fcBody, len(existingToolCalls))
		if len(fcCalls) > 0 {
			existingToolCalls = append(existingToolCalls, fcCalls...)
			toolParserLogger.Debug("Salvaged tool call(s) from incomplete [function_calls] remainder", "count", len(fcCalls))
			return fcCalls
		}
	}

	// 再尝试 ```tool_code 格式
	loc := ToolCodeStartPattern.FindStringIndex(text)
	if loc == nil {
		// 也许是一个裸 JSON 对象
		objText, _ := ExtractFirstJSONObject(text, 0)
		if objText == "" {
			return nil
		}
		payload := TryParseJSON(objText)
		if payload == nil {
			return nil
		}
		blockCalls := ExtractCallsFromPayload(payload, len(existingToolCalls))
		if len(blockCalls) > 0 {
			existingToolCalls = append(existingToolCalls, blockCalls...)
			toolParserLogger.Debug("Salvaged tool call(s) from incomplete bare JSON remainder", "count", len(blockCalls))
			return blockCalls
		}
		return nil
	}

	body := text[loc[1]:]
	objText, _ := ExtractFirstJSONObject(body, 0)
	if objText == "" {
		objText = body
	}
	payload := TryParseJSON(objText)
	if payload == nil {
		return nil
	}
	blockCalls := ExtractCallsFromPayload(payload, len(existingToolCalls))
	if len(blockCalls) > 0 {
		existingToolCalls = append(existingToolCalls, blockCalls...)
		toolParserLogger.Debug("Salvaged tool call(s) from incomplete tool_code remainder", "count", len(blockCalls))
		return blockCalls
	}
	return nil
}

// findFrom 从 text 的 startFrom 位置开始，查找正则表达式 re 的第一次匹配位置。
// 返回匹配在原文中的绝对起始位置；如果未找到或 startFrom 超出文本范围，返回 -1。
// 用于在流式解析中从指定偏移位置搜索下一个标记。
func findFrom(startFrom int, text string, re *regexp.Regexp) int {
	if startFrom >= len(text) {
		return -1
	}
	loc := re.FindStringIndex(text[startFrom:])
	if loc == nil {
		return -1
	}
	return startFrom + loc[0]
}

// ResetLogger 将工具解析器的日志记录器重置为默认值。
// 主要用于测试场景，确保测试间日志状态隔离。
func ResetLogger() {
	toolParserLogger = slog.Default().With("logger", "glm2api.tool_parser")
}

// SetLogger 设置自定义的日志记录器，替换默认的工具解析器日志记录器。
// 传入 nil 不会产生任何效果，保留现有的日志记录器不变。
func SetLogger(l *slog.Logger) {
	if l != nil {
		toolParserLogger = l
	}
}

// time.Now 引用确保 time 包被使用（避免未使用的导入编译错误）
var _ = time.Now
