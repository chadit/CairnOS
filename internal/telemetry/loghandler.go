package telemetry

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// Log attribute keys for the trace correlation fields.
const (
	logTraceID = "trace_id"
	logSpanID  = "span_id"
)

// LogHandler keeps slog as the logging API while joining logs to traces:
// records logged from a span context gain trace_id and span_id attributes,
// everything else passes through untouched. Wrap whatever handler the
// process already uses. Embedding the wrapped handler lets Enabled pass
// through unchanged, since correlation never decides whether a record is kept.
type LogHandler struct {
	slog.Handler
}

// NewLogHandler wraps inner with trace correlation.
func NewLogHandler(inner slog.Handler) *LogHandler {
	return &LogHandler{Handler: inner}
}

// Handle implements slog.Handler, stamping the span identity from ctx onto
// the record when one is present.
//
//nolint:gocritic // hugeParam: the record-by-value signature is fixed by the slog.Handler interface
func (h *LogHandler) Handle(ctx context.Context, record slog.Record) error {
	if spanCtx := trace.SpanContextFromContext(ctx); spanCtx.IsValid() {
		record.AddAttrs(
			slog.String(logTraceID, spanCtx.TraceID().String()),
			slog.String(logSpanID, spanCtx.SpanID().String()),
		)
	}

	if err := h.Handler.Handle(ctx, record); err != nil {
		return fmt.Errorf("inner log handler: %w", err)
	}

	return nil
}

// WithAttrs implements slog.Handler.
func (h *LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &LogHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup implements slog.Handler.
func (h *LogHandler) WithGroup(name string) slog.Handler {
	return &LogHandler{Handler: h.Handler.WithGroup(name)}
}
