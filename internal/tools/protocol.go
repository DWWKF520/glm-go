package tools

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/bytedance/sonic"
)

// ServerSideToolNames 存储服务端工具名称的集合（目前为空集合）。
// 用于区分客户端工具和服务端自动执行的工具，在构建提示词时会产生不同的调用指令。
var ServerSideToolNames = map[string]bool{}

// SafeJSONDumpsCompact 将任意值序列化为紧凑的 JSON 字符串（无缩进、不转义 HTML）。
// 序列化失败时返回空对象 "{}"。
func SafeJSONDumpsCompact(v any) string {
	data, err := sonic.MarshalString(v)
	if err != nil {
		return "{}"
	}
	return data
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

// ToolsToPrompt 将 OpenAI 格式的工具列表转换为发送给 GLM API 的提示词文本。
// 遍历每个工具，提取函数名、描述和参数 schema，生成 Markdown 格式的工具定义。
// 然后调用 BuildToolCallInstructions 生成工具调用协议指令，拼接为完整的提示词。
// 参数 policy 控制工具选择策略，serverSideToolNames 指定服务端工具集合。
// 返回过滤掉空行后的完整提示词文本。
func ToolsToPrompt(tools []map[string]any, serverSideToolNames map[string]bool) string {
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
	}
	var filtered []string
	for _, p := range parts {
		if p != "" {
			filtered = append(filtered, p)
		}
	}
	return strings.TrimSpace(strings.Join(filtered, "\n"))
}
