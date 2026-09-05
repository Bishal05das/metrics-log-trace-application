// Package logging is the second observability pillar.
//
// The division of labour between the three is worth stating plainly, because
// teams routinely use the wrong one and pay for it:
//
//	METRICS  cheap, aggregatable, bounded. Answer "is something wrong?" and
//	         "how much?". You cannot ask them about one specific request.
//	LOGS     expensive, high-cardinality, per-event. Answer "what exactly
//	         happened to THIS request?". You cannot afford to aggregate them.
//	TRACES   answer "where did the time go, across services?".
//
// A metric told you the error rate jumped. A log tells you it was customer
// cust-0421 hitting a unique-constraint violation on insert_order. Metrics
// scope the incident; logs explain it. Reaching for logs to compute a rate is
// the single most expensive mistake in observability — that is what the
// counter in Phase 2 is for.
package logging

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

type Format string

const (
	FormatJSON Format = "json"
	FormatText Format = "text"
)

type Config struct {
	Level  string
	Format Format

	// Source adds file:line to every record. Genuinely useful and genuinely
	// not free — it costs a runtime.Caller per record. Default it off in
	// production and turn it on with the runtime level endpoint when you need
	// it.
	Source bool

	// Constant identity attached to every line. These are what let you filter
	// a log stream down to one service and one deploy.
	Service string
	Version string
	Env     string
}

// New builds the application logger.
//
// It returns the *slog.LevelVar alongside the logger so the level can be
// changed at RUNTIME without a redeploy — see level.go. That capability is
// worth the extra return value: the moment you actually want debug logs is
// during an incident, and "ship a config change and wait for a rolling
// restart" is not an acceptable answer at that moment.
func New(cfg Config) (*slog.Logger, *slog.LevelVar) {
// 	Development:
	// DEBUG
	// INFO
	// WARN
	// ERROR

// Production:
	// INFO
	// WARN
	// ERROR
	levelVar := new(slog.LevelVar)
	levelVar.Set(parseLevel(cfg.Level))

	opts := &slog.HandlerOptions{
		Level:       levelVar,
		AddSource:   cfg.Source, //from where log was generated
		ReplaceAttr: replaceAttr,
	}
    // we want log as json format
	var base slog.Handler
	switch cfg.Format {
	case FormatText:
		base = slog.NewTextHandler(os.Stdout, opts)
	default:
		base = slog.NewJSONHandler(os.Stdout, opts)
	}

	// ContextHandler wraps the base so that request-scoped attributes stored
	// in the context are attached to EVERY record automatically. See handler.go
	// — it is the piece that makes correlation work without touching call sites.
	h := slog.Handler(&ContextHandler{Handler: base})

	logger := slog.New(h)

	//with each log this will be added automatically
	return logger.With(
		slog.String("service", cfg.Service),
		slog.String("version", cfg.Version),
		slog.String("env", cfg.Env),
	), levelVar
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToLower(s))); err != nil {
		return slog.LevelInfo
	}
	return l
}

// replaceAttr normalises and redacts every attribute on its way out.
//
// This is the ONE place that can guarantee a secret never reaches stdout. Rely
// on developers remembering not to log a password and you will eventually find
// one in your log store, replicated to three regions, retained for 90 days.
func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	// Trim absolute build paths: "/src/internal/store/orders.go:42" is useful,
	// "/home/runner/work/.../orders.go:42" is noise that also leaks your CI
	// directory layout.
	if a.Key == slog.SourceKey {
		if src, ok := a.Value.Any().(*slog.Source); ok {
			src.File = filepath.Join(filepath.Base(filepath.Dir(src.File)), filepath.Base(src.File))
		}
		return a
	}

	if isSensitive(a.Key) {
		return slog.String(a.Key, "[REDACTED]")
	}

	// Errors do not implement LogValuer, and the JSON handler would otherwise
	// render some of them as "{}". Force the message text.
	if err, ok := a.Value.Any().(error); ok {
		return slog.String(a.Key, err.Error())
	}
	return a
}

// sensitiveKeys is deliberately matched on a SUBSTRING basis: `token`,
// `access_token` and `refresh_token` should all be caught without enumerating
// every variant someone invents.
var sensitiveKeys = []string{
	"password", "passwd", "secret", "token", "authorization", "auth",
	"api_key", "apikey", "credential", "cookie", "session", "private_key",
	"card_number", "cvv", "ssn",
}

func isSensitive(key string) bool {
	k := strings.ToLower(key)
	for _, s := range sensitiveKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// PII wraps a value that identifies a person.
//
// Redaction is not the answer here: you frequently need to correlate all the
// events for one customer during an incident, which a blanket [REDACTED]
// prevents. A stable pseudonym gives you that correlation without storing the
// identifier itself — the log store can be breached and it reveals nothing,
// while "show me every line for this customer" still works.
//
// Truncated FNV-1a is used because it is fast and this is a pseudonym, not a
// security boundary. Where the identifier space is small enough to brute-force
// (an email list, say), you need a keyed hash — HMAC with a secret salt — and
// the salt must not be in the log store.
func PII(key string, value string) slog.Attr {
	if value == "" {
		return slog.String(key, "")
	}
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	h := uint64(offset64)
	for i := 0; i < len(value); i++ {
		h ^= uint64(value[i])
		h *= prime64
	}
	return slog.String(key, fmt.Sprintf("px_%012x", h&0xffffffffffff))
}

// Discard is a no-op logger for tests.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{
		Level: slog.LevelError + 1,
	}))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
