package openai

// images.go — OpenAI Images API 请求/响应类型

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/bytedance/sonic"
)

// ImageGenerationRequest OpenAI images/generations 请求体。
// style / scene 为本代理扩展参数，映射到 GLM cogview 的风格与场景。
type ImageGenerationRequest struct {
	Model          string  `json:"model"`
	Prompt         string  `json:"prompt"`
	N              FlexInt `json:"n,omitempty"`
	Size           string  `json:"size,omitempty"`
	ResponseFormat string  `json:"response_format,omitempty"`
	Style          string  `json:"style,omitempty"`
	Scene          string  `json:"scene,omitempty"`
}

// FlexInt 兼容整数与浮点数（如 2.0）的整型字段。
// 浮点值向零截断（与原 coercePositiveInt 的 int(v) 行为一致）。
type FlexInt int

// UnmarshalJSON 实现 json.Unmarshaler
func (f *FlexInt) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		*f = 0
		return nil
	}
	if i, err := strconv.ParseInt(string(trimmed), 10, 64); err == nil {
		*f = FlexInt(i)
		return nil
	}
	var fl float64
	if err := sonic.Unmarshal(trimmed, &fl); err != nil {
		return fmt.Errorf("无法将 %s 解析为整数", string(trimmed))
	}
	*f = FlexInt(fl)
	return nil
}

// ImagesResponse OpenAI images/generations 响应体
type ImagesResponse struct {
	Created int64       `json:"created"`
	Data    []ImageData `json:"data"`
}

// ImageData 单张生成图片结果
type ImageData struct {
	URL           string `json:"url,omitempty"`
	B64JSON       string `json:"b64_json,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}
