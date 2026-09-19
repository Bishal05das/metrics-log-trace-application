package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// capture builds a logger writing JSON into a buffer, wrapped the same way
// New() wraps it, so tests exercise the real handler chain.
func capture() (*slog.Logger, *bytes.Buffer, *slog.LevelVar) {
	buf := &bytes.Buffer{}
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelDebug)
	base := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: lv, ReplaceAttr: replaceAttr})
	return slog.New(&ContextHandler{Handler: base}), buf, lv
}

func lastRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("no log output")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, lines[len(lines)-1])
	}
	return m
}

// TestContextAttrsReachEveryRecord is the correlation mechanism itself.
func TestContextAttrsReachEveryRecord(t *testing.T) {
	log, buf, _ := capture()

	ctx := WithRequestID(context.Background(), "abc123")
	ctx = WithAttrs(ctx, slog.String("route", "/orders"))

	log.InfoContext(ctx, "something happened")

	rec := lastRecord(t, buf)
	if rec[RequestIDKey] != "abc123" {
		t.Errorf("request_id = %v, want abc123", rec[RequestIDKey])
	}
	if rec["route"] != "/orders" {
		t.Errorf("route = %v, want /orders", rec["route"])
	}
}

// TestNonContextCallsLoseCorrelation documents the one real footgun in slog:
// Info() passes context.Background(), so it silently has nothing to enrich.
// If this ever starts passing, slog changed and the guidance should be updated.
func TestNonContextCallsLoseCorrelation(t *testing.T) {
	log, buf, _ := capture()

	ctx := WithRequestID(context.Background(), "abc123")
	_ = ctx
	log.Info("no context passed")

	if _, ok := lastRecord(t, buf)[RequestIDKey]; ok {
		t.Error("expected NO request_id — use InfoContext to get correlation")
	}
}

// TestWithAttrsDoesNotAliasParentSlice — appending in place to a slice held by
// a parent context is a data race that shows up as attributes appearing on
// unrelated log lines.
func TestWithAttrsDoesNotAliasParentSlice(t *testing.T) {
	root := WithAttrs(context.Background(), slog.String("a", "1"))

	left := WithAttrs(root, slog.String("b", "left"))
	right := WithAttrs(root, slog.String("b", "right"))

	if len(Attrs(root)) != 1 {
		t.Fatalf("parent mutated: %v", Attrs(root))
	}
	if Attrs(left)[1].Value.String() != "left" || Attrs(right)[1].Value.String() != "right" {
		t.Fatalf("branches aliased: left=%v right=%v", Attrs(left), Attrs(right))
	}
}

// TestLoggerWithStillEnriches guards the classic slog wrapper bug: if
// ContextHandler.WithAttrs forgets to re-wrap, logger.With(...) returns the
// bare base handler and correlation silently stops from that point on.
//
// logger.With() is the case that matters — it is how every component-scoped
// logger in this project is built.
func TestLoggerWithStillEnriches(t *testing.T) {
	log, buf, _ := capture()

	child := log.With(slog.String("component", "worker"))
	child.InfoContext(WithRequestID(context.Background(), "zz9"), "from the worker")

	rec := lastRecord(t, buf)
	if rec[RequestIDKey] != "zz9" {
		t.Errorf("correlation lost after With(): %v", rec)
	}
	if rec["component"] != "worker" {
		t.Errorf("component attr lost: %v", rec)
	}
}

// TestWithGroupNestsContextAttrs pins a real slog behaviour that bites
// context-based enrichment.
//
// Attributes added to a Record inside Handle are qualified by any group opened
// with WithGroup. So after .WithGroup("g"), request_id is emitted as
// {"g":{"request_id":"..."}} rather than at the top level.
//
// That silently breaks correlation downstream: anything looking for a
// top-level request_id finds nothing, because the field is now nested. There is
// no clean way for an outer wrapper to hoist attributes out of a group the
// inner handler has already opened.
//
// The practical rule, which this test exists to keep honest: DO NOT use
// WithGroup on the application logger. Use flat attribute names instead.
func TestWithGroupNestsContextAttrs(t *testing.T) {
	log, buf, _ := capture()

	log.WithGroup("g").InfoContext(WithRequestID(context.Background(), "zz9"), "grouped")

	rec := lastRecord(t, buf)
	if _, topLevel := rec[RequestIDKey]; topLevel {
		t.Fatal("request_id is now top-level under WithGroup — slog changed, " +
			"revisit the guidance in ContextHandler's doc comment")
	}
	g, ok := rec["g"].(map[string]any)
	if !ok || g[RequestIDKey] != "zz9" {
		t.Fatalf("expected request_id nested under the group, got %v", rec)
	}
}

func TestSecretsAreRedacted(t *testing.T) {
	log, buf, _ := capture()

	log.InfoContext(context.Background(), "login",
		"password", "hunter2",
		"api_key", "sk-live-123",
		"Authorization", "Bearer abc",
		"refresh_token", "rt-9",
		"customer_id", "cust-1")

	rec := lastRecord(t, buf)
	for _, k := range []string{"password", "api_key", "Authorization", "refresh_token"} {
		if rec[k] != "[REDACTED]" {
			t.Errorf("%s = %v, want [REDACTED]", k, rec[k])
		}
	}
	// Not sensitive by this rule — it is pseudonymised with PII() instead.
	if rec["customer_id"] != "cust-1" {
		t.Errorf("customer_id was unexpectedly altered: %v", rec["customer_id"])
	}
}

func TestPIIIsStableAndOpaque(t *testing.T) {
	a := PII("customer", "cust-0421")
	b := PII("customer", "cust-0421")
	c := PII("customer", "cust-0422")

	if a.Value.String() != b.Value.String() {
		t.Error("pseudonym is not stable — correlation across lines would break")
	}
	if a.Value.String() == c.Value.String() {
		t.Error("different inputs collided")
	}
	if strings.Contains(a.Value.String(), "cust-0421") {
		t.Errorf("original value leaked: %s", a.Value.String())
	}
}

func TestSanitizeRequestID(t *testing.T) {
	cases := map[string]bool{
		"abc123":                 true,
		"a-b_c.d":                true,
		"":                       false,
		"has space":              false,
		"newline\ninjected":      false,
		`{"level":"ERROR"}`:      false,
		strings.Repeat("x", 129): false,
		strings.Repeat("x", 128): true,
	}
	for in, wantOK := range cases {
		if _, ok := SanitizeRequestID(in); ok != wantOK {
			t.Errorf("SanitizeRequestID(%q) ok = %v, want %v", in, ok, wantOK)
		}
	}
}

func TestNewRequestIDIsUniqueAndHex(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewRequestID()
		if len(id) != 32 {
			t.Fatalf("length %d, want 32 hex chars", len(id))
		}
		if seen[id] {
			t.Fatal("collision in 1000 ids")
		}
		seen[id] = true
	}
}

// TestSamplingDropsRepeatsButNeverWarnings — the safety property. Dropping a
// warning or error to save volume loses information you cannot recover.
func TestSamplingDropsRepeatsButNeverWarnings(t *testing.T) {
	buf := &bytes.Buffer{}
	base := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	log := slog.New(NewSamplingHandler(base, 3, time.Hour, nil))

	for i := 0; i < 50; i++ {
		log.Info("noisy")
	}
	for i := 0; i < 10; i++ {
		log.Warn("important")
	}

	var info, warn int
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		switch {
		case strings.Contains(line, `"noisy"`):
			info++
		case strings.Contains(line, `"important"`):
			warn++
		}
	}
	if info != 3 {
		t.Errorf("emitted %d of 50 sampled info records, want 3", info)
	}
	if warn != 10 {
		t.Errorf("emitted %d of 10 warnings, want all 10", warn)
	}
}

func TestSamplingReportsDropCount(t *testing.T) {
	buf := &bytes.Buffer{}
	base := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := NewSamplingHandler(base, 1, 10*time.Millisecond, nil)
	log := slog.New(h)

	for i := 0; i < 5; i++ {
		log.Info("repeated")
	}
	time.Sleep(15 * time.Millisecond)
	log.Info("repeated") // first record of the new window carries the drop count

	if !strings.Contains(buf.String(), "sampled_dropped") {
		t.Error("dropped count was never reported — sampling would be invisible")
	}
}

func TestLevelVarChangesTakeEffectImmediately(t *testing.T) {
	log, buf, lv := capture()

	lv.Set(slog.LevelWarn)
	log.InfoContext(context.Background(), "should be filtered")
	if buf.Len() != 0 {
		t.Fatalf("info emitted at warn level: %s", buf.String())
	}

	lv.Set(slog.LevelDebug)
	log.DebugContext(context.Background(), "now visible")
	if buf.Len() == 0 {
		t.Fatal("debug not emitted after level change")
	}
}

// TestSamplingBudgetIsSharedAcrossDerivedLoggers.
//
// slog.Logger.With() calls WithAttrs, which returns a NEW handler. An earlier
// version gave that new handler a fresh bucket map, so "3 per interval" quietly
// became "3 per interval PER derived logger" — and since New() itself calls
// .With(service, version, env), and call sites add more, the sampler would have
// let a flood straight through the component built to stop it.
func TestSamplingBudgetIsSharedAcrossDerivedLoggers(t *testing.T) {
	buf := &bytes.Buffer{}
	base := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	root := slog.New(NewSamplingHandler(base, 3, time.Hour, nil))

	a := root.With("component", "a")
	b := root.With("component", "b")

	for i := 0; i < 10; i++ {
		a.Info("same message")
		b.Info("same message")
	}

	if got := strings.Count(buf.String(), "same message"); got != 3 {
		t.Errorf("emitted %d records, want 3 — the budget is not shared "+
			"across handlers derived with With()", got)
	}
}

// New() must actually install the sampler, and must leave WARN+ alone.
func TestNewWiresSampling(t *testing.T) {
	logger, _ := New(Config{Level: "debug", SampleN: 2, SampleInterval: time.Hour})
	if logger == nil {
		t.Fatal("New returned nil")
	}

	buf := &bytes.Buffer{}
	base := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := NewSamplingHandler(&ContextHandler{Handler: base}, 2, time.Hour, nil)
	l := slog.New(h).With("service", "orders")

	for i := 0; i < 5; i++ {
		l.Info("chatty")
		l.Warn("rare but important")
	}

	if got := strings.Count(buf.String(), "chatty"); got != 2 {
		t.Errorf("INFO emitted %d times, want 2 (sampled)", got)
	}
	if got := strings.Count(buf.String(), "rare but important"); got != 5 {
		t.Errorf("WARN emitted %d times, want 5 — WARN and above must never "+
			"be sampled; dropping one loses information you cannot recover", got)
	}
}

// Sampling must not break correlation: a record that survives still needs its
// context attributes, which means ContextHandler has to stay in the chain
// underneath the sampler.
func TestSampledRecordsKeepContextAttrs(t *testing.T) {
	buf := &bytes.Buffer{}
	base := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	l := slog.New(NewSamplingHandler(&ContextHandler{Handler: base}, 5, time.Hour, nil))

	l.InfoContext(WithRequestID(context.Background(), "req-abc"), "hello")

	if !strings.Contains(buf.String(), `"request_id":"req-abc"`) {
		t.Errorf("context attrs lost under sampling: %s", buf.String())
	}
}

// fakeRecorder counts calls, standing in for metrics.Logs.
type fakeRecorder struct {
	mu      sync.Mutex
	records map[string]int
	dropped map[string]int
}

func newFakeRecorder() *fakeRecorder {
	return &fakeRecorder{records: map[string]int{}, dropped: map[string]int{}}
}

func (f *fakeRecorder) LogRecord(level string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[level]++
}

func (f *fakeRecorder) LogRecordDropped(level string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped[level]++
}

// TestRecorderCountsAttemptsNotSurvivors.
//
// countingHandler must sit OUTSIDE the sampler. Inside, orders_log_records_total
// would count only the records that survived, the drop RATIO would always be
// zero, and the alert built on it could never fire.
func TestRecorderCountsAttemptsNotSurvivors(t *testing.T) {
	rec := newFakeRecorder()
	buf := &bytes.Buffer{}
	base := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})

	var h slog.Handler = NewSamplingHandler(&ContextHandler{Handler: base}, 2, time.Hour, rec)
	h = &countingHandler{Handler: h, rec: rec}
	l := slog.New(h)

	for i := 0; i < 10; i++ {
		l.Info("chatty")
	}

	if got := rec.records["INFO"]; got != 10 {
		t.Errorf("records counted = %d, want 10 (every ATTEMPT, not just survivors)", got)
	}
	if got := rec.dropped["INFO"]; got != 8 {
		t.Errorf("dropped counted = %d, want 8", got)
	}
	if got := strings.Count(buf.String(), "chatty"); got != 2 {
		t.Errorf("emitted = %d, want 2", got)
	}
}

// Counting must survive logger.With(), the classic slog wrapper bug.
func TestRecorderSurvivesWith(t *testing.T) {
	rec := newFakeRecorder()
	base := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})
	l := slog.New(&countingHandler{Handler: base, rec: rec}).With("component", "x")

	l.Info("hello")
	l.WithGroup("g").Warn("grouped")

	if rec.records["INFO"] != 1 || rec.records["WARN"] != 1 {
		t.Errorf("counting stopped after With/WithGroup: %v", rec.records)
	}
}

// WARN and above are never sampled, so they must never be counted as dropped.
func TestWarningsAreNeverDropped(t *testing.T) {
	rec := newFakeRecorder()
	base := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})
	var h slog.Handler = NewSamplingHandler(base, 1, time.Hour, rec)
	h = &countingHandler{Handler: h, rec: rec}
	l := slog.New(h)

	for i := 0; i < 5; i++ {
		l.Error("boom")
	}

	if got := rec.dropped["ERROR"]; got != 0 {
		t.Errorf("dropped %d ERROR records; WARN and above must never be sampled", got)
	}
	if got := rec.records["ERROR"]; got != 5 {
		t.Errorf("counted %d ERROR records, want 5", got)
	}
}

// New() must wire the Recorder through; nil must remain safe.
func TestNewWiresRecorder(t *testing.T) {
	rec := newFakeRecorder()
	l, _ := New(Config{Level: "debug", SampleN: 100, SampleInterval: time.Hour, Recorder: rec})
	l.Info("wired")
	if rec.records["INFO"] == 0 {
		t.Error("New did not install the Recorder")
	}

	// nil Recorder must not panic.
	l2, _ := New(Config{Level: "debug", SampleN: 100, SampleInterval: time.Hour})
	l2.Info("no recorder")
}
