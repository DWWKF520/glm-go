package glmclient

// client.go — GLM Web 客户端核心实现
//
// 本文件实现了与 GLM (ChatGLM) Web API 交互的完整客户端，包括：
//   - 并发请求队列（ConcurrentRequestQueue）：控制同时向 GLM 发送的请求数量，避免触发限流
//   - 多账号故障转移（callWithAccountFailover）：支持多个 GLM 账号轮换使用，单个账号失败时自动切换
//   - 流式聊天补全（StreamChatCompletion）：将 OpenAI 格式的 chat 请求转换为 GLM 格式并转发
//   - 图片生成（GenerateImages）：将 OpenAI 格式的 image 请求转发到 GLM 的 cogview 绘图接口
//   - SSE 流解析（iterSSEEvents）：解析 GLM 返回的 Server-Sent Events 流
//   - 错误处理与重试：包括忙碌重试、游客账号重试、错误事件过滤等
//
// 数据流向：
//   OpenAI API 请求 → StreamChatCompletion / GenerateImages
//     → resolveTools（过滤工具）
//     → openChatStream / openImageStream（构建 GLM 请求并通过账号故障转移发送）
//     → iterSSEEvents（解析 SSE 流）
//     → GLMEventAccumulator（累积事件，转换为 OpenAI 格式的 SSE chunks）
//     → 返回给调用方

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"

	"github.com/google/uuid"

	"glm2api/internal/auth"
	"glm2api/internal/config"
	"glm2api/internal/logging"
	"glm2api/internal/tools"
	"glm2api/internal/translator"
)

// FileSizeLimit 文件上传大小限制（100MB）
const (
	FileSizeLimit = 100 * 1024 * 1024
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

// UpstreamAPIError 上游 API 错误，别名自 auth.UpstreamError
type UpstreamAPIError = auth.UpstreamError

// QueueTimeoutError 队列等待超时错误
// 当请求在并发队列中等待时间超过配置的 GLM_QUEUE_WAIT_TIMEOUT_SECONDS 时返回
type QueueTimeoutError struct {
	msg string
}

// Error 实现 error 接口
func (e *QueueTimeoutError) Error() string { return e.msg }

// QueueLease 队列租约，代表一个请求占用了队列中的一个执行槽位
// 使用租约模式确保请求完成后正确释放槽位
type QueueLease struct {
	ticket          int       // 分配的票据号（递增整数）
	releaseCallback func(int) // 释放时的回调函数
	released        bool      // 是否已释放（防止重复释放）
}

// Release 释放租约，将执行槽位归还给队列
func (l *QueueLease) Release() {
	if l.released {
		return
	}
	l.released = true
	l.releaseCallback(l.ticket)
}

// ConcurrentRequestQueue 并发请求队列
// 通过票据机制控制同时向 GLM 发送的请求数量，实现请求排队和限流
//
// 原理：
//   - 每个请求获取一个递增的 ticket（票据号）
//   - 只有当 ticket - servingTicket < maxConcurrency 时，请求才能执行
//   - 请求完成后通过 release 释放槽位，servingTicket 前进
//   - 超过 waitTimeout 仍未获得槽位的请求返回 QueueTimeoutError
type ConcurrentRequestQueue struct {
	logger          *slog.Logger  // 日志记录器
	waitTimeout     time.Duration // 最大等待超时时间
	maxConcurrency  int           // 最大并发数（GLM 同时处理的对话数）
	mu              *sync.Cond    // 条件变量，用于请求等待/唤醒
	nextTicket      int           // 下一个将分配的票据号
	servingTicket   int           // 当前正在服务的最小票据号
	releasedTickets map[int]bool  // 已释放但尚未推进 servingTicket 的票据
}

// NewConcurrentRequestQueue 创建并发请求队列
// logger: 日志记录器
// waitTimeout: 请求最大等待时间
// maxConcurrency: 最大并发执行数
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

// Acquire 获取队列租约，阻塞直到获得执行槽位或超时
// requestName: 请求描述（用于日志）
// 返回值：
//   - *QueueLease: 租约对象，使用完毕后必须调用 Release()
//   - error: 超时返回 *QueueTimeoutError
func (q *ConcurrentRequestQueue) Acquire(requestName string) (*QueueLease, error) {
	q.mu.L.Lock()
	defer q.mu.L.Unlock()

	ticket := q.nextTicket
	q.nextTicket++
	// 计算队列前方等待的请求数量
	queueAhead := ticket - (q.servingTicket + q.maxConcurrency) + 1
	if queueAhead < 0 {
		queueAhead = 0
	}
	start := time.Now()

	if queueAhead > 0 {
		q.logger.Info("请求进入 GLM 队列", "ticket", ticket, "ahead", queueAhead, "request", requestName)
	}

	// 等待直到 ticket 落入并发窗口内
	for ticket >= q.servingTicket+q.maxConcurrency {
		remaining := q.waitTimeout - time.Since(start)
		if remaining <= 0 {
			return nil, &QueueTimeoutError{
				msg: fmt.Sprintf("GLM 队列等待超时，前方仍有 %d 个请求，请稍后重试。", ticket-(q.servingTicket+q.maxConcurrency)+1),
			}
		}
		// sync.Cond 不支持带超时的 Wait，使用 goroutine + 定时器模拟
		done := make(chan struct{})
		go func() {
			time.Sleep(remaining)
			close(done)
			q.mu.Broadcast() // 超时后唤醒所有等待者
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

// release 释放指定票据对应的执行槽位
// 通过遍历已释放票据，推进 servingTicket 到下一个连续未释放的位置
func (q *ConcurrentRequestQueue) release(ticket int) {
	q.mu.L.Lock()
	defer q.mu.L.Unlock()
	q.releasedTickets[ticket] = true
	// 推进 servingTicket：跳过所有已释放的连续票据
	for q.releasedTickets[q.servingTicket] {
		delete(q.releasedTickets, q.servingTicket)
		q.servingTicket++
	}
	q.logger.Info("请求离开 GLM 执行槽位", "ticket", ticket)
	q.mu.Broadcast() // 唤醒等待中的请求
}

// Client GLM Web 客户端
// 封装了与 GLM Web API 交互的所有逻辑，包括认证、请求发送、响应解析、账号故障转移等
type Client struct {
	config       *config.AppConfig       // 应用配置
	logger       *slog.Logger            // 日志记录器
	Auth         *auth.Manager           // 认证管理器（管理多个账号的 access token）
	RequestQueue *ConcurrentRequestQueue // 并发请求队列
	httpClient   *http.Client            // HTTP 客户端（带超时）
}

// NewClient 创建 GLM 客户端
// 初始化认证管理器、并发请求队列和 HTTP 客户端
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
	// 使用 Transport 级别的超时控制，而不是 Client.Timeout。
	// Client.Timeout 会限制整个请求的总时长（包括读取响应体），
	// 对于 SSE 流式响应，这会导致流还未读完就被强制终止。
	// MaxIdleConnsPerHost 至少等于最大并发数，避免高并发时连接无法复用、
	// 每次请求都重新 TCP+TLS 握手；ForceAttemptHTTP2 让自定义 Transport 也能协商 HTTP/2。
	maxIdlePerHost := cfg.GLMMaxConcurrency
	if maxIdlePerHost < 10 {
		maxIdlePerHost = 10
	}
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   time.Duration(cfg.RequestTimeout) * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   time.Duration(cfg.RequestTimeout) * time.Second,
		ResponseHeaderTimeout: time.Duration(cfg.RequestTimeout) * time.Second,
		MaxIdleConns:          maxIdlePerHost * 2,
		MaxIdleConnsPerHost:   maxIdlePerHost,
		IdleConnTimeout:       90 * time.Second,
	}
	return &Client{
		config:       cfg,
		logger:       logger,
		Auth:         authMgr,
		RequestQueue: queue,
		httpClient:   &http.Client{Transport: transport},
	}
}

// StreamChatCompletion 流式聊天补全
// 将 OpenAI 格式的 chat completion 请求转发到 GLM Web API，以 SSE 流式返回结果
//
// 流程：
//  1. 从请求中提取并过滤工具定义
//  2. 从并发队列获取执行槽位（租约）
//  3. 打开与 GLM 的流式连接
//  4. 在 goroutine 中持续读取 SSE 事件，通过 accumulator 转换为 OpenAI 格式
//  5. 若流中途断开，使用 continue_stream API 从 history_id（最后一个事件的 id 字段）处续流
//  6. 完成后删除 GLM 会话并释放队列槽位
//
// 返回值：一个 channel，持续输出 SSE 格式的 []byte chunks
// wkf
func (c *Client) StreamChatCompletion(ctx context.Context, payload map[string]any) (<-chan []byte, error) {
	lease, err := c.RequestQueue.Acquire(fmt.Sprintf("stream:%v", payload["model"]))
	if err != nil {
		return nil, err
	}

	response, assistantID, err := c.openChatStream(ctx, payload, c.getPreferredAccountIndex(lease.ticket))
	if err != nil {
		lease.Release()
		return nil, err
	}

	// 获取工具定义列表（用于参数类型矫正）
	filteredTools := c.resolveTools(payload)

	accumulator := translator.NewGLMEventAccumulator(
		fmt.Sprintf("%v", payload["model"]),
		translator.ExtractRecentUserURL(getMessagesList(payload)),
		filteredTools,
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
			finished, historyID, scanErr := c.drainStream(currentResp, accumulator, out)
			if finished {
				return
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
//
// 调用方负责在 scanErr != nil 且 historyID != "" 时通过 continueStream 续流。
func (c *Client) drainStream(response *http.Response, accumulator *translator.GLMEventAccumulator, out chan<- []byte) (bool, string, error) {
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
			return true, historyID, nil
		}
	}
	scanErr := <-scanErrCh
	return false, historyID, scanErr
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

// DeleteConversation 删除 GLM 会话
// 在请求完成后清理 GLM 端的会话数据，避免占用 GLM 的会话配额
func (c *Client) DeleteConversation(ctx context.Context, conversationID, assistantID string) {
	if !c.config.GLMDeleteConversation {
		return
	}
	// 随机触发删除：以配置概率决定是否执行，避免频繁删除会话
	probability := c.config.GLMDeleteConversationProbability
	if probability >= 1 {
		// 概率为 1 时总是删除
	} else if probability <= 0 {
		return
	} else if rand.Float64() >= probability {
		c.logger.Info("随机跳过删除 GLM 会话", "conversation_id", conversationID)
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
	if (status != nil && fmt.Sprintf("%v", status) != "0") || (code != nil && fmt.Sprintf("%v", code) != "0") {
		c.logger.Warn("GLM 会话删除返回非成功状态", "conversation_id", conversationID, "assistant_id", actualAssistantID, "payload", payload)
		return
	}
	c.logger.Info("已删除 GLM 会话", "conversation_id", conversationID, "assistant_id", actualAssistantID)
}

// resolveTools 从 OpenAI 请求 payload 中提取工具定义列表
func (c *Client) resolveTools(openaiPayload map[string]any) []map[string]any {
	var rawTools []map[string]any
	if t, ok := openaiPayload["tools"].([]any); ok {
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				rawTools = append(rawTools, m)
			}
		}
	}
	return rawTools
}

// uploadReferencedFiles 扫描 OpenAI 消息中引用的图片和文件附件并上传到 GLM
// 返回可作为 GLM 消息 content 前缀的引用列表（image_url / file 结构）
func (c *Client) uploadReferencedFiles(ctx context.Context, messages []map[string]any) []map[string]any {
	var refs []map[string]any
	for _, message := range messages {
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, rawItem := range content {
			item, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			itemType, _ := item["type"].(string)
			var url string
			isImage := false
			switch itemType {
			case "image_url":
				url = getContentItemURL(item, "image_url")
				isImage = true
			case "file":
				url = getContentItemURL(item, "file_url")
			default:
				continue
			}
			if url == "" {
				continue
			}
			ref := c.uploadFileReference(ctx, url, isImage)
			if ref != nil {
				refs = append(refs, ref)
			}
		}
	}
	if len(refs) > 0 {
		c.logger.Info("上传附件完成", "success_count", len(refs))
	}
	return refs
}

// getContentItemURL 从 content item 的嵌套对象中提取 url 字段（如 image_url.url / file_url.url）
func getContentItemURL(item map[string]any, key string) string {
	obj, ok := item[key].(map[string]any)
	if !ok {
		return ""
	}
	url, _ := obj["url"].(string)
	return url
}

// uploadFileReference 下载文件并通过 GLM file_upload 接口上传，返回引用结构
// 上传失败时记录警告并返回 nil（不阻断聊天请求）
func (c *Client) uploadFileReference(ctx context.Context, fileURL string, isImage bool) map[string]any {
	filename, mimeType, payload, err := c.fetchFilePayload(ctx, fileURL)
	if err != nil {
		c.logger.Warn("上传附件失败", "url", fileURL, "error", err)
		return nil
	}
	boundary := "glmgo" + strings.ReplaceAll(uuid.New().String(), "-", "")
	body := buildMultipartBody(boundary, filename, mimeType, payload)
	uploadURL := c.config.FileUploadURL()
	logging.DebugDump(c.logger, c.config.DebugDumpAll, "准备上传附件",
		map[string]any{"url": fileURL, "filename": filename, "mime_type": mimeType, "bytes": len(payload)})

	operation := func(accountIndex int, accessToken string) (any, error) {
		timestamp, nonce, sign := auth.BuildSign()
		req, err := http.NewRequestWithContext(ctx, "POST", uploadURL, bytes.NewReader(body))
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
		logging.DebugDump(c.logger, c.config.DebugDumpAll, fmt.Sprintf("转发到 GLM 的 file_upload 请求头 account=%d", accountIndex), headers)
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
	resultPayload, err := c.Auth.ReadJSONResponse(resp)
	if err != nil {
		c.logger.Warn("上传附件读取响应失败", "url", fileURL, "error", err)
		return nil
	}
	result, _ := resultPayload["result"].(map[string]any)
	logging.DebugDump(c.logger, c.config.DebugDumpAll, "GLM 文件上传响应 result", result)
	sourceID, _ := result["source_id"].(string)
	fileResultURL, _ := result["file_url"].(string)
	if fileResultURL == "" {
		fileResultURL = fileURL
	}
	if sourceID == "" {
		return nil
	}
	if isImage {
		url := fileResultURL
		if url == "" {
			url = sourceID
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
	}
	return map[string]any{"type": "file", "file": []map[string]any{{"source_id": sourceID, "file_url": fileResultURL}}}
}

// fetchFilePayload 获取待上传文件的内容
// 支持 data URL（base64 编码）和 HTTP(S) 链接下载，返回 (文件名, MIME 类型, 文件内容)
func (c *Client) fetchFilePayload(ctx context.Context, fileURL string) (string, string, []byte, error) {
	if strings.HasPrefix(fileURL, "data:") {
		header, encoded, found := strings.Cut(fileURL, ",")
		if !found {
			return "", "", nil, fmt.Errorf("无效的 data URL")
		}
		mimeType := strings.TrimPrefix(strings.Split(header, ";")[0], "data:")
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		payload, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", "", nil, fmt.Errorf("解码 data URL 失败: %w", err)
		}
		return fmt.Sprintf("upload-%s%s", uuid.New().String(), extensionForMime(mimeType)), mimeType, payload, nil
	}

	parsed, err := url.Parse(fileURL)
	if err != nil {
		return "", "", nil, err
	}
	filename := parsed.Path
	if idx := strings.LastIndex(filename, "/"); idx >= 0 {
		filename = filename[idx+1:]
	}
	if filename == "" {
		filename = fmt.Sprintf("upload-%s.bin", uuid.New().String())
	}

	req, err := http.NewRequestWithContext(ctx, "GET", fileURL, nil)
	if err != nil {
		return "", "", nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", nil, fmt.Errorf("下载文件失败 HTTP %d", resp.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, FileSizeLimit+1))
	if err != nil {
		return "", "", nil, err
	}
	if len(payload) > FileSizeLimit {
		return "", "", nil, fmt.Errorf("文件超过 100MB，拒绝上传。")
	}
	mimeType := resp.Header.Get("Content-Type")
	if idx := strings.Index(mimeType, ";"); idx >= 0 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}
	if mimeType == "" {
		if t := mime.TypeByExtension(filepath.Ext(filename)); t != "" {
			mimeType = t
		}
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return filename, mimeType, payload, nil
}

// buildMultipartBody 构建与 GLM file_upload 接口兼容的 multipart/form-data 请求体
func buildMultipartBody(boundary, filename, mimeType string, payload []byte) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "--%s\r\n", boundary)
	fmt.Fprintf(&buf, "Content-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\n", filename)
	fmt.Fprintf(&buf, "Content-Type: %s\r\n\r\n", mimeType)
	buf.Write(payload)
	fmt.Fprintf(&buf, "\r\n--%s--\r\n", boundary)
	return buf.Bytes()
}

// extensionForMime 根据 MIME 类型推测文件扩展名，无法识别时返回 ".bin"
func extensionForMime(mimeType string) string {
	if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

// openChatStream 打开与 GLM 的流式聊天连接
// 构建 GLM 格式的请求体，通过账号故障转移机制发送请求，返回 HTTP 响应流
//
// 核心步骤：
//  1. 从 OpenAI payload 中解析工具定义
//  2. 调用 ConvertMessages 将 OpenAI 消息格式转换为 GLM 格式
//  3. 上传消息中引用的图片和文件附件
//  4. 构建 GLM 请求体（含 meta_data、chat_mode 等）
//  5. 通过 callWithAccountFailover 发送请求，自动处理忙碌重试
func (c *Client) openChatStream(ctx context.Context, openaiPayload map[string]any, preferredAccountIndex *int) (*http.Response, string, error) {
	upstreamModel := openaiPayload["model"].(string)
	assistantID := c.config.GLMAssistantID
	filteredTools := c.resolveTools(openaiPayload)

	// 将 OpenAI 消息格式转换为 GLM 能理解的文本提示词格式
	convertedMessages := translator.ConvertMessages(
		getMessagesList(openaiPayload),
		filteredTools,
		tools.ServerSideToolNames,
	)

	logging.DebugDump(c.logger, c.config.DebugDumpAll, "OpenAI 原始 chat 请求 payload", openaiPayload)
	logging.DebugDump(c.logger, c.config.DebugDumpAll, "转换后的 GLM messages", convertedMessages)

	// 上传消息中引用的图片和文件附件，将上传后的引用前置到 GLM 消息 content 中
	refs := c.uploadReferencedFiles(ctx, getMessagesList(openaiPayload))
	if len(refs) > 0 {
		if contentList, ok := convertedMessages[0]["content"].([]map[string]any); ok {
			convertedMessages[0]["content"] = append(append([]map[string]any{}, refs...), contentList...)
		}
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
			"selected_model":      "glm-5.3", //glm-5.3-flash
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

// prepareChatResponse 预处理 GLM 聊天响应
// 根据 Content-Type 区分处理：
//   - application/json: 非流式响应，检查状态码后重新包装为标准 HTTP 响应
//   - 其他（text/event-stream）: 流式响应，处理 gzip 解压后返回
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
		// status 非 0 或 message 非空表示 GLM 返回了业务错误
		if (status != nil && status != 0) || message != "" {
			return nil, &UpstreamAPIError{
				StatusCode: 502,
				Message:    c.buildErrorMessage(200, payload),
				Payload:    payload,
			}
		}
		// 将 JSON 响应体重新包装为 HTTP 响应（供 SSE 解析器统一处理）
		bodyBytes, _ := sonic.Marshal(payload)
		newResp := &http.Response{
			Status:     "200 OK",
			StatusCode: 200,
			Header:     http.Header{},
			Body:       io.NopCloser(bytes.NewReader(bodyBytes)),
		}
		return newResp, nil
	}
	// 流式响应：处理 gzip 解压
	return c.wrapStreamResponse(resp), nil
}

// wrapStreamResponse 处理 gzip 压缩的流式响应
// 如果响应体使用 gzip 编码，则用 gzip.NewReader 包装 body 以透明解压
func (c *Client) wrapStreamResponse(resp *http.Response) *http.Response {
	contentEncoding := strings.ToLower(resp.Header.Get("Content-Encoding"))
	if contentEncoding == "gzip" {
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return resp // 解压失败时返回原始响应
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

// iterSSEEvents 解析 GLM 返回的 SSE（Server-Sent Events）流
// 返回解析后的 JSON 事件对象 channel，以及读流错误 channel。
// scanErrCh 在流结束时发送一个值后关闭：
//   - nil：流正常结束（EOF），或调用方通过取消 ctx 主动结束（如收到 finish 事件后关闭 body）
//   - 非 nil：流读取过程中连接意外中断（供上层触发续流）
//
// ctx 用于区分"主动结束"与"意外中断"：当 ctx 已被取消时，scanner 读到的
// "use of closed network connection" 错误属于调用方主动关闭 body 所致，
// 不再上报为 WARNING，避免正常完成的请求产生误导性告警。
//
// SSE 解析逻辑：
//   - 以 "\n\n" 作为事件分隔符
//   - 提取 "data:" 前缀后的内容作为 JSON payload
//   - "[DONE]" 标记表示流结束（不发送到 channel）
//   - 无法解析的 JSON 片段被静默忽略
func (c *Client) iterSSEEvents(ctx context.Context, body io.Reader) (<-chan map[string]any, <-chan error) {
	out := make(chan map[string]any, 32)
	scanErrCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(scanErrCh)
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024) // 增大缓冲区以处理大型事件
		var pending strings.Builder
		// 处理每个事件块
		emitBlock := func(block string) {
			block = strings.TrimSpace(block)
			if block == "" {
				return
			}
			// 查找 "data:" 前缀，SSE 规范要求每个事件以 "data:" 开头
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
				return // 流结束标记，不发送
			}
			var parsed map[string]any
			if err := sonic.UnmarshalString(payload, &parsed); err != nil {
				c.logger.Debug("忽略无法解析的 SSE 片段", "payload", payload)
				return
			}
			logging.DebugDump(c.logger, c.config.DebugDumpAll, "GLM 解析后的 SSE payload", parsed)
			out <- parsed
		}

		// 逐行读取，累积到 pending 缓冲区，直到遇到空行（事件分隔符）
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
		// 读取中断：若 ctx 已取消，说明是调用方主动关闭 body（如收到 finish 后收尾），
		// 属预期行为，不记录 WARNING，按正常结束上报 nil。
		if err := scanner.Err(); err != nil {
			if ctx.Err() != nil {
				scanErrCh <- nil
				return
			}
			c.logger.Warn("GLM SSE 流读取中断", "error", err)
			scanErrCh <- err
			return
		}
		// 处理流中剩余的不完整事件
		if pending.Len() > 0 {
			emitBlock(pending.String())
		}
		scanErrCh <- nil
	}()
	return out, scanErrCh
}

// readErrorPayload 读取 HTTP 错误响应体并尝试解析为 JSON
// 支持 gzip 压缩的响应体
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

// shouldRetryBusyError 判断是否为可重试的忙碌错误
// GLM 返回 HTTP 429 时，如果内层 status 为 10061 或 message 包含"请等待其他对话生成完毕"，
// 则表示 GLM 正忙，应等待后重试
func (c *Client) shouldRetryBusyError(statusCode int, payload map[string]any) bool {
	if statusCode != 429 {
		return false
	}
	message, _ := payload["message"].(string)
	innerStatus := getAny(payload, "status")
	return (innerStatus != nil && innerStatus == 10061) || strings.Contains(message, "请等待其他对话生成完毕")
}

// buildErrorMessage 构建人类可读的错误消息
// 组合 HTTP 状态码、GLM 内层状态码、错误消息和请求 ID
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

// raiseForEventError 检查 SSE 事件是否包含错误
// 从事件的 status、last_error 和 parts 中提取错误信息
// 返回 nil 表示无错误，返回 *UpstreamAPIError 表示有错误
// wkf
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

// extractEventError 从事件的 parts 中提取错误信息
// 遍历所有 part，查找包含 error 对象或 status 为 "error" 的 part
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
			return map[string]any{"message": "GLM part status error", "part": part}
		}
	}
	return nil
}

// filterErrorParts 过滤掉事件中包含错误的 parts
// 返回只包含正常 parts 的切片，供后续处理继续使用
func (c *Client) filterErrorParts(event map[string]any) []any {
	parts, ok := event["parts"].([]any)
	if !ok {
		return nil
	}
	var cleaned []any
	for _, p := range parts {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if _, hasErr := part["error"].(map[string]any); hasErr {
			continue
		}
		partStatus, _ := part["status"].(string)
		if strings.ToLower(strings.TrimSpace(partStatus)) == "error" {
			continue
		}
		cleaned = append(cleaned, part)
	}
	return cleaned
}

// getPreferredAccountIndex 根据票据号计算首选账号索引
// 通过 ticket % accountCount 实现账号的均匀分配
func (c *Client) getPreferredAccountIndex(ticket int) *int {
	count := c.Auth.GetAccountCount()
	if count <= 0 {
		return nil
	}
	idx := ticket % count
	return &idx
}

// operationFunc 账号操作函数类型
// 参数：(账号索引, access token)
// 返回：(操作结果, 错误)
type operationFunc func(accountIndex int, accessToken string) (any, error)

// callWithAccountFailover 使用账号失败转移机制执行操作
// 当一个账号的请求失败时，自动切换到下一个账号重试
//
// 故障转移策略：
//  1. 从首选账号（或当前账号）开始
//  2. 如果获取 access token 失败且需要切换账号，则跳到下一个账号
//  3. 如果操作执行失败且需要切换账号，则跳到下一个账号
//  4. 游客账号支持额外的重试次数（GLMGuestMaxRetries）
//  5. 所有账号都失败后，重置账号轮换状态并返回最后一个错误
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
		// 游客账号有额外的重试机会（因为游客 token 可能过期需要刷新）
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
				// 切换到下一个账号
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
				// 切换到下一个账号
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

// --- 辅助函数 ---

// getMessagesList 从 OpenAI payload 中提取 messages 列表
// payload["messages"] 是 []any 类型，需要转换为 []map[string]any
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

// getModelName 从 payload 中获取模型名称，若为空则返回默认名称
func getModelName(payload map[string]any, defaultName string) string {
	if m, ok := payload["model"].(string); ok && strings.TrimSpace(m) != "" {
		return m
	}
	return defaultName
}

// getAny 安全地从 map 中获取指定 key 的值，key 不存在时返回 nil
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

// coercePositiveInt 将任意类型的安全转换为正整数
// 支持 float64 和 int 类型，其他类型返回 defaultValue
// 结果会被限制在 [1, maximum] 范围内
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

// 用于消除未使用导入的告警
var _ = sort.Strings
