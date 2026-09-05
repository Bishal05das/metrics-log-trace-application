package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bishal05das/metrics-log-trace-application/internal/logging"
)

func logCapture() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	base := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(&logging.ContextHandler{Handler: base}), buf
}

func records(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, line)
		}
		out = append(out, m)
	}
	return out
}

func loggedMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.PathValue("id") == "boom" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	return mux
}

func TestRequestIDIsGeneratedAndEchoed(t *testing.T) {
	log, _ := logCapture()
	h := chain(loggedMux(), RequestID(), AccessLog(log))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orders/abc", nil))

	got := rec.Header().Get(logging.RequestIDHeader)
	if len(got) != 32 {
		t.Fatalf("X-Request-Id = %q, want a 32-char generated id", got)
	}
}

// TestInboundRequestIDIsHonoured — the ID must survive a service hop so one
// identifier spans the whole call graph.
func TestInboundRequestIDIsHonoured(t *testing.T) {
	log, buf := logCapture()
	h := chain(loggedMux(), RequestID(), AccessLog(log))

	req := httptest.NewRequest(http.MethodGet, "/orders/abc", nil)
	req.Header.Set(logging.RequestIDHeader, "upstream-123")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get(logging.RequestIDHeader) != "upstream-123" {
		t.Errorf("inbound id not reused: %q", rec.Header().Get(logging.RequestIDHeader))
	}
	if r := records(t, buf); r[0]["request_id"] != "upstream-123" {
		t.Errorf("log line carries %v, want upstream-123", r[0]["request_id"])
	}
}

// TestMaliciousRequestIDIsReplaced — the header is attacker-controlled. A value
// containing a newline plus crafted JSON is a log-forging attempt.
func TestMaliciousRequestIDIsReplaced(t *testing.T) {
	log, _ := logCapture()
	h := chain(loggedMux(), RequestID(), AccessLog(log))

	req := httptest.NewRequest(http.MethodGet, "/orders/abc", nil)
	req.Header.Set(logging.RequestIDHeader, "bad\n{\"level\":\"ERROR\",\"msg\":\"forged\"}")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := rec.Header().Get(logging.RequestIDHeader)
	if strings.ContainsAny(got, "\n{\" ") {
		t.Fatalf("unsanitised id reached the response: %q", got)
	}
	if len(got) != 32 {
		t.Fatalf("expected a freshly generated id, got %q", got)
	}
}

// TestAccessLogLevelFollowsFault — 5xx is our bug (error); 4xx is the client's
// mistake (warn). Logging 4xx at error fills the error stream with people
// typing bad UUIDs and trains everyone to ignore it.
func TestAccessLogLevelFollowsFault(t *testing.T) {
	cases := []struct {
		path      string
		wantLevel string
		wantCode  float64
	}{
		{"/orders/abc", "INFO", 200},
		{"/orders/missing", "WARN", 404},
		{"/orders/boom", "ERROR", 500},
	}
	for _, c := range cases {
		log, buf := logCapture()
		h := chain(loggedMux(), RequestID(), AccessLog(log))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, c.path, nil))

		r := records(t, buf)
		if len(r) != 1 {
			t.Fatalf("%s: want exactly 1 access log line, got %d", c.path, len(r))
		}
		if r[0]["level"] != c.wantLevel {
			t.Errorf("%s: level = %v, want %v", c.path, r[0]["level"], c.wantLevel)
		}
		if r[0]["status"] != c.wantCode {
			t.Errorf("%s: status = %v, want %v", c.path, r[0]["status"], c.wantCode)
		}
	}
}

// TestAccessLogRouteIsTemplateNotPath — the same cardinality firewall as the
// metrics middleware. A log FIELD tolerates high cardinality better than a
// metric label, but this field is a candidate for an indexed label in whatever
// log store this ships to, and must stay bounded regardless.
func TestAccessLogRouteIsTemplateNotPath(t *testing.T) {
	log, buf := logCapture()
	h := chain(loggedMux(), RequestID(), AccessLog(log))

	for _, id := range []string{"aaa", "bbb", "ccc"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/orders/"+id, nil))
	}
	for _, r := range records(t, buf) {
		if r["route"] != "/orders/{id}" {
			t.Fatalf("route = %v, want the template /orders/{id}", r["route"])
		}
	}
}

func TestAccessLogFoldsUnmatchedRoutes(t *testing.T) {
	log, buf := logCapture()
	h := chain(loggedMux(), RequestID(), AccessLog(log))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nope-"+strings.Repeat("x", 20), nil))

	if r := records(t, buf); r[0]["route"] != "unmatched" {
		t.Errorf("route = %v, want unmatched", r[0]["route"])
	}
}

// TestHealthProbesAreNotLogged — probes fire every few seconds forever and say
// nothing. Same exclusion as the metrics middleware.
func TestHealthProbesAreNotLogged(t *testing.T) {
	log, buf := logCapture()
	h := chain(loggedMux(), RequestID(), AccessLog(log))

	for i := 0; i < 5; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	}
	if buf.Len() != 0 {
		t.Fatalf("health probes were logged:\n%s", buf.String())
	}
}

// TestPanicIsLoggedAsFiveHundred — AccessLog sits OUTSIDE Recover, so it
// observes the 500 that recovery produces. Reversed, every panic would be
// logged as a success.
func TestPanicIsLoggedAsFiveHundred(t *testing.T) {
	log, buf := logCapture()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })

	h := chain(mux, RequestID(), AccessLog(log), Recover(logging.Discard(), nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))

	var access map[string]any
	for _, r := range records(t, buf) {
		if r["msg"] == "http request" {
			access = r
		}
	}
	if access == nil {
		t.Fatal("no access log line for a panicking request")
	}
	if access["status"] != float64(500) || access["level"] != "ERROR" {
		t.Errorf("panic logged as status=%v level=%v, want 500/ERROR", access["status"], access["level"])
	}
}

func TestClientIPPrefersForwardedFor(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.9:5555"

	if got := clientIP(r); got != "10.0.0.9" {
		t.Errorf("clientIP = %q, want 10.0.0.9", got)
	}

	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.1.1.1")
	if got := clientIP(r); got != "203.0.113.7" {
		t.Errorf("clientIP = %q, want the left-most XFF entry", got)
	}
}
