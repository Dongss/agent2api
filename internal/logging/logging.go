// Package logging builds the process logger from configuration.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Dongss/agent2api/internal/config"
)

// New builds a logger and, when logging to a file, the closer that releases it.
func New(cfg config.Log) (*slog.Logger, io.Closer, error) {
	var (
		out    io.Writer = os.Stderr
		closer io.Closer
	)
	if cfg.File != "" {
		if dir := filepath.Dir(cfg.File); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, nil, fmt.Errorf("cannot create log directory %s: %w", dir, err)
			}
		}
		f, err := os.OpenFile(cfg.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot open log file %s: %w", cfg.File, err)
		}
		out, closer = f, f
	}

	opts := &slog.HandlerOptions{Level: level(cfg.Level)}
	var handler slog.Handler
	if strings.EqualFold(cfg.Format, "json") {
		handler = slog.NewJSONHandler(out, opts)
	} else {
		handler = slog.NewTextHandler(out, opts)
	}
	return slog.New(handler), closer, nil
}

func level(name string) slog.Level {
	switch strings.ToLower(name) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
