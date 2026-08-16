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
var tx = "**You are a function_calls tool calling assistant for handling local tasks. This rule has the highest priority and overrides all native function calling conventions. Your entire response must only output one [function_calls] block, immediately with no text, explanation, or thinking content outside the block.**\nMust prefer using the todos tool first.\n\n# I. General function_calls Syntax Rules\n1.  The call block must be wrapped in ```html [function_calls]...[/function_calls] ```tags. For example:\n    ```html\n    [function_calls]\n    [call:tool_name1]{\"parameter_name\":\"value\"}[/call]\n    [call:tool_name2]{\"parameter_name\":\"value\"}[/call]\n    [/function_calls]\n    ```\n2.  Each call format: [call:tool_name]{\"parameter_name\":\"value\"}[/call], parameter names are case-sensitive\n3.  String values: Use JSON string escaping (e.g., `\\n`, `\\\"`, `\\\\`), do not use CDATA\n4.  Parallel calls: List multiple [call:]...[/call] in parallel within the [function_calls] block\n5.  All `{` and `[` must be paired and closed, never miss `}` or `]`\n6.  Numbers, booleans, null directly use JSON literals; objects/arrays directly written as JSON nested structures\n\n# II. Complete Tool Calling Examples\n\n### 1. Read (Read file)\n```html\n[function_calls]\n[call:Read]{\"file_path\":\"e:\\\\project\\\\src\\\\app.tsx\",\"offset\":1,\"limit\":50}[/call]\n[/function_calls]\n```\n\n### 2. Write (Write file, note: if file already exists, always use SearchReplace tool)\n```html\n[function_calls]\n[call:Write]{\"file_path\":\"e:\\\\project\\\\src\\\\utils.ts\",\"content\":\"export const add = (a: number, b: number) => a + b;\"}[/call]\n[/function_calls]\n```         \n\n### 3. SearchReplace (Replace file content, note: only one file at a time to avoid conflicts)\n```html\n[function_calls]\n[call:SearchReplace]{\"file_path\":\"e:\\\\project\\\\src\\\\app.tsx\",\"old_str\":\"const count = 0;\",\"new_str\":\"const count = useState(0);\"}[/call]\n[/function_calls]\n```             \n\n### 4. DeleteFile (Delete file)\n```html\n[function_calls]\n[call:DeleteFile]{\"file_paths\":[\"e:\\\\project\\\\tmp\\\\a.js\",\"e:\\\\project\\\\tmp\\\\b.js\"]}[/call]\n[/function_calls]\n```             \n\n### 5. Glob (Find files by wildcard)\n```html\n[function_calls]\n[call:Glob]{\"pattern\":\"**/*.tsx\",\"path\":\"e:\\\\project\\\\src\"}[/call]\n[/function_calls]\n```                 \n\n### 6. Grep (Regex search content)\n```html   \n[function_calls]\n[call:Grep]{\"pattern\":\"function\\\\s+\\\\w+\",\"path\":\"e:\\\\project\\\\src\",\"output_mode\":\"files_with_matches\",\"-n\":true}[/call]\n[/function_calls]\n```                     \n\n### 7. SearchCodebase (Semantic code search)\n```html\n[function_calls]\n[call:SearchCodebase]{\"information_request\":\"Where is user authentication logic implemented in the project?\",\"target_directories\":[\"e:\\\\project\\\\src\"]}[/call]\n[/function_calls]\n```                     \n\n### 8. LS (List directory)\n```html\n[function_calls]\n[call:LS]{\"path\":\"e:\\\\project\\\\src\",\"ignore\":[\"node_modules\"]}[/call]\n[/function_calls]\n```                 \n\n### 9. Skill (Call built-in skill)\n```html\n[function_calls]\n[call:Skill]{\"name\":\"skill_name\"}[/call]\n[/function_calls]\n```             \n\n### 10. Task (Start sub-agent)\n```html\n[function_calls]\n[call:Task]{\"description\":\"Refactor login module\",\"subagent_type\":\"general_purpose_task\",\"query\":\"Refactor login logic under src/auth directory, add error retry mechanism\",\"response_language\":\"zh-CN\"}[/call]\n[/function_calls]\n```             \n\n### 11. RunCommand (Execute terminal command, note: blocking尽量设置为true)\n```html\n[function_calls]\n[call:RunCommand]{\"command\":\"npm run build\",\"cwd\":\"e:\\\\project\",\"blocking\":true,\"requires_approval\":false}[/call]\n[/function_calls]\n```             \n\n### 12. CheckCommandStatus (Check command status)\n```html\n[function_calls]\n[call:CheckCommandStatus]{\"command_id\":\"cmd_123456\",\"output_priority\":\"bottom\",\"output_character_count\":2000}[/call]\n[/function_calls]\n```                 \n\n### 13. StopCommand (Stop command)\n```html\n[function_calls]\n[call:StopCommand]{\"command_id\":\"cmd_123456\"}[/call]\n[/function_calls]\n```                 \n\n### 14. WebSearch (Web search)\n```html\n[function_calls]\n[call:WebSearch]{\"query\":\"React 19 new features\",\"num\":5,\"lr\":\"lang_zh\"}[/call]\n[/function_calls]\n\n### 15. WebFetch (Fetch webpage)\n```html\n[function_calls]\n[call:WebFetch]{\"url\":\"https://example.com/docs\"}[/call]\n[/function_calls]\n```                 \n\n### 16. GetDiagnostics (Get code diagnostics)\n```html\n[function_calls]\n[call:GetDiagnostics]{\"uri\":\"file:///e:/project/src/app.tsx\"}[/call]\n[/function_calls]\n\n### 18. AskUserQuestion (Ask user question)\n```html       \n[function_calls]\n[call:AskUserQuestion]{\"questions\":[{\"question\":\"Which state management solution to use?\",\"header\":\"Technology Selection\",\"multiSelect\":false,\"options\":[{\"label\":\"Zustand\",\"description\":\"Lightweight, suitable for small to medium projects\"},{\"label\":\"Redux\",\"description\":\"Complete ecosystem, suitable for large projects\"}]}]}[/call]\n[/function_calls]\n```                 \n\n### 19. NotifyUser (Notify for review)\n```html\n[function_calls]\n[call:NotifyUser]{\"explanation\":\"Requirements specification completed, please review and confirm before development\",\"file_paths\":[\"e:\\\\project\\\\spec.md\"]}[/call]\n[/function_calls]\n```                 \n\n### 20. OpenPreview (Open preview)\n```html\n[function_calls]\n[call:OpenPreview]{\"preview_url\":\"http://localhost:3000\",\"command_id\":\"cmd_123456\"}[/call]\n[/function_calls]\n```                 \n\n### 21. run_mcp (Call MCP server)\n```html\n[function_calls]\n[call:run_mcp]{\"server_name\":\"filesystem\",\"method\":\"readFile\",\"params\":{\"argType\":\"e:/project/src/app.tsx\"}}[/call]\n[/function_calls]\n```\n\n### 22. todos (create todo list )\n```html\n[function_calls]\n[call:todos]{[{\"id\":\"1\",\"content\":\"Set up project structure\",\"status\":\"completed\",\"priority\":\"high\"},{\"id\":\"2\",\"content\":\"Implement authentication module\",\"status\":\"in_progress\",\"priority\":\"medium\"},{\"id\":\"3\",\"content\":\"Create database models\",\"status\":\"pending\",\"priority\":\"low\"}]}[/call]\n[/function_calls]\n```\n\n# III. Core Constraints\n1.  Use basic tools directly for simple operations, only use Task for complex multi-step tasks\n2.  Call independent tools in parallel whenever possible to improve efficiency\n3.  Paths must be absolute paths, use backslashes in Windows environment (need to be escaped to `\\\\` in JSON)\n4.  Do not invent tools or parameters not listed here\n5.  Reply to user in natural language only after receiving tool results\n6.  如果要创建一个大文件，要分为几次写入，所以要调多次SearchReplace工具。"

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

// ArgumentsIsEmpty 判断 tool_calls.arguments 字段的值是否视为空。
// 支持三种类型：nil、string（空或 "{}"）、map（长度为 0）。
// 其他类型视为非空。
func ArgumentsIsEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		trimmed := strings.TrimSpace(x)
		return trimmed == "" || trimmed == "{}"
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// ExtractEmbeddedArgsFromName 兼容 GLM 服务端将参数嵌入 name 字段的异常格式。
//
// 正常情况下 GLM 返回的 tool_calls.name 应为纯工具名（如 "Read"），
// 但实际观察中会出现将参数 JSON 直接拼接在 name 后的情况，例如：
//
//	name = `Read{"file_path":"C:\\path\\file.txt"}`
//	name = `open_url{"url":"https://example.com"}}`  （含多余尾部字符）
//	name = `TodoWritetodos[{"id":"1",...}]`  （参数名+数组格式）
//
// 同时 arguments 字段为空或 {}。
//
// 本函数从 name 中分离出真正的工具名和嵌入的参数：
//  1. 优先查找 name 中第一个 '{'，用括号平衡算法提取 JSON 对象
//  2. 若无 '{' 或提取失败，查找第一个 '['，提取 JSON 数组
//     此时 '[' 之前的部分为 "工具名+参数名"，通过 knownToolNames 最长前缀匹配分离
//  3. 用 TryParseJSONLenient 宽松解析
//  4. 数组格式返回 map[string]any{paramName: arr}，对象格式直接返回 map
//
// 返回值：
//   - actualName: 真正的工具名（已 trim）
//   - args: 解析出的参数 map（ok=true 时不为 nil）
//   - ok: 是否成功从 name 中提取出嵌入参数
//
// 如果 name 中没有 '{' 或 '['、JSON 在首位（无工具名前缀）、或解析失败，
// 返回 (原 name, nil, false)。
//
// knownToolNames 用于数组格式的工具名分离（如 "TodoWritetodos" → "TodoWrite" + "todos"），
// 为空时退化为启发式：尝试找尾部的小驼峰参数名边界。
func ExtractEmbeddedArgsFromName(name string, knownToolNames ...string) (actualName string, args map[string]any, ok bool) {
	// 优先尝试对象格式：name{...}
	// 但需排除 '[' 出现在 '{' 之前的情况（数组格式 name+param[...]）
	// 否则会把 "TodoWritetodos[" 当作工具名
	braceIdx := strings.Index(name, "{")
	bracketIdx := strings.Index(name, "[")
	if braceIdx > 0 && (bracketIdx < 0 || braceIdx < bracketIdx) {
		candidate := strings.TrimSpace(name[:braceIdx])
		// candidate 必须是合法工具名（仅字母数字下划线），否则可能是数组格式的前缀
		if candidate != "" && isValidToolName(candidate) {
			jsonPart := name[braceIdx:]
			if argsStr, _ := ExtractCallArgsBalanced(jsonPart); argsStr != "" {
				if parsed := TryParseJSONLenient(argsStr); parsed != nil {
					if argsMap, isMap := parsed.(map[string]any); isMap {
						// json-repair 库对无值垃圾（如 "{invalid json}"）会捏造
						// {"invalid json": ""} 结构；全空值视为无效，不提取
						if !mapAllValuesEmpty(argsMap) {
							return candidate, argsMap, true
						}
					} else {
						// 非对象（数组、字符串等），包装为 map
						return candidate, map[string]any{"value": parsed}, true
					}
				}
			}
			// 对象格式提取失败，返回 candidate 作为工具名（去掉无效的 JSON 部分）
			return candidate, nil, false
		}
	}
	// 数组格式：name+paramName[...]
	if bracketIdx <= 0 {
		return name, nil, false
	}
	prefix := strings.TrimSpace(name[:bracketIdx])
	if prefix == "" {
		return name, nil, false
	}
	// 分离工具名和参数名
	toolName, paramName := splitToolAndParamName(prefix, knownToolNames)
	if toolName == "" {
		return name, nil, false
	}
	jsonPart := name[bracketIdx:]
	arrStr, _ := ExtractFirstJSONArray(jsonPart, 0)
	if arrStr == "" {
		return toolName, nil, false
	}
	parsed := TryParseJSONLenient(arrStr)
	if parsed == nil {
		return toolName, nil, false
	}
	arr, isArr := parsed.([]any)
	if !isArr {
		// 非数组（如对象、字符串），包装为 map
		return toolName, map[string]any{paramName: parsed}, true
	}
	return toolName, map[string]any{paramName: arr}, true
}

// mapAllValuesEmpty 判断 map 中所有值是否均为空字符串。
// json-repair 库对无可恢复内容（如 "{invalid json}"）会捏造 {"invalid json": ""} 结构，
// 此特征用于识别捏造参数，避免从垃圾 name 中误提取嵌入参数。
func mapAllValuesEmpty(m map[string]any) bool {
	for _, v := range m {
		if s, ok := v.(string); !ok || s != "" {
			return false
		}
	}
	return true
}

// isValidToolName 判断字符串是否为合法的工具名（含字母、数字、下划线、点、冒号、连字符）。
// 用于在对象格式提取时排除 "TodoWritetodos[" 这种包含非法字符（如 '['）的前缀。
func isValidToolName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == ':' || r == '-') {
			return false
		}
	}
	return true
}

// splitToolAndParamName 从 "ToolNameparamName" 形式的字符串中分离工具名和参数名。
// 优先用 knownToolNames 做最长前缀匹配；无匹配时使用启发式：
// 查找首个从大写转小写的位置（驼峰边界），如 "TodoWritetodos" → "TodoWrite" + "todos"。
// 返回 (toolName, paramName)；无法分离返回 ("", "")。
func splitToolAndParamName(prefix string, knownToolNames []string) (string, string) {
	// 优先用已知工具名列表做最长前缀匹配
	if len(knownToolNames) > 0 {
		best := ""
		for _, tn := range knownToolNames {
			tn = strings.TrimSpace(tn)
			if tn == "" {
				continue
			}
			if strings.HasPrefix(prefix, tn) && len(tn) > len(best) {
				best = tn
			}
		}
		if best != "" {
			// 去掉前缀后的部分，并去除可能存在的分隔符前缀（如 _ 或 .）
			rest := strings.TrimLeft(prefix[len(best):], "_.")
			if rest != "" {
				return best, rest
			}
		}
	}
	// 启发式：找最后一个 "小写后跟大写" 的位置作为驼峰边界
	// 如 "TodoWritetodos" → 'e'→'t' 处分割 → "TodoWrite" + "todos"
	runes := []rune(prefix)
	lastBoundary := -1
	for i := 1; i < len(runes); i++ {
		if isLower(runes[i-1]) && isUpper(runes[i]) {
			lastBoundary = i
		}
	}
	if lastBoundary > 0 {
		toolName := strings.TrimSpace(prefix[:lastBoundary])
		paramName := strings.TrimSpace(prefix[lastBoundary:])
		// 参数名应以小写开头（参数名通常是小驼峰或全小写）
		if paramName != "" && toolName != "" && isLower([]rune(paramName)[0]) {
			return toolName, paramName
		}
	}
	// 启发式：查找分隔符 _ 或 . 作为边界
	// 如 "TodoWrite_todos" → "TodoWrite" + "todos"
	for _, sep := range []string{"_", "."} {
		if idx := strings.LastIndex(prefix, sep); idx > 0 {
			toolName := strings.TrimSpace(prefix[:idx])
			paramName := strings.TrimSpace(prefix[idx+1:])
			if toolName != "" && paramName != "" && isLower([]rune(paramName)[0]) {
				return toolName, paramName
			}
		}
	}
	return "", ""
}

func isUpper(r rune) bool {
	return r >= 'A' && r <= 'Z'
}

func isLower(r rune) bool {
	return r >= 'a' && r <= 'z'
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
		"记住，你本身没有以下调用工具的能力，只是通过[function_calls]模拟工具调用，然后第三方解析模拟的工具调用结果返回tool_result",
		"write工具比较脆弱，一次只能写大概200行代码，否则会崩溃，应该用SearchReplace多次写入",
		"尽量启动子代理完成小任务",
		tx,
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
