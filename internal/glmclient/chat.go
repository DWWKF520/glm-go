package glmclient

// chat.go — 流式聊天补全
//
// 将 OpenAI 格式的 chat completion 请求转发到 GLM Web API，支持：
//   - 流式 SSE 响应
//   - 流中断续流（continue_stream）
//   - 多账号故障转移与忙碌重试

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"

	"glm2api/internal/auth"
	"glm2api/internal/logging"
	"glm2api/internal/openai"
	"glm2api/internal/tools"
	"glm2api/internal/translator"
)

// StreamChatCompletion 流式聊天补全
// 将 OpenAI 格式的 chat completion 请求转发到 GLM Web API，以 SSE 流式返回结果
//
// 流程：
//  1. 校验请求并从并发队列获取执行槽位（租约）
//  2. 打开与 GLM 的流式连接
//  3. 在 goroutine 中持续读取 SSE 事件，通过 accumulator 转换为 OpenAI 格式
//  4. 若流中途断开，使用 continue_stream API 从 history_id（最后一个事件的 id 字段）处续流
//  5. 完成后删除 GLM 会话并释放队列槽位
//
// 返回值：一个 channel，持续输出 SSE 格式的 []byte chunks
// wkf
func (c *Client) StreamChatCompletion(ctx context.Context, req *openai.ChatCompletionRequest) (<-chan []byte, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, &UpstreamAPIError{StatusCode: http.StatusBadRequest, Message: "chat completion 请求缺少 model 字段"}
	}
	lease, err := c.RequestQueue.Acquire(fmt.Sprintf("stream:%s", req.Model))
	if err != nil {
		return nil, err
	}

	response, assistantID, err := c.openChatStream(ctx, req, c.getPreferredAccountIndex(lease.ticket))
	if err != nil {
		lease.Release()
		return nil, err
	}

	accumulator := translator.NewGLMEventAccumulator(
		req.Model,
		translator.ExtractRecentUserURL(req.Messages),
		req.Tools,
		c.config.DebugDumpAll,
		c.logger,
	)

	out := make(chan []byte, 32)
	go func() {
		defer close(out)
		usedContinueStream := false
		defer func() {
			response.Body.Close()
			if !usedContinueStream {
				// 异步删除会话：删除是后台清理操作，不应阻塞输出流的关闭
				go c.DeleteConversation(context.Background(), accumulator.ConversationID, assistantID)
			} else {
				c.logger.Info("GLM 会话经过 continue_stream 续流，跳过删除会话",
					"conversation_id", accumulator.ConversationID)
			}
			lease.Release()
		}()

		currentResp := response
		for attempt := 0; ; attempt++ {
			finished, historyID, scanErr, retry := c.drainStream(currentResp, accumulator, out)
			if finished {
				return
			}
			// 流式事件遇到可重试错误（如 code=10062 高峰期排队），立即切换到下一个账号重新打开流
			if retry {
				if attempt >= c.config.GLMStreamContinueMaxRetries {
					c.logger.Warn("GLM 流式重试次数耗尽",
						"attempt", attempt+1, "max", c.config.GLMStreamContinueMaxRetries)
					break
				}
				preferredIdx := c.getPreferredAccountIndex(lease.ticket)
				if preferredIdx == nil {
					c.logger.Warn("GLM 流式事件触发重试后无可用账号", "attempt", attempt+1)
					break
				}
				// 每次重试切换到下一个账号，避免反复命中排队账号
				nextIdx := (*preferredIdx + attempt + 1) % c.Auth.GetAccountCount()
				c.logger.Warn("GLM 流式事件触发重试，切换到下一个账号重新打开流",
					"attempt", attempt+1, "account", nextIdx,
					"max", c.config.GLMStreamContinueMaxRetries)
				c.Auth.AdvanceAccount(*preferredIdx, "stream_error_10062_retry")
				newResp, _, err := c.openChatStream(ctx, req, &nextIdx)
				if err != nil {
					c.logger.Warn("GLM 重试打开流失败", "error", err)
					break
				}
				usedContinueStream = true
				currentResp = newResp
				continue
			}
			// 流正常结束（未收到 finish 状态），执行收尾
			if scanErr == nil {
				break
			}
			// 流中断：使用 continue_stream API 从最后一个事件的 id 处续流
			if historyID == "" || attempt >= c.config.GLMStreamContinueMaxRetries {
				c.logger.Warn("GLM 流中断且无法续流",
					"error", scanErr, "history_id", historyID, "attempt", attempt+1,
					"max", c.config.GLMStreamContinueMaxRetries)
				break
			}
			c.logger.Warn("GLM SSE 流中断，尝试续流",
				"history_id", historyID, "attempt", attempt+1,
				"max", c.config.GLMStreamContinueMaxRetries, "error", scanErr)
			contResp, err := c.continueStream(ctx, historyID, c.getPreferredAccountIndex(lease.ticket))
			if err != nil {
				c.logger.Warn("GLM 续流请求失败", "error", err, "history_id", historyID)
				break
			}
			usedContinueStream = true
			currentResp = contResp
		}
		for _, chunk := range accumulator.Finalize("stop", nil) {
			out <- []byte(chunk)
		}
	}()

	return out, nil
}

// drainStream 从响应中读取 SSE 事件流，转换为 OpenAI chunks 并输出到 out
//
// 返回值：
//   - finished: 是否已收到终止状态（finish/intervene/tool_call_complete），此时收尾已完成
//   - historyID: 最后一个事件的 id 字段（断流续流时作为 continue_stream 的 history_id）
//   - scanErr: 读流错误，nil 表示流正常结束
//   - retry: 遇到可重试的流式错误（如 code=10062 高峰期排队），调用方应重新打开流
//
// 调用方负责在 retry=true 时重新打开流，或在 scanErr != nil 且 historyID != "" 时通过 continueStream 续流。
func (c *Client) drainStream(response *http.Response, accumulator *translator.GLMEventAccumulator, out chan<- []byte) (bool, string, error, bool) {
	defer response.Body.Close()
	// 用 ctx 标记"主动结束"：收到终止状态后 cancel()，
	// 这样 iterSSEEvents 的读流 goroutine 在 body 被关闭时不会误报 WARNING。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, scanErrCh := c.iterSSEEvents(ctx, response.Body)
	var historyID string
	for event := range events {
		if event == nil {
			continue
		}
		if id, ok := event["id"].(string); ok && id != "" {
			historyID = id
		}
		// 检查事件中是否有错误 part，尝试过滤掉错误部分后继续处理
		if err := c.raiseForEventError(event, true); err != nil {
			c.logger.Warn("GLM 流式事件所有 part 均有错误，跳过此事件", "error", err)
			if c.isRetryableStreamError(err) {
				return false, historyID, nil, true
			}
			continue
		}
		// 将 GLM 事件转换为 OpenAI 格式的 SSE chunks
		chunks, status := accumulator.ConsumeEvent(event)
		for _, chunk := range chunks {
			out <- []byte(chunk)
		}
		// 收到终止状态（finish/intervene/tool_call_complete）时，执行收尾并结束
		if status == "finish" || status == "intervene" || status == "tool_call_complete" {
			lastError, _ := event["last_error"].(map[string]any)
			for _, chunk := range accumulator.Finalize(status, lastError) {
				out <- []byte(chunk)
			}
			// 主动取消：通知读流 goroutine 即将关闭 body，无需记录中断告警
			cancel()
			return true, historyID, nil, false
		}
	}
	scanErr := <-scanErrCh
	return false, historyID, scanErr, false
}

// continueStream 通过 GLM continue_stream API 从中断处继续流式响应
//
// 参数：
//   - historyID: 最后一个成功收到的 SSE 事件的 id 字段
//
// continue_stream 请求体为 {"history_id": historyID, "logic_id": <新 UUID>}，
// 认证头与主流请求一致，同样经过忙碌重试与账号故障转移。
func (c *Client) continueStream(ctx context.Context, historyID string, preferredAccountIndex *int) (*http.Response, error) {
	bodyBytes, _ := sonic.Marshal(map[string]any{
		"history_id": historyID,
		"logic_id":   uuid.New().String(),
	})
	logging.DebugDump(c.logger, c.config.DebugDumpAll, "GLM continue_stream 请求体", string(bodyBytes))

	operation := func(accountIndex int, accessToken string) (any, error) {
		var lastErr error
		for attempt := 0; attempt <= c.config.GLMBusyMaxRetries; attempt++ {
			timestamp, nonce, sign := auth.BuildSign()
			req, err := http.NewRequestWithContext(ctx, "POST", c.config.ContinueStreamURL(), bytes.NewReader(bodyBytes))
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

			// HTTP 429 表示 GLM 正在处理其他对话
			if resp.StatusCode == 429 {
				errorPayload := c.readErrorPayload(resp)
				if c.shouldRetryBusyError(resp.StatusCode, errorPayload) && attempt < c.config.GLMBusyMaxRetries {
					resp.Body.Close()
					// 多账号时立即返回错误以触发账号故障转移（callWithAccountFailover 会切换到下一个账号），
					// 比在同一账号上固定等待重试更快；单账号时保持原有的等待重试逻辑
					if c.Auth.GetAccountCount() > 1 {
						return nil, &UpstreamAPIError{
							StatusCode: resp.StatusCode,
							Message:    c.buildErrorMessage(resp.StatusCode, errorPayload),
							Payload:    errorPayload,
						}
					}
					waitSeconds := c.config.GLMBusyRetryInterval
					c.logger.Warn("GLM 续流遇到忙碌，等待重试",
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

			// 其他 4xx/5xx 错误直接返回
			if resp.StatusCode >= 400 {
				errorPayload := c.readErrorPayload(resp)
				resp.Body.Close()
				message := c.buildErrorMessage(resp.StatusCode, errorPayload)
				return nil, &UpstreamAPIError{StatusCode: resp.StatusCode, Message: message, Payload: errorPayload}
			}

			// 根据响应 Content-Type 决定是流式还是非流式处理
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

	respAny, err := c.callWithAccountFailover(ctx, "continue_stream", operation, preferredAccountIndex)
	if err != nil {
		return nil, err
	}
	resp, ok := respAny.(*http.Response)
	if !ok || resp == nil {
		return nil, fmt.Errorf("无效的响应类型")
	}
	return resp, nil
}

// openChatStream 打开与 GLM 的流式聊天连接
// 构建 GLM 格式的请求体，通过账号故障转移机制发送请求，返回 HTTP 响应流
//
// 核心步骤：
//  1. 调用 ConvertMessages 将 OpenAI 消息格式转换为 GLM 格式
//  2. 上传消息中引用的图片和文件附件
//  3. 构建 GLM 请求体（含 meta_data、chat_mode 等）
//  4. 通过 callWithAccountFailover 发送请求，自动处理忙碌重试
func (c *Client) openChatStream(ctx context.Context, req *openai.ChatCompletionRequest, preferredAccountIndex *int) (*http.Response, string, error) {
	upstreamModel := req.Model
	assistantID := c.config.GLMAssistantID

	// 将 OpenAI 消息格式转换为 GLM 能理解的文本提示词格式
	convertedMessages := translator.ConvertMessages(
		req.Messages,
		req.Tools,
		tools.ServerSideToolNames,
	)

	logging.DebugDump(c.logger, c.config.DebugDumpAll, "OpenAI 原始 chat 请求 payload", req)
	logging.DebugDump(c.logger, c.config.DebugDumpAll, "转换后的 GLM messages", convertedMessages)

	// 上传消息中引用的图片和文件附件，将上传后的引用前置到 GLM 消息 content 中
	refs := c.uploadReferencedFiles(ctx, req.Messages)
	if len(refs) > 0 {
		convertedMessages[0].Content = append(refs, convertedMessages[0].Content...)
		logging.DebugDump(c.logger, c.config.DebugDumpAll, "附加上传引用后的 GLM messages", convertedMessages)
	}

	// 构建 GLM API 请求体
	requestBody := map[string]any{
		"assistant_id":    assistantID,
		"conversation_id": "",
		"project_id":      "",
		"chat_type":       "user_chat",
		"messages":        convertedMessages,
		"meta_data": map[string]any{
			"channel":             "",
			"chat_mode":           "thinking", // 启用思考模式"deep_"
			"draft_id":            "",
			"if_plus_model":       true,
			"input_question_type": "xxxx",
			"selected_model":      "glm-5.3-flash", //glm-5.3-flash
			"is_networking":       false,
			"is_test":             false,
			"platform":            "pc",
			"quote_log_id":        "",
			"cogview":             map[string]any{"rm_label_watermark": false},
		},
	}

	bodyBytes, _ := sonic.Marshal(requestBody)
	c.logger.Info("转发请求", "upstream", upstreamModel, "stream", req.Stream)
	logging.DebugDump(c.logger, c.config.DebugDumpAll, "转发到 GLM 的 chat 原始请求体", bodyBytes)

	// 定义请求操作（支持忙碌重试）
	operation := func(accountIndex int, accessToken string) (any, error) {
		var lastErr error
		for attempt := 0; attempt <= c.config.GLMBusyMaxRetries; attempt++ {
			timestamp, nonce, sign := auth.BuildSign()
			req, err := http.NewRequestWithContext(ctx, "POST", c.config.ChatStreamURL(), bytes.NewReader(bodyBytes))
			if err != nil {
				return nil, err
			}
			// 设置 GLM Web API 所需的认证和设备标识头
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

			// HTTP 429 表示 GLM 正在处理其他对话
			if resp.StatusCode == 429 {
				errorPayload := c.readErrorPayload(resp)
				if c.shouldRetryBusyError(resp.StatusCode, errorPayload) && attempt < c.config.GLMBusyMaxRetries {
					resp.Body.Close()
					// 多账号时立即返回错误以触发账号故障转移（callWithAccountFailover 会切换到下一个账号），
					// 比在同一账号上固定等待重试更快；单账号时保持原有的等待重试逻辑
					if c.Auth.GetAccountCount() > 1 {
						return nil, &UpstreamAPIError{
							StatusCode: resp.StatusCode,
							Message:    c.buildErrorMessage(resp.StatusCode, errorPayload),
							Payload:    errorPayload,
						}
					}
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

			// 其他 4xx/5xx 错误直接返回
			if resp.StatusCode >= 400 {
				errorPayload := c.readErrorPayload(resp)
				resp.Body.Close()
				message := c.buildErrorMessage(resp.StatusCode, errorPayload)
				return nil, &UpstreamAPIError{StatusCode: resp.StatusCode, Message: message, Payload: errorPayload}
			}

			// 根据响应 Content-Type 决定是流式还是非流式处理
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
