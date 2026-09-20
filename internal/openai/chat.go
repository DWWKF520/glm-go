package openai

// chat.go — OpenAI Chat Completions 请求与工具类型
//
// 入站请求在 server 层一次性反序列化为 ChatCompletionRequest，
// 之后全链路传递类型化数据，避免反复的 map 类型断言。

import (
	"bytes"
	"encoding/json"

	"github.com/bytedance/sonic"
)

// ChatCompletionRequest OpenAI chat completion 请求体。
// 仅声明本项目实际消费的字段；其余请求字段（temperature 等）不影响代理功能，忽略。
type ChatCompletionRequest struct {
	Model      string          `json:"model"`
	Messages   []Message       `json:"messages"`
	Tools      []Tool          `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"` // 当前未被下游消费，仅保留用于调试
	Stream     bool            `json:"stream,omitempty"`
}

// Message chat 消息。role: system / user / assistant / tool
type Message struct {
	Role       string     `json:"role"`
	Content    Content    `json:"content"` // 多态字段，struct 上 omitempty 无效；缺失/ null 由 Content.MarshalJSON 还原为 null
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// Tool OpenAI function 工具定义
type Tool struct {
	Type     string       `json:"type"` // 固定 "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction 工具的函数描述与 JSON Schema 参数定义
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema，保留原始字节
}

// ToolCall 消息中的工具调用（assistant 消息）
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction 工具调用的函数名与参数。
// OpenAI 规范中 arguments 是 JSON 字符串；自定义 UnmarshalJSON 兼容部分客户端直接发对象的情况。
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// UnmarshalJSON 兼容 arguments 为 string / object / null / 缺失四种形态。
// object 统一转换为紧凑 JSON 字符串；null 或缺失视为 "{}"。
func (f *ToolCallFunction) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := sonic.Unmarshal(data, &raw); err != nil {
		return err
	}
	f.Name = raw.Name
	args := bytes.TrimSpace(raw.Arguments)
	if len(args) == 0 || bytes.Equal(args, []byte("null")) {
		f.Arguments = "{}"
		return nil
	}
	if args[0] == '"' {
		var s string
		if err := sonic.Unmarshal(args, &s); err != nil {
			return err
		}
		f.Arguments = s
		return nil
	}
	// 对象等形态：统一为紧凑 JSON 字符串
	var v any
	if err := sonic.Unmarshal(args, &v); err != nil {
		return err
	}
	compact, err := sonic.MarshalString(v)
	if err != nil {
		return err
	}
	f.Arguments = compact
	return nil
}
