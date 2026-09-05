package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
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
	log := slog.New(NewSamplingHandler(base, 3, time.Hour))

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
	h := NewSamplingHandler(base, 1, 10*time.Millisecond)
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
