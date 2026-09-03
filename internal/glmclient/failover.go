package glmclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"

	"glm2api/internal/auth"
	"glm2api/internal/logging"
	"glm2api/internal/openai"
	"glm2api/internal/translator"
)

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

// uploadReferencedFiles 扫描 OpenAI 消息中引用的图片和文件附件并上传到 GLM
// 返回可作为 GLM 消息 content 前缀的引用列表（image / file content part）
func (c *Client) uploadReferencedFiles(ctx context.Context, messages []openai.Message) []translator.GLMContentPart {
	var refs []translator.GLMContentPart
	for _, message := range messages {
		for _, part := range message.Content.Parts() {
			var url string
			isImage := false
			switch part.Type {
			case "image_url":
				if part.ImageURL == nil {
					continue
				}
				url = part.ImageURL.URL
				isImage = true
			case "file":
				if part.FileURL == nil {
					continue
				}
				url = part.FileURL.URL
			default:
				continue
			}
			if url == "" {
				continue
			}
			ref := c.UploadFileReference(ctx, url, isImage)
			if ref != nil {
				refs = append(refs, *ref)
			}
		}
	}
	if len(refs) > 0 {
		c.logger.Info("上传附件完成", "success_count", len(refs))
	}
	return refs
}

// uploadFileReference 下载文件并通过 GLM file_upload 接口上传，返回引用结构
// 上传失败时记录警告并返回 nil（不阻断聊天请求）
func (c *Client) UploadFileReference(ctx context.Context, fileURL string, isImage bool) *translator.GLMContentPart {
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
	fileID, _ := result["file_id"].(string)
	fileResultURL, _ := result["file_url"].(string)
	fileName, _ := result["file_name"].(string)
	fileSize, _ := result["file_size"].(float64)
	if isImage {
		return &translator.GLMContentPart{
			Type: "image",
			Image: []translator.GLMImageRef{{
				FileName: fileName,
				FileID:   fileID,
				ImageURL: fileResultURL,
				FileSize: int64(fileSize),
			}},
		}
	}
	return &translator.GLMContentPart{
		Type: "file",
		File: []translator.GLMFileRef{{
			FileName: fileName,
			FileID:   fileID,
			FileURL:  fileResultURL,
			FileSize: int64(fileSize),
		}},
	}
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

// commonMimeExtensions 常见 MIME 类型到扩展名的映射
// Go 的 mime.ExtensionsByType 对部分类型返回的扩展名与常见习惯不符（如 text/plain → .asc），优先使用此映射
var commonMimeExtensions = map[string]string{
	"text/plain":             ".txt",
	"text/csv":               ".csv",
	"text/markdown":          ".md",
	"text/html":              ".html",
	"text/css":               ".css",
	"image/jpeg":             ".jpg",
	"image/tiff":             ".tif",
	"image/svg+xml":          ".svg",
	"application/json":       ".json",
	"application/pdf":        ".pdf",
	"application/javascript": ".js",
}

// extensionForMime 根据 MIME 类型推测文件扩展名，无法识别时返回 ".bin"
func extensionForMime(mimeType string) string {
	if ext, ok := commonMimeExtensions[mimeType]; ok {
		return ext
	}
	if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
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

// --- 辅助函数 ---

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
