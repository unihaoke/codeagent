// Package logx 提供结构化日志与全链路埋点能力。
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Logger 结构化日志器包装。
type Logger struct {
	l *slog.Logger
}

// Nop 返回不输出任何内容的日志器。
func Nop() *Logger { return &Logger{l: slog.New(slog.NewTextHandler(io.Discard, nil))} }

var (
	defaultLogger = Nop()
	mu            sync.RWMutex
)

// SetDefault 设置全局默认日志器。
func SetDefault(l *Logger) {
	mu.Lock()
	defer mu.Unlock()
	defaultLogger = l
}

// Default 返回全局默认日志器。
func Default() *Logger {
	mu.RLock()
	defer mu.RUnlock()
	return defaultLogger
}

// New 创建日志器。
func New(level, format, file string) (*Logger, io.Closer, error) {
	var w io.Writer = os.Stdout
	var closer io.Closer
	if file != "" {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return nil, nil, fmt.Errorf("创建日志目录失败: %w", err)
		}
		f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("打开日志文件失败: %w", err)
		}
		w = io.MultiWriter(os.Stdout, f)
		closer = f
	}

	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	logger := &Logger{l: slog.New(h)}
	SetDefault(logger)
	return logger, closer, nil
}

// With 派生带字段的日志器。
func (l *Logger) With(kv ...any) *Logger {
	if l == nil || l.l == nil {
		return Nop()
	}
	return &Logger{l: l.l.With(kv...)}
}

// WithRun 派生带任务链路字段的日志器，实现全链路埋点。
func (l *Logger) WithRun(tenantID, taskID, runID, requestID string) *Logger {
	return l.With(
		"tenant", tenantID,
		"task", taskID,
		"run", runID,
		"req", requestID,
	)
}

// Debug 调试日志。
func (l *Logger) Debug(msg string, kv ...any) { l.log(slog.LevelDebug, msg, kv...) }

// Info 普通日志。
func (l *Logger) Info(msg string, kv ...any) { l.log(slog.LevelInfo, msg, kv...) }

// Warn 警告日志。
func (l *Logger) Warn(msg string, kv ...any) { l.log(slog.LevelWarn, msg, kv...) }

// Error 错误日志。
func (l *Logger) Error(msg string, kv ...any) { l.log(slog.LevelError, msg, kv...) }

func (l *Logger) log(lv slog.Level, msg string, kv ...any) {
	if l == nil || l.l == nil {
		return
	}
	l.l.Log(context.Background(), lv, msg, sanitize(kv)...)
}

// sanitize 过滤敏感字段，防止密钥泄露到日志。
func sanitize(kv []any) []any {
	out := make([]any, 0, len(kv))
	for i := 0; i < len(kv); i++ {
		if i+1 < len(kv) {
			key, ok := kv[i].(string)
			if ok && isSensitiveKey(key) {
				out = append(out, key, "***")
				i++
				continue
			}
		}
		out = append(out, kv[i])
	}
	return out
}

func isSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	for _, s := range []string{"password", "secret", "token", "apikey", "api_key", "authorization", "private_key", "credential"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// --- 包级快捷方法（使用全局默认日志器） ---

// Debug 使用默认日志器输出调试日志。
func Debug(msg string, kv ...any) { Default().Debug(msg, kv...) }

// Info 使用默认日志器输出普通日志。
func Info(msg string, kv ...any) { Default().Info(msg, kv...) }

// Warn 使用默认日志器输出警告日志。
func Warn(msg string, kv ...any) { Default().Warn(msg, kv...) }

// Error 使用默认日志器输出错误日志。
func Error(msg string, kv ...any) { Default().Error(msg, kv...) }
