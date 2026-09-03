package glmclient

// delete.go — 会话删除
//
// 在请求完成后清理 GLM 端的会话数据，支持概率触发和多账号故障转移。

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"

	"glm2api/internal/auth"
)

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
