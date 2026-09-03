package glmclient

// client.go — GLM Web 客户端核心
//
// 本文件定义 Client 结构体、构造函数、响应预处理和 SSE 流解析。

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bytedance/sonic"

	"glm2api/internal/auth"
	"glm2api/internal/config"
	"glm2api/internal/logging"
)

// FileSizeLimit 文件上传大小限制（100MB）
const (
	FileSizeLimit = 100 * 1024 * 1024
)

// UpstreamAPIError 上游 API 错误，别名自 auth.UpstreamError
type UpstreamAPIError = auth.UpstreamError

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
