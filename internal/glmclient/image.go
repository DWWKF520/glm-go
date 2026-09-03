package glmclient

// image.go — 图片生成
//
// 将 OpenAI 格式的 image generation 请求转发到 GLM 的 cogview 绘图接口，支持：
//   - 多种图片尺寸和宽高比
//   - 风格和场景参数
//   - URL 和 base64 响应格式
//   - 多账号故障转移

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"

	"glm2api/internal/auth"
	"glm2api/internal/logging"
	"glm2api/internal/translator"
)

// imageSizeToAspectRatio 将 OpenAI 格式的图片尺寸映射为 GLM cogview 使用的宽高比字符串
var imageSizeToAspectRatio = map[string]string{
	"1024x1024": "1:1",
	"1024x1536": "2:3",
	"1536x1024": "3:2",
	"1024x1792": "9:16",
	"1792x1024": "16:9",
}

// sizePattern 匹配 "宽x高" 格式的图片尺寸字符串（如 "1024x1024"）
var sizePattern = regexp.MustCompile(`^\d+x\d+$`)

// GenerateImages 生成图片
// 将 OpenAI 格式的 image generation 请求转发到 GLM 的 cogview 绘图接口
//
// 流程：
//  1. 获取队列槽位
//  2. 打开图片生成流
//  3. 读取所有事件直到 finish
//  4. 从累积器中提取图片 URL 并构建响应
func (c *Client) GenerateImages(ctx context.Context, payload map[string]any) (map[string]any, error) {
	lease, err := c.RequestQueue.Acquire(fmt.Sprintf("image:%v", payload["model"]))
	if err != nil {
		return nil, err
	}

	response, assistantID, err := c.openImageStream(ctx, payload, c.getPreferredAccountIndex(lease.ticket))
	if err != nil {
		lease.Release()
		return nil, err
	}

	accumulator := translator.NewGLMEventAccumulator(
		getModelName(payload, c.config.GLMImageModelName),
		"",
		nil, // 图片生成无需工具定义
		c.config.DebugDumpAll,
		c.logger,
	)

	defer func() {
		response.Body.Close()
		// 异步删除会话：删除是后台清理操作，不应阻塞图片结果返回给调用方
		go c.DeleteConversation(context.Background(), accumulator.ConversationID, assistantID)
		lease.Release()
	}()

	// 用 ctx 标记"主动结束"：收到 finish 后 cancel()，
	// 这样 iterSSEEvents 的读流 goroutine 在 body 被关闭时不会误报 WARNING。
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	var finalEvent map[string]any
	events, _ := c.iterSSEEvents(streamCtx, response.Body)
	for event := range events {
		if event == nil {
			continue
		}
		accumulator.ConsumeEvent(event)
		if status, _ := event["status"].(string); status == "finish" {
			finalEvent = event
			// 主动取消：通知读流 goroutine 即将关闭 body，无需记录中断告警
			streamCancel()
			break
		}
	}
	return c.buildImagesResponse(payload, finalEvent, accumulator)
}

// openImageStream 打开与 GLM 的图片生成连接
// 构建 cogview 绘图请求并通过账号故障转移发送
func (c *Client) openImageStream(ctx context.Context, payload map[string]any, preferredAccountIndex *int) (*http.Response, string, error) {
	prompt, _ := payload["prompt"].(string)
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, "", &UpstreamAPIError{StatusCode: 400, Message: "图片生成请求缺少 prompt"}
	}

	// 解析图片尺寸并转换为宽高比
	size, _ := payload["size"].(string)
	if size == "" {
		size = "1024x1024"
	}
	size = strings.ToLower(strings.TrimSpace(size))
	aspectRatio := c.resolveAspectRatio(size)

	userModel, _ := payload["model"].(string)
	if strings.TrimSpace(userModel) == "" {
		userModel = c.config.GLMImageModelName
	}

	// 构建 GLM 图片生成请求体
	requestBody := map[string]any{
		"assistant_id":    c.config.GLMImageAssistantID,
		"conversation_id": "",
		"project_id":      "",
		"chat_type":       "user_chat",
		"meta_data": map[string]any{
			"cogview": map[string]any{
				"aspect_ratio":       aspectRatio,
				"style":              c.resolveImageStyle(payload),
				"scene":              c.resolveImageScene(payload),
				"chat_model":         "",
				"rm_label_watermark": false,
			},
			"is_test":             false,
			"input_question_type": "xxxx",
			"channel":             "",
			"draft_id":            "",
			"chat_mode":           "",
			"is_networking":       false,
			"quote_log_id":        "",
			"platform":            "pc",
		},
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": prompt},
				},
			},
		},
	}
	bodyBytes, _ := sonic.Marshal(requestBody)

	c.logger.Info("转发绘图请求", "model", userModel, "assistant_id", c.config.GLMImageAssistantID, "size", size, "n", payload["n"])
	logging.DebugDump(c.logger, c.config.DebugDumpAll, "OpenAI 原始 image 请求 payload", payload)
	logging.DebugDump(c.logger, c.config.DebugDumpAll, "转发到 GLM 的 image 原始请求体", bodyBytes)

	operation := func(accountIndex int, accessToken string) (any, error) {
		timestamp, nonce, sign := auth.BuildSign()
		req, err := http.NewRequestWithContext(ctx, "POST", c.config.ChatStreamURL(), bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		headers := c.Auth.GetBrowserHeaders("")
		headers["Authorization"] = "Bearer " + accessToken
		headers["X-Device-Id"] = uuid.New().String()
		headers["X-Nonce"] = nonce
		headers["X-Request-Id"] = uuid.New().String()
		headers["X-Sign"] = sign
		headers["X-Timestamp"] = timestamp
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			errorPayload := c.readErrorPayload(resp)
			resp.Body.Close()
			message := c.buildErrorMessage(resp.StatusCode, errorPayload)
			return nil, &UpstreamAPIError{StatusCode: resp.StatusCode, Message: message, Payload: errorPayload}
		}
		return c.prepareChatResponse(resp)
	}

	respAny, err := c.callWithAccountFailover(ctx, fmt.Sprintf("image:%s", userModel), operation, preferredAccountIndex)
	if err != nil {
		return nil, "", err
	}
	resp, ok := respAny.(*http.Response)
	if !ok || resp == nil {
		return nil, "", fmt.Errorf("无效的响应类型")
	}
	return resp, c.config.GLMImageAssistantID, nil
}

// buildImagesResponse 构建图片生成的 OpenAI 格式响应
// 从 GLM accumulator 中提取生成的图片，构建符合 OpenAI Images API 规范的响应体
//
// 支持两种响应格式：
//   - "url": 返回图片的直接 URL
//   - "b64_json": 下载图片并转换为 base64 编码
func (c *Client) buildImagesResponse(requestPayload map[string]any, finalEvent map[string]any, accumulator *translator.GLMEventAccumulator) (map[string]any, error) {
	requestedCount := coercePositiveInt(requestPayload["n"], 1, 10)
	responseFormat, _ := requestPayload["response_format"].(string)
	responseFormat = strings.ToLower(strings.TrimSpace(responseFormat))
	if responseFormat == "" {
		responseFormat = "url"
	}
	created := time.Now().Unix()

	var data []map[string]any
	orderedParts := accumulator.GetOrderedParts()

	for _, part := range orderedParts {
		if len(data) >= requestedCount {
			break
		}
		partStatus, _ := part["status"].(string)
		if partStatus != "finish" {
			continue
		}
		contentItems, ok := part["content"].([]any)
		if !ok {
			continue
		}
		for _, contentItem := range contentItems {
			if len(data) >= requestedCount {
				break
			}
			content, ok := contentItem.(map[string]any)
			if !ok {
				continue
			}
			contentType, _ := content["type"].(string)
			if contentType != "image" {
				continue
			}
			images, _ := content["image"].([]any)
			revisedPrompt, _ := content["code"].(string)
			revisedPrompt = strings.TrimSpace(revisedPrompt)
			for _, img := range images {
				if len(data) >= requestedCount {
					break
				}
				image, ok := img.(map[string]any)
				if !ok {
					continue
				}
				imageURL, _ := image["image_url"].(string)
				imageURL = strings.TrimSpace(imageURL)
				if imageURL == "" {
					continue
				}
				item := map[string]any{}
				if responseFormat == "b64_json" {
					b64, err := c.downloadImageAsBase64(imageURL)
					if err != nil {
						return nil, err
					}
					item["b64_json"] = b64
				} else {
					item["url"] = imageURL
				}
				if revisedPrompt != "" {
					item["revised_prompt"] = revisedPrompt
				}
				data = append(data, item)
			}
		}
	}

	if len(data) == 0 {
		return nil, &UpstreamAPIError{
			StatusCode: 502,
			Message:    "GLM 绘图请求已完成，但未返回可用图片结果。",
			Payload:    finalEvent,
		}
	}
	c.logger.Info("绘图完成", "返回图片数", len(data))
	return map[string]any{
		"created": created,
		"data":    data,
	}, nil
}

// resolveAspectRatio 将 OpenAI 格式的尺寸字符串解析为 GLM cogview 使用的宽高比
// 支持预定义尺寸（如 "1024x1024" → "1:1"）和自定义 WxH 格式
func (c *Client) resolveAspectRatio(size string) string {
	normalized := strings.ToLower(strings.TrimSpace(size))
	if ar, ok := imageSizeToAspectRatio[normalized]; ok {
		return ar
	}
	if sizePattern.MatchString(normalized) {
		parts := strings.SplitN(normalized, "x", 2)
		w, _ := strconv.Atoi(parts[0])
		h, _ := strconv.Atoi(parts[1])
		if w < 1 {
			w = 1
		}
		if h < 1 {
			h = 1
		}
		return fmt.Sprintf("%d:%d", w, h)
	}
	return "1:1"
}

// resolveImageStyle 从 payload 中提取图片风格参数
func (c *Client) resolveImageStyle(payload map[string]any) string {
	style, _ := payload["style"].(string)
	style = strings.ToLower(strings.TrimSpace(style))
	if style == "" {
		return "none"
	}
	return style
}

// resolveImageScene 从 payload 中提取图片场景参数
func (c *Client) resolveImageScene(payload map[string]any) string {
	scene, _ := payload["scene"].(string)
	scene = strings.ToLower(strings.TrimSpace(scene))
	if scene == "" {
		return "none"
	}
	return scene
}

// downloadImageAsBase64 下载图片并转换为 base64 编码字符串
// 用于 OpenAI Images API 的 b64_json 响应格式
func (c *Client) downloadImageAsBase64(imageURL string) (string, error) {
	resp, err := c.httpClient.Get(imageURL)
	if err != nil {
		return "", &UpstreamAPIError{StatusCode: 502, Message: fmt.Sprintf("下载图片失败: %s error=%v", imageURL, err)}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", &UpstreamAPIError{StatusCode: 502, Message: fmt.Sprintf("下载图片失败: %s error=%v", imageURL, err)}
	}
	return base64.StdEncoding.EncodeToString(data), nil
}
