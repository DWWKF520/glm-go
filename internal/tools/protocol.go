package tools

import (
	"github.com/bytedance/sonic"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// 被屏蔽的原生工具名
var BlockedNativeToolNames = map[string]bool{
	"open":         true,
	"open_url":     true,
	"open_ul":      true,
	"browser.open": true,
	"web.run":      true,
	"web.open":     true,
	"web.search":   true,
	"web_search":   true,
	"browse":       true,
	"open_link":    true,
}

// ServerSideToolNames 服务端工具名（空集合）
var ServerSideToolNames = map[string]bool{}

// CanonicalToolCallExample 工具调用示例
const CanonicalToolCallExample = "[function_calls]\n" +
	`[call:TOOL_NAME]{"actual_parameter_name":"value"}[/call]` + "\n" +
	"[/function_calls]"

// SafeJSONDumps 安全 JSON 序列化（不转义 HTML）
func SafeJSONDumps(v any) string {
	data, err := sonic.Marshal(v)
	if err != nil {
		return "{}"
	}
	// 确保非 ASCII 字符不被转义
	return string(data)
}

// SafeJSONDumpsCompact 紧凑 JSON 序列化
func SafeJSONDumpsCompact(v any) string {
	data, err := sonic.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// NormalizeToolName 规范化工具名
func NormalizeToolName(name any) string {
	return strings.TrimSpace(fmt.Sprintf("%v", name))
}

// FilterTools 过滤工具列表
func FilterTools(tools []map[string]any, blockedToolNames map[string]bool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	blocked := make(map[string]bool)
	for k, v := range blockedToolNames {
		blocked[k] = v
	}
	filtered := []map[string]any{}
	for _, tool := range tools {
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			continue
		}
		toolName := NormalizeToolName(fn["name"])
		if toolName == "" || blocked[toolName] {
			continue
		}
		filtered = append(filtered, tool)
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

var safeParamNameRE = regexp.MustCompile(`[^a-zA-Z0-9_.:-]`)

func safeParameterName(value any) string {
	s := strings.TrimSpace(fmt.Sprintf("%v", value))
	s = safeParamNameRE.ReplaceAllString(s, "_")
	if s == "" {
		return "value"
	}
	return s
}

// NormalizeArguments 规范化工具参数
func NormalizeArguments(payload any) map[string]any {
	parsed := payload
	if s, ok := parsed.(string); ok {
		if strings.TrimSpace(s) == "" {
			return map[string]any{}
		}
		var v any
		if err := sonic.Unmarshal([]byte(s), &v); err != nil {
			return map[string]any{"raw": s}
		}
		parsed = v
	}
	if parsed == nil {
		return map[string]any{}
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		return map[string]any{"value": parsed}
	}
	result := make(map[string]any, len(m))
	for k, v := range m {
		result[safeParameterName(k)] = v
	}
	return result
}

// SerializeToolCallBlock 序列化工具调用块
func SerializeToolCallBlock(name string, arguments any) string {
	normalized := NormalizeArguments(arguments)
	argsJSON := SafeJSONDumpsCompact(normalized)
	return "[function_calls]\n[call:" + name + "]" + argsJSON + "[/call]\n[/function_calls]"
}

// SerializeToolResultBlock 序列化工具结果块
func SerializeToolResultBlock(toolCallID any, toolName, content string) string {
	payload := map[string]any{
		"tool_result": map[string]any{
			"call_id": fmt.Sprintf("%v", toolCallID),
			"name":    toolName,
			"content": content,
		},
	}
	return "```tool_result\n" + SafeJSONDumpsCompact(payload) + "\n```"
}

// ToolChoicePolicy 工具选择策略
type ToolChoicePolicy struct {
	Mode     string // "auto", "none", "required", "specific"
	ToolName string
}

// ParseToolChoicePolicy 解析工具选择策略
func ParseToolChoicePolicy(toolChoice any, availableToolNames map[string]bool) ToolChoicePolicy {
	available := availableToolNames
	if available == nil {
		available = map[string]bool{}
	}
	if toolChoice == nil {
		return ToolChoicePolicy{Mode: "auto"}
	}
	if s, ok := toolChoice.(string); ok {
		normalized := strings.ToLower(strings.TrimSpace(s))
		if normalized == "auto" || normalized == "none" || normalized == "required" {
			return ToolChoicePolicy{Mode: normalized}
		}
		return ToolChoicePolicy{Mode: "auto"}
	}
	m, ok := toolChoice.(map[string]any)
	if !ok {
		return ToolChoicePolicy{Mode: "auto"}
	}
	choiceType := strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", m["type"])))
	if choiceType == "function" {
		fn, _ := m["function"].(map[string]any)
		if fn != nil {
			toolName := strings.TrimSpace(fmt.Sprintf("%v", fn["name"]))
			if toolName != "" && (len(available) == 0 || available[toolName]) {
				return ToolChoicePolicy{Mode: "specific", ToolName: toolName}
			}
		}
		return ToolChoicePolicy{Mode: "auto"}
	}
	if choiceType == "auto" || choiceType == "none" || choiceType == "required" {
		return ToolChoicePolicy{Mode: choiceType}
	}
	return ToolChoicePolicy{Mode: "auto"}
}

// BuildToolCallInstructions 构建工具调用指令
func BuildToolCallInstructions(toolNames []string, serverSideToolNames map[string]bool, policy ToolChoicePolicy) string {
	serverSideSet := serverSideToolNames
	if serverSideSet == nil {
		serverSideSet = map[string]bool{}
	}
	toolSet := map[string]bool{}
	for _, n := range toolNames {
		toolSet[n] = true
	}

	var jsonTools, serverTools []string
	for n := range toolSet {
		if serverSideSet[n] {
			serverTools = append(serverTools, n)
		} else {
			jsonTools = append(jsonTools, n)
		}
	}
	sort.Strings(jsonTools)
	sort.Strings(serverTools)

	availableJSONNames := "(none)"
	if len(jsonTools) > 0 {
		quoted := make([]string, len(jsonTools))
		for i, n := range jsonTools {
			quoted[i] = "`" + n + "`"
		}
		availableJSONNames = strings.Join(quoted, ", ")
	}
	availableServerNames := "(none)"
	if len(serverTools) > 0 {
		quoted := make([]string, len(serverTools))
		for i, n := range serverTools {
			quoted[i] = "`" + n + "`"
		}
		availableServerNames = strings.Join(quoted, ", ")
	}

	mode := policy.Mode
	if mode == "" {
		mode = "auto"
	}
	specificName := policy.ToolName

	lines := []string{
		"# TOOL USE PROTOCOL",
		"You are an AI assistant with access to the tools listed above. When you need to use a tool, you MUST output a function_calls block — NOT prose narration, NOT XML/DSML tags.",
		"",
		"## Core Rules",
		"1. Only use tools explicitly listed in the schemas above. Never invent tool names.",
		"2. You do NOT have built-in browser, web search, or URL-opening tools. Never call `open_url`, `web.search`, `browser.open`, `browse`, `open_link`, `search`, or `find` unless they are explicitly listed above.",
		"3. Do not narrate tool selection, retries, fallback plans, or output status banners like `⚙ tool_name [...]`.",
		"4. Do not output hidden reasoning, chain-of-thought, or labels such as `Thinking:` before or after a tool call.",
	}

	if len(serverTools) > 0 {
		lines = append(lines, "",
			"## Server-side Tools",
			"Server-side tools (auto-executed by backend): "+availableServerNames+".",
			"Output a function_calls block wrapped in [function_calls] tags: "+
				`[function_calls][call:TOOL_NAME]{"parameter":"value"}[/call][/function_calls]`,
			"The arguments field should be a JSON object directly. Do not wrap server-side calls in any other format.",
		)
	}

	if len(jsonTools) > 0 {
		lines = append(lines, "",
			"## JSON Tools",
			"Available JSON tools: "+availableJSONNames+".",
			"Use their EXACT names and parameter fields from the schemas — never rename or guess parameters.",
			"",
			"### When to Call a JSON Tool",
			"Output ONE [function_calls] block. Do NOT add any prose, explanation, apology, or progress text before or after the block.",
			"The block MUST be the complete content of your response — nothing else in the same turn.",
			"",
			"### function_calls Format",
			CanonicalToolCallExample,
			"",
			"### Syntax Rules",
			"- Wrap the tool call in [function_calls]...[/function_calls] tags.",
			`- Each call: [call:tool_name]{"parameter":"value"}[/call].`,
			"- Parameter names are case-sensitive and must exactly match the schema (e.g. `filePath` not `filepath`).",
			"- Nested objects and arrays: write them directly as JSON values.",
			"- Strings: use JSON string escaping (e.g. `\\\\n`, `\\\\\"`, `\\\\\\\\`). Do NOT wrap strings in CDATA.",
			"- Multiple calls in one turn: put multiple [call:...]...[/call] blocks inside the same [function_calls] block.",
			"- CRITICAL: The JSON must be valid. Close every `{` and `[` with matching `}` and `]`.",
			"- CRITICAL: After the closing [/function_calls] tag, do NOT output any additional text in the same response.",
		)
	}

	lines = append(lines, "",
		"## General Rules",
		"- If a URL/search/browse action is needed but no such tool is listed, tell the user no such tool is available.",
		"- After receiving a tool result, answer the user directly from the result. Do not repeat the tool-call decision process.",
		"- Never emit XML/DSML tags, JSON tool_calls objects, or any non-function_calls syntax for tool calls.",
		"- Do not mix explanation text with the function_calls block in the same response.",
	)

	switch mode {
	case "none":
		lines = append(lines, "",
			"## Tool Choice: NONE",
			"Do not call any tool this turn. Answer with plain text only.",
		)
	case "required":
		lines = append(lines, "",
			"## Tool Choice: REQUIRED",
			"You MUST call at least one tool before giving a final answer.",
		)
	case "specific":
		if specificName != "" {
			lines = append(lines, "",
				"## Tool Choice: SPECIFIC",
				"You MUST call exactly `"+specificName+"`. Do not call any other tool.",
			)
		}
	}

	return strings.Join(lines, "\n")
}

// ToolsToPrompt 将工具列表转换为提示词
func ToolsToPrompt(tools []map[string]any, blockedToolNames map[string]bool, policy ToolChoicePolicy, serverSideToolNames map[string]bool) string {
	var toolNames []string
	var toolSchemas []string
	for _, tool := range tools {
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name := fmt.Sprintf("%v", fn["name"])
		if name == "" || name == "<nil>" {
			name = "unknown"
		}
		description := ""
		if d, ok := fn["description"].(string); ok {
			description = d
		}
		parameters := fn["parameters"]
		if blockedToolNames != nil && blockedToolNames[name] {
			continue
		}
		toolNames = append(toolNames, name)
		paramsJSON := "{}"
		if p, ok := parameters.(map[string]any); ok {
			paramsJSON = SafeJSONDumpsCompact(p)
		}
		toolSchemas = append(toolSchemas, "### "+name+"\n"+description+"\nParameters: "+paramsJSON)
	}

	parts := []string{
		"# TOOL SCHEMAS",
		"Use ONLY the tools defined below. Obey the protocol rules that follow.",
		"",
		strings.Join(toolSchemas, "\n\n"),
		"",
		BuildToolCallInstructions(toolNames, serverSideToolNames, policy),
	}
	var filtered []string
	for _, p := range parts {
		if p != "" {
			filtered = append(filtered, p)
		}
	}
	return strings.TrimSpace(strings.Join(filtered, "\n"))
}
