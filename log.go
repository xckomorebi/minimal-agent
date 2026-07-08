package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/openai/openai-go"
)

const logFileName = "minimal-agent.log"

// logLevel holds the minimum slog level that will be written to the log file.
// The default (LevelWarn) keeps logs quiet; use SetLogLevel(LevelInfo) to
// enable informational messages via the -log CLI flag.
var logLevel atomic.Int32

func init() {
	logLevel.Store(int32(slog.LevelWarn))
}

// SetLogLevel changes the minimum log level. Call before any logging occurs.
func SetLogLevel(level slog.Level) {
	logLevel.Store(int32(level))
}

// logFilePath returns the log location in the system temp directory
// (e.g. $TMPDIR/minimal-agent.log on macOS, /tmp/minimal-agent.log on Linux).
func logFilePath() string {
	return filepath.Join(os.TempDir(), logFileName)
}

// lazyFileHandler is a slog.Handler that opens the log file the first time a
// record is handled, so the file is only created when something is actually
// logged. If the file cannot be opened, records are discarded.
type lazyFileHandler struct {
	once  sync.Once
	inner slog.Handler
	file  *os.File
}

func (h *lazyFileHandler) open() {
	h.once.Do(func() {
		h.inner = slog.NewTextHandler(io.Discard, nil)
		f, err := os.OpenFile(logFilePath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return
		}
		h.file = f
		h.inner = slog.NewTextHandler(f, nil)
	})
}

func (h *lazyFileHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.Level(logLevel.Load())
}

func (h *lazyFileHandler) Handle(ctx context.Context, r slog.Record) error {
	h.open()
	return h.inner.Handle(ctx, r)
}

func (h *lazyFileHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.open()
	return h.inner.WithAttrs(attrs)
}

func (h *lazyFileHandler) WithGroup(name string) slog.Handler {
	h.open()
	return h.inner.WithGroup(name)
}

var logHandler = &lazyFileHandler{}

// setupLogger installs the lazy file handler as the slog default.
func setupLogger() {
	slog.SetDefault(slog.New(logHandler))
}

// closeLogger closes the log file if it was opened.
func closeLogger() {
	if logHandler.file != nil {
		logHandler.file.Close()
	}
}

// logPathIfWritten returns the log file path if a record has been written
// this run, or "" if the file was never created.
func logPathIfWritten() string {
	if logHandler.file != nil {
		return logHandler.file.Name()
	}
	return ""
}

// errAttrs returns a slog attribute describing err. If err is an OpenAI API
// error, the attribute is a group that also carries the status code, request
// method/URL, response headers, and response body.
func errAttrs(err error) slog.Attr {
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) {
		return slog.String("error", err.Error())
	}
	attrs := []any{
		slog.String("message", err.Error()),
		slog.Int("status_code", apiErr.StatusCode),
		slog.String("request_method", apiErr.Request.Method),
		slog.String("request_url", apiErr.Request.URL.String()),
	}
	if apiErr.Response != nil {
		attrs = append(attrs, slog.Any("response_headers", apiErr.Response.Header.Clone()))
	}
	if raw := apiErr.RawJSON(); raw != "" {
		attrs = append(attrs, slog.String("response_body", raw))
	}
	return slog.Group("error", attrs...)
}
