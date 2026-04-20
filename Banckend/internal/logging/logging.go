package logging

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

type contextKey string

const (
	requestIDContextKey contextKey = "logging_request_id"
	traceIDContextKey   contextKey = "logging_trace_id"
)

var (
	rootMu     sync.RWMutex
	rootLogger zerolog.Logger
)

func init() {
	Configure("info")
}

func Configure(level string) {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	zerolog.TimestampFieldName = "time"
	zerolog.ErrorFieldName = "error"
	zerolog.SetGlobalLevel(parseLevel(level))

	logger := zerolog.New(os.Stdout).With().Timestamp().Logger().Level(parseLevel(level))

	rootMu.Lock()
	rootLogger = logger
	rootMu.Unlock()
}

func Root() zerolog.Logger {
	rootMu.RLock()
	defer rootMu.RUnlock()
	return rootLogger
}

func Component(component string) *zerolog.Logger {
	logger := Root().With().Str("component", component).Logger()
	return &logger
}

type Logger struct {
	component string
	logger    zerolog.Logger
}

func New(component string) *Logger {
	return &Logger{component: component, logger: *Component(component)}
}

func (l *Logger) Debugf(format string, args ...any) {
	l.logger.Debug().Msgf(format, args...)
}

func (l *Logger) Infof(format string, args ...any) {
	l.logger.Info().Msgf(format, args...)
}

func (l *Logger) Warnf(format string, args ...any) {
	l.logger.Warn().Msgf(format, args...)
}

func (l *Logger) Errorf(format string, args ...any) {
	l.logger.Error().Msgf(format, args...)
}

func (l *Logger) Fatalf(format string, args ...any) {
	l.logger.Fatal().Msgf(format, args...)
}

func NewRequestID() string {
	return uuid.NewString()
}

func WithRequestID(ctx context.Context, requestID string) context.Context {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDContextKey, requestID)
}

func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(requestIDContextKey).(string)
	return strings.TrimSpace(value)
}

func WithTraceID(ctx context.Context, traceID string) context.Context {
	traceID = strings.TrimSpace(traceID)
	if traceID == "" {
		return ctx
	}
	return context.WithValue(ctx, traceIDContextKey, traceID)
}

func TraceIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(traceIDContextKey).(string)
	return strings.TrimSpace(value)
}

func WithRequestContext(ctx context.Context, requestID, traceID string) context.Context {
	ctx = WithRequestID(ctx, requestID)
	ctx = WithTraceID(ctx, traceID)
	return ctx
}

func EventWithContext(ctx context.Context, event *zerolog.Event) *zerolog.Event {
	if event == nil {
		return nil
	}
	if requestID := RequestIDFromContext(ctx); requestID != "" {
		event = event.Str("request_id", requestID)
	}
	if traceID := TraceIDFromContext(ctx); traceID != "" {
		event = event.Str("trace_id", traceID)
	}
	return event
}

func LoggerWithContext(ctx context.Context, logger zerolog.Logger) zerolog.Logger {
	builder := logger.With()
	if requestID := RequestIDFromContext(ctx); requestID != "" {
		builder = builder.Str("request_id", requestID)
	}
	if traceID := TraceIDFromContext(ctx); traceID != "" {
		builder = builder.Str("trace_id", traceID)
	}
	return builder.Logger()
}

func FormatArgs(args []any) []any {
	if len(args) == 0 {
		return nil
	}
	formatted := make([]any, 0, len(args))
	for _, arg := range args {
		formatted = append(formatted, formatValue(arg))
	}
	return formatted
}

func formatValue(value any) any {
	if isNilValue(value) {
		return nil
	}
	switch typed := value.(type) {
	case nil:
		return nil
	case []byte:
		if len(typed) == 0 {
			return ""
		}
		return string(typed)
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	case *time.Time:
		if typed == nil {
			return nil
		}
		return typed.UTC().Format(time.RFC3339Nano)
	case fmt.Stringer:
		return typed.String()
	case error:
		return typed.Error()
	default:
		return typed
	}
}

func isNilValue(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

func parseLevel(raw string) zerolog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "trace":
		return zerolog.TraceLevel
	case "debug":
		return zerolog.DebugLevel
	case "warn", "warning":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	case "fatal":
		return zerolog.FatalLevel
	case "panic":
		return zerolog.PanicLevel
	case "disabled", "off":
		return zerolog.Disabled
	default:
		return zerolog.InfoLevel
	}
}
