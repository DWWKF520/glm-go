package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"glm2api/internal/config"
	"glm2api/internal/glmclient"
	"glm2api/internal/logging"
	"glm2api/internal/server"
)

func main() {
	envFile := flag.String("env", ".env", "配置文件路径")
	flag.Parse()

	// 加载配置
	cfg, err := config.LoadConfig(*envFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}

	// 初始化日志
	logging.SetupLogging(cfg.LogLevel, cfg.LogFile)
	logger := logging.GetLogger("glm2api.main")

	if cfg.EnvFileCreated {
		logger.Info("未找到 .env 文件，已从示例自动创建", "path", cfg.EnvFile)
		logger.Info("请编辑 .env 配置文件后重新启动")
		os.Exit(0)
	}

	logger.Info("配置加载完成",
		"env_file", cfg.EnvFile,
		"host", cfg.Host,
		"port", cfg.Port,
		"api_prefix", cfg.APIPrefix,
		"log_level", cfg.LogLevel,
		"guest_mode", cfg.GLMUseGuestRefreshToken,
		"account_count", len(cfg.GLMRefreshTokens),
		"max_concurrency", cfg.GLMMaxConcurrency,
		"exposed_models", len(cfg.ExposedModels),
	)

	// 创建 GLM 客户端
	clientLogger := logging.GetLogger("glm2api.client")
	client := glmclient.NewClient(cfg, clientLogger)

	// 创建并启动服务器
	srv := server.NewServer(cfg, client, logger)

	// 监听信号，优雅关闭
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		logger.Info("收到终止信号，开始优雅关闭", "signal", sig.String())

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("服务器关闭失败", "error", err)
		}
	}()

	if err := srv.Run(); err != nil {
		logger.Error("服务器启动失败", "error", err)
		os.Exit(1)
	}
	logger.Info("服务器已退出")
	_ = slog.Default
}
