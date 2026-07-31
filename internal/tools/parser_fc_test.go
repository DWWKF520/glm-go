package tools

import (
	"strings"
	"testing"
)

func TestSplitStreamText_FunctionCalls(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		final        bool
		wantVisible  string
		wantCalls    int
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
			name:        "不完整块 final=false 应保留在 remainder",
			input:       "text [function_calls]\n[call:Skill]",
			final:       false,
			wantVisible: "text ",
			wantCalls:   0,
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
