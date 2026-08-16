package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// initLogger configures slog: a compact human-readable format on the console
// and, when -log-file is set, a JSONL (one JSON object per line) file handler
// alongside it.
func initLogger(logFilePath string, verbose bool) (*os.File, error) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}

	handlers := []slog.Handler{newConsoleHandler(os.Stderr, level)}
	var f *os.File
	if logFilePath != "" {
		var err error
		f, err = os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("failed to open log file %q: %w", logFilePath, err)
		}
		handlers = append(handlers, slog.NewJSONHandler(f, &slog.HandlerOptions{Level: level}))
	}

	slog.SetDefault(slog.New(&multiHandler{handlers: handlers}))
	return f, nil
}

// multiHandler dispatches every record to all underlying handlers.
type multiHandler struct {
	handlers []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range m.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, 0, len(m.handlers))
	for _, h := range m.handlers {
		handlers = append(handlers, h.WithAttrs(attrs))
	}
	return &multiHandler{handlers: handlers}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, 0, len(m.handlers))
	for _, h := range m.handlers {
		handlers = append(handlers, h.WithGroup(name))
	}
	return &multiHandler{handlers: handlers}
}

// consoleHandler prints compact, single-line records, e.g.:
//
//	14:25:39.530 INF Discovering physical Chromecast on LAN timeout=3s
type consoleHandler struct {
	mu     sync.Mutex
	w      io.Writer
	level  slog.Leveler
	attrs  []slog.Attr
	groups []string
	color  bool
}

func newConsoleHandler(w io.Writer, level slog.Leveler) *consoleHandler {
	// Colorize only when writing to an actual terminal, not a pipe/file.
	color := false
	if f, ok := w.(*os.File); ok {
		if fi, err := f.Stat(); err == nil {
			color = fi.Mode()&os.ModeCharDevice != 0
		}
	}
	return &consoleHandler{w: w, level: level, color: color}
}

func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	var buf bytes.Buffer
	buf.WriteString(r.Time.Format("15:04:05.000"))
	buf.WriteByte(' ')
	buf.WriteString(h.levelColor(r.Level, shortLevel(r.Level)))
	buf.WriteByte(' ')
	buf.WriteString(r.Message)

	attrs := make([]slog.Attr, 0, r.NumAttrs()+len(h.attrs))
	attrs = append(attrs, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	prefix := strings.Join(h.groups, ".")

	for _, a := range attrs {
		if a.Equal(slog.Attr{}) {
			continue
		}
		if a.Value.Kind() == slog.KindGroup {
			for _, ga := range a.Value.Group() {
				if ga.Equal(slog.Attr{}) {
					continue
				}
				h.writeAttr(&buf, prefix, a.Key+"."+ga.Key, ga.Value)
			}
			continue
		}
		h.writeAttr(&buf, prefix, a.Key, a.Value)
	}

	buf.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write(buf.Bytes())
	return err
}

func (h *consoleHandler) writeAttr(buf *bytes.Buffer, prefix, key string, v slog.Value) {
	if prefix != "" {
		key = prefix + "." + key
	}
	buf.WriteByte(' ')
	buf.WriteString(key)
	buf.WriteByte('=')
	buf.WriteString(renderValue(v))
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := &consoleHandler{
		w:      h.w,
		level:  h.level,
		color:  h.color,
		attrs:  append(append([]slog.Attr{}, h.attrs...), attrs...),
		groups: h.groups,
	}
	return c
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	c := &consoleHandler{
		w:      h.w,
		level:  h.level,
		color:  h.color,
		attrs:  h.attrs,
		groups: append(append([]string{}, h.groups...), name),
	}
	return c
}

func (h *consoleHandler) levelColor(l slog.Level, s string) string {
	if !h.color {
		return s
	}
	switch {
	case l >= slog.LevelError:
		return "\033[31m" + s + "\033[0m" // red
	case l >= slog.LevelWarn:
		return "\033[33m" + s + "\033[0m" // yellow
	case l >= slog.LevelInfo:
		return "\033[32m" + s + "\033[0m" // green
	default:
		return "\033[90m" + s + "\033[0m" // bright black
	}
}

func shortLevel(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERR"
	case l >= slog.LevelWarn:
		return "WRN"
	case l >= slog.LevelInfo:
		return "INF"
	default:
		return "DBG"
	}
}

func renderValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		s := v.String()
		if strings.ContainsAny(s, " \t\n\"=") {
			return strconv.Quote(s)
		}
		return s
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339)
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'f', -1, 64)
	case slog.KindLogValuer:
		return renderValue(v.LogValuer().LogValue())
	default:
		return fmt.Sprintf("%v", v.Any())
	}
}
