package auth

import (
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"glm2api/internal/config"
	"glm2api/internal/logging"
)

const (
	SignSecret              = "8a1317a7468aa3ad86e997d08f3f31cb"
	AccessTokenExpiresSecs = 3600
)

// BuildSign 生成请求签名
// 返回 (timestamp, nonce, sign)
func BuildSign() (string, string, string) {
	now := fmt.Sprintf("%d", time.Now().UnixMilli())
	digits := make([]int, 0, len(now))
	for _, c := range now {
		digits = append(digits, int(c-'0'))
	}
	if len(digits) < 2 {
		return now, uuid.New().String(), ""
	}
	checksum := 0
	for i, d := range digits {
		if i == len(digits)-2 {
			continue
		}
		checksum += d
	}
	checksum = checksum % 10
	timestamp := now[:len(now)-2] + fmt.Sprintf("%d", checksum) + now[len(now)-1:]
	nonce := strings.ReplaceAll(uuid.New().String(), "-", "")

	h := md5.New()
	h.Write([]byte(timestamp + "-" + nonce + "-" + SignSecret))
	sign := hex.EncodeToString(h.Sum(nil))
	return timestamp, nonce, sign
}

// BuildRandomXForwardedFor 生成随机 X-Forwarded-For
func BuildRandomXForwardedFor() string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for {
		first := r.Intn(223) + 1
		if first == 10 || first == 127 || first == 169 || first == 172 || first == 192 {
			continue
		}
		octets := []int{first}
		for i := 0; i < 3; i++ {
			octets = append(octets, r.Intn(256))
		}
		parts := make([]string, 4)
		for i, o := range octets {
			parts[i] = fmt.Sprintf("%d", o)
		}
		return strings.Join(parts, ".")
	}
}

// AccessToken 访问令牌
type AccessToken struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    float64
}

// AccountState 账号状态
type AccountState struct {
	RefreshToken string
	IsGuest      bool
	CachedToken  *AccessToken
}

// Manager 访问令牌管理器
type Manager struct {
	config       *config.AppConfig
	logger       *slog.Logger
	accounts     []*AccountState
	currentIndex int
	mu           sync.Mutex
	persistMu    sync.Mutex
	httpClient   *http.Client
}

// NewManager 创建访问令牌管理器
func NewManager(cfg *config.AppConfig, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = logging.GetLogger("glm2api.auth")
	}
	accounts := make([]*AccountState, 0, len(cfg.GLMRefreshTokens))
	for _, token := range cfg.GLMRefreshTokens {
		isGuest := token == config.GuestRefreshTokenMarker
		refreshToken := ""
		if !isGuest {
			refreshToken = token
		}
		accounts = append(accounts, &AccountState{
			RefreshToken: refreshToken,
			IsGuest:      isGuest,
		})
	}
	m := &Manager{
		config:       cfg,
		logger:       logger,
		accounts:     accounts,
		currentIndex: 0,
		httpClient:   &http.Client{Timeout: time.Duration(cfg.RequestTimeout) * time.Second},
	}
	m.logger.Info("账号管理器初始化", "账号数", len(accounts), "游客模式", m.hasGuestAccount())
	return m
}

func (m *Manager) hasGuestAccount() bool {
	for _, a := range m.accounts {
		if a.IsGuest {
			return true
		}
	}
	return false
}

// GetBrowserHeaders 返回浏览器伪装请求头
func (m *Manager) GetBrowserHeaders(appFr string) map[string]string {
	if appFr == "" {
		appFr = "browser_extension"
	}
	accept := "text/event-stream"
	acceptEncoding := "identity"
	if appFr == "default" {
		accept = "application/json, text/plain, */*"
		acceptEncoding = "gzip, deflate"
	}
	return map[string]string{
		"Accept":             accept,
		"Accept-Encoding":    acceptEncoding,
		"Accept-Language":    "zh-CN,zh;q=0.9,en;q=0.8,en-GB;q=0.7,en-US;q=0.6",
		"App-Name":           "chatglm",
		"Cache-Control":      "no-cache",
		"Content-Type":       "application/json",
		"Origin":             "https://chatglm.cn",
		"Pragma":             "no-cache",
		"Priority":           "u=1, i",
		"Sec-Ch-Ua":          `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`,
		"Sec-Ch-Ua-Mobile":   "?0",
		"Sec-Ch-Ua-Platform": `"Windows"`,
		"Sec-Fetch-Dest":     "empty",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Site":     "same-origin",
		"User-Agent":         m.config.GLMUserAgent,
		"X-App-Fr":           appFr,
		"X-App-Platform":     "pc",
		"X-App-Version":      "0.0.1",
		"X-Device-Brand":     "",
		"X-Device-Model":     "",
		"X-Lang":             "zh",
		"X-Forwarded-For":    BuildRandomXForwardedFor(),
	}
}

// ReadJSONResponse 读取并解析 JSON 响应
func (m *Manager) ReadJSONResponse(resp *http.Response) (map[string]any, error) {
	var reader io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("GLM 响应 gzip 解压失败: %w", err)
		}
		defer gr.Close()
		reader = gr
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("读取 GLM 响应失败: %w", err)
	}
	logging.DebugDump(m.logger, m.config.DebugDumpAll, "GLM 原始 JSON 响应体", data)

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("GLM 响应不是合法 JSON: %w", err)
	}
	return payload, nil
}

// GetAccountCount 获取账号数量
func (m *Manager) GetAccountCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.accounts)
}

// GetCurrentAccountIndex 获取当前账号索引
func (m *Manager) GetCurrentAccountIndex() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentIndex
}

// IsGuestAccount 判断指定账号是否为游客
func (m *Manager) IsGuestAccount(index int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if index < 0 || index >= len(m.accounts) {
		return false
	}
	return m.accounts[index].IsGuest
}

// AdvanceAccount 切换到下一个账号
func (m *Manager) AdvanceAccount(failedIndex int, reason string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if failedIndex != m.currentIndex {
		return m.currentIndex
	}
	if len(m.accounts) == 0 {
		return 0
	}
	nextIndex := (failedIndex + 1) % len(m.accounts)
	m.currentIndex = nextIndex
	m.logger.Warn("账号请求失败，切换 refresh_token", "from", failedIndex, "to", nextIndex, "reason", reason)
	return nextIndex
}

// ResetAccountCycle 重置账号轮换
func (m *Manager) ResetAccountCycle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.currentIndex = 0
}

// InvalidateAccount 使账号缓存失效
func (m *Manager) InvalidateAccount(index int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if index < 0 || index >= len(m.accounts) {
		return
	}
	m.accounts[index].CachedToken = nil
}

// GetAccessTokenForAccount 获取指定账号的 access token
func (m *Manager) GetAccessTokenForAccount(index int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getAccessTokenForIndex(index)
}

func (m *Manager) getAccessTokenForIndex(index int) (string, error) {
	if index < 0 || index >= len(m.accounts) {
		return "", fmt.Errorf("账号索引越界: %d", index)
	}
	account := m.accounts[index]
	if account.CachedToken != nil && time.Now().Unix() < int64(account.CachedToken.ExpiresAt)-60 {
		m.logger.Debug("使用缓存 access_token", "account", index, "剩余秒", int(account.CachedToken.ExpiresAt-float64(time.Now().Unix())))
		return account.CachedToken.AccessToken, nil
	}
	token, err := m.refreshAccessToken(index)
	if err != nil {
		return "", err
	}
	account.CachedToken = token
	return token.AccessToken, nil
}

func (m *Manager) refreshAccessToken(index int) (*AccessToken, error) {
	account := m.accounts[index]
	if account.IsGuest || account.RefreshToken == "" {
		return m.fetchGuestAccessToken(index)
	}

	timestamp, nonce, sign := BuildSign()
	headers := m.GetBrowserHeaders("")
	headers["Authorization"] = "Bearer " + account.RefreshToken
	headers["X-Device-Id"] = uuid.New().String()
	headers["X-Nonce"] = nonce
	headers["X-Request-Id"] = uuid.New().String()
	headers["X-Sign"] = sign
	headers["X-Timestamp"] = timestamp

	req, err := http.NewRequest("POST", m.config.RefreshURL(), strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("刷新 GLM token 失败: %w", err)
	}
	defer resp.Body.Close()

	payload, err := m.ReadJSONResponse(resp)
	if err != nil {
		return nil, err
	}

	code := getNumeric(payload, "code")
	status := getNumeric(payload, "status")
	if code == nil && status != nil {
		code = status
	}
	result, _ := payload["result"].(map[string]any)
	accessTokenStr, _ := result["access_token"].(string)
	refreshTokenStr, _ := result["refresh_token"].(string)
	if refreshTokenStr == "" {
		refreshTokenStr = account.RefreshToken
	}

	if resp.StatusCode != 200 || (code != nil && *code != 0) || accessTokenStr == "" {
		return nil, fmt.Errorf("刷新 GLM token 失败: %v", payload)
	}

	if refreshTokenStr != account.RefreshToken {
		if err := m.persistRefreshToken(index, refreshTokenStr); err != nil {
			m.logger.Warn("写回 GLM refresh_token 失败", "index", index, "error", err)
		}
		account.RefreshToken = refreshTokenStr
		m.config.GLMRefreshTokens[index] = refreshTokenStr
		if index == 0 {
			m.config.GLMRefreshToken = refreshTokenStr
		}
		m.logger.Info("GLM refresh_token 已自动刷新并写回账号存储", "index", index)
	}

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	return &AccessToken{
		AccessToken:  accessTokenStr,
		RefreshToken: refreshTokenStr,
		ExpiresAt:    float64(time.Now().Unix()) + float64(AccessTokenExpiresSecs) - float64(r.Intn(20)+10),
	}, nil
}

func (m *Manager) fetchGuestAccessToken(index int) (*AccessToken, error) {
	account := m.accounts[index]
	timestamp, nonce, sign := BuildSign()
	requestID := uuid.New().String()
	deviceID := uuid.New().String()

	headers := m.GetBrowserHeaders("default")
	headers["Content-Length"] = "0"
	headers["Referer"] = "https://chatglm.cn/"
	headers["X-Device-Id"] = deviceID
	headers["X-Nonce"] = nonce
	headers["X-Request-Id"] = requestID
	headers["X-Sign"] = sign
	headers["X-Timestamp"] = timestamp

	req, err := http.NewRequest("POST", m.config.GuestRefreshURL(), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("获取 GLM 游客 token 失败: %w", err)
	}
	defer resp.Body.Close()

	payload, err := m.ReadJSONResponse(resp)
	if err != nil {
		return nil, err
	}

	code := getNumeric(payload, "code")
	status := getNumeric(payload, "status")
	if code == nil && status != nil {
		code = status
	}
	result, _ := payload["result"].(map[string]any)
	accessTokenStr, _ := result["access_token"].(string)
	refreshTokenStr, _ := result["refresh_token"].(string)

	if resp.StatusCode != 200 || (code != nil && *code != 0) || accessTokenStr == "" || refreshTokenStr == "" {
		return nil, fmt.Errorf("获取 GLM 游客 token 失败: %v", payload)
	}

	account.RefreshToken = refreshTokenStr
	m.logger.Info("已获取新的 GLM 游客 refresh_token", "index", index)

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	return &AccessToken{
		AccessToken:  accessTokenStr,
		RefreshToken: refreshTokenStr,
		ExpiresAt:    float64(time.Now().Unix()) + float64(AccessTokenExpiresSecs) - float64(r.Intn(20)+10),
	}, nil
}

func (m *Manager) persistRefreshToken(index int, refreshToken string) error {
	m.persistMu.Lock()
	defer m.persistMu.Unlock()

	if m.accounts[index].IsGuest {
		return nil
	}

	// 多账号模式写回 token 文件
	if m.config.TokenFile != "" {
		if _, err := os.Stat(m.config.TokenFile); err == nil || len(m.config.GLMRefreshTokens) > 1 {
			tokens := make([]string, len(m.config.GLMRefreshTokens))
			copy(tokens, m.config.GLMRefreshTokens)
			tokens[index] = refreshToken
			content := strings.Join(tokens, "\n") + "\n"
			return os.WriteFile(m.config.TokenFile, []byte(content), 0644)
		}
	}
	// 单账号模式写回 .env
	return m.config.SetRefreshToken(index, refreshToken)
}

// ShouldSwitchAccount 判断异常是否应该切换账号
func (m *Manager) ShouldSwitchAccount(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(*UpstreamError); ok {
		return true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "token") {
		return true
	}
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") {
		return true
	}
	return false
}

// UpstreamError 上游 API 错误
type UpstreamError struct {
	StatusCode int
	Message    string
	Payload    map[string]any
}

func (e *UpstreamError) Error() string { return e.Message }

// 辅助函数
func getNumeric(m map[string]any, key string) *float64 {
	v, ok := m[key]
	if !ok {
		return nil
	}
	switch n := v.(type) {
	case float64:
		return &n
	case int:
		f := float64(n)
		return &f
	case int64:
		f := float64(n)
		return &f
	default:
		return nil
	}
}

// Context 用于传递请求上下文
type Context = context.Context
