package openai

// content.go — OpenAI chat 消息 content 的多态表示
//
// OpenAI 规范中 message.content 有三种常见形态：
//  1. 字符串："content": "hello"
//  2. 内容分块数组："content": [{"type": "text", "text": "..."}]
//  3. null / 字段缺失（assistant 消息带 tool_calls 时的常见形态）
//
// 另有极少数客户端发送对象等其他 JSON 形态，用 Raw 兜底保留原始字节。
// 通过 Text() 统一提取纯文本，替代旧的 ExtractTextContent 类型断言链。

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/bytedance/sonic"
)

// contentKind 标记 Content 的实际形态
type contentKind uint8

const (
	contentAbsent contentKind = iota // 字段缺失（零值）
	contentNull                      // 显式 null
	contentString                    // 字符串形态
	contentParts                     // 内容分块数组形态
	contentRaw                       // 其他 JSON 形态（对象等），保留原始字节
)

// Content 多态消息内容：string | []ContentPart | null | 其他 JSON
type Content struct {
	kind  contentKind
	text  string
	parts []ContentPart
	raw   json.RawMessage
}

// StringContent 构造字符串形态的 Content
func StringContent(s string) Content {
	return Content{kind: contentString, text: s}
}

// PartsContent 构造内容分块数组形态的 Content
func PartsContent(parts ...ContentPart) Content {
	return Content{kind: contentParts, parts: parts}
}

// UnmarshalJSON 兼容 string / []ContentPart / null / 其他 JSON 形态
func (c *Content) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		c.kind = contentNull
		return nil
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := sonic.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		c.kind = contentString
		c.text = s
	case '[':
		var parts []ContentPart
		if err := sonic.Unmarshal(trimmed, &parts); err != nil {
			return err
		}
		c.kind = contentParts
		c.parts = parts
	default:
		// 对象等其他形态：保留原始字节，Text() 时以紧凑 JSON 输出
		raw := make(json.RawMessage, len(trimmed))
		copy(raw, trimmed)
		c.kind = contentRaw
		c.raw = raw
	}
	return nil
}

// MarshalJSON 按实际形态还原 JSON
func (c Content) MarshalJSON() ([]byte, error) {
	switch c.kind {
	case contentString:
		return sonic.Marshal(c.text)
	case contentParts:
		return sonic.Marshal(c.parts)
	case contentRaw:
		if c.raw == nil {
			return []byte("null"), nil
		}
		return c.raw, nil
	default:
		return []byte("null"), nil
	}
}

// Text 提取纯文本：
//   - 字符串形态：原文
//   - 分块形态：拼接所有 type=text 分块（过滤空文本，换行连接）
//   - 其他 JSON 形态：紧凑 JSON 字符串（与原 SafeJSONDumpsCompact 行为一致）
//   - null / 缺失：空字符串
func (c Content) Text() string {
	switch c.kind {
	case contentString:
		return c.text
	case contentParts:
		var texts []string
		for _, p := range c.parts {
			if p.Type == "text" && p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	case contentRaw:
		var v any
		if err := sonic.Unmarshal(c.raw, &v); err != nil {
			return "{}"
		}
		s, err := sonic.MarshalString(v)
		if err != nil {
			return "{}"
		}
		return s
	default:
		return ""
	}
}

// IsEmpty 报告内容是否为 null、缺失或空
func (c Content) IsEmpty() bool {
	switch c.kind {
	case contentString:
		return c.text == ""
	case contentParts:
		return len(c.parts) == 0
	case contentRaw:
		return len(c.raw) == 0
	default:
		return true
	}
}

// Parts 返回分块形态的内容分块（非分块形态返回 nil）
func (c Content) Parts() []ContentPart {
	return c.parts
}

// ContentPart 内容分块，支持 text / image_url / file 三种类型
type ContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *ImageURLPart `json:"image_url,omitempty"`
	FileURL  *FileURLPart  `json:"file_url,omitempty"`
}

// ImageURLPart image_url 分块的载荷
type ImageURLPart struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// FileURLPart file 分块的载荷
type FileURLPart struct {
	URL string `json:"url"`
}
