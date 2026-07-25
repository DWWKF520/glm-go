package glmclient

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"github.com/bytedance/sonic"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"glm2api/internal/auth"
	"glm2api/internal/config"
	"glm2api/internal/logging"
	"glm2api/internal/tools"
	"glm2api/internal/translator"
)

const (
	FileSizeLimit = 100 * 1024 * 1024
)

var imageSizeToAspectRatio = map[string]string{
	"1024x1024": "1:1",
	"1024x1536": "2:3",
	"1536x1024": "3:2",
	"1024x1792": "9:16",
	"1792x1024": "16:9",
}

var sizePattern = regexp.MustCompile(`^\d+x\d+$`)

// UpstreamAPIError 上游 API 错误
type UpstreamAPIError = auth.UpstreamError

// QueueTimeoutError 队列超时错误
type QueueTimeoutError struct {
	msg string
}

func (e *QueueTimeoutError) Error() string { return e.msg }

// QueueLease 队列租约
type QueueLease struct {
	ticket          int
	releaseCallback func(int)
	released        bool
}

// Release 释放租约
func (l *QueueLease) Release() {
	if l.released {
		return
	}
	l.released = true
	l.releaseCallback(l.ticket)
}

// ConcurrentRequestQueue 并发请求队列
type ConcurrentRequestQueue struct {
	logger          *slog.Logger
	waitTimeout     time.Duration
	maxConcurrency  int
	mu              *sync.Cond
	nextTicket      int
	servingTicket   int
	releasedTickets map[int]bool
}

// NewConcurrentRequestQueue 创建并发请求队列
func NewConcurrentRequestQueue(logger *slog.Logger, waitTimeout time.Duration, maxConcurrency int) *ConcurrentRequestQueue {
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	q := &ConcurrentRequestQueue{
		logger:          logger,
		waitTimeout:     waitTimeout,
		maxConcurrency:  maxConcurrency,
		releasedTickets: map[int]bool{},
	}
	q.mu = sync.NewCond(&sync.Mutex{})
	return q
}

// MaxConcurrency 返回最大并发数
func (q *ConcurrentRequestQueue) MaxConcurrency() int {
	return q.maxConcurrency
}

// Acquire 获取队列租约
func (q *ConcurrentRequestQueue) Acquire(requestName string) (*QueueLease, error) {
	q.mu.L.Lock()
	defer q.mu.L.Unlock()

	ticket := q.nextTicket
	q.nextTicket++
	queueAhead := ticket - (q.servingTicket + q.maxConcurrency) + 1
	if queueAhead < 0 {
		queueAhead = 0
	}
	start := time.Now()

	if queueAhead > 0 {
		q.logger.Info("请求进入 GLM 队列", "ticket", ticket, "ahead", queueAhead, "request", requestName)
	}

	for ticket >= q.servingTicket+q.maxConcurrency {
		remaining := q.waitTimeout - time.Since(start)
		if remaining <= 0 {
			return nil, &QueueTimeoutError{
				msg: fmt.Sprintf("GLM 队列等待超时，前方仍有 %d 个请求，请稍后重试。", ticket-(q.servingTicket+q.maxConcurrency)+1),
			}
		}
		// sync.Cond 不支持带超时的 Wait，使用 goroutine + 定时器
		done := make(chan struct{})
		go func() {
			time.Sleep(remaining)
			close(done)
			q.mu.Broadcast()
		}()
		q.mu.Wait()
		select {
		case <-done:
		default:
		}
	}

	activeSlots := ticket - q.servingTicket + 1
	q.logger.Info("请求获得 GLM 执行槽位", "ticket", ticket, "active", activeSlots, "max", q.maxConcurrency, "request", requestName)
	return &QueueLease{ticket: ticket, releaseCallback: q.release}, nil
}

func (q *ConcurrentRequestQueue) release(ticket int) {
	q.mu.L.Lock()
	defer q.mu.L.Unlock()
	q.releasedTickets[ticket] = true
	for q.releasedTickets[q.servingTicket] {
		delete(q.releasedTickets, q.servingTicket)
		q.servingTicket++
	}
	q.logger.Info("请求离开 GLM 执行槽位", "ticket", ticket)
	q.mu.Broadcast()
}

// Client GLM Web 客户端
type Client struct {
	config       *config.AppConfig
	logger       *slog.Logger
	Auth         *auth.Manager
	RequestQueue *ConcurrentRequestQueue
	httpClient   *http.Client
}

// NewClient 创建 GLM 客户端
func NewClient(cfg *config.AppConfig, logger *slog.Logger) *Client {
	if logger == nil {
		logger = logging.GetLogger("glm2api.client")
	}
	authMgr := auth.NewManager(cfg, logging.GetLogger("glm2api.auth"))
	queue := NewConcurrentRequestQueue(
		logger,
		time.Duration(cfg.GLMQueueWaitTimeout)*time.Second,
		cfg.GLMMaxConcurrency,
	)
	return &Client{
		config:       cfg,
		logger:       logger,
		Auth:         authMgr,
		RequestQueue: queue,
		httpClient:   &http.Client{Timeout: time.Duration(cfg.RequestTimeout) * time.Second},
	}
}

// StreamChatCompletion 流式聊天补全
// 返回一个 channel，持续输出 SSE chunks ([]byte)
func (c *Client) StreamChatCompletion(ctx context.Context, payload map[string]any) (<-chan []byte, error) {
	_, allowedToolNames := c.resolveTools(payload)
	lease, err := c.RequestQueue.Acquire(fmt.Sprintf("stream:%v", payload["model"]))
	if err != nil {
		return nil, err
	}

	response, assistantID, err := c.openChatStream(ctx, payload, c.getPreferredAccountIndex(lease.ticket))
	if err != nil {
		lease.Release()
		return nil, err
	}

	accumulator := translator.NewGLMEventAccumulator(
		fmt.Sprintf("%v", payload["model"]),
		allowedToolNames,
		translator.ExtractRecentUserURL(getMessagesList(payload)),
		c.config.DebugDumpAll,
		c.logger,
	)

	out := make(chan []byte, 32)
	go func() {
		defer close(out)
		defer func() {
			response.Body.Close()
			c.DeleteConversation(ctx, accumulator.ConversationID, assistantID)
			lease.Release()
		}()

		events := c.iterSSEEvents(response.Body)
		for event := range events {
			if event == nil {
				continue
			}
			if err := c.raiseForEventError(event, true); err != nil {
				// 发送错误事件后退出
				out <- []byte(formatErrorChunk(err))
				return
			}
			chunks, status := accumulator.ConsumeEvent(event)
			for _, chunk := range chunks {
				out <- []byte(chunk)
			}
			if status == "finish" || status == "intervene" {
				lastError, _ := event["last_error"].(map[string]any)
				for _, chunk := range accumulator.Finalize(status, lastError) {
					out <- []byte(chunk)
				}
				return
			}
		}
		for _, chunk := range accumulator.Finalize("stop", nil) {
			out <- []byte(chunk)
		}
	}()

	return out, nil
}

// GenerateImages 生成图片
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
		nil,
		"",
		c.config.DebugDumpAll,
		c.logger,
	)

	defer func() {
		response.Body.Close()
		c.DeleteConversation(ctx, accumulator.ConversationID, assistantID)
		lease.Release()
	}()

	var finalEvent map[string]any
	events := c.iterSSEEvents(response.Body)
	for event := range events {
		if event == nil {
			continue
		}
		accumulator.ConsumeEvent(event)
		if status, _ := event["status"].(string); status == "finish" {
			finalEvent = event
			break
		}
	}
	return c.buildImagesResponse(payload, finalEvent, accumulator)
}

// DeleteConversation 删除会话
func (c *Client) DeleteConversation(ctx context.Context, conversationID, assistantID string) {
	if !c.config.GLMDeleteConversation {
		return
	}
	if conversationID == "" {
		c.logger.Warn("跳过删除 GLM 会话：未获取到 conversation_id", "assistant_id", assistantID)
		return
	}
	actualAssistantID := assistantID
	if actualAssistantID == "" {
		actualAssistantID = c.config.GLMAssistantID
	}

	body, _ := sonic.Marshal(map[string]any{
		"assistant_id":    actualAssistantID,
		"conversation_id": conversationID,
	})

	operation := func(accountIndex int, accessToken string) (any, error) {
		timestamp, nonce, sign := auth.BuildSign()
		req, err := http.NewRequestWithContext(ctx, "POST", c.config.DeleteConversationURL(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		headers := c.Auth.GetBrowserHeaders("")
		headers["Authorization"] = "Bearer " + accessToken
		headers["Referer"] = "https://chatglm.cn/main/alltoolsdetail"
		headers["X-Device-Id"] = uuid.New().String()
		headers["X-Nonce"] = nonce
		headers["X-Request-Id"] = uuid.New().String()
		headers["X-Sign"] = sign
		headers["X-Timestamp"] = timestamp
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return c.httpClient.Do(req)
	}

	respAny, err := c.callWithAccountFailover(ctx, "delete_conversation", operation, nil)
	if err != nil {
		c.logger.Warn("删除 GLM 会话失败", "conversation_id", conversationID, "assistant_id", actualAssistantID, "error", err)
		return
	}
	resp, ok := respAny.(*http.Response)
	if !ok || resp == nil {
		return
	}
	defer resp.Body.Close()
	payload, err := c.Auth.ReadJSONResponse(resp)
	if err != nil {
		c.logger.Warn("删除 GLM 会话读取响应失败", "error", err)
		return
	}
	status := getAny(payload, "status")
	code := getAny(payload, "code")
	if (status != nil && status != 0) || (code != nil && code != 0) {
		c.logger.Warn("GLM 会话删除返回非成功状态", "conversation_id", conversationID, "assistant_id", actualAssistantID, "payload", payload)
		return
	}
	c.logger.Info("已删除 GLM 会话", "conversation_id", conversationID, "assistant_id", actualAssistantID)
}

func (c *Client) resolveTools(openaiPayload map[string]any) ([]map[string]any, map[string]bool) {
	var rawTools []map[string]any
	if tools, ok := openaiPayload["tools"].([]any); ok {
		for _, t := range tools {
			if m, ok := t.(map[string]any); ok {
				rawTools = append(rawTools, m)
			}
		}
	}
	blockedToolNames := map[string]bool{}
	for _, name := range c.config.BlockedToolNames {
		name = strings.TrimSpace(name)
		if name != "" {
			blockedToolNames[name] = true
		}
	}
	for n := range tools.BlockedNativeToolNames {
		blockedToolNames[n] = true
	}
	filteredTools := tools.FilterTools(rawTools, blockedToolNames)

	if len(rawTools) > 0 && len(filteredTools) != len(rawTools) {
		var blockedNames []string
		for _, t := range rawTools {
			fn, _ := t["function"].(map[string]any)
			if fn == nil {
				continue
			}
			name, _ := fn["name"].(string)
			name = strings.TrimSpace(name)
			if blockedToolNames[name] {
				blockedNames = append(blockedNames, name)
			}
		}
		if len(blockedNames) > 0 {
			c.logger.Info("已过滤不受支持的工具", "tools", strings.Join(blockedNames, ", "))
		}
	}

	allowedToolNames := map[string]bool{}
	for _, t := range filteredTools {
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		name = strings.TrimSpace(name)
		if name != "" {
			allowedToolNames[name] = true
		}
	}
	return filteredTools, allowedToolNames
}

func (c *Client) openChatStream(ctx context.Context, openaiPayload map[string]any, preferredAccountIndex *int) (*http.Response, string, error) {
	upstreamModel := openaiPayload["model"].(string)
	assistantID := c.config.GLMAssistantID
	filteredTools, _ := c.resolveTools(openaiPayload)

	blockedToolNames := map[string]bool{}
	for _, name := range c.config.BlockedToolNames {
		name = strings.TrimSpace(name)
		if name != "" {
			blockedToolNames[name] = true
		}
	}
	convertedMessages := translator.ConvertMessages(
		getMessagesList(openaiPayload),
		filteredTools,
		blockedToolNames,
		openaiPayload["tool_choice"],
		tools.ServerSideToolNames,
	)

	logging.DebugDump(c.logger, c.config.DebugDumpAll, "OpenAI 原始 chat 请求 payload", openaiPayload)
	logging.DebugDump(c.logger, c.config.DebugDumpAll, "转换后的 GLM messages", convertedMessages)

	refs := c.uploadReferencedFiles(ctx, getMessagesList(openaiPayload))
	if len(refs) > 0 {
		if len(convertedMessages) > 0 {
			contentList, _ := convertedMessages[0]["content"].([]map[string]any)
			for i, item := range contentList {
				if t, _ := item["type"].(string); t == "text" {
					if text, _ := item["text"].(string); text != "" {
						cleaned := translator.ImageRefRE.ReplaceAllString(text, "")
						cleaned = strings.TrimSpace(cleaned)
						contentList[i]["text"] = cleaned
					}
				}
			}
			convertedMessages[0]["content"] = append(contentList, refs...)
			logging.DebugDump(c.logger, c.config.DebugDumpAll, "附加上传引用后的 GLM messages", convertedMessages)
		}
	}

	requestBody := map[string]any{
		"assistant_id":    assistantID,
		"conversation_id": "",
		"project_id":      "",
		"chat_type":       "user_chat",
		"messages":        convertedMessages,
		"meta_data": map[string]any{
			"channel":             "",
			"chat_mode":           "thinking",
			"draft_id":            "",
			"if_plus_model":       true,
			"input_question_type": "xxxx",
			"is_networking":       false,
			"is_test":             false,
			"platform":            "pc",
			"quote_log_id":        "",
			"cogview":             map[string]any{"rm_label_watermark": false},
		},
	}

	bodyBytes, _ := sonic.Marshal(requestBody)
	c.logger.Info("转发请求", "upstream", upstreamModel, "stream", openaiPayload["stream"])
	logging.DebugDump(c.logger, c.config.DebugDumpAll, "转发到 GLM 的 chat 原始请求体", bodyBytes)

	operation := func(accountIndex int, accessToken string) (any, error) {
		var lastErr error
		for attempt := 0; attempt <= c.config.GLMBusyMaxRetries; attempt++ {
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

			// 检查是否需要重试
			if resp.StatusCode == 429 {
				errorPayload := c.readErrorPayload(resp)
				if c.shouldRetryBusyError(resp.StatusCode, errorPayload) && attempt < c.config.GLMBusyMaxRetries {
					resp.Body.Close()
					waitSeconds := c.config.GLMBusyRetryInterval
					c.logger.Warn("GLM 正在处理其他对话，等待重试",
						"attempt", attempt+1,
						"max", c.config.GLMBusyMaxRetries,
						"wait", waitSeconds,
						"account", accountIndex,
					)
					time.Sleep(time.Duration(waitSeconds * float64(time.Second)))
					continue
				}
				resp.Body.Close()
				message := c.buildErrorMessage(resp.StatusCode, errorPayload)
				return nil, &UpstreamAPIError{StatusCode: resp.StatusCode, Message: message, Payload: errorPayload}
			}

			if resp.StatusCode >= 400 {
				errorPayload := c.readErrorPayload(resp)
				resp.Body.Close()
				message := c.buildErrorMessage(resp.StatusCode, errorPayload)
				return nil, &UpstreamAPIError{StatusCode: resp.StatusCode, Message: message, Payload: errorPayload}
			}

			// 检查内容类型决定是否需要预处理
			preparedResp, err := c.prepareChatResponse(resp)
			if err != nil {
				return nil, err
			}
			return preparedResp, nil
		}
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, &UpstreamAPIError{StatusCode: 429, Message: "GLM 长时间忙碌，请稍后重试。"}
	}

	respAny, err := c.callWithAccountFailover(ctx, fmt.Sprintf("chat:%s", upstreamModel), operation, preferredAccountIndex)
	if err != nil {
		return nil, "", err
	}
	resp, ok := respAny.(*http.Response)
	if !ok || resp == nil {
		return nil, "", fmt.Errorf("无效的响应类型")
	}
	return resp, assistantID, nil
}

func (c *Client) openImageStream(ctx context.Context, payload map[string]any, preferredAccountIndex *int) (*http.Response, string, error) {
	prompt, _ := payload["prompt"].(string)
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, "", &UpstreamAPIError{StatusCode: 400, Message: "图片生成请求缺少 prompt"}
	}

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

func (c *Client) prepareChatResponse(resp *http.Response) (*http.Response, error) {
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "application/json") {
		payload, err := c.Auth.ReadJSONResponse(resp)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		logging.DebugDump(c.logger, c.config.DebugDumpAll, "GLM 非流式原始 JSON 响应", payload)
		status := getAny(payload, "status")
		message, _ := payload["message"].(string)
		message = strings.TrimSpace(message)
		if (status != nil && status != 0) || message != "" {
			return nil, &UpstreamAPIError{
				StatusCode: 502,
				Message:    c.buildErrorMessage(200, payload),
				Payload:    payload,
			}
		}
		bodyBytes, _ := sonic.Marshal(payload)
		// 创建新响应
		newResp := &http.Response{
			Status:     "200 OK",
			StatusCode: 200,
			Header:     http.Header{},
			Body:       io.NopCloser(bytes.NewReader(bodyBytes)),
		}
		return newResp, nil
	}
	return c.wrapStreamResponse(resp), nil
}

func (c *Client) wrapStreamResponse(resp *http.Response) *http.Response {
	contentEncoding := strings.ToLower(resp.Header.Get("Content-Encoding"))
	if contentEncoding == "gzip" {
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return resp
		}
		newResp := &http.Response{
			Status:     resp.Status,
			StatusCode: resp.StatusCode,
			Header:     resp.Header.Clone(),
			Body:       io.NopCloser(gr),
		}
		return newResp
	}
	return resp
}

func (c *Client) buildImagesResponse(requestPayload map[string]any, finalEvent map[string]any, accumulator *translator.GLMEventAccumulator) (map[string]any, error) {
	requestedCount := coercePositiveInt(requestPayload["n"], 1, 10)
	responseFormat, _ := requestPayload["response_format"].(string)
	responseFormat = strings.ToLower(strings.TrimSpace(responseFormat))
	if responseFormat == "" {
		responseFormat = "url"
	}
	created := time.Now().Unix()

	var data []map[string]any
	// 通过反射访问 accumulator 内部
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

func (c *Client) resolveImageStyle(payload map[string]any) string {
	style, _ := payload["style"].(string)
	style = strings.ToLower(strings.TrimSpace(style))
	if style == "" {
		return "none"
	}
	return style
}

func (c *Client) resolveImageScene(payload map[string]any) string {
	scene, _ := payload["scene"].(string)
	scene = strings.ToLower(strings.TrimSpace(scene))
	if scene == "" {
		return "none"
	}
	return scene
}

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

// iterSSEEvents 迭代 SSE 事件
func (c *Client) iterSSEEvents(body io.Reader) <-chan map[string]any {
	out := make(chan map[string]any, 32)
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		var pending strings.Builder

		emitBlock := func(block string) {
			block = strings.TrimSpace(block)
			if block == "" {
				return
			}
			dataStart := strings.Index(block, "data:")
			if dataStart == -1 {
				return
			}
			payload := strings.TrimSpace(block[dataStart+5:])
			if payload == "" {
				return
			}
			logging.DebugDump(c.logger, c.config.DebugDumpAll, "GLM 原始 SSE block", block)
			if payload == "[DONE]" {
				return
			}
			var parsed map[string]any
			if err := sonic.UnmarshalString(payload, &parsed); err != nil {
				c.logger.Debug("忽略无法解析的 SSE 片段", "payload", payload)
				return
			}
			logging.DebugDump(c.logger, c.config.DebugDumpAll, "GLM 解析后的 SSE payload", parsed)
			out <- parsed
		}

		for scanner.Scan() {
			line := scanner.Text()
			pending.WriteString(line)
			pending.WriteString("\n")

			for {
				s := pending.String()
				sepPos := strings.Index(s, "\n\n")
				if sepPos == -1 {
					break
				}
				block := s[:sepPos]
				pending.Reset()
				pending.WriteString(s[sepPos+2:])
				emitBlock(block)
			}
		}
		// 处理剩余
		if pending.Len() > 0 {
			emitBlock(pending.String())
		}
	}()
	return out
}

func (c *Client) uploadReferencedFiles(ctx context.Context, messages []map[string]any) []map[string]any {
	var refs []map[string]any
	imageOrder := 0
	for _, message := range messages {
		contentList, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, item := range contentList {
			itemMap, ok := item.(map[string]any)
			if !ok {
				continue
			}
			itemType, _ := itemMap["type"].(string)
			switch itemType {
			case "image_url":
				iu, _ := itemMap["image_url"].(map[string]any)
				if iu != nil {
					if u, _ := iu["url"].(string); u != "" {
						ref := c.uploadFileReference(ctx, u, true, imageOrder)
						if ref != nil {
							refs = append(refs, ref)
							imageOrder++
						}
					}
				}
			case "file":
				fu, _ := itemMap["file_url"].(map[string]any)
				if fu != nil {
					if u, _ := fu["url"].(string); u != "" {
						ref := c.uploadFileReference(ctx, u, false, 0)
						if ref != nil {
							refs = append(refs, ref)
						}
					}
				}
			}
		}
	}
	if len(refs) > 0 {
		c.logger.Info("上传附件完成", "成功数", len(refs))
	}
	return refs
}

func (c *Client) uploadFileReference(ctx context.Context, fileURL string, isImage bool, order int) map[string]any {
	filename, mimeType, payload, err := c.fetchFilePayload(fileURL)
	if err != nil {
		c.logger.Warn("上传附件失败", "url", fileURL, "error", err)
		return nil
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		c.logger.Warn("上传附件失败", "url", fileURL, "error", err)
		return nil
	}
	if _, err := part.Write(payload); err != nil {
		c.logger.Warn("上传附件失败", "url", fileURL, "error", err)
		return nil
	}
	if err := writer.Close(); err != nil {
		c.logger.Warn("上传附件失败", "url", fileURL, "error", err)
		return nil
	}
	boundary := writer.Boundary()

	uploadURL := c.config.FileUploadURL()
	logging.DebugDump(c.logger, c.config.DebugDumpAll, fmt.Sprintf("准备上传附件 url=%s filename=%s mime=%s", fileURL, filename, mimeType), map[string]any{
		"filename": filename, "mime_type": mimeType, "bytes": len(payload),
	})

	operation := func(accountIndex int, accessToken string) (any, error) {
		timestamp, nonce, sign := auth.BuildSign()
		req, err := http.NewRequestWithContext(ctx, "POST", uploadURL, bytes.NewReader(body.Bytes()))
		if err != nil {
			return nil, err
		}
		headers := c.Auth.GetBrowserHeaders("")
		headers["Authorization"] = "Bearer " + accessToken
		headers["Content-Type"] = "multipart/form-data; boundary=" + boundary
		headers["Referer"] = "https://chatglm.cn/"
		headers["X-Device-Id"] = uuid.New().String()
		headers["X-Nonce"] = nonce
		headers["X-Request-Id"] = uuid.New().String()
		headers["X-Sign"] = sign
		headers["X-Timestamp"] = timestamp
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return c.httpClient.Do(req)
	}

	respAny, err := c.callWithAccountFailover(ctx, "file_upload", operation, nil)
	if err != nil {
		c.logger.Warn("上传附件失败", "url", fileURL, "error", err)
		return nil
	}
	resp, ok := respAny.(*http.Response)
	if !ok || resp == nil {
		return nil
	}
	defer resp.Body.Close()
	payload2, err := c.Auth.ReadJSONResponse(resp)
	if err != nil {
		c.logger.Warn("上传附件读取响应失败", "url", fileURL, "error", err)
		return nil
	}
	result, _ := payload2["result"].(map[string]any)
	logging.DebugDump(c.logger, c.config.DebugDumpAll, "GLM 文件上传响应 result", result)
	sourceID, _ := result["file_id"].(string)
	if sourceID == "" {
		return nil
	}

	if isImage {
		fileName, _ := result["file_name"].(string)
		if fileName == "" {
			fileName = filename
		}
		fileURLResult, _ := result["file_url"].(string)
		if fileURLResult == "" {
			fileURLResult = fileURL
		}
		fileSize := 0
		if fs, ok := result["file_size"].(float64); ok {
			fileSize = int(fs)
		}
		if fileSize == 0 {
			fileSize = len(payload)
		}
		width := 0
		if w, ok := result["width"].(float64); ok {
			width = int(w)
		}
		height := 0
		if h, ok := result["height"].(float64); ok {
			height = int(h)
		}
		return map[string]any{
			"type": "image",
			"image": []map[string]any{
				{
					"file_name": fileName,
					"file_id":   sourceID,
					"image_url": fileURLResult,
					"file_size": fileSize,
					"order":     order,
					"width":     width,
					"height":    height,
				},
			},
		}
	}
	fileURLResult, _ := result["file_url"].(string)
	if fileURLResult == "" {
		fileURLResult = fileURL
	}
	return map[string]any{
		"type": "file",
		"file": []map[string]any{
			{"file_id": sourceID, "file_url": fileURLResult},
		},
	}
}

func (c *Client) fetchFilePayload(fileURL string) (string, string, []byte, error) {
	if strings.HasPrefix(fileURL, "data:") {
		commaIdx := strings.Index(fileURL, ",")
		if commaIdx == -1 {
			return "", "", nil, fmt.Errorf("无效的 data URL")
		}
		header := fileURL[:commaIdx]
		encoded := fileURL[commaIdx+1:]
		mimeType := ""
		if strings.HasPrefix(header, "data:") {
			semiIdx := strings.Index(header, ";")
			if semiIdx > 5 {
				mimeType = header[5:semiIdx]
			}
		}
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		ext := filepath.Ext(mimeType)
		if ext == "" {
			ext = ".bin"
		}
		payload, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", "", nil, err
		}
		filename := "upload-" + strings.ReplaceAll(uuid.New().String(), "-", "") + ext
		return filename, mimeType, payload, nil
	}

	parsed, err := url.Parse(fileURL)
	if err != nil {
		return "", "", nil, err
	}
	filename := filepath.Base(parsed.Path)
	if filename == "" || filename == "/" || filename == "." {
		filename = "upload-" + strings.ReplaceAll(uuid.New().String(), "-", "") + ".bin"
	}

	resp, err := c.httpClient.Get(fileURL)
	if err != nil {
		return "", "", nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, FileSizeLimit+1))
	if err != nil {
		return "", "", nil, err
	}
	if len(payload) > FileSizeLimit {
		return "", "", nil, fmt.Errorf("文件超过 100MB，拒绝上传")
	}
	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return filename, mimeType, payload, nil
}

func (c *Client) readErrorPayload(resp *http.Response) map[string]any {
	defer resp.Body.Close()
	var reader io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return map[string]any{"message": fmt.Sprintf("读取上游错误响应失败: %v", err)}
		}
		defer gr.Close()
		reader = gr
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return map[string]any{"message": fmt.Sprintf("读取上游错误响应失败: %v", err)}
	}
	text := string(data)
	var payload map[string]any
	if err := sonic.Unmarshal(data, &payload); err == nil {
		return payload
	}
	return map[string]any{"message": text}
}

func (c *Client) shouldRetryBusyError(statusCode int, payload map[string]any) bool {
	if statusCode != 429 {
		return false
	}
	message, _ := payload["message"].(string)
	innerStatus := getAny(payload, "status")
	return (innerStatus != nil && innerStatus == 10061) || strings.Contains(message, "请等待其他对话生成完毕")
}

func (c *Client) buildErrorMessage(statusCode int, payload map[string]any) string {
	message, _ := payload["message"].(string)
	message = strings.TrimSpace(message)
	innerStatus := getAny(payload, "status")
	rid, _ := payload["rid"].(string)

	parts := []string{fmt.Sprintf("GLM 请求失败 HTTP %d", statusCode)}
	if innerStatus != nil {
		parts = append(parts, fmt.Sprintf("status=%v", innerStatus))
	}
	if message != "" {
		parts = append(parts, message)
	}
	if rid != "" {
		parts = append(parts, "rid="+rid)
	}
	return strings.Join(parts, " | ")
}

func (c *Client) raiseForEventError(event map[string]any, stream bool) error {
	status, _ := event["status"].(string)
	statusLower := strings.ToLower(strings.TrimSpace(status))
	lastError, _ := event["last_error"].(map[string]any)
	eventError := c.extractEventError(event)

	if statusLower != "error" && eventError == nil && lastError == nil {
		return nil
	}

	errorPayload := map[string]any{}
	if lastError != nil {
		for k, v := range lastError {
			errorPayload[k] = v
		}
	}
	if eventError != nil {
		for k, v := range eventError {
			errorPayload[k] = v
		}
	}
	if len(errorPayload) == 0 && statusLower != "error" {
		return nil
	}

	errorCode := getAny(errorPayload, "error_code")
	if errorCode == nil {
		errorCode = getAny(errorPayload, "code")
	}
	errorMessage := ""
	if m, ok := errorPayload["err_msg"].(string); ok && m != "" {
		errorMessage = m
	} else if m, ok := errorPayload["message"].(string); ok && m != "" {
		errorMessage = m
	} else if stream {
		errorMessage = "GLM stream request error"
	} else {
		errorMessage = "GLM request error"
	}
	errorMessage = strings.TrimSpace(errorMessage)

	detail := ""
	if errorCode != nil {
		detail = fmt.Sprintf("code=%v ", errorCode)
	}
	return &UpstreamAPIError{
		StatusCode: 502,
		Message:    strings.TrimSpace(fmt.Sprintf("GLM 上游返回错误 | %s%s", detail, errorMessage)),
		Payload:    errorPayload,
	}
}

func (c *Client) extractEventError(event map[string]any) map[string]any {
	parts, ok := event["parts"].([]any)
	if !ok {
		return nil
	}
	for _, p := range parts {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if errObj, ok := part["error"].(map[string]any); ok && len(errObj) > 0 {
			return errObj
		}
		partStatus, _ := part["status"].(string)
		if strings.ToLower(strings.TrimSpace(partStatus)) == "error" {
			return map[string]any{"message": "GLM part status error"}
		}
	}
	return nil
}

func (c *Client) getPreferredAccountIndex(ticket int) *int {
	count := c.Auth.GetAccountCount()
	if count <= 0 {
		return nil
	}
	idx := ticket % count
	return &idx
}

// callWithAccountFailover 使用账号失败转移执行操作
type operationFunc func(accountIndex int, accessToken string) (any, error)

func (c *Client) callWithAccountFailover(ctx context.Context, requestName string, operation operationFunc, preferredAccountIndex *int) (any, error) {
	accountCount := c.Auth.GetAccountCount()
	if accountCount <= 0 {
		return nil, fmt.Errorf("没有可用的 GLM 账号或游客 token 配置")
	}

	startIndex := c.Auth.GetCurrentAccountIndex()
	if preferredAccountIndex != nil {
		startIndex = *preferredAccountIndex % accountCount
	}

	var lastErr error
	for offset := 0; offset < accountCount; offset++ {
		accountIndex := (startIndex + offset) % accountCount
		guestRetryLimit := 0
		if c.Auth.IsGuestAccount(accountIndex) {
			guestRetryLimit = c.config.GLMGuestMaxRetries
		}
		for attempt := 0; attempt <= guestRetryLimit; attempt++ {
			accessToken, err := c.Auth.GetAccessTokenForAccount(accountIndex)
			if err != nil {
				lastErr = err
				shouldSwitch := c.Auth.ShouldSwitchAccount(err)
				if shouldSwitch {
					c.Auth.InvalidateAccount(accountIndex)
				}
				if shouldSwitch && attempt < guestRetryLimit {
					c.logger.Warn("游客账号请求失败，重新获取游客 ck 重试",
						"attempt", attempt+1, "max", guestRetryLimit,
						"request", requestName, "account", accountIndex, "error", err)
					continue
				}
				if !shouldSwitch || accountCount == 1 {
					return nil, err
				}
				c.Auth.AdvanceAccount(accountIndex, fmt.Sprintf("%s: %v", requestName, err))
				break
			}

			result, err := operation(accountIndex, accessToken)
			if err != nil {
				lastErr = err
				shouldSwitch := c.Auth.ShouldSwitchAccount(err)
				if shouldSwitch {
					c.Auth.InvalidateAccount(accountIndex)
				}
				if shouldSwitch && attempt < guestRetryLimit {
					c.logger.Warn("游客账号请求失败，重新获取游客 ck 重试",
						"attempt", attempt+1, "max", guestRetryLimit,
						"request", requestName, "account", accountIndex, "error", err)
					continue
				}
				if !shouldSwitch || accountCount == 1 {
					return nil, err
				}
				c.Auth.AdvanceAccount(accountIndex, fmt.Sprintf("%s: %v", requestName, err))
				break
			}
			return result, nil
		}
	}

	c.Auth.ResetAccountCycle()
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("账号轮换失败：%s", requestName)
}

// 辅助函数
func getMessagesList(payload map[string]any) []map[string]any {
	messages, ok := payload["messages"].([]any)
	if !ok {
		return nil
	}
	var result []map[string]any
	for _, m := range messages {
		if mm, ok := m.(map[string]any); ok {
			result = append(result, mm)
		}
	}
	return result
}

func getModelName(payload map[string]any, defaultName string) string {
	if m, ok := payload["model"].(string); ok && strings.TrimSpace(m) != "" {
		return m
	}
	return defaultName
}

func getAny(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok {
		return nil
	}
	return v
}

func coercePositiveInt(value any, defaultValue, maximum int) int {
	switch v := value.(type) {
	case float64:
		n := int(v)
		if n < 1 {
			n = defaultValue
		}
		if n > maximum {
			n = maximum
		}
		return n
	case int:
		if v < 1 {
			return defaultValue
		}
		if v > maximum {
			return maximum
		}
		return v
	default:
		return defaultValue
	}
}

func formatErrorChunk(err error) string {
	errPayload := map[string]any{
		"error": map[string]any{
			"message": err.Error(),
			"type":    "upstream_error",
		},
	}
	data, _ := sonic.Marshal(errPayload)
	return "data: " + string(data) + "\n\n"
}

// 用于消除未使用导入的告警
var _ = sort.Strings
