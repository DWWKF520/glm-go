package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	defaultLogger *slog.Logger
	mu            sync.Mutex
)

// SetupLogging 初始化日志系统
func SetupLogging(level, logFile string) {
	mu.Lock()
	defer mu.Unlock()

	var lvl slog.Level
	switch strings.ToUpper(level) {
	case "DEBUG":
		lvl = slog.LevelDebug
	case "INFO":
		lvl = slog.LevelInfo
	case "WARNING":
		lvl = slog.LevelWarn
	case "ERROR":
		lvl = slog.LevelError
	case "CRITICAL":
		lvl = slog.LevelError + 4
	default:
		lvl = slog.LevelInfo
	}

	var handler slog.Handler = NewTUIHandler(os.Stdout, lvl)
	if logFile != "" {
		// 每次启动生成带时间戳的新日志文件
		dir := filepath.Dir(logFile)
		ext := filepath.Ext(logFile)
		base := strings.TrimSuffix(filepath.Base(logFile), ext)
		if dir == "" {
			dir = "."
		}
		if ext == "" {
			ext = ".log"
		}
		ts := time.Now().Format("20060102_150405")
		logFilePath := filepath.Join(dir, base+"_"+ts+ext)

		if err := os.MkdirAll(dir, 0755); err == nil {
			if f, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
				handler = NewMultiHandler(os.Stdout, f, lvl)
			}
		}
	}
	defaultLogger = slog.New(handler)
	slog.SetDefault(defaultLogger)
}

// GetLogger 获取命名 logger
func GetLogger(name string) *slog.Logger {
	if defaultLogger == nil {
		SetupLogging("INFO", "")
	}
	return defaultLogger.With("logger", name)
}

// DefaultLogger 获取默认 logger
func DefaultLogger() *slog.Logger {
	if defaultLogger == nil {
		SetupLogging("INFO", "")
	}
	return defaultLogger
}

// DebugDump 调试输出
func DebugDump(logger *slog.Logger, enabled bool, title string, value any) {
	if !enabled || logger == nil {
		return
	}
	logger.Debug(title, "value", SerializeForDebug(value))
}

// SerializeForDebug 序列化值为可读字符串
func SerializeForDebug(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case []byte:
		s := string(v)
		if len(s) > 2000 {
			return s[:2000] + fmt.Sprintf("... [%d bytes total]", len(s))
		}
		return s
	case string:
		if len(v) > 2000 {
			return v[:2000] + fmt.Sprintf("... [%d chars total]", len(v))
		}
		return v
	default:
		s := fmt.Sprintf("%v", v)
		if len(s) > 2000 {
			return s[:2000] + fmt.Sprintf("... [%d chars total]", len(s))
		}
		return s
	}
}

// TUIHandler 终端 UI 风格的日志处理器
type TUIHandler struct {
	w     io.Writer
	level slog.Level
	mu    sync.Mutex
}

func NewTUIHandler(w io.Writer, level slog.Level) *TUIHandler {
	return &TUIHandler{w: w, level: level}
}

func (h *TUIHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

// Handle 实现 slog.Handler 接口
func (h *TUIHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	var levelName string
	var icon string
	switch {
	case r.Level >= slog.LevelError+4:
		levelName = "CRITICAL"
		icon = "X"
	case r.Level >= slog.LevelError:
		levelName = "ERROR"
		icon = "x"
	case r.Level >= slog.LevelWarn:
		levelName = "WARNING"
		icon = "!"
	case r.Level >= slog.LevelInfo:
		levelName = "INFO"
		icon = ">"
	default:
		levelName = "DEBUG"
		icon = "*"
	}

	// 收集 logger 名和属性
	loggerName := ""
	attrs := []string{}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "logger" {
			loggerName = a.Value.String()
			return true
		}
		attrs = append(attrs, fmt.Sprintf("%s=%v", a.Key, a.Value.Any()))
		return true
	})
	loggerName = strings.TrimPrefix(loggerName, "glm2api.")
	if len(loggerName) > 22 {
		loggerName = loggerName[:22]
	}
	for len(loggerName) < 16 {
		loggerName += " "
	}

	timeStr := r.Time.Format("15:04:05")
	msg := r.Message
	for _, a := range attrs {
		msg += " " + a
	}

	fmt.Fprintf(h.w, "%s %s %-7s | %s | %s\n", timeStr, icon, levelName, loggerName, msg)
	return nil
}

// WithAttrs 实现 slog.Handler 接口
func (h *TUIHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}

// WithGroup 实现 slog.Handler 接口
func (h *TUIHandler) WithGroup(name string) slog.Handler {
	return h
}

// MultiHandler 多输出处理器（控制台+文件）
type MultiHandler struct {
	console *TUIHandler
	file    *TUIHandler
}

func NewMultiHandler(console, file io.Writer, level slog.Level) *MultiHandler {
	return &MultiHandler{
		console: NewTUIHandler(console, level),
		file:    NewTUIHandler(file, slog.LevelDebug),
	}
}

func (h *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.console.Enabled(ctx, level) || h.file.Enabled(ctx, level)
}

func (h *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.console.Enabled(ctx, r.Level) {
		if err := h.console.Handle(ctx, r); err != nil {
			return err
		}
	}
	if h.file.Enabled(ctx, r.Level) {
		r2 := r.Clone()
		if err := h.file.Handle(ctx, r2); err != nil {
			return err
		}
	}
	return nil
}

func (h *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}

func (h *MultiHandler) WithGroup(name string) slog.Handler {
	return h
}
