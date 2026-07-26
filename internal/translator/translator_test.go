package translator

import (
	"strings"
	"testing"
)

// TestLocalFileHintConstant 验证 LocalFileHint 与 Python LOCAL_FILE_HINT 一致
func TestLocalFileHintConstant(t *testing.T) {
	want := "(本地文件，不在服务区上，应该用[function_calls]工具)"
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
	acc := NewGLMEventAccumulator("model", "", false, nil)

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
