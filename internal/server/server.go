package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"glm2api/internal/config"
	"glm2api/internal/glmclient"
	"glm2api/internal/logging"
)

// Server HTTP 服务器
type Server struct {
	config *config.AppConfig
	logger *slog.Logger
	client *glmclient.Client
	router *gin.Engine
	srv    *http.Server
}

// NewServer 创建服务器
func NewServer(cfg *config.AppConfig, client *glmclient.Client, logger *slog.Logger) *Server {
	if logger == nil {
		logger = logging.GetLogger("glm2api.server")
	}
	if cfg.LogLevel != "DEBUG" {
		gin.SetMode(gin.ReleaseMode)
	}

	s := &Server{
		config: cfg,
		logger: logger,
		client: client,
	}
	s.setupRouter()
	return s
}

func (s *Server) setupRouter() {
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(s.loggingMiddleware())
	r.Use(s.corsMiddleware())
	r.Use(s.authMiddleware())

	r.GET("/health", s.handleHealth)
	r.GET("/", s.handleRoot)

	v1 := r.Group(s.config.APIPrefix)
	{
		v1.POST("/chat/completions", s.handleChatCompletions)
		v1.POST("/images/generations", s.handleImagesGenerations)
		v1.POST("/files/references", s.handleFilesReferences)
	}

	s.router = r
}

// Run 启动服务器
func (s *Server) Run() error {
	addr := fmt.Sprintf("%s:%d", s.config.Host, s.config.Port)
	s.srv = &http.Server{
		Addr:    addr,
		Handler: s.router,
	}
	s.logger.Info("HTTP 服务已启动", "host", s.config.Host, "port", s.config.Port)
	s.logger.Info("可用端点", "prefix", s.config.APIPrefix)
	return s.srv.ListenAndServe()
}

// Shutdown 优雅关闭
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// loggingMiddleware 日志中间件
func (s *Server) loggingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		latency := time.Since(start)
		s.logger.Info("HTTP 请求",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"latency_ms", latency.Milliseconds(),
			"client", c.ClientIP(),
		)
	}
}

// corsMiddleware CORS 中间件
func (s *Server) corsMiddleware() gin.HandlerFunc {
	origin := s.config.CORSAllowOrigin
	if origin == "" {
		origin = "*"
	}
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, X-Api-Key")
		c.Header("Access-Control-Max-Age", "86400")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// authMiddleware 鉴权中间件
func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if len(s.config.ServerAPIKeys) == 0 {
			c.Next()
			return
		}
		// 跳过健康检查和根路径
		if c.Request.URL.Path == "/health" || c.Request.URL.Path == "/" {
			c.Next()
			return
		}

		apiKey := extractAPIKey(c)
		if apiKey == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{
					"message": "缺少 API Key。请在 Authorization 头或 x-api-key 头中提供。",
					"type":    "invalid_request_error",
					"code":    "invalid_api_key",
				},
			})
			return
		}

		valid := false
		for _, k := range s.config.ServerAPIKeys {
			if apiKey == k {
				valid = true
				break
			}
		}
		if !valid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{
					"message": "无效的 API Key。",
					"type":    "invalid_request_error",
					"code":    "invalid_api_key",
				},
			})
			return
		}
		c.Next()
	}
}

func extractAPIKey(c *gin.Context) string {
	// 1. Authorization: Bearer <key>
	if auth := c.GetHeader("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	// 2. x-api-key
	if k := c.GetHeader("x-api-key"); k != "" {
		return strings.TrimSpace(k)
	}
	// 3. X-Api-Key
	if k := c.GetHeader("X-Api-Key"); k != "" {
		return strings.TrimSpace(k)
	}
	// 4. 查询参数 api_key
	if k := c.Query("api_key"); k != "" {
		return k
	}
	return ""
}

func (s *Server) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":        "ok",
		"version":       "1.0.0",
		"uptime":        time.Since(startTime).Seconds(),
		"queue_size":    s.client.RequestQueue.MaxConcurrency(),
		"account_count": s.client.Auth.GetAccountCount(),
	})
}

var startTime = time.Now()

func (s *Server) handleRoot(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"name":    "glm2api",
		"version": "1.0.0",
		"endpoints": gin.H{
			"chat":   s.config.APIPrefix + "/chat/completions",
			"images": s.config.APIPrefix + "/images/generations",
		},
	})
}

func (s *Server) handleChatCompletions(c *gin.Context) {
	var payload map[string]any
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"message": "请求体解析失败: " + err.Error(),
				"type":    "invalid_request_error",
			},
		})
		return
	}

	s.handleChatCompletionsStream(c, payload)
}

func (s *Server) handleChatCompletionsStream(c *gin.Context, payload map[string]any) {
	ctx := c.Request.Context()
	ch, err := s.client.StreamChatCompletion(ctx, payload)
	if err != nil {
		s.writeError(c, err)
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.ResponseWriter)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming not supported"})
		return
	}

	clientGone := ctx.Done()
	for {
		select {
		case <-clientGone:
			return
		case chunk, ok := <-ch:
			if !ok {
				return
			}
			_, _ = c.Writer.Write(chunk)
			flusher.(interface{ Flush() }).Flush()
		}
	}
}

func (s *Server) handleImagesGenerations(c *gin.Context) {
	var payload map[string]any
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"message": "请求体解析失败: " + err.Error(),
				"type":    "invalid_request_error",
			},
		})
		return
	}
	ctx := c.Request.Context()
	response, err := s.client.GenerateImages(ctx, payload)
	if err != nil {
		s.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

// writeError 写入错误响应
func (s *Server) writeError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	var payload any
	if upstreamErr, ok := err.(*glmclient.UpstreamAPIError); ok {
		status = http.StatusBadGateway
		if upstreamErr.StatusCode >= 400 && upstreamErr.StatusCode < 600 {
			status = upstreamErr.StatusCode
		}
		payload = gin.H{
			"error": gin.H{
				"message": upstreamErr.Message,
				"type":    "upstream_error",
				"code":    upstreamErr.StatusCode,
				"payload": upstreamErr.Payload,
			},
		}
	} else {
		payload = gin.H{
			"error": gin.H{
				"message": err.Error(),
				"type":    "internal_error",
			},
		}
	}
	c.JSON(status, payload)
}

type UploadFileReferenceRequest struct {
	FilePath string `json:"file_path"`
	IsImage  bool   `json:"is_image"`
}

func (s *Server) handleFilesReferences(c *gin.Context) {
	var req UploadFileReferenceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"message": "请求体解析失败: " + err.Error(),
				"type":    "invalid_request_error",
			},
		})
		return
	}
	ctx := c.Request.Context()
	response := s.client.UploadFileReference(ctx, req.FilePath, req.IsImage)
	c.JSON(http.StatusOK, response)
}
