package logging

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// ContextHandler attaches context-carried attributes to every record.
//
// This is the whole correlation mechanism, and it is about fifteen lines. Any
// code that calls log.InfoContext(ctx, ...) gets request_id, route and method
// attached without knowing they exist.
//
// Two caveats, both of which silently break correlation rather than failing
// loudly, and both worth knowing before you rely on this:
//
//  1. It only works for the *Context variants. slog.Info(...) and log.Info(...)
//     pass context.Background(), so their records arrive here with nothing to
//     attach. Every call site in this project uses InfoContext/ErrorContext,
//     and a linter rule enforcing that is worth having.
//
//  2. DO NOT use WithGroup on this logger. Attributes added to a Record inside
//     Handle are qualified by any open group, so after .WithGroup("g") the
//     request_id is emitted as {"g":{"request_id":"..."}} instead of at the top
//     level. Any downstream tooling looking for a top-level request_id — a log
//     store's JSON parser, a grep, a dashboard link — then silently finds
//     nothing. An outer wrapper cannot hoist attributes back out of a group the
//     inner handler has already opened, so use flat attribute names.
//     TestWithGroupNestsContextAttrs pins this behaviour.
type ContextHandler struct {
	slog.Handler
}

func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs := Attrs(ctx); len(attrs) > 0 {
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup must be forwarded and re-wrapped, or logger.With(...)
// would return the UNWRAPPED base handler and quietly disable context
// enrichment from that point on. This is the classic slog wrapper bug.
func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{Handler: h.Handler.WithGroup(name)}
}

// SamplingHandler rate-limits repetitive low-severity records.
//
// Why you need it: logs cost roughly 10-100x what the equivalent metric costs,
// per event, forever. A single hot path logging at INFO on a service doing
// 5k rps produces 432 million lines a day, which is both an enormous bill and
// — worse — completely unreadable. The failure mode is not that logging breaks;
// it is that the one line explaining your outage is buried under ten million
// identical ones.
//
// The rule implemented here: WARN and above are ALWAYS emitted, because they
// are rare by construction and dropping one loses information you cannot
// recover. Below that, allow up to `perInterval` records per distinct message
// per interval, and count what was dropped so the volume is still visible.
//
// Sampling by MESSAGE (not by line) is deliberate: a flood is almost always the
// same message repeated, and keeping the first few of each distinct message
// preserves variety, which is what you actually want when reading.
type SamplingHandler struct {
	slog.Handler

	perInterval int
	interval    time.Duration

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	windowStart time.Time
	seen        int
	dropped     int
}

func NewSamplingHandler(h slog.Handler, perInterval int, interval time.Duration) *SamplingHandler {
	if perInterval <= 0 {
		perInterval = 10
	}
	if interval <= 0 {
		interval = time.Second
	}
	return &SamplingHandler{
		Handler:     h,
		perInterval: perInterval,
		interval:    interval,
		buckets:     make(map[string]*bucket),
	}
}

func (h *SamplingHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		return h.Handler.Handle(ctx, r)
	}

	// One critical section. Releasing the lock between reading and updating the
	// bucket would let a concurrent record observe a half-updated window, and
	// under load "concurrent" is the only case that matters.
	h.mu.Lock()

	// Bound the map. Log messages are supposed to be static strings, but a
	// caller that interpolates a value into the message ("order abc123 failed")
	// would otherwise grow this map without limit — a memory leak inside the
	// component whose job is to prevent runaway logging.
	if len(h.buckets) > maxSampleKeys {
		h.buckets = make(map[string]*bucket, maxSampleKeys)
	}

	b, ok := h.buckets[r.Message]
	if !ok {
		b = &bucket{windowStart: r.Time}
		h.buckets[r.Message] = b
	}

	var dropped int
	switch {
	case r.Time.Sub(b.windowStart) >= h.interval:
		// Window rolled over. Carry the previous window's drop count onto this
		// record so the true volume is never invisible — silent sampling is how
		// people end up believing a hot path executes far less than it does.
		dropped = b.dropped
		b.windowStart, b.seen, b.dropped = r.Time, 1, 0
	case b.seen >= h.perInterval:
		b.dropped++
		h.mu.Unlock()
		return nil // dropped
	default:
		b.seen++
	}
	h.mu.Unlock()

	if dropped > 0 {
		r.AddAttrs(slog.Int("sampled_dropped", dropped))
	}
	return h.Handler.Handle(ctx, r)
}

const maxSampleKeys = 1024

func (h *SamplingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &SamplingHandler{
		Handler: h.Handler.WithAttrs(attrs), perInterval: h.perInterval,
		interval: h.interval, buckets: make(map[string]*bucket),
	}
}

func (h *SamplingHandler) WithGroup(name string) slog.Handler {
	return &SamplingHandler{
		Handler: h.Handler.WithGroup(name), perInterval: h.perInterval,
		interval: h.interval, buckets: make(map[string]*bucket),
	}
}
