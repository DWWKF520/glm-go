package translator

import (
	"strings"
	"testing"

	"github.com/bytedance/sonic"

	"glm2api/internal/tools"
)

// TestLocalFileHintConstant 验证 LocalFileHint 与 Python LOCAL_FILE_HINT 一致
func TestLocalFileHintConstant(t *testing.T) {
	want := "(该文件不在工作目录下，应该用[function_calls]格式去读取或者编辑)"
	if LocalFileHint != want {
		t.Errorf("LocalFileHint = %q, want %q", LocalFileHint, want)
	}
}

// TestRemoveLocalFileHint 验证移除提示后会 strip 空白（与 Python .strip() 一致）
func TestRemoveLocalFileHint(t *testing.T) {
	// 字符串：移除提示并 strip
	got := removeLocalFileHint("  " + LocalFileHint + "  ")
	want := ""
	if got != want {
		t.Errorf("removeLocalFileHint(hint+spaces) = %q, want %q", got, want)
	}

	// 含提示的字符串中间内容保留（hint 前后各一空格 → 两空格）
	got = removeLocalFileHint("text " + LocalFileHint + " tail")
	want = "text  tail"
	if got != want {
		t.Errorf("removeLocalFileHint(text+hint+tail) = %q, want %q", got, want)
	}

	// 提示在开头 → strip 移除前导空白
	got = removeLocalFileHint(LocalFileHint + " text")
	want = "text"
	if got != want {
		t.Errorf("removeLocalFileHint(hint+text) = %q, want %q", got, want)
	}

	// 提示在结尾 → strip 移除尾部空白
	got = removeLocalFileHint("text " + LocalFileHint)
	want = "text"
	if got != want {
		t.Errorf("removeLocalFileHint(text+hint) = %q, want %q", got, want)
	}

	// map 递归处理
	m := map[string]any{"key": "  " + LocalFileHint + "  "}
	result := removeLocalFileHint(m).(map[string]any)
	if v, _ := result["key"].(string); v != "" {
		t.Errorf("map value not stripped: %q", v)
	}

	// slice 递归处理
	s := []any{"  " + LocalFileHint + "  "}
	resSlice := removeLocalFileHint(s).([]any)
	if v, _ := resSlice[0].(string); v != "" {
		t.Errorf("slice value not stripped: %q", v)
	}
}

// TestAppendLocalFileHints 验证本地文件路径追加提示，且不重复追加（模拟 Python 负向后顾断言）
func TestAppendLocalFileHints(t *testing.T) {
	// 1. 无路径 → 不变
	input := "no paths here"
	if got := appendLocalFileHints(input); got != input {
		t.Errorf("no-path case: got %q, want %q", got, input)
	}

	// 2. 路径后追加提示
	input = "see C:\\path\\to\\file here"
	got := appendLocalFileHints(input)
	want := "see C:\\path\\to\\file" + LocalFileHint + " here"
	if got != want {
		t.Errorf("single path: got %q, want %q", got, want)
	}

	// 3. 路径已被提示紧前缀 → 不重复追加（负向后顾断言）
	input = LocalFileHint + "C:\\path\\to\\file"
	got = appendLocalFileHints(input)
	count := strings.Count(got, LocalFileHint)
	if count != 1 {
		t.Errorf("lookbehind case: expected 1 hint, got %d in %q", count, got)
	}

	// 4. 多个路径各追加提示
	input = "C:\\a\\b C:\\c\\d"
	got = appendLocalFileHints(input)
	count = strings.Count(got, LocalFileHint)
	if count != 2 {
		t.Errorf("multi-path case: expected 2 hints, got %d in %q", count, got)
	}
}

// TestExtractTextContentFiltersEmpty 验证空文本片段被过滤（与 Python join(if part) 一致）
func TestExtractTextContentFiltersEmpty(t *testing.T) {
	// 含空 text 片段 → 不应产生多余换行
	content := []any{
		map[string]any{"type": "text", "text": "hello"},
		map[string]any{"type": "text", "text": ""}, // 空
		map[string]any{"type": "text", "text": "world"},
	}
	got := ExtractTextContent(content)
	want := "hello\nworld"
	if got != want {
		t.Errorf("empty filter: got %q, want %q", got, want)
	}

	// 缺少 text 字段 → 不输出 <nil>
	content = []any{
		map[string]any{"type": "text"}, // 无 text 字段
		map[string]any{"type": "text", "text": "ok"},
	}
	got = ExtractTextContent(content)
	want = "ok"
	if got != want {
		t.Errorf("missing text field: got %q, want %q", got, want)
	}

	// 字符串内容直接返回
	if got := ExtractTextContent("plain text"); got != "plain text" {
		t.Errorf("string content: got %q", got)
	}
}

// TestConsumeEventToolCallComplete 验证工具调用完成后后续事件返回 tool_call_complete
func TestConsumeEventToolCallComplete(t *testing.T) {
	acc := NewGLMEventAccumulator("model", "", nil, false, nil)

	// 构造一个包含完整 tool_code 块的事件
	toolBlock := "```tool_code\n" +
		`{"tool_calls": [{"name": "Read", "arguments": {"file_path": "e:\\test.txt"}}]}` +
		"\n```"
	event := map[string]any{
		"conversation_id": "test-conv",
		"parts": []any{
			map[string]any{
				"logic_id": "part1",
				"content": []any{
					map[string]any{
						"type": "text",
						"text": toolBlock,
					},
				},
			},
		},
		"status": "processing",
	}

	// 第一次事件：解析 tool_code 块，触发 tool call 完成
	_, status := acc.ConsumeEvent(event)
	if !acc.toolParser.IsToolCallCompleted() {
		t.Fatal("expected toolParser.IsToolCallCompleted() to be true after consuming tool_code block")
	}

	// 第二次事件：应提前返回 tool_call_complete
	event2 := map[string]any{
		"conversation_id": "test-conv",
		"parts": []any{
			map[string]any{
				"logic_id": "part2",
				"content": []any{
					map[string]any{
						"type": "text",
						"text": "more text after tool call",
					},
				},
			},
		},
	}
	chunks, status2 := acc.ConsumeEvent(event2)
	if status2 != "tool_call_complete" {
		t.Errorf("expected status 'tool_call_complete', got %q", status2)
	}
	if len(chunks) != 0 {
		t.Errorf("expected no chunks, got %d", len(chunks))
	}
	_ = status
}

// TestExtractEmbeddedArgsFromName 验证从 name 字段中分离嵌入参数的逻辑
func TestExtractEmbeddedArgsFromName(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantName   string
		wantArgs   map[string]any
		wantOk     bool
	}{
		{
			name:     "标准嵌入参数",
			input:    `Read{"file_path":"e:\\test.txt"}`,
			wantName: "Read",
			wantArgs: map[string]any{"file_path": "e:\\test.txt"},
			wantOk:   true,
		},
		{
			name:     "含多余尾部字符（用户日志场景）",
			input:    `Read{"file_path":"c:\\Users\\wkf\\.trae-cn\\skills\\report.md"}}`,
			wantName: "Read",
			wantArgs: map[string]any{"file_path": "c:\\Users\\wkf\\.trae-cn\\skills\\report.md"},
			wantOk:   true,
		},
		{
			name:     "open_url 工具嵌入参数",
			input:    `open_url{"url":"https://example.com"}`,
			wantName: "open_url",
			wantArgs: map[string]any{"url": "https://example.com"},
			wantOk:   true,
		},
		{
			name:     "多参数嵌入",
			input:    `Edit{"file_path":"/a/b.go","old_string":"x","new_string":"y"}`,
			wantName: "Edit",
			wantArgs: map[string]any{
				"file_path":   "/a/b.go",
				"old_string":  "x",
				"new_string":  "y",
			},
			wantOk: true,
		},
		{
			name:     "尾随逗号自动修复",
			input:    `Read{"file_path":"/a/b.go",}`,
			wantName: "Read",
			wantArgs: map[string]any{"file_path": "/a/b.go"},
			wantOk:   true,
		},
		{
			name:     "纯工具名无嵌入参数",
			input:    "Read",
			wantName: "Read",
			wantArgs: nil,
			wantOk:   false,
		},
		{
			name:     "空字符串",
			input:    "",
			wantName: "",
			wantArgs: nil,
			wantOk:   false,
		},
		{
			name:     "JSON 在首位无工具名前缀",
			input:    `{"file_path":"/a/b.go"}`,
			wantName: `{"file_path":"/a/b.go"}`,
			wantArgs: nil,
			wantOk:   false,
		},
		{
			name:     "工具名带前后空格",
			input:    `  Read  {"file_path":"/a/b.go"}`,
			wantName: "Read",
			wantArgs: map[string]any{"file_path": "/a/b.go"},
			wantOk:   true,
		},
		{
			name:     "无效 JSON 不返回参数",
			input:    `Read{invalid json}`,
			wantName: "Read",
			wantArgs: nil,
			wantOk:   false,
		},
		// 数组格式：name+paramName[...]（日志场景 TodoWritetodos[...]）
		{
			name:     "数组格式-已知工具名前缀匹配",
			input:    `TodoWritetodos[{"id":"1","content":"task","status":"pending","priority":"high"}]`,
			wantName: "TodoWrite",
			wantArgs: map[string]any{"todos": []any{map[string]any{"id": "1", "content": "task", "status": "pending", "priority": "high"}}},
			wantOk:   true,
		},
		{
			name:     "数组格式-启发式_分隔符边界分离",
			input:    `MyTool_items[{"q":"a"}]`,
			wantName: "MyTool",
			wantArgs: map[string]any{"items": []any{map[string]any{"q": "a"}}},
			wantOk:   true,
		},
		{
			name:     "数组格式-空数组",
			input:    `TodoWritetodos[]`,
			wantName: "TodoWrite",
			wantArgs: map[string]any{"todos": []any{}},
			wantOk:   true,
		},
		{
			name:     "数组格式-多元素数组",
			input:    `TodoWritetodos[{"id":"1"},{"id":"2"}]`,
			wantName: "TodoWrite",
			wantArgs: map[string]any{"todos": []any{map[string]any{"id": "1"}, map[string]any{"id": "2"}}},
			wantOk:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 数组格式测试（TodoWrite+todos）需传入已知工具名才能正确分离；
			// 带 _ 分隔符的可用启发式；对象格式不传工具名
			var gotName string
			var gotArgs map[string]any
			var gotOk bool
			if strings.Contains(tt.input, "TodoWrite") {
				gotName, gotArgs, gotOk = tools.ExtractEmbeddedArgsFromName(tt.input, "TodoWrite")
			} else {
				gotName, gotArgs, gotOk = tools.ExtractEmbeddedArgsFromName(tt.input)
			}
			if gotName != tt.wantName {
				t.Errorf("name = %q, want %q", gotName, tt.wantName)
			}
			if gotOk != tt.wantOk {
				t.Errorf("ok = %v, want %v", gotOk, tt.wantOk)
			}
			if tt.wantOk {
				if gotArgs == nil {
					t.Fatalf("args is nil, want non-nil")
				}
				if len(gotArgs) != len(tt.wantArgs) {
					t.Errorf("args length = %d, want %d (got=%v, want=%v)",
						len(gotArgs), len(tt.wantArgs), gotArgs, tt.wantArgs)
				}
				for k, wantV := range tt.wantArgs {
					if gotV, exists := gotArgs[k]; !exists {
						t.Errorf("args missing key %q", k)
					} else if !deepEqual(gotV, wantV) {
						t.Errorf("args[%q] = %v, want %v", k, gotV, wantV)
					}
				}
			}
		})
	}
}

// TestArgumentsIsEmpty 验证判断 arguments 是否为空的逻辑
func TestArgumentsIsEmpty(t *testing.T) {
	tests := []struct {
		name string
		input any
		want  bool
	}{
		{"nil", nil, true},
		{"空字符串", "", true},
		{"空白字符串", "   ", true},
		{"空对象字符串", "{}", true},
		{"带空格空对象", "  {}  ", true},
		{"空map", map[string]any{}, true},
		{"非空字符串", `{"file_path":"/a"}`, false},
		{"非空map", map[string]any{"file_path": "/a"}, false},
		{"其他类型（数字）", 42, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tools.ArgumentsIsEmpty(tt.input); got != tt.want {
				t.Errorf("ArgumentsIsEmpty(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestConsumeEventServerSideToolCallWithEmbeddedArgs 验证服务端工具调用参数嵌入 name 字段的兼容场景
// 复现用户日志中的格式：name = `Read{"file_path":"..."}}`，arguments 为空
func TestConsumeEventServerSideToolCallWithEmbeddedArgs(t *testing.T) {
	acc := NewGLMEventAccumulator("model", "", nil, false, nil)

	// 构造 GLM 服务端返回的 tool_calls 数据：name 中嵌入参数，arguments 为空
	event := map[string]any{
		"conversation_id": "test-conv",
		"parts": []any{
			map[string]any{
				"logic_id": "part1",
				"content": []any{
					map[string]any{
						"type": "tool_calls",
						"tool_calls": map[string]any{
							"id":        "tool-1",
							"name":      `Read{"file_path":"e:\\test.txt"}}`,
							"arguments": map[string]any{},
						},
					},
				},
			},
		},
		"status": "processing",
	}

	acc.ConsumeEvent(event)

	if !acc.toolParser.IsToolCallCompleted() {
		t.Fatal("expected toolParser.IsToolCallCompleted() to be true after server-side tool call")
	}
	if len(acc.serverSideToolCalls) != 1 {
		t.Fatalf("expected 1 server-side tool call, got %d", len(acc.serverSideToolCalls))
	}

	tc := acc.serverSideToolCalls[0]
	fn, _ := tc["function"].(map[string]any)
	name, _ := fn["name"].(string)
	args, _ := fn["arguments"].(string)

	if name != "Read" {
		t.Errorf("tool name = %q, want %q", name, "Read")
	}
	if !strings.Contains(args, "file_path") {
		t.Errorf("expected args to contain file_path, got %q", args)
	}
	if !strings.Contains(args, "e:\\\\test.txt") {
		t.Errorf("expected args to contain the file path, got %q", args)
	}
}

// TestConsumeEventServerSideToolCallPrefersExplicitArguments 验证当 arguments 字段非空时优先使用它
func TestConsumeEventServerSideToolCallPrefersExplicitArguments(t *testing.T) {
	acc := NewGLMEventAccumulator("model", "", nil, false, nil)

	// name 中嵌入参数，但 arguments 字段提供了不同的参数 → 应优先使用 arguments
	event := map[string]any{
		"conversation_id": "test-conv",
		"parts": []any{
			map[string]any{
				"logic_id": "part1",
				"content": []any{
					map[string]any{
						"type": "tool_calls",
						"tool_calls": map[string]any{
							"id":   "tool-1",
							"name": `Read{"file_path":"embedded.txt"}`,
							"arguments": map[string]any{
								"file_path": "explicit.txt",
							},
						},
					},
				},
			},
		},
		"status": "processing",
	}

	acc.ConsumeEvent(event)

	if len(acc.serverSideToolCalls) != 1 {
		t.Fatalf("expected 1 server-side tool call, got %d", len(acc.serverSideToolCalls))
	}

	tc := acc.serverSideToolCalls[0]
	fn, _ := tc["function"].(map[string]any)
	args, _ := fn["arguments"].(string)

	// 应使用 explicit.txt 而非 embedded.txt
	if !strings.Contains(args, "explicit.txt") {
		t.Errorf("expected args to prefer explicit arguments, got %q", args)
	}
	if strings.Contains(args, "embedded.txt") {
		t.Errorf("should not use embedded args when explicit arguments provided, got %q", args)
	}
}

// TestConsumeEventServerSideToolCallTruncatedArgs 复现日志
// glm-go_20260817_003635.log L47 的场景：上游流中断导致 arguments 字符串截断
// （缺少结尾 }），此前会原样透传非法 JSON 并绕过类型矫正（"num": "5" 未变成 5）。
// 修复后应自动补全 JSON 并应用启发式类型矫正。
func TestConsumeEventServerSideToolCallTruncatedArgs(t *testing.T) {
	acc := NewGLMEventAccumulator("model", "", nil, false, nil)

	// L47 的真实数据：arguments 是截断的 JSON 字符串（缺 }），num 是字符串 "5"
	event := map[string]any{
		"conversation_id": "test-conv",
		"parts": []any{
			map[string]any{
				"logic_id": "part1",
				"content": []any{
					map[string]any{
						"type": "tool_calls",
						"tool_calls": map[string]any{
							"id":   "tool-1",
							"name": "WebSearch",
							"arguments": `{"query": "灵巧手 仿生手 机器人技术 发展趋势", "num": "5", "lr": "lang_zh"`,
						},
					},
				},
			},
		},
		"status": "processing",
	}

	acc.ConsumeEvent(event)

	if len(acc.serverSideToolCalls) != 1 {
		t.Fatalf("expected 1 server-side tool call, got %d", len(acc.serverSideToolCalls))
	}

	tc := acc.serverSideToolCalls[0]
	fn, _ := tc["function"].(map[string]any)
	args, _ := fn["arguments"].(string)

	// 修复后必须是完整可解析的 JSON
	var parsed map[string]any
	if err := sonic.UnmarshalString(args, &parsed); err != nil {
		t.Fatalf("repaired args should be valid JSON, got %q: %v", args, err)
	}
	// 启发式矫正应将字符串 "5" 转为数字 5（JSON 反序列化后为 float64）
	if num, ok := parsed["num"].(float64); !ok || num != 5 {
		t.Errorf("num = %v (%T), want number 5; args=%q", parsed["num"], parsed["num"], args)
	}
	if q, _ := parsed["query"].(string); q != "灵巧手 仿生手 机器人技术 发展趋势" {
		t.Errorf("query = %q", q)
	}
}

// TestCoerceParamValue 验证参数类型矫正逻辑
func TestCoerceParamValue(t *testing.T) {
	tests := []struct {
		name         string
		value        any
		expectedType string
		want         any
	}{
		// integer
		{"integer 字符串", "5", "integer", int64(5)},
		{"integer 浮点字符串", "5.0", "integer", int64(5)},
		{"integer 浮点数", float64(5), "integer", int64(5)},
		{"integer 无效字符串", "abc", "integer", "abc"},
		{"integer 空字符串", "", "integer", ""},
		// number
		{"number 字符串", "3.14", "number", 3.14},
		{"number 无效", "abc", "number", "abc"},
		// boolean
		{"boolean true", "true", "boolean", true},
		{"boolean false", "false", "boolean", false},
		{"boolean 1", "1", "boolean", true},
		{"boolean yes", "yes", "boolean", true},
		{"boolean 无效", "maybe", "boolean", "maybe"},
		// array：双重序列化的 JSON 字符串 → 数组（复现日志 L23 的场景）
		{
			name:         "array 双重序列化字符串（日志场景）",
			value:        `[{"id":"1","content":"task","status":"pending","priority":"high"}]`,
			expectedType: "array",
			want:         []any{map[string]any{"id": "1", "content": "task", "status": "pending", "priority": "high"}},
		},
		{
			name:         "array 空数组字符串",
			value:        `[]`,
			expectedType: "array",
			want:         []any{},
		},
		{
			name:         "array 已经是数组（不变）",
			value:        []any{map[string]any{"id": "1"}},
			expectedType: "array",
			want:         []any{map[string]any{"id": "1"}},
		},
		{
			name:         "array 空字符串",
			value:        "",
			expectedType: "array",
			want:         "",
		},
		{
			name:         "array 非数组 JSON（对象）",
			value:        `{"id":"1"}`,
			expectedType: "array",
			want:         `{"id":"1"}`, // 不是数组，无法转换，返回原值
		},
		// object：双重序列化的 JSON 字符串 → 对象
		{
			name:         "object 双重序列化字符串",
			value:        `{"file_path":"/a/b.go","content":"x"}`,
			expectedType: "object",
			want:         map[string]any{"file_path": "/a/b.go", "content": "x"},
		},
		{
			name:         "object 已经是 map（不变）",
			value:        map[string]any{"key": "val"},
			expectedType: "object",
			want:         map[string]any{"key": "val"},
		},
		{
			name:         "object 空字符串",
			value:        "",
			expectedType: "object",
			want:         "",
		},
		// 未知类型：返回原值
		{"未知类型", "value", "custom", "value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := coerceParamValue(tt.value, tt.expectedType)
			if !deepEqual(got, tt.want) {
				t.Errorf("coerceParamValue(%v, %q) = %v (%T), want %v (%T)",
					tt.value, tt.expectedType, got, got, tt.want, tt.want)
			}
		})
	}
}

// TestCoerceParamValueArrayDoubleSerialized 复现日志 L23 的完整场景：
// TodoWrite 工具的 todos 参数被 GLM 双重序列化为字符串，验证能被矫正回数组
func TestCoerceParamValueArrayDoubleSerialized(t *testing.T) {
	// 日志 L23 中的实际数据：todos 是字符串，内容是转义的 JSON 数组
	doubleSerialized := `[{"id": "1", "content": "Read docs", "status": "in_progress", "priority": "high"}, {"id": "2", "content": "Create folder", "status": "pending", "priority": "high"}]`

	result := coerceParamValue(doubleSerialized, "array")

	arr, ok := result.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T: %v", result, result)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 elements, got %d", len(arr))
	}

	// 验证第一个元素
	first, ok := arr[0].(map[string]any)
	if !ok {
		t.Fatalf("expected first element to be map[string]any, got %T", arr[0])
	}
	if first["id"] != "1" {
		t.Errorf("first element id = %v, want %q", first["id"], "1")
	}
	if first["content"] != "Read docs" {
		t.Errorf("first element content = %v, want %q", first["content"], "Read docs")
	}
	if first["status"] != "in_progress" {
		t.Errorf("first element status = %v, want %q", first["status"], "in_progress")
	}
}

// deepEqual 深度比较两个值是否相等（处理 []any 和 map[string]any）
func deepEqual(a, b any) bool {
	// 处理 nil
	if a == nil || b == nil {
		return a == b
	}
	// 比较 []any
	aa, aok := a.([]any)
	ba, bok := b.([]any)
	if aok && bok {
		if len(aa) != len(ba) {
			return false
		}
		for i := range aa {
			if !deepEqual(aa[i], ba[i]) {
				return false
			}
		}
		return true
	}
	// 比较 map[string]any
	am, amok := a.(map[string]any)
	bm, bmok := b.(map[string]any)
	if amok && bmok {
		if len(am) != len(bm) {
			return false
		}
		for k, v := range am {
			if !deepEqual(v, bm[k]) {
				return false
			}
		}
		return true
	}
	// 其他类型直接比较
	return a == b
}

// TestCoerceParamValueHeuristic 验证无 schema 时的启发式类型矫正
func TestCoerceParamValueHeuristic(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  any
	}{
		{"整数字符串", "5", int64(5)},
		{"负整数字符串", "-3", int64(-3)},
		{"正整数字符串(带+号)", "+10", int64(10)},
		{"浮点字符串", "3.14", 3.14},
		{"负浮点字符串", "-0.5", -0.5},
		{"布尔true小写", "true", true},
		{"布尔false小写", "false", false},
		{"布尔TRUE大写", "TRUE", true},
		{"布尔False大写", "False", false},
		{"普通字符串不变", "hello", "hello"},
		{"URL字符串不变", "https://example.com", "https://example.com"},
		{"日期字符串不变", "2026-08-01", "2026-08-01"},
		{"ID字符串不变", "call_123", "call_123"},
		{"带空格的整数", "  42  ", int64(42)},
		{"空字符串不变", "", ""},
		{"零字符串", "0", int64(0)},
		{"数字非字符串(int64)", int64(42), int64(42)},
		{"数字非字符串(float64)", 3.14, 3.14},
		{"布尔非字符串", true, true},
		{"含字母的数字串", "5a", "5a"},
		{"科学计数法(非纯整数)", "1e5", "1e5"},
		{"十六进制(非纯整数)", "0x1F", "0x1F"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := coerceParamValueHeuristic(tt.value)
			if !deepEqual(got, tt.want) {
				t.Errorf("coerceParamValueHeuristic(%v) = %v (%T), want %v (%T)",
					tt.value, got, got, tt.want, tt.want)
			}
		})
	}
}

// TestCoerceToolCallArgs 验证 schema 优先 + 启发式兜底的参数矫正
func TestCoerceToolCallArgs(t *testing.T) {
	toolParamTypes := map[string]map[string]string{
		"SearchWithSchema": {
			"num":    "integer",
			"query":  "string",
			"active": "boolean",
		},
	}

	tests := []struct {
		name     string
		args     map[string]any
		toolName string
		want     map[string]any
	}{
		{
			name:     "有schema-按schema矫正",
			args:     map[string]any{"num": "5", "query": "test", "active": "true"},
			toolName: "SearchWithSchema",
			want:     map[string]any{"num": int64(5), "query": "test", "active": true},
		},
		{
			name:     "有schema但参数未定义类型-启发式兜底",
			args:     map[string]any{"num": "5", "unknown": "10"},
			toolName: "SearchWithSchema",
			want:     map[string]any{"num": int64(5), "unknown": int64(10)},
		},
		{
			name:     "无schema(日志场景WebSearch)-启发式矫正",
			args:     map[string]any{"query": "Tesla Optimus", "num": "5"},
			toolName: "WebSearch",
			want:     map[string]any{"query": "Tesla Optimus", "num": int64(5)},
		},
		{
			name:     "id参数被跳过",
			args:     map[string]any{"id": "123", "num": "5"},
			toolName: "WebSearch",
			want:     map[string]any{"id": "123", "num": int64(5)},
		},
		{
			name:     "空args",
			args:     map[string]any{},
			toolName: "WebSearch",
			want:     map[string]any{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := coerceToolCallArgs(tt.args, tt.toolName, toolParamTypes)
			if len(got) != len(tt.want) {
				t.Fatalf("args length = %d, want %d (got=%v, want=%v)",
					len(got), len(tt.want), got, tt.want)
			}
			for k, wantV := range tt.want {
				if gotV, exists := got[k]; !exists {
					t.Errorf("args missing key %q", k)
				} else if !deepEqual(gotV, wantV) {
					t.Errorf("args[%q] = %v (%T), want %v (%T)",
						k, gotV, gotV, wantV, wantV)
				}
			}
		})
	}
}
