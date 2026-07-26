package tools

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/bytedance/sonic"
)

// ServerSideToolNames 存储服务端工具名称的集合（目前为空集合）。
// 用于区分客户端工具和服务端自动执行的工具，在构建提示词时会产生不同的调用指令。
var ServerSideToolNames = map[string]bool{}

// CanonicalToolCallExample 是工具调用的标准格式示例，用于在提示词中展示给模型。
// 格式为 [function_calls] 标签包裹多个 [call:TOOL_NAME]{...}[/call] 调用块。
const CanonicalToolCallExample = "[function_calls]\n" +
	`[call:TOOL_NAME_1]{"actual_parameter_name":"value"}[/call]` + "\n" + `[call:TOOL_NAME_2]{"actual_parameter_name":"value"}[/call]` + "\n" +
	"[/function_calls]"

// SafeJSONDumps 将任意值序列化为 JSON 字符串，使用 sonic 库且不转义 HTML 字符。
// 序列化失败时返回空对象 "{}"。
func SafeJSONDumps(v any) string {
	data, err := sonic.MarshalString(v)
	if err != nil {
		return "{}"
	}
	return data
}

// SafeJSONDumpsCompact 将任意值序列化为紧凑的 JSON 字符串（无缩进、不转义 HTML）。
// 序列化失败时返回空对象 "{}"。
func SafeJSONDumpsCompact(v any) string {
	data, err := sonic.MarshalString(v)
	if err != nil {
		return "{}"
	}
	return data
}

// NormalizeToolName 将工具名称转换为规范化的字符串形式。
// 对输入值调用 Sprintf 转为字符串后去除首尾空白。
// 用于统一处理不同来源的工具名称（可能为 string、float64 等类型）。
func NormalizeToolName(name any) string {
	return strings.TrimSpace(fmt.Sprintf("%v", name))
}

// safeParamNameRE 匹配参数名中不允许的字符（非字母数字、下划线、点、冒号、连字符），
// 用于 safeParameterName 将其替换为下划线
var safeParamNameRE = regexp.MustCompile(`[^a-zA-Z0-9_.:-]`)

// safeParameterName 将参数名规范化为安全的标识符格式。
// 移除所有非字母数字、下划线、点、冒号、连字符的字符（替换为下划线）。
// 如果结果为空，返回默认值 "value"。
// 用于确保从 LLM 输出中提取的参数名不包含非法字符。
func safeParameterName(value any) string {
	s := strings.TrimSpace(fmt.Sprintf("%v", value))
	s = safeParamNameRE.ReplaceAllString(s, "_")
	if s == "" {
		return "value"
	}
	return s
}

// NormalizeArguments 将工具参数规范化为 map[string]any 格式。
// 处理多种输入类型：
// - string 类型：尝试 JSON 解析，失败则包装为 {"raw": 原文本}
// - nil：返回空 map
// - map 类型：对所有 key 调用 safeParameterName 规范化
// - 其他类型：包装为 {"value": 原值}
// 确保返回值始终为 map[string]any，方便后续序列化。
func NormalizeArguments(payload any) map[string]any {
	parsed := payload
	if s, ok := parsed.(string); ok {
		if strings.TrimSpace(s) == "" {
			return map[string]any{}
		}
		var v any
		if err := sonic.UnmarshalString(s, &v); err != nil {
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

// SerializeToolCallBlock 将工具调用序列化为 [function_calls] 格式的文本块。
// 参数 name 是工具名称，arguments 是原始参数（会被 NormalizeArguments 规范化后序列化）。
// 返回格式：[function_calls]\n[call:name]{"param":"value"}[/call]\n[/function_calls]
// 用于在提示词中向模型展示工具调用的标准格式。
func SerializeToolCallBlock(name string, arguments any) string {
	normalized := NormalizeArguments(arguments)
	argsJSON := SafeJSONDumpsCompact(normalized)
	return "[function_calls]\n[call:" + name + "]" + argsJSON + "[/call]\n[/function_calls]"
}

// SerializeToolResultBlock 将工具执行结果序列化为 ```tool_result 格式的文本块。
// 参数 toolCallID 是对应的工具调用 ID，toolName 是工具名称，content 是执行结果文本。
// 返回格式：```tool_result\n{"tool_result":{"call_id":"...","name":"...","content":"..."}}\n```
// 用于在发送给 GLM API 的消息中包含工具执行结果。
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

// ToolChoicePolicy 定义工具选择策略，控制模型是否应该使用工具以及使用哪个工具。
// Mode 可选值："auto"（自动决定）、"none"（禁止使用）、"required"（必须使用）、"specific"（指定特定工具）
// ToolName 仅在 Mode 为 "specific" 时有效，指定必须调用的工具名称。
type ToolChoicePolicy struct {
	Mode     string // "auto", "none", "required", "specific"
	ToolName string
}

// ParseToolChoicePolicy 从 OpenAI 格式的 tool_choice 参数解析出工具选择策略。
// 支持的输入格式：
// - 字符串："auto"、"none"、"required"
// - 对象：{"type":"function","function":{"name":"tool_name"}}
// - nil：默认返回 "auto" 策略
// availableToolNames 参数用于验证指定的工具名是否在可用工具列表中（传 nil 或空则跳过验证）。
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

// BuildToolCallInstructions 构建完整的工具调用协议指令文本，插入到系统提示词中。
// 将工具分为两类：服务端工具（serverSideToolNames）和 JSON 工具，分别列出可用工具名。
// 根据 policy.Mode 附加不同的约束指令（auto/none/required/specific）。
// 返回的指令文本包含：工具调用核心规则、各类型工具的使用说明、格式示例、语法规则和通用规则。
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

// ToolsToPrompt 将 OpenAI 格式的工具列表转换为发送给 GLM API 的提示词文本。
// 遍历每个工具，提取函数名、描述和参数 schema，生成 Markdown 格式的工具定义。
// 然后调用 BuildToolCallInstructions 生成工具调用协议指令，拼接为完整的提示词。
// 参数 policy 控制工具选择策略，serverSideToolNames 指定服务端工具集合。
// 返回过滤掉空行后的完整提示词文本。
func ToolsToPrompt(tools []map[string]any, policy ToolChoicePolicy, serverSideToolNames map[string]bool) string {
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
