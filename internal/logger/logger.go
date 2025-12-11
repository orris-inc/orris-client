package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

var (
	defaultLogger *slog.Logger
	defaultLevel  = new(slog.LevelVar)
)

func init() {
	defaultLevel.Set(slog.LevelInfo)
	defaultLogger = slog.New(NewHandler(os.Stdout, defaultLevel))
}

// SetLevel sets the minimum log level.
func SetLevel(level slog.Level) {
	defaultLevel.Set(level)
}

func Default() *slog.Logger {
	return defaultLogger
}

func Info(msg string, args ...any) {
	defaultLogger.Info(msg, args...)
}

func Warn(msg string, args ...any) {
	defaultLogger.Warn(msg, args...)
}

func Error(msg string, args ...any) {
	defaultLogger.Error(msg, args...)
}

func Debug(msg string, args ...any) {
	defaultLogger.Debug(msg, args...)
}

type Handler struct {
	out   io.Writer
	mu    *sync.Mutex
	level *slog.LevelVar
	attrs []slog.Attr
	group string
}

func NewHandler(out io.Writer, level *slog.LevelVar) *Handler {
	return &Handler{
		out:   out,
		mu:    &sync.Mutex{},
		level: level,
	}
}

func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	timestamp := r.Time.Format("2006-01-02 15:04:05")
	level := levelToString(r.Level)

	line := fmt.Sprintf("%s %s %s", timestamp, level, r.Message)

	for _, attr := range h.attrs {
		line += " " + attrToString(attr, h.group)
	}

	r.Attrs(func(attr slog.Attr) bool {
		line += " " + attrToString(attr, h.group)
		return true
	})

	_, err := fmt.Fprintln(h.out, line)
	return err
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs), len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	newAttrs = append(newAttrs, attrs...)

	return &Handler{
		out:   h.out,
		mu:    h.mu,
		level: h.level,
		attrs: newAttrs,
		group: h.group,
	}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	newGroup := name
	if h.group != "" {
		newGroup = h.group + "." + name
	}

	return &Handler{
		out:   h.out,
		mu:    h.mu,
		level: h.level,
		attrs: h.attrs,
		group: newGroup,
	}
}

const (
	colorReset  = "\033[0m"
	colorGray   = "\033[90m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorRed    = "\033[31m"
)

func levelToString(level slog.Level) string {
	switch level {
	case slog.LevelDebug:
		return colorGray + "DBG" + colorReset
	case slog.LevelInfo:
		return colorGreen + "INF" + colorReset
	case slog.LevelWarn:
		return colorYellow + "WRN" + colorReset
	case slog.LevelError:
		return colorRed + "ERR" + colorReset
	default:
		return colorGreen + "INF" + colorReset
	}
}

func attrToString(attr slog.Attr, group string) string {
	key := attr.Key
	if group != "" {
		key = group + "." + key
	}

	switch attr.Value.Kind() {
	case slog.KindTime:
		return fmt.Sprintf("%s=%s", key, attr.Value.Time().Format(time.RFC3339))
	case slog.KindDuration:
		return fmt.Sprintf("%s=%s", key, attr.Value.Duration().String())
	default:
		return fmt.Sprintf("%s=%v", key, attr.Value.Any())
	}
}
