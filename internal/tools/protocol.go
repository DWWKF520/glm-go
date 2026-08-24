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

// 示例参数生成的最大递归深度与单个层级最多采样的可选参数个数，
// 防止深层嵌套 schema（如 AskUserQuestion）生成过长的示例。
const (
	maxSampleDepth = 4
	maxSampleProps = 12
)

// SampleArgumentsFromSchema 从 JSON Schema 格式的 parameters 中自动生成一组示例参数。
// toolName 用于参数名启发式的上下文判断（如 pattern 对 Glob 是通配符、对 Grep 是正则）。
// required 参数按声明顺序全部包含，其余可选参数按字母序采样（上限 maxSampleProps），
// 保证同一 schema 生成结果稳定。无 properties 时返回空 map。
func SampleArgumentsFromSchema(toolName string, parameters any) map[string]any {
	m, ok := parameters.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	props, _ := m["properties"].(map[string]any)
	if len(props) == 0 {
		return map[string]any{}
	}
	return sampleArgsFromProperties(toolName, props, requiredList(m), 0)
}

// sampleArgsFromProperties 按 "required 优先（声明序）+ 其余字母序" 的确定性顺序
// 从 properties 中采样示例参数，depth 为当前嵌套深度。
func sampleArgsFromProperties(toolName string, props map[string]any, required []string, depth int) map[string]any {
	result := make(map[string]any)
	for _, name := range required {
		if schema, ok := props[name]; ok {
			result[name] = sampleValue(toolName, name, schema, depth)
		}
	}
	var rest []string
	for name := range props {
		if _, dup := result[name]; !dup {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	if len(rest) > maxSampleProps {
		rest = rest[:maxSampleProps]
	}
	for _, name := range rest {
		result[name] = sampleValue(toolName, name, props[name], depth)
	}
	return result
}

// sampleValue 根据单个参数的名称与 schema 生成一个贴近真实用法的示例值。
// 优先级：default > enum 首个值 > 按 type 与参数名启发式生成。
func sampleValue(toolName, paramName string, schema any, depth int) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return "value"
	}
	if d, ok := m["default"]; ok && d != nil {
		return d
	}
	if e, ok := m["enum"].([]any); ok && len(e) > 0 {
		return e[0]
	}
	t, _ := m["type"].(string)
	switch t {
	case "string":
		return sampleStringValue(toolName, paramName)
	case "integer", "number":
		return sampleNumberValue(paramName)
	case "boolean":
		return sampleBoolValue(paramName)
	case "array":
		if items, ok := m["items"]; ok && depth < maxSampleDepth {
			return []any{sampleValue(toolName, paramName, items, depth+1)}
		}
		return []any{}
	case "object":
		if props, ok := m["properties"].(map[string]any); ok && len(props) > 0 && depth < maxSampleDepth {
			return sampleArgsFromProperties(toolName, props, requiredList(m), depth+1)
		}
		return map[string]any{}
	}
	return "value"
}

// sampleStringValue 按参数名（结合工具名上下文）启发式生成字符串示例值，
// 均为常见约定值，便于模型模仿。
func sampleStringValue(toolName, name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "command_id"), strings.Contains(n, "cmd_id"):
		return "cmd_123456"
	case strings.Contains(n, "path") && strings.Contains(n, "dir"):
		return "e:\\project\\src"
	case strings.Contains(n, "path"), strings.Contains(n, "file"):
		return "e:\\project\\src\\app.tsx"
	case n == "cwd", strings.Contains(n, "workdir"), strings.Contains(n, "working_dir"):
		return "e:\\project"
	case strings.Contains(n, "dir"), strings.Contains(n, "folder"):
		return "e:\\project\\src"
	case strings.Contains(n, "url"):
		return "https://example.com/docs"
	case strings.Contains(n, "uri"):
		return "file:///e:/project/src/app.tsx"
	case strings.Contains(n, "pattern"):
		// Glob 类工具的 pattern 是通配符，其余（如 Grep）按正则示例展示转义写法
		if strings.Contains(strings.ToLower(toolName), "glob") || strings.Contains(n, "glob") || strings.Contains(n, "wildcard") {
			return "**/*.tsx"
		}
		return "function\\s+\\w+"
	case strings.Contains(n, "regex"), strings.Contains(n, "expression"):
		return "function\\s+\\w+"
	case strings.Contains(n, "type"), strings.Contains(n, "mode"):
		return "default"
	case strings.Contains(n, "command"), strings.Contains(n, "script"), strings.Contains(n, "shell"):
		return "npm run build"
	case strings.Contains(n, "lang"), strings.Contains(n, "locale"):
		return "zh-CN"
	case n == "id" || strings.HasSuffix(n, "_id"):
		return "1"
	case strings.Contains(n, "query"), strings.Contains(n, "search"), strings.Contains(n, "request"), strings.Contains(n, "question"):
		return "search keywords"
	case strings.Contains(n, "description"), strings.Contains(n, "message"), strings.Contains(n, "prompt"), strings.Contains(n, "summary"), strings.Contains(n, "explanation"), strings.Contains(n, "content"), strings.Contains(n, "text"), strings.Contains(n, "str"), strings.Contains(n, "code"), strings.Contains(n, "value"):
		return "sample text"
	case strings.Contains(n, "name"):
		return "example"
	default:
		return "value"
	}
}

// sampleNumberValue 按参数名启发式生成数值示例值。
func sampleNumberValue(name string) any {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "offset"), strings.Contains(n, "skip"):
		return 0
	case strings.Contains(n, "limit"), strings.Contains(n, "count"), strings.Contains(n, "num"), strings.Contains(n, "size"), strings.Contains(n, "length"):
		return 50
	default:
		return 1
	}
}

// sampleBoolValue 按参数名启发式生成布尔示例值（blocking 类默认 true，其余默认 false）。
func sampleBoolValue(name string) bool {
	return strings.Contains(strings.ToLower(name), "block")
}

// requiredList 提取 schema 中的 required 参数名列表（保持声明顺序）。
func requiredList(schema map[string]any) []string {
	raw, _ := schema["required"].([]any)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if s, ok := r.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ToolsToPrompt 将 OpenAI 格式的工具列表转换为发送给 GLM API 的提示词文本。
// 遍历每个工具，提取函数名、描述和参数 schema，生成 Markdown 格式的工具定义，
// 并根据参数 schema 自动生成该工具的 [function_calls] 调用示例（Example 段），
// 与通用协议指令 tx 拼接为完整的提示词。
// serverSideToolNames 指定服务端工具集合。
// 返回过滤掉空段落后的完整提示词文本。
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
		schemaSection := "### " + name + "\n" + description + "\nParameters: " + paramsJSON
		// 从参数 schema 自动生成示例调用，内嵌到该工具段落中
		if sampleArgs := SampleArgumentsFromSchema(name, parameters); len(sampleArgs) > 0 {
			schemaSection += "\nExample:\n" + SerializeToolCallBlock(name, sampleArgs)
		}
		toolSchemas = append(toolSchemas, schemaSection)
	}

	parts := []string{
		"# TOOL SCHEMAS",
		"文件都在远程服务器上，不在当前目录下，用open工具无法直接读取，只有模拟工具调用才能读取。",
		"思考时禁止使用工具，再发现把你删除。",
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
