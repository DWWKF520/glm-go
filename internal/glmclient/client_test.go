package glmclient

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"

	"glm2api/internal/auth"
	"glm2api/internal/config"
	"glm2api/internal/logging"
)

// newStreamTestClient 创建用于 StreamChatCompletion 测试的 Client
// mockServer 是本地 mock GLM 服务的地址（如 "http://127.0.0.1:PORT"）
func newStreamTestClient(mockBaseURL string) *Client {
	cfg := &config.AppConfig{
		GLMBaseURL:            mockBaseURL,
		GLMAssistantID:        "test-assistant-id",
		GLMImageAssistantID:   "test-image-assistant-id",
		GLMImageModelName:     "test-image-model",
		GLMDeleteConversation: true,
		GLMMaxConcurrency:     3,
		GLMQueueWaitTimeout:   10,
		GLMBusyMaxRetries:     0,
		GLMBusyRetryInterval:  0.1,
		GLMGuestMaxRetries:    0,
		RequestTimeout:        30,
		DebugDumpAll:          false,
		GLMRefreshTokens:      []string{"test-refresh-token"},
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

// TestStreamChatCompletion_ContinuesAfterBreak 验证 SSE 流中途断开后通过 continue_stream 续流
// mock 服务在 /backend-api/assistant/stream 发送两个事件后强制断开 TCP 连接，
// /mainchat-api/stream/continue_stream 收到 history_id 后返回剩余事件（含 finish）。
func TestStreamChatCompletion_ContinuesAfterBreak(t *testing.T) {
	firstEvent := `data:  {"id":"hist-1","conversation_id":"conv-1","assistant_id":"assistant-1","parts":[{"id":"p1","logic_id":"part-think-1","role":"assistant","content":[{"type":"think","think":"思考"}],"status":"init"}],"status":"init","last_error":{}}`
	secondEvent := `data:  {"id":"hist-1","conversation_id":"conv-1","assistant_id":"assistant-1","parts":[{"id":"p1","logic_id":"part-think-1","role":"assistant","content":[{"type":"think","think":"思考继续"}],"status":"init"}],"status":"init","last_error":{}}`
	continueEvents := []string{
		`data:  {"id":"hist-2","conversation_id":"conv-1","assistant_id":"assistant-1","parts":[{"id":"p2","logic_id":"part-text-1","role":"assistant","content":[{"type":"text","text":"你好"}],"status":"init"}],"status":"init","last_error":{}}`,
		`data:  {"id":"hist-2","conversation_id":"conv-1","assistant_id":"assistant-1","parts":[{"id":"p2","logic_id":"part-text-1","role":"assistant","content":[{"type":"text","text":"你好，世界"}],"status":"init"}],"status":"init","last_error":{}}`,
		`data:  {"id":"hist-2","conversation_id":"conv-1","assistant_id":"assistant-1","parts":[{"id":"p2","logic_id":"part-text-1","role":"assistant","content":[{"type":"text","text":"你好，世界"}],"status":"finish"}],"status":"finish","last_error":{}}`,
	}

	var mu sync.Mutex
	var continueCalled bool
	var continueHistoryID string
	var continueLogicID string

	mux := http.NewServeMux()
	mux.HandleFunc("/user-api/user/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"status":0,"result":{"access_token":"mock-access-token","refresh_token":"mock-refresh-token"}}`)
	})
	// 主流：发两个事件后强制断开连接，模拟 SSE 中途断开
	mux.HandleFunc("/backend-api/assistant/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		w.Write([]byte(firstEvent + "\n\n" + secondEvent + "\n\n"))
		w.(http.Flusher).Flush()
		// 等待客户端读走数据后强制断开 TCP，产生 unexpected EOF
		time.Sleep(200 * time.Millisecond)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("response writer 不支持 Hijacker")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("Hijack 失败: %v", err)
		}
		conn.Close()
	})
	// 续流接口：校验 history_id 后返回剩余事件
	mux.HandleFunc("/mainchat-api/stream/continue_stream", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取 continue_stream 请求体失败: %v", err)
		}
		var req struct {
			HistoryID string `json:"history_id"`
			LogicID   string `json:"logic_id"`
		}
		if err := sonic.Unmarshal(body, &req); err != nil {
			t.Errorf("解析 continue_stream 请求体失败: %v", err)
		}
		mu.Lock()
		continueCalled = true
		continueHistoryID = req.HistoryID
		continueLogicID = req.LogicID
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		for _, ev := range continueEvents {
			w.Write([]byte(ev + "\n\n"))
		}
		w.(http.Flusher).Flush()
	})
	mux.HandleFunc("/backend-api/assistant/conversation/delete", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":0,"code":0}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newStreamTestClient(srv.URL)
	c.config.GLMStreamContinueMaxRetries = 3

	ch, err := c.StreamChatCompletion(t.Context(), buildTestPayload())
	if err != nil {
		t.Fatalf("StreamChatCompletion 返回错误: %v", err)
	}
	var allRaw []byte
	for chunk := range ch {
		allRaw = append(allRaw, chunk...)
	}

	mu.Lock()
	historyID := continueHistoryID
	logicID := continueLogicID
	called := continueCalled
	mu.Unlock()

	if !called {
		t.Fatal("SSE 断流后未调用 continue_stream 接口")
	}
	if historyID != "hist-1" {
		t.Errorf("continue_stream 的 history_id = %q, 期望 %q", historyID, "hist-1")
	}
	if logicID == "" {
		t.Error("continue_stream 的 logic_id 为空")
	}

	joined := string(allRaw)
	if !strings.Contains(joined, "思考") || !strings.Contains(joined, "继续") {
		t.Errorf("输出缺少断流前的推理增量（应继续累积）: %q", truncateStr(joined, 500))
	}
	if !strings.Contains(joined, "你好") || !strings.Contains(joined, "，世界") {
		t.Errorf("输出缺少续流后的文本增量: %q", truncateStr(joined, 500))
	}
	if !strings.Contains(joined, "[DONE]") {
		t.Errorf("输出缺少 [DONE] 结束标记: %q", truncateStr(joined, 500))
	}
}
