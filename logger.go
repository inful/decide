package decide

// Logger is the minimal logging interface used by the client. *slog.Logger
// satisfies it directly, so callers can pass a configured slog.Logger with no
// adapter.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// DiscardLogger satisfies Logger by dropping every call.
type DiscardLogger struct{}

func (DiscardLogger) Debug(string, ...any) {}
func (DiscardLogger) Info(string, ...any)  {}
func (DiscardLogger) Warn(string, ...any)  {}
func (DiscardLogger) Error(string, ...any) {}