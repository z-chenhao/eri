package observability

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const defaultLogFileMode = 0o600

var (
	credentialAssignment = regexp.MustCompile(`(?i)(authorization|api[_-]?key|access[_-]?token|refresh[_-]?token|session[_-]?token|token|password|passwd|secret|cookie)(["']?\s*[:=]\s*["']?)([^\s,;"']+)`)
	bearerCredential     = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{12,}`)
	commonCredential     = regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9_]{30,}|AKIA[A-Z0-9]{16})\b`)
	urlCredential        = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)([^/@\s:]+):([^/@\s]+)@`)
)

// SafeError preserves enough operational context for diagnosis without
// allowing credentials, control characters or the user's home path into logs.
func SafeError(err error) string {
	if err == nil {
		return ""
	}
	return SafeText(err.Error(), 500)
}

// SafeText is intended only for operational metadata. User messages, prompts
// and tool results must not be passed to it or written to logs at all.
func SafeText(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	value = urlCredential.ReplaceAllString(value, "$1[REDACTED]@")
	value = bearerCredential.ReplaceAllString(value, "Bearer [REDACTED]")
	value = credentialAssignment.ReplaceAllString(value, "$1$2[REDACTED]")
	value = commonCredential.ReplaceAllString(value, "[REDACTED]")
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		value = strings.ReplaceAll(value, home, "$HOME")
	}
	value = strings.Join(strings.Fields(value), " ")
	if limit > 0 && len(value) > limit {
		value = value[:limit]
	}
	return value
}

// ErrorCode deliberately exposes only a small stable classification. Detailed
// provider and tool failures remain in governed traces, never raw log fields.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "not_found"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	}
	text := strings.ToLower(err.Error())
	for _, candidate := range []string{"sqlite_busy", "deadline_exceeded", "context_canceled", "connection_refused", "connection_reset", "broken_pipe"} {
		needle := strings.ReplaceAll(candidate, "_", " ")
		if strings.Contains(text, needle) || (candidate == "sqlite_busy" && strings.Contains(text, "database is locked")) {
			return candidate
		}
	}
	return "operation_failed"
}

// NewProcessLogger creates one structured lifecycle stream with a durable JSON
// sink and, when terminal is non-nil, a human-readable foreground sink. The
// caller owns the returned closer. Business code should receive only the
// logger so every sink observes the same records.
func NewProcessLogger(path string, terminal io.Writer) (*slog.Logger, io.Closer, error) {
	file, err := NewRotatingFile(path, 5*1024*1024, 3)
	if err != nil {
		return nil, nil, err
	}
	handlers := []slog.Handler{slog.NewJSONHandler(file, &slog.HandlerOptions{Level: slog.LevelInfo})}
	if terminal != nil {
		handlers = append(handlers, slog.NewTextHandler(terminal, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	return slog.New(safeHandler{next: fanoutHandler{handlers: handlers}}), file, nil
}

type safeHandler struct{ next slog.Handler }

func (h safeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h safeHandler) Handle(ctx context.Context, record slog.Record) error {
	safe := slog.NewRecord(record.Time, record.Level, SafeText(record.Message, 500), record.PC)
	record.Attrs(func(attribute slog.Attr) bool {
		safe.AddAttrs(safeAttribute(attribute))
		return true
	})
	return h.next.Handle(ctx, safe)
}

func (h safeHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	safe := make([]slog.Attr, 0, len(attributes))
	for _, attribute := range attributes {
		safe = append(safe, safeAttribute(attribute))
	}
	return safeHandler{next: h.next.WithAttrs(safe)}
}

func (h safeHandler) WithGroup(name string) slog.Handler {
	return safeHandler{next: h.next.WithGroup(SafeText(name, 100))}
}

func safeAttribute(attribute slog.Attr) slog.Attr {
	attribute.Value = attribute.Value.Resolve()
	switch attribute.Value.Kind() {
	case slog.KindString:
		attribute.Value = slog.StringValue(SafeText(attribute.Value.String(), 1000))
	case slog.KindAny:
		if err, ok := attribute.Value.Any().(error); ok {
			attribute.Value = slog.StringValue(SafeError(err))
		} else {
			// Arbitrary values may hide strings inside maps, structs, byte
			// slices, or Stringer implementations. Flatten and sanitize them
			// instead of delegating secret handling to each sink.
			attribute.Value = slog.StringValue(SafeText(fmt.Sprint(attribute.Value.Any()), 1000))
		}
	case slog.KindGroup:
		group := attribute.Value.Group()
		for index := range group {
			group[index] = safeAttribute(group[index])
		}
		attribute.Value = slog.GroupValue(group...)
	}
	return attribute
}

type fanoutHandler struct{ handlers []slog.Handler }

func (h fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h fanoutHandler) Handle(ctx context.Context, record slog.Record) error {
	var result error
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, record.Level) {
			result = errors.Join(result, handler.Handle(ctx, record.Clone()))
		}
	}
	return result
}

func (h fanoutHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		handlers = append(handlers, handler.WithAttrs(attributes))
	}
	return fanoutHandler{handlers: handlers}
}

func (h fanoutHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		handlers = append(handlers, handler.WithGroup(name))
	}
	return fanoutHandler{handlers: handlers}
}

// RotatingFile is a process-local bounded log sink. Rotation happens before a
// write would cross maxBytes. Existing backups are numbered .1 through .N.
type RotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	backups  int
	file     *os.File
	size     int64
}

func NewRotatingFile(path string, maxBytes int64, backups int) (*RotatingFile, error) {
	if !filepath.IsAbs(path) || maxBytes <= 0 || backups < 1 {
		return nil, fmt.Errorf("rotating log requires an absolute path, positive size and at least one backup")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("protect log directory: %w", err)
	}
	for index := 0; index <= backups; index++ {
		candidate := path
		if index > 0 {
			candidate = fmt.Sprintf("%s.%d", path, index)
		}
		if err := os.Chmod(candidate, defaultLogFileMode); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("protect rotating log: %w", err)
		}
	}
	result := &RotatingFile{path: path, maxBytes: maxBytes, backups: backups}
	if err := result.open(); err != nil {
		return nil, err
	}
	return result, nil
}

func (w *RotatingFile) Write(body []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(body)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	written, err := w.file.Write(body)
	w.size += int64(written)
	return written, err
}

func (w *RotatingFile) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *RotatingFile) open() error {
	file, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, defaultLogFileMode)
	if err != nil {
		return fmt.Errorf("open rotating log: %w", err)
	}
	if err := file.Chmod(defaultLogFileMode); err != nil {
		file.Close()
		return fmt.Errorf("protect rotating log: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *RotatingFile) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file = nil
	_ = os.Remove(fmt.Sprintf("%s.%d", w.path, w.backups))
	for index := w.backups - 1; index >= 1; index-- {
		oldPath := fmt.Sprintf("%s.%d", w.path, index)
		newPath := fmt.Sprintf("%s.%d", w.path, index+1)
		if err := os.Rename(oldPath, newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return w.open()
}

var _ io.WriteCloser = (*RotatingFile)(nil)
