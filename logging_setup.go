package main

// Agent-driven log rotation — when LOG_DIR is set, tee slog to a rotating
// <LOG_DIR>/agent.log (lumberjack) alongside stderr so `docker logs` keeps
// working while the on-disk file stays bounded. No-op when LOG_DIR is empty.

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"
)

func attachRotatingLogFile(level string) {
	dir := strings.TrimSpace(os.Getenv("LOG_DIR"))
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("LOG_DIR mkdir failed; falling back to stderr-only", "dir", dir, "error", err)
		return
	}

	rot := &lumberjack.Logger{
		Filename:   filepath.Join(dir, "agent.log"),
		MaxSize:    20, // megabytes
		MaxBackups: 5,
		MaxAge:     14, // days
		Compress:   true,
	}

	combined := io.MultiWriter(os.Stderr, rot)

	lvl := parseLogLevel(level)
	var handler slog.Handler
	if strings.ToLower(os.Getenv("LOGUTIL_FORMAT")) == "text" {
		handler = slog.NewTextHandler(combined, &slog.HandlerOptions{Level: lvl})
	} else {
		handler = slog.NewJSONHandler(combined, &slog.HandlerOptions{Level: lvl})
	}
	slog.SetDefault(slog.New(handler))
	slog.Info("agent log file attached",
		"path", rot.Filename, "max_mb", rot.MaxSize, "max_backups", rot.MaxBackups, "max_age_days", rot.MaxAge)
}

func parseLogLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
