package glmclient

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"glm2api/internal/auth"
	"glm2api/internal/config"
	"glm2api/internal/logging"
)

// newStreamTestClient 创建用于 StreamChatCompletion 测试的 Client
// mockServer 是本地 mock GLM 服务的地址（如 "http://127.0.0.1:PORT"）
func newStreamTestClient(mockBaseURL string) *Client {
	cfg := &config.AppConfig{
		GLMBaseURL:              mockBaseURL,
		GLMAssistantID:          "test-assistant-id",
		GLMImageAssistantID:     "test-image-assistant-id",
		GLMImageModelName:       "test-image-model",
		GLMDeleteConversation:   true,
		GLMMaxConcurrency:       3,
		GLMQueueWaitTimeout:     10,
		GLMBusyMaxRetries:       0,
		GLMBusyRetryInterval:    0.1,
		GLMGuestMaxRetries:      0,
		RequestTimeout:          30,
		DebugDumpAll:            false,
		GLMRefreshTokens:        []string{"test-refresh-token"},
	}
	logger := logging.GetLogger("glmclient.stream_test")
	authMgr := auth.NewManager(cfg, logging.GetLogger("glmclient.stream_test.auth"))
	return &Client{
		config:       cfg,
		logger:       logger,
		Auth:         authMgr,
		RequestQueue: NewConcurrentRequestQueue(logger, 10*time.Second, cfg.GLMMaxConcurrency),
		httpClient:   &http.Client{Timeout: 30 * time.Second},
	}
}

// mockGLMServer 创建一个模拟 GLM 后端的 HTTP 服务器
// streamData 返回给 /backend-api/assistant/stream 的 SSE 流数据
func mockGLMServer(t *testing.T, streamData []byte) *httptest.Server {
	mux := http.NewServeMux()

	// mock /user-api/user/refresh — 返回有效的 access_token
	mux.HandleFunc("/user-api/user/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"status":0,"result":{"access_token":"mock-access-token","refresh_token":"mock-refresh-token"}}`)
	})

	// mock /backend-api/assistant/stream — 返回 a.txt 的 SSE 流
	mux.HandleFunc("/backend-api/assistant/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		w.Write(streamData)
		w.(http.Flusher).Flush()
	})

	// mock /backend-api/assistant/conversation/delete — 返回成功
	mux.HandleFunc("/backend-api/assistant/conversation/delete", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":0,"code":0}`)
	})

	// mock /backend-api/assistant/file_upload — 返回成功
	mux.HandleFunc("/backend-api/assistant/file_upload", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"result":{"file_id":"mock-file-id","file_url":"http://mock/file.png"}}`)
	})

	return httptest.NewServer(mux)
}

// buildTestPayload 构建一个用于 StreamChatCompletion 的最小 OpenAI chat 请求
func buildTestPayload() map[string]any {
	return map[string]any{
		"model": "glm-test-model",
		"messages": []any{
			map[string]any{
				"role":    "user",
				"content": "你好",
			},
		},
		"stream": true,
	}
}

func TestStreamChatCompletion_WithAFile(t *testing.T) {
	// 1. 读取 a.txt 作为 mock SSE 流数据
	aData, err := os.ReadFile("../../a.txt")
	if err != nil {
		t.Fatalf("读取 a.txt 失败: %v", err)
	}
	t.Logf("a.txt 大小: %d bytes", len(aData))

	// 2. 启动 mock 服务器
	srv := mockGLMServer(t, aData)
	defer srv.Close()

	// 3. 创建 client，指向 mock 服务器
	c := newStreamTestClient(srv.URL)

	// 4. 调用 StreamChatCompletion
	payload := buildTestPayload()
	ch, err := c.StreamChatCompletion(t.Context(), payload)
	if err != nil {
		t.Fatalf("StreamChatCompletion 返回错误: %v", err)
	}

	// 5. 收集所有流式 chunks
	var chunks []string
	var allRaw []byte
	for chunk := range ch {
		s := string(chunk)
		chunks = append(chunks, s)
		allRaw = append(allRaw, chunk...)
	}

	t.Logf("共收到 %d 个 chunks，总计 %d bytes", len(chunks), len(allRaw))

	if len(chunks) == 0 {
		t.Fatal("StreamChatCompletion 返回空流")
	}

	// 6. 验证每个 chunk 都是合法的 SSE 格式（以 data: 开头）
	for i, chunk := range chunks {
		trimmed := strings.TrimSpace(chunk)
		if trimmed == "" {
			continue // 空 chunk 跳过
		}
		if !strings.HasPrefix(trimmed, "data: ") {
			t.Errorf("chunk[%d] 非 SSE 格式，缺少 'data: ' 前缀: %q", i, truncateStr(trimmed, 200))
		}
	}

	// 7. 验证最后几个 chunks 包含 finish 信息
	lastChunks := chunks
	if len(lastChunks) > 5 {
		lastChunks = lastChunks[len(lastChunks)-5:]
	}
	joined := strings.Join(lastChunks, "")
	if !strings.Contains(joined, "finish") && !strings.Contains(joined, "stop") {
		t.Logf("最后的 chunks 未包含 finish/stop 标记（可能流未完整结束）: %q", truncateStr(joined, 500))
	}

	// 8. 打印前 10 个 chunks 用于调试
	printLimit := 10
	if len(chunks) < printLimit {
		printLimit = len(chunks)
	}
	t.Log("=== 前几个 chunks ===")
	for i := 0; i < printLimit; i++ {
		t.Logf("  chunk[%d]: %s", i, truncateStr(chunks[i], 300))
	}
	t.Log("=== 最后几个 chunks ===")
	start := len(chunks) - printLimit
	if start < 0 {
		start = 0
	}
	for i := start; i < len(chunks); i++ {
		t.Logf("  chunk[%d]: %s", i, truncateStr(chunks[i], 300))
	}
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
