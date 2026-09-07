package iptvproxy

import (
	"context"
	"log/slog"
)

type loggerCtxKeyType struct{}

var loggerCtxKey = loggerCtxKeyType{}

// withLogger attaches logger to ctx so every layer downstream of the HTTP
// handler (service, repository) can retrieve the same request-scoped logger
// via loggerFromContext instead of threading it through every call signature.
func withLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerCtxKey, logger)
}

// loggerFromContext returns the logger attached to ctx by withLogger, or
// fallback if none was attached (e.g. ctx wasn't derived from a request).
func loggerFromContext(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if logger, ok := ctx.Value(loggerCtxKey).(*slog.Logger); ok {
		return logger
	}
	return fallback
}
