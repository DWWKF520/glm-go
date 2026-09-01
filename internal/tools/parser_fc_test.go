package tools

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
)

func TestSplitStreamText_FunctionCalls(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		final         bool
		wantVisible   string
		wantCalls     int
		wantRemainder bool
	}{
		{
			name:        "完整 [function_calls] 块",
			input:       "[function_calls]\n[call:Skill]{\"name\":\"report-page\"}[/call]\n[/function_calls]",
			final:       false,
			wantVisible: "",
			wantCalls:   1,
		},
		{
			name:        "带前导文本的 [function_calls] 块",
			input:       "Here is the result:\n[function_calls]\n[call:Skill]{\"name\":\"report-page\"}[/call]\n[/function_calls]",
			final:       false,
			wantVisible: "Here is the result:\n",
			wantCalls:   1,
		},
		{
			name:          "不完整块 final=false 应保留在 remainder",
			input:         "text [function_calls]\n[call:Skill]",
			final:         false,
			wantVisible:   "text ",
			wantCalls:     0,
			wantRemainder: true,
		},
		{
			name:        "不完整块 final=true 应尝试挽救",
			input:       "text [function_calls]\n[call:Skill]{\"name\":\"x\"}[/call]",
			final:       true,
			wantVisible: "text ",
			wantCalls:   1,
		},
		{
			name:        "多个调用",
			input:       "[function_calls]\n[call:A]{\"k\":\"v1\"}[/call]\n[call:B]{\"k\":\"v2\"}[/call]\n[/function_calls]",
			final:       false,
			wantVisible: "",
			wantCalls:   2,
		},
		{
			name:        "纯文本无标记",
			input:       "Hello, how can I help?",
			final:       false,
			wantVisible: "Hello, how can I help?",
			wantCalls:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			visible, remainder, calls := SplitStreamText(tt.input, tt.final)
			if len(calls) != tt.wantCalls {
				t.Errorf("got %d calls, want %d", len(calls), tt.wantCalls)
			}
			if visible != tt.wantVisible {
				t.Errorf("visible = [%s], want [%s]", visible, tt.wantVisible)
			}
			if tt.wantRemainder && remainder == "" {
				t.Error("expected non-empty remainder but got empty")
			}
			if !tt.wantRemainder && remainder != "" {
				t.Errorf("expected empty remainder but got [%s]", remainder)
			}
		})
	}
}

func TestStreamingToolParser_FunctionCalls(t *testing.T) {
	parser := NewStreamingToolParser()

	// 模拟流式输入
	chunks := []string{
		"Let me ",
		"search for ",
		"that.\n[",
		"function_calls",
		"]\n[call:",
		"Skill]{\"name\":",
		"\"report-page\"}",
		"[/call]\n[/",
		"function_calls]",
	}

	var allVisible string
	for _, chunk := range chunks {
		visible := parser.Consume(chunk)
		allVisible += visible
	}

	if parser.IsToolCallCompleted() != true {
		t.Error("expected tool call to be completed")
	}
	if len(parser.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(parser.ToolCalls))
	}
	fn, _ := parser.ToolCalls[0]["function"].(map[string]any)
	if fn == nil || fn["name"] != "Skill" {
		t.Errorf("expected tool name Skill, got %v", fn)
	}
	// 可见文本不应包含 function_calls 标记
	if len(allVisible) > 0 && strings.Contains(allVisible, "[function_calls]") {
		t.Errorf("visible text should not contain [function_calls]: %s", allVisible)
	}
}

func TestParseToolCallsFromText_FunctionCalls(t *testing.T) {
	text := "Some text [function_calls]\n[call:Skill]{\"name\":\"report-page\"}[/call]\n[/function_calls] end"
	cleaned, calls := ParseToolCallsFromText(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if strings.Contains(cleaned, "[function_calls]") {
		t.Errorf("cleaned text should not contain [function_calls]: %s", cleaned)
	}
}

// TestExtractEmbeddedArgsFromName 验证 name 字段内嵌参数的分离逻辑。
func TestExtractEmbeddedArgsFromName(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantName  string
		wantArgs  map[string]any
		wantOK    bool
		knownTool string
	}{
		{
			// 真实场景：GLM 把内联 [call:todo_write]{...} 标记整体塞进 name，
			// 残留 ']' 和 "call:" 前缀；修复前会掉进 '_' 启发式，
			// 错误产出 name="call:todo"、args={"write]{"todos": [...]}
			name:     "内联标记整体塞入 name（call: 前缀 + 尾部 ]）",
			input:    `call:todo_write]{"todos":[{"content":"加载 report-page 技能","status":"completed"}]}`,
			wantName: "todo_write",
			wantArgs: map[string]any{
				"todos": []any{map[string]any{"content": "加载 report-page 技能", "status": "completed"}},
			},
			wantOK: true,
		},
		{
			name:     "仅残留尾部 ]",
			input:    `Read]{"file_path":"/tmp/a.txt"}`,
			wantName: "Read",
			wantArgs: map[string]any{"file_path": "/tmp/a.txt"},
			wantOK:   true,
		},
		{
			name:     "常规对象格式不受影响",
			input:    `Read{"file_path":"C:\\path\\file.txt"}`,
			wantName: "Read",
			wantArgs: map[string]any{"file_path": `C:\path\file.txt`},
			wantOK:   true,
		},
		{
			name:      "数组格式不受影响",
			input:     `TodoWritetodos[{"id":"1","content":"x"}]`,
			knownTool: "TodoWrite",
			wantName:  "TodoWrite",
			wantArgs:  map[string]any{"todos": []any{map[string]any{"id": "1", "content": "x"}}},
			wantOK:    true,
		},
		{
			// 清理后仍非法（如纯垃圾前缀）不应被采用，避免误伤
			name:     "清理后仍非法则不提取",
			input:    `bad name!]{"a":1}`,
			wantName: `bad name!]{"a":1}`,
			wantOK:   false,
		},
		{
			name:     "无内嵌参数原样返回",
			input:    "todo_write",
			wantName: "todo_write",
			wantOK:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			known := []string{}
			if tt.knownTool != "" {
				known = append(known, tt.knownTool)
			}
			gotName, gotArgs, ok := ExtractEmbeddedArgsFromName(tt.input, known...)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (name=%q args=%v)", ok, tt.wantOK, gotName, gotArgs)
			}
			if gotName != tt.wantName {
				t.Errorf("name = %q, want %q", gotName, tt.wantName)
			}
			if tt.wantOK && !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Errorf("args = %#v, want %#v", gotArgs, tt.wantArgs)
			}
		})
	}
}

// TestRepairTruncatedJSON 验证畸形 JSON 的自动修复（委托 json-repair 库）。
// 采用值比较：库会重建 JSON（键序可能重排），字符串比较无意义。
func TestRepairTruncatedJSON(t *testing.T) {
	// want 为 nil 表示期望无法修复、原样返回
	tests := []struct {
		name  string
		input string
		want  any
	}{
		{
			// 用户日志 glm-go_20260817_003635.log L47 的真实场景：缺少结尾 }
			name:  "缺少结尾大括号",
			input: `{"query": "灵巧手 仿生手 机器人技术 发展趋势", "num": "5", "lr": "lang_zh"`,
			want:  map[string]any{"query": "灵巧手 仿生手 机器人技术 发展趋势", "num": "5", "lr": "lang_zh"},
		},
		{
			name:  "字符串被截断",
			input: `{"query": "灵巧手 仿`,
			want:  map[string]any{"query": "灵巧手 仿"},
		},
		{
			name:  "字符串末尾悬挂反斜杠",
			input: `{"path": "e:\\test\`,
			want:  map[string]any{"path": `e:\test`},
		},
		{
			name:  "尾随逗号",
			input: `{"a": 1,`,
			want:  map[string]any{"a": float64(1)},
		},
		{
			// 库比旧手写实现更激进：悬空键补空字符串而非删除
			name:  "悬空键（冒号后无值）",
			input: `{"a": 1, "b":`,
			want:  map[string]any{"a": float64(1), "b": ""},
		},
		{
			name:  "键名后截断",
			input: `{"a": 1, "b"`,
			want:  map[string]any{"a": float64(1), "b": ""},
		},
		{
			name:  "嵌套结构截断",
			input: `{"todos": [{"id": "1"`,
			want:  map[string]any{"todos": []any{map[string]any{"id": "1"}}},
		},
		{
			name:  "数组截断",
			input: `{"a": [1, 2`,
			want:  map[string]any{"a": []any{float64(1), float64(2)}},
		},
		{
			// 库的额外能力：单引号 + 缺逗号
			name:  "单引号和缺逗号",
			input: `{'a': 'x' 'b': 'y'`,
			want:  map[string]any{"a": "x", "b": "y"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RepairTruncatedJSON(tt.input)
			if tt.want == nil {
				if got != tt.input {
					t.Errorf("RepairTruncatedJSON(%q) = %q, want 原样返回", tt.input, got)
				}
				return
			}
			var parsed any
			if err := sonic.UnmarshalString(got, &parsed); err != nil {
				t.Fatalf("repaired result is not valid JSON: %q: %v", got, err)
			}
			if !reflect.DeepEqual(parsed, tt.want) {
				t.Errorf("RepairTruncatedJSON(%q) parsed = %#v, want %#v", tt.input, parsed, tt.want)
			}
		})
	}

	// 合法 JSON 原样返回（不经过库，保持调用方 repaired != original 语义）
	t.Run("合法JSON原样返回", func(t *testing.T) {
		input := `{"a": 1, "b": [true, null]}`
		if got := RepairTruncatedJSON(input); got != input {
			t.Errorf("valid JSON should be returned as-is, got %q", got)
		}
	})

	// 空输入 / 纯文本（无可恢复内容）原样返回
	for _, input := range []string{"", "   ", "Sure, here is the result:"} {
		if got := RepairTruncatedJSON(input); got != input {
			t.Errorf("RepairTruncatedJSON(%q) = %q, want 原样返回", input, got)
		}
	}
}
