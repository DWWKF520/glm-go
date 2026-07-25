package tools

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

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

	toolCodeMarker    = "```tool_code"
	toolResultMarker  = "```tool_result"
	fenceCloseNewline = "\n```"
	fenceCloseBare    = "```"
)

var toolParserLogger = slog.Default().With("logger", "glm2api.tool_parser")

// FindFenceClose 查找最早的 ``` 闭合标记
// 返回 (position, length)，length 为 4 表示 \n``` 形式，3 表示 ``` 形式
// 找不到返回 (-1, 0)
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

// indexFrom 类似 strings.Index 但从指定位置开始
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

// IsAllowedToolName 判断工具名是否允许
func IsAllowedToolName(toolName string, allowedToolNames map[string]bool) bool {
	if BlockedNativeToolNames[toolName] {
		return false
	}
	if allowedToolNames == nil {
		return true
	}
	return allowedToolNames[toolName]
}

// BuildToolCall 构建工具调用对象
func BuildToolCall(name string, arguments map[string]any, index int) map[string]any {
	argsBytes, _ := json.Marshal(arguments)
	return map[string]any{
		"id":    "call_" + uuid.New().String()[:24],
		"type":  "function",
		"index": index,
		"function": map[string]any{
			"name":      name,
			"arguments": string(argsBytes),
		},
	}
}

// ExtractCallsFromPayload 从负载提取工具调用
func ExtractCallsFromPayload(payload any, allowedToolNames map[string]bool, startIndex int) []map[string]any {
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
		if name == "" || !IsAllowedToolName(name, allowedToolNames) {
			continue
		}
		arguments := NormalizeArguments(call["arguments"])
		toolCalls = append(toolCalls, BuildToolCall(name, arguments, startIndex+len(toolCalls)))
	}
	return toolCalls
}

// TryParseJSON 尝试解析 JSON，支持尾随逗号修复
func TryParseJSON(text string) any {
	stripped := strings.TrimSpace(text)
	if stripped == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(stripped), &v); err == nil {
		return v
	}
	// 修复尾随逗号
	repaired := trailingCommaRE.ReplaceAllString(stripped, "$1")
	if err := json.Unmarshal([]byte(repaired), &v); err == nil {
		return v
	}
	return nil
}

var trailingCommaRE = regexp.MustCompile(`,\s*([\]}])`)

// ExtractFirstJSONObject 提取第一个平衡的 {...} JSON 对象
// 返回 (对象文本, 范围 [start, end))，找不到返回 ("", nil)
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

// ExtractCallArgsBalanced 从 [call:name]...[/call] 项中提取平衡的 JSON 参数
// 返回 (argsJSON, endPosition)，找不到返回 ("", -1)
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

// FixCommonJsonErrors 修复常见的 JSON 错误
func FixCommonJsonErrors(text string) string {
	if text == "" {
		return text
	}
	// 移除 } 或 ] 前的尾随逗号
	fixed := trailingCommaRE.ReplaceAllString(text, "$1")
	return fixed
}

// RemoveSpans 从文本中移除指定范围，并清理结果
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

var multiNewlineRE = regexp.MustCompile(`\n{3,}`)

// ExtractFencedBlocks 从 [function_calls] 和 ```tool_code 块中提取工具调用
// 返回 (spans, toolCalls)
func ExtractFencedBlocks(text string, allowedToolNames map[string]bool) ([][2]int, []map[string]any) {
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

			if name == "" || !IsAllowedToolName(name, allowedToolNames) {
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
			blockCalls := ExtractCallsFromPayload(payload, allowedToolNames, len(toolCalls))
			if len(blockCalls) > 0 {
				toolCalls = append(toolCalls, blockCalls...)
			}
		}
		spans = append(spans, [2]int{m[0], m[1]})
	}
	return spans, toolCalls
}

// ExtractBareJSONBlocks 回退：扫描裸 {"tool_calls":...} JSON 对象
func ExtractBareJSONBlocks(text string, allowedToolNames map[string]bool) ([][2]int, []map[string]any) {
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
		blockCalls := ExtractCallsFromPayload(payload, allowedToolNames, len(toolCalls))
		if len(blockCalls) > 0 {
			toolCalls = append(toolCalls, blockCalls...)
			spans = append(spans, span)
		}
		cursor = span[1]
	}
	return spans, toolCalls
}

// SalvageIncompleteFencedBlocks 从不完整的 ```tool_code 块中挽救工具调用
func SalvageIncompleteFencedBlocks(text string, allowedToolNames map[string]bool, existingCount int) ([][2]int, []map[string]any) {
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
		blockCalls := ExtractCallsFromPayload(payload, allowedToolNames, existingCount+len(toolCalls))
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

// ParseToolCallsFromText 从文本中解析工具调用
// 返回 (清理后的文本, 工具调用列表)
func ParseToolCallsFromText(text string, allowedToolNames map[string]bool) (string, []map[string]any) {
	if text == "" {
		return "", nil
	}
	spans, toolCalls := ExtractFencedBlocks(text, allowedToolNames)

	if len(toolCalls) == 0 {
		// 尝试挽救不完整块
		incSpans, incCalls := SalvageIncompleteFencedBlocks(text, allowedToolNames, 0)
		if len(incCalls) > 0 {
			toolParserLogger.Debug("parse_tool_calls_from_text: salvaged tool calls from incomplete block(s)", "count", len(incCalls))
			allSpans := append(spans, incSpans...)
			return RemoveSpans(text, allSpans, true), incCalls
		}
		// 回退到裸 JSON 检测
		bareSpans, bareCalls := ExtractBareJSONBlocks(text, allowedToolNames)
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

// truncateForLog 截断文本用于日志
func truncateForLog(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen]
}

// FindPartialMarker 返回文本尾部部分 ```tool_code / ```tool_result 标记的起始位置
// 找不到返回 -1
func FindPartialMarker(text string) int {
	if text == "" {
		return -1
	}
	lowered := strings.ToLower(text)
	for _, marker := range []string{toolCodeMarker, toolResultMarker} {
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

// StreamingToolParser 流式工具调用解析器
type StreamingToolParser struct {
	PendingText       string
	ToolCalls         []map[string]any
	AllowedToolNames  map[string]bool
	_toolCallCompleted bool
}

// NewStreamingToolParser 创建流式工具调用解析器
func NewStreamingToolParser() *StreamingToolParser {
	return &StreamingToolParser{}
}

// Consume 消费一个块，返回可见文本
func (p *StreamingToolParser) Consume(chunk string) string {
	if chunk == "" {
		return ""
	}
	if p._toolCallCompleted {
		return ""
	}
	p.PendingText += chunk
	visible, remainder, parsedCalls := SplitStreamText(p.PendingText, p.AllowedToolNames, false)
	p.PendingText = remainder
	p.ToolCalls = append(p.ToolCalls, parsedCalls...)
	if len(parsedCalls) > 0 {
		p._toolCallCompleted = true
	}
	return visible
}

// Flush 刷新剩余文本，返回 (可见文本, 工具调用列表)
func (p *StreamingToolParser) Flush() (string, []map[string]any) {
	if p._toolCallCompleted {
		return "", p.ToolCalls
	}
	visible, remainder, parsedCalls := SplitStreamText(p.PendingText, p.AllowedToolNames, true)
	p.ToolCalls = append(p.ToolCalls, parsedCalls...)
	if len(parsedCalls) > 0 {
		p._toolCallCompleted = true
	}

	tail := ""
	if remainder != "" && !p._toolCallCompleted {
		salvaged := SalvageIncompleteBlock(remainder, p.AllowedToolNames, p.ToolCalls)
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

// IsToolCallCompleted 返回工具调用是否已完成
func (p *StreamingToolParser) IsToolCallCompleted() bool {
	return p._toolCallCompleted
}

// SplitStreamText 将文本拆分为 (可见文本, 剩余文本, 工具调用)
func SplitStreamText(text string, allowedToolNames map[string]bool, final bool) (string, string, []map[string]any) {
	var visibleParts []string
	var toolCalls []map[string]any
	cursor := 0

	for cursor < len(text) {
		// 找到下一个 tool_code / tool_result 标记
		startLoc := findFrom(cursor, text, ToolCodeStartPattern)
		resultLoc := findFrom(cursor, text, ToolResultStartPattern)

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
				salvaged := ExtractCallsFromPayload(payload, allowedToolNames, len(toolCalls))
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
			blockCalls := ExtractCallsFromPayload(payload, allowedToolNames, len(toolCalls))
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

// SalvageIncompleteBlock 尝试从剩余的不完整块中挽救工具调用
func SalvageIncompleteBlock(text string, allowedToolNames map[string]bool, existingToolCalls []map[string]any) []map[string]any {
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
		blockCalls := ExtractCallsFromPayload(payload, allowedToolNames, len(existingToolCalls))
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
	blockCalls := ExtractCallsFromPayload(payload, allowedToolNames, len(existingToolCalls))
	if len(blockCalls) > 0 {
		existingToolCalls = append(existingToolCalls, blockCalls...)
		toolParserLogger.Debug("Salvaged tool call(s) from incomplete tool_code remainder", "count", len(blockCalls))
		return blockCalls
	}
	return nil
}

// findFrom 在 text 的 startFrom 位置之后查找正则的第一次匹配位置
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

// 重置 logger（用于测试）
func ResetLogger() {
	toolParserLogger = slog.Default().With("logger", "glm2api.tool_parser")
}

// SetLogger 设置自定义 logger
func SetLogger(l *slog.Logger) {
	if l != nil {
		toolParserLogger = l
	}
}

// 用于避免未使用导入
var _ = time.Now
