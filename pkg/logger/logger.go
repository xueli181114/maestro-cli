// Package logger provides structured logging functionality for the maestro-cli application.
package logger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
)

// Logger wraps slog.Logger with HyperFleet logging standards
type Logger struct {
	logger *slog.Logger
	config Config
}

// Config holds logging configuration following HyperFleet standards
type Config struct {
	Level     string // debug, info, warn, error
	Format    string // text, json
	Output    string // stdout, stderr
	Component string // component name
	Version   string // component version
	Hostname  string // pod name or hostname
}

// Fields represents structured log fields
type Fields map[string]interface{}

// ContextKey is the type for context keys
type ContextKey string

const (
	// TraceIDKey is the context key for distributed tracing trace ID
	TraceIDKey ContextKey = "trace_id"
	// SpanIDKey is the context key for distributed tracing span ID
	SpanIDKey ContextKey = "span_id"
	// RequestIDKey is the context key for request ID
	RequestIDKey  ContextKey = "request_id"
	EventIDKey    ContextKey = "event_id"
	ClusterIDKey  ContextKey = "cluster_id"
	ResourceType  ContextKey = "resource_type"
	ResourceIDKey ContextKey = "resource_id"
)

// New creates a new logger with HyperFleet logging standards
func New(config Config) *Logger {
	// Set defaults
	if config.Level == "" {
		config.Level = getEnvOrDefault("LOG_LEVEL", "info")
	}
	if config.Format == "" {
		config.Format = getEnvOrDefault("LOG_FORMAT", "text")
	}
	if config.Output == "" {
		config.Output = getEnvOrDefault("LOG_OUTPUT", "stdout")
	}
	if config.Component == "" {
		config.Component = "maestro-cli"
	}
	if config.Version == "" {
		config.Version = "dev" // Should be set by build process
	}
	if config.Hostname == "" {
		if hostname, err := os.Hostname(); err == nil {
			config.Hostname = hostname
		} else {
			config.Hostname = "unknown"
		}
	}

	// Parse log level
	var level slog.Level
	switch strings.ToLower(config.Level) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	// Set output destination
	var output io.Writer
	switch config.Output {
	case "stderr":
		output = os.Stderr
	default:
		output = os.Stdout
	}

	// Create handler based on format
	var handler slog.Handler
	opts := &slog.HandlerOptions{
		Level: level,
	}

	switch config.Format {
	case "json":
		handler = slog.NewJSONHandler(output, opts)
	default:
		handler = slog.NewTextHandler(output, opts)
	}

	// Create base logger with required fields
	logger := slog.New(handler).With(
		"component", config.Component,
		"version", config.Version,
		"hostname", config.Hostname,
	)

	return &Logger{
		logger: logger,
		config: config,
	}
}

// WithContext extracts correlation fields from context and returns a logger with those fields
func (l *Logger) WithContext(ctx context.Context) *Logger {
	attrs := make([]slog.Attr, 0)

	// Extract correlation fields from context
	if traceID := getFromContext(ctx, TraceIDKey); traceID != "" {
		attrs = append(attrs, slog.String("trace_id", traceID))
	}
	if spanID := getFromContext(ctx, SpanIDKey); spanID != "" {
		attrs = append(attrs, slog.String("span_id", spanID))
	}
	if requestID := getFromContext(ctx, RequestIDKey); requestID != "" {
		attrs = append(attrs, slog.String("request_id", requestID))
	}
	if eventID := getFromContext(ctx, EventIDKey); eventID != "" {
		attrs = append(attrs, slog.String("event_id", eventID))
	}
	if clusterID := getFromContext(ctx, ClusterIDKey); clusterID != "" {
		attrs = append(attrs, slog.String("cluster_id", clusterID))
	}
	if resourceType := getFromContext(ctx, ResourceType); resourceType != "" {
		attrs = append(attrs, slog.String("resource_type", resourceType))
	}
	if resourceID := getFromContext(ctx, ResourceIDKey); resourceID != "" {
		attrs = append(attrs, slog.String("resource_id", resourceID))
	}

	if len(attrs) == 0 {
		return l // No context fields to add
	}

	// Create new logger with context fields
	// Convert []slog.Attr to []any for slog.Logger.With()
	args := make([]any, len(attrs))
	for i, attr := range attrs {
		args[i] = attr
	}
	newLogger := l.logger.With(args...)
	return &Logger{
		logger: newLogger,
		config: l.config,
	}
}

// With returns a new logger with additional fields
func (l *Logger) With(fields Fields) *Logger {
	attrs := make([]slog.Attr, 0, len(fields))
	for key, value := range fields {
		attrs = append(attrs, slog.Any(key, value))
	}

	// Convert []slog.Attr to []any for slog.Logger.With()
	args := make([]any, len(attrs))
	for i, attr := range attrs {
		args[i] = attr
	}
	newLogger := l.logger.With(args...)
	return &Logger{
		logger: newLogger,
		config: l.config,
	}
}

// Info logs an info message
func (l *Logger) Info(ctx context.Context, msg string, fields ...Fields) {
	l.log(ctx, slog.LevelInfo, msg, fields...)
}

// Debug logs a debug message
func (l *Logger) Debug(ctx context.Context, msg string, fields ...Fields) {
	l.log(ctx, slog.LevelDebug, msg, fields...)
}

// Warn logs a warning message
func (l *Logger) Warn(ctx context.Context, msg string, fields ...Fields) {
	l.log(ctx, slog.LevelWarn, msg, fields...)
}

// Error logs an error message with optional error and request context
func (l *Logger) Error(ctx context.Context, err error, msg string, fields ...Fields) {
	allFields := make(Fields)

	// Merge all field maps
	for _, f := range fields {
		for k, v := range f {
			allFields[k] = v
		}
	}

	// Add error field
	if err != nil {
		allFields["error"] = err.Error()

		// Add stack trace for debug level or unexpected errors
		if l.logger.Enabled(ctx, slog.LevelDebug) {
			allFields["stack_trace"] = getStackTrace()
		}
	}

	l.log(ctx, slog.LevelError, msg, allFields)
}

// log is the internal logging method
func (l *Logger) log(ctx context.Context, level slog.Level, msg string, fields ...Fields) {
	// Create logger with context if needed
	logger := l.WithContext(ctx).logger

	// Build attributes from fields
	attrs := make([]slog.Attr, 0)
	for _, f := range fields {
		for key, value := range f {
			attrs = append(attrs, slog.Any(key, value))
		}
	}

	logger.LogAttrs(ctx, level, msg, attrs...)
}

// getFromContext safely extracts string values from context
func getFromContext(ctx context.Context, key ContextKey) string {
	if value := ctx.Value(key); value != nil {
		if str, ok := value.(string); ok {
			return str
		}
	}
	return ""
}

// getEnvOrDefault returns environment variable value or default
func getEnvOrDefault(envKey, defaultValue string) string {
	if value := os.Getenv(envKey); value != "" {
		return value
	}
	return defaultValue
}

// getStackTrace returns the current stack trace
func getStackTrace() []string {
	const maxFrames = 15
	pcs := make([]uintptr, maxFrames)
	n := runtime.Callers(3, pcs) // Skip runtime.Callers, getStackTrace, and Error method

	frames := runtime.CallersFrames(pcs[:n])
	stack := make([]string, 0, n)

	for {
		frame, more := frames.Next()
		if !more {
			break
		}

		// Format: function() file:line
		funcName := frame.Function
		if idx := strings.LastIndex(funcName, "."); idx >= 0 {
			funcName = funcName[idx+1:]
		}

		stack = append(stack, fmt.Sprintf("%s() %s:%d", funcName, frame.File, frame.Line))

		if len(stack) >= 10 { // Limit to 10 frames per specification
			break
		}
	}

	return stack
}

// ContextWithTraceID adds trace_id to context
func ContextWithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, TraceIDKey, traceID)
}

// ContextWithSpanID adds span_id to context
func ContextWithSpanID(ctx context.Context, spanID string) context.Context {
	return context.WithValue(ctx, SpanIDKey, spanID)
}

// ContextWithEventID adds event_id to context
func ContextWithEventID(ctx context.Context, eventID string) context.Context {
	return context.WithValue(ctx, EventIDKey, eventID)
}

// ContextWithClusterID adds cluster_id to context
func ContextWithClusterID(ctx context.Context, clusterID string) context.Context {
	return context.WithValue(ctx, ClusterIDKey, clusterID)
}

// ContextWithResource adds resource_type and resource_id to context
func ContextWithResource(ctx context.Context, resourceType, resourceID string) context.Context {
	ctx = context.WithValue(ctx, ResourceType, resourceType)
	return context.WithValue(ctx, ResourceIDKey, resourceID)
}

// RequestContext represents request context for error logging
type RequestContext map[string]interface{}

// ToJSON converts RequestContext to JSON string, masking sensitive fields
func (rc RequestContext) ToJSON() string {
	masked := make(map[string]interface{})
	sensitiveKeys := map[string]bool{
		"password":      true,
		"token":         true,
		"secret":        true,
		"key":           true,
		"credential":    true,
		"auth":          true,
		"authorization": true,
	}

	for k, v := range rc {
		keyLower := strings.ToLower(k)
		isSensitive := false

		for sensitiveKey := range sensitiveKeys {
			if strings.Contains(keyLower, sensitiveKey) {
				isSensitive = true
				break
			}
		}

		if isSensitive {
			masked[k] = "[REDACTED]"
		} else {
			masked[k] = v
		}
	}

	data, _ := json.Marshal(masked)
	return string(data)
}
