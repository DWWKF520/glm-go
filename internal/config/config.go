package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultAssistantID      = "6a28c9a9a499f6cbf77bfef3"
	DefaultImageAssistantID = "65a232c082ff90a2ad2f15e2"
	DefaultImageModelName   = "glm-image-1"
	DefaultGLMBaseURL       = "https://chatglm.cn/chatglm"
	GuestRefreshTokenMarker = "__glm_guest__"
)

var (
	BuiltinExposedModels = []string{
		"cogView-4-250304",
		"glm-5.1",
		"glm-5v-turbo",
		"glm-5-turbo",
		"glm-5",
		"glm-4.7-flash",
		"glm-4.7",
		"glm-4.6v-flash",
		"glm-4.6",
		"glm-4.5",
		"glm-4.1v-thinking-flashx",
		"glm-4",
		"glm-4-flash",
		"glm-4-air",
		"glm-4v",
		"glm-4-flashx-250414",
		"glm-4-flash-250414",
		"glm-zero-preview",
		"glm-deep-research",
		DefaultImageModelName,
	}
	ModelVariantExcludedModels = map[string]bool{
		"cogView-4-250304":    true,
		DefaultImageModelName: true,
	}
)

// ConfigError 配置错误
type ConfigError struct {
	msg string
}

func (e *ConfigError) Error() string { return e.msg }

func NewConfigError(msg string) error { return &ConfigError{msg: msg} }

// AppConfig 应用配置
type AppConfig struct {
	EnvFile                          string
	EnvFileCreated                   bool
	TokenFile                        string
	Host                             string
	Port                             int
	APIPrefix                        string
	LogLevel                         string
	LogFile                          string
	DebugDumpAll                     bool
	RequestTimeout                   int
	GLMBaseURL                       string
	GLMUseGuestRefreshToken          bool
	GLMRefreshToken                  string
	GLMRefreshTokens                 []string
	GLMAssistantID                   string
	GLMImageAssistantID              string
	GLMImageModelName                string
	GLMUserAgent                     string
	GLMDeleteConversation            bool
	GLMDeleteConversationProbability float64
	GLMMaxConcurrency                int
	GLMQueueWaitTimeout              int
	GLMBusyMaxRetries                int
	GLMBusyRetryInterval             float64
	GLMGuestMaxRetries               int
	GLMStreamContinueMaxRetries      int
	ExposedModels                    []string
	ModelAliases                     map[string]string
	ServerAPIKeys                    []string
	CORSAllowOrigin                  string
}

// RefreshURL 刷新 token URL
func (c *AppConfig) RefreshURL() string {
	return c.GLMBaseURL + "/user-api/user/refresh"
}

// GuestRefreshURL 游客刷新 token URL
func (c *AppConfig) GuestRefreshURL() string {
	return c.GLMBaseURL + "/user-api/guest/access"
}

// ChatStreamURL 聊天流式 URL
func (c *AppConfig) ChatStreamURL() string {
	return c.GLMBaseURL + "/backend-api/assistant/stream"
}

// ContinueStreamURL 续流 URL
// 当 SSE 流中途断开时，通过该接口携带 history_id（最后一个事件的 id 字段）从断点继续未完成的响应
func (c *AppConfig) ContinueStreamURL() string {
	return c.GLMBaseURL + "/mainchat-api/stream/continue_stream"
}

// DeleteConversationURL 删除会话 URL
func (c *AppConfig) DeleteConversationURL() string {
	return c.GLMBaseURL + "/backend-api/assistant/conversation/delete"
}

// FileUploadURL 文件上传 URL
func (c *AppConfig) FileUploadURL() string {
	return c.GLMBaseURL + "/productivity-api/file/chat_upload"
}

// ParseDotenv 解析 .env 文件
func ParseDotenv(path string) (map[string]string, error) {
	values := make(map[string]string)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return values, nil
		}
		return nil, fmt.Errorf("读取配置文件失败: %s error=%w", path, err)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		if len(value) >= 2 {
			if (strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'")) ||
				(strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"")) {
				value = value[1 : len(value)-1]
			}
		}
		values[key] = value
	}
	return values, nil
}

// ParseBool 解析布尔值
func ParseBool(value string, defaultValue bool) bool {
	if value == "" {
		return defaultValue
	}
	v := strings.ToLower(strings.TrimSpace(value))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// ParseInt 解析整数
func ParseInt(value string, defaultValue int) (int, error) {
	if value == "" {
		return defaultValue, nil
	}
	v, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, NewConfigError(fmt.Sprintf("整数配置值无效: %s", value))
	}
	return v, nil
}

// ParseFloat 解析浮点数
func ParseFloat(value string, defaultValue float64) (float64, error) {
	if value == "" {
		return defaultValue, nil
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0, NewConfigError(fmt.Sprintf("浮点配置值无效: %s", value))
	}
	return v, nil
}

// ParseList 解析逗号分隔列表
func ParseList(value string, defaultValue []string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		if defaultValue == nil {
			return []string{}
		}
		result := make([]string, len(defaultValue))
		copy(result, defaultValue)
		return result
	}
	parts := strings.Split(value, ",")
	result := []string{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// LoadRefreshTokens 从文件加载 refresh tokens
func LoadRefreshTokens(tokenFilePath string) ([]string, error) {
	data, err := os.ReadFile(tokenFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, NewConfigError(fmt.Sprintf("读取 token 文件失败: %s error=%v", tokenFilePath, err))
	}
	tokens := []string{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		tokens = append(tokens, line)
	}
	return tokens, nil
}

// IsGuestTokenValue 判断是否为游客 token 标识
func IsGuestTokenValue(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	guestValues := map[string]bool{
		"guest": true, "guest_ck": true, "guest-ck": true,
		"visitor": true, "tourist": true, "游客": true,
		GuestRefreshTokenMarker: true,
	}
	return guestValues[normalized]
}

// EnsureEnvFile 确保存在 .env 文件，若不存在则从示例复制
func EnsureEnvFile(envPath string) (bool, error) {
	if _, err := os.Stat(envPath); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}

	candidates := []string{
		filepath.Join(filepath.Dir(envPath), ".env.example"),
		".env.example",
	}
	examplePath := ""
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			examplePath = c
			break
		}
	}
	if examplePath == "" {
		return false, nil
	}

	if err := os.MkdirAll(filepath.Dir(envPath), 0755); err != nil {
		return false, NewConfigError(fmt.Sprintf("自动创建配置文件失败: source=%s target=%s error=%v", examplePath, envPath, err))
	}
	src, err := os.ReadFile(examplePath)
	if err != nil {
		return false, NewConfigError(fmt.Sprintf("读取示例配置失败: %s error=%v", examplePath, err))
	}
	if err := os.WriteFile(envPath, src, 0644); err != nil {
		return false, NewConfigError(fmt.Sprintf("写入配置文件失败: %s error=%v", envPath, err))
	}
	return true, nil
}

// getOrDefault 从 map 获取值或返回默认值
func getOrDefault(values map[string]string, key, defaultValue string) string {
	if v, ok := values[key]; ok && v != "" {
		return v
	}
	return defaultValue
}

// LoadConfig 加载配置
func LoadConfig(envFile string) (*AppConfig, error) {
	envPath := envFile
	if envPath == "" {
		envPath = ".env"
	}
	envFileCreated, err := EnsureEnvFile(envPath)
	if err != nil {
		return nil, err
	}
	fileValues, err := ParseDotenv(envPath)
	if err != nil {
		return nil, err
	}
	// 合并环境变量（环境变量优先）
	values := make(map[string]string)
	for k, v := range fileValues {
		values[k] = v
	}
	for _, kv := range os.Environ() {
		idx := strings.Index(kv, "=")
		if idx > 0 {
			values[kv[:idx]] = kv[idx+1:]
		}
	}

	glmMaxConcurrency, err := ParseInt(values["GLM_MAX_CONCURRENCY"], 3)
	if err != nil {
		return nil, err
	}
	if glmMaxConcurrency < 1 {
		glmMaxConcurrency = 1
	}

	tokenFile := getOrDefault(values, "GLM_TOKEN_FILE", "token.txt")
	if !filepath.IsAbs(tokenFile) {
		tokenFile, _ = filepath.Abs(filepath.Join(filepath.Dir(envPath), tokenFile))
	}

	refreshTokens, err := LoadRefreshTokens(tokenFile)
	if err != nil {
		return nil, err
	}
	singleRefreshToken := strings.TrimSpace(values["GLM_REFRESH_TOKEN"])
	explicitGuestMode := ParseBool(values["GLM_USE_GUEST_REFRESH_TOKEN"], false) || IsGuestTokenValue(singleRefreshToken)

	if explicitGuestMode {
		refreshTokens = make([]string, glmMaxConcurrency)
		for i := range refreshTokens {
			refreshTokens[i] = GuestRefreshTokenMarker
		}
		singleRefreshToken = GuestRefreshTokenMarker
	} else if len(refreshTokens) == 0 && singleRefreshToken != "" {
		refreshTokens = []string{singleRefreshToken}
	} else if len(refreshTokens) == 0 {
		refreshTokens = make([]string, glmMaxConcurrency)
		for i := range refreshTokens {
			refreshTokens[i] = GuestRefreshTokenMarker
		}
		singleRefreshToken = GuestRefreshTokenMarker
		explicitGuestMode = true
	}

	host := strings.TrimSpace(getOrDefault(values, "HOST", "127.0.0.1"))
	if host == "" {
		host = "127.0.0.1"
	}
	apiPrefix := strings.TrimSpace(getOrDefault(values, "API_PREFIX", "/v1"))
	if apiPrefix == "" {
		apiPrefix = "/v1"
	}
	if !strings.HasPrefix(apiPrefix, "/") {
		apiPrefix = "/" + apiPrefix
	}
	apiPrefix = strings.TrimRight(apiPrefix, "/")
	if apiPrefix == "" {
		apiPrefix = "/v1"
	}
	logLevel := strings.ToUpper(strings.TrimSpace(getOrDefault(values, "LOG_LEVEL", "INFO")))
	if logLevel == "" {
		logLevel = "INFO"
	}
	debugDumpAll := ParseBool(values["DEBUG_DUMP_ALL"], false)
	if debugDumpAll {
		logLevel = "DEBUG"
	}
	validLevels := map[string]bool{"DEBUG": true, "INFO": true, "WARNING": true, "ERROR": true, "CRITICAL": true}
	if !validLevels[logLevel] {
		logLevel = "INFO"
	}
	logFilePath := strings.TrimSpace(values["LOG_FILE_PATH"])
	imageModelName := DefaultImageModelName

	port, err := ParseInt(values["PORT"], 8000)
	if err != nil {
		return nil, err
	}
	requestTimeout, err := ParseInt(values["REQUEST_TIMEOUT_SECONDS"], 120)
	if err != nil {
		return nil, err
	}
	queueWaitTimeout, err := ParseInt(values["GLM_QUEUE_WAIT_TIMEOUT_SECONDS"], 600)
	if err != nil {
		return nil, err
	}
	busyMaxRetries, err := ParseInt(values["GLM_BUSY_MAX_RETRIES"], 30)
	if err != nil {
		return nil, err
	}
	busyRetryInterval, err := ParseFloat(values["GLM_BUSY_RETRY_INTERVAL_SECONDS"], 2.0)
	if err != nil {
		return nil, err
	}
	guestMaxRetries, err := ParseInt(values["GLM_GUEST_MAX_RETRIES"], 3)
	if err != nil {
		return nil, err
	}
	if guestMaxRetries < 0 {
		guestMaxRetries = 0
	}

	streamContinueMaxRetries, err := ParseInt(values["GLM_STREAM_CONTINUE_MAX_RETRIES"], 3)
	if err != nil {
		return nil, err
	}
	if streamContinueMaxRetries < 0 {
		streamContinueMaxRetries = 0
	}

	deleteConversationProbability, err := ParseFloat(values["GLM_DELETE_CONVERSATION_PROBABILITY"], 0.5)
	if err != nil {
		return nil, err
	}
	if deleteConversationProbability < 0 {
		deleteConversationProbability = 0
	}
	if deleteConversationProbability > 1 {
		deleteConversationProbability = 1
	}

	defaultUA := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0"

	config := &AppConfig{
		EnvFile:                          envPath,
		EnvFileCreated:                   envFileCreated,
		TokenFile:                        tokenFile,
		Host:                             host,
		Port:                             port,
		APIPrefix:                        apiPrefix,
		LogLevel:                         logLevel,
		LogFile:                          logFilePath,
		DebugDumpAll:                     debugDumpAll,
		RequestTimeout:                   requestTimeout,
		GLMBaseURL:                       strings.TrimRight(getOrDefault(values, "GLM_BASE_URL", DefaultGLMBaseURL), "/"),
		GLMUseGuestRefreshToken:          explicitGuestMode,
		GLMRefreshToken:                  singleRefreshToken,
		GLMRefreshTokens:                 refreshTokens,
		GLMAssistantID:                   strings.TrimSpace(getOrDefault(values, "GLM_ASSISTANT_ID", DefaultAssistantID)),
		GLMImageAssistantID:              strings.TrimSpace(getOrDefault(values, "GLM_IMAGE_ASSISTANT_ID", DefaultImageAssistantID)),
		GLMImageModelName:                imageModelName,
		GLMUserAgent:                     strings.TrimSpace(getOrDefault(values, "GLM_USER_AGENT", defaultUA)),
		GLMDeleteConversation:            ParseBool(values["GLM_DELETE_CONVERSATION"], true),
		GLMDeleteConversationProbability: deleteConversationProbability,
		GLMMaxConcurrency:                glmMaxConcurrency,
		GLMQueueWaitTimeout:              queueWaitTimeout,
		GLMBusyMaxRetries:                busyMaxRetries,
		GLMBusyRetryInterval:             busyRetryInterval,
		GLMGuestMaxRetries:               guestMaxRetries,
		GLMStreamContinueMaxRetries:      streamContinueMaxRetries,
		ServerAPIKeys:                    ParseList(values["SERVER_API_KEYS"], nil),
		CORSAllowOrigin:                  getOrDefault(values, "CORS_ALLOW_ORIGIN", "*"),
	}

	if config.Port < 1 || config.Port > 65535 {
		return nil, NewConfigError(fmt.Sprintf("端口配置超出范围: PORT=%d", config.Port))
	}
	if config.RequestTimeout <= 0 {
		return nil, NewConfigError(fmt.Sprintf("请求超时必须大于 0: REQUEST_TIMEOUT_SECONDS=%d", config.RequestTimeout))
	}
	if config.GLMQueueWaitTimeout <= 0 {
		return nil, NewConfigError(fmt.Sprintf("队列等待时间必须大于 0: GLM_QUEUE_WAIT_TIMEOUT_SECONDS=%d", config.GLMQueueWaitTimeout))
	}
	if config.GLMBusyRetryInterval < 0 {
		return nil, NewConfigError(fmt.Sprintf("忙碌重试间隔不能小于 0: GLM_BUSY_RETRY_INTERVAL_SECONDS=%v", config.GLMBusyRetryInterval))
	}
	if !strings.HasPrefix(config.GLMBaseURL, "http://") && !strings.HasPrefix(config.GLMBaseURL, "https://") {
		return nil, NewConfigError(fmt.Sprintf("GLM_BASE_URL 必须以 http:// 或 https:// 开头: %s", config.GLMBaseURL))
	}

	return config, nil
}

// SetRefreshToken 写回 refresh token 到存储（多账号写回 token 文件，单账号写回 .env）
func (c *AppConfig) SetRefreshToken(index int, refreshToken string) error {
	c.GLMRefreshTokens[index] = refreshToken
	if index == 0 {
		c.GLMRefreshToken = refreshToken
	}

	// 多账号模式写回 token 文件
	if c.TokenFile != "" {
		if _, err := os.Stat(c.TokenFile); err == nil || len(c.GLMRefreshTokens) > 1 {
			content := strings.Join(c.GLMRefreshTokens, "\n") + "\n"
			return os.WriteFile(c.TokenFile, []byte(content), 0644)
		}
	}

	// 单账号模式写回 .env
	return c.persistEnvRefreshToken(refreshToken)
}

func (c *AppConfig) persistEnvRefreshToken(refreshToken string) error {
	if _, err := os.Stat(c.EnvFile); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	data, err := os.ReadFile(c.EnvFile)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	updated := false
	prefix := "GLM_REFRESH_TOKEN="
	for i, line := range lines {
		if strings.HasPrefix(line, prefix) {
			lines[i] = prefix + refreshToken
			updated = true
			break
		}
	}
	if !updated {
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, prefix+refreshToken)
	}
	content := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(c.EnvFile, []byte(content), 0644)
}
