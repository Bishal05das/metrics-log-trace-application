// Command loadgen drives synthetic traffic at the orders API.
//
// It exists because metrics are unreadable without load: a histogram with four
// observations tells you nothing, and `rate()` over a flat line is zero. This
// generator produces a realistic *mix*, including deliberate error traffic, so
// that error-rate and latency-percentile panels have something real in them.
//
// Deliberately zero dependencies beyond the standard library.
//
//	go run ./cmd/loadgen -rate 50 -duration 60s
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"
)

type opKind string

const (
	opCreate      opKind = "create"
	opGet         opKind = "get"
	opList        opKind = "list"
	opGetMissing  opKind = "get_missing_404"
	opBadPayload  opKind = "bad_payload_422"
	opBadUUID     opKind = "bad_uuid_400"
	opUnknownPath opKind = "unknown_path_404"
	opSlow        opKind = "slow_query"
)

// The mix. Weights are relative, not percentages. ~20% of traffic is errors,
// which is unrealistically high for a real service but makes the error-rate
// panels legible immediately.
var mix = []struct {
	kind   opKind
	weight int
}{
	{opCreate, 35},
	{opGet, 28},
	{opList, 15},
	{opGetMissing, 8},
	{opBadPayload, 7},
	{opBadUUID, 4},
	{opUnknownPath, 3},
}

func main() {
	var (
		baseURL  = flag.String("url", "http://localhost:8086", "base URL of the orders API")
		rate     = flag.Int("rate", 20, "target requests per second")
		duration = flag.Duration("duration", 60*time.Second, "how long to run; 0 means forever")
		workers  = flag.Int("concurrency", 16, "number of concurrent workers")
		seed     = flag.Int64("seed", 0, "RNG seed; 0 means time-based")

		// Pool-saturation mode. Each slow request holds one database
		// connection for slowDelay, so sustaining more than DB_MAX_CONNS of
		// them concurrently forces callers to queue for a connection.
		slowFrac  = flag.Float64("slow", 0, "fraction of traffic (0..1) hitting /debug/slow")
		slowDelay = flag.Duration("slow-delay", 200*time.Millisecond, "how long each slow query holds a connection")
	)
	flag.Parse()

	if *slowFrac < 0 || *slowFrac > 1 {
		fmt.Fprintln(os.Stderr, "-slow must be between 0 and 1")
		os.Exit(2)
	}

	if *seed == 0 {
		*seed = time.Now().UnixNano()
	}
	if *rate < 1 {
		fmt.Fprintln(os.Stderr, "rate must be >= 1")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			// Without connection reuse the generator spends its time in TCP
			// handshakes and you end up measuring the kernel, not the service.
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 256,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	g := &generator{
		base:      *baseURL,
		client:    client,
		ids:       newIDRing(2048),
		res:       newResults(),
		slowFrac:  *slowFrac,
		slowDelay: *slowDelay,
	}

	fmt.Printf("loadgen → %s   rate=%d/s  workers=%d  duration=%s  seed=%d\n",
		*baseURL, *rate, *workers, *duration, *seed)
	if *slowFrac > 0 {
		concurrentSlow := float64(*rate) * *slowFrac * slowDelay.Seconds()
		fmt.Printf("saturation mode: %.0f%% of traffic × %s ≈ %.1f connections held concurrently\n",
			*slowFrac*100, *slowDelay, concurrentSlow)
	}
	fmt.Println()

	// A single ticker is the rate source; workers pull from it. This gives an
	// open-loop generator: request pacing does not slow down when the service
	// gets slow, which is what you want for load testing. (A closed-loop
	// generator hides latency problems by naturally backing off.)
	ticks := make(chan struct{}, *rate)
	var wg sync.WaitGroup

	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func(seedOffset int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(*seed + seedOffset))
			for range ticks {
				g.do(ctx, rng)
			}
		}(int64(i))
	}

	interval := time.Second / time.Duration(*rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var dropped int
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			select {
			case ticks <- struct{}{}:
			default:
				// Every worker is busy. Count it: sustained drops mean the
				// service cannot keep up with the requested rate.
				dropped++
			}
		}
	}
	close(ticks)
	wg.Wait()

	g.res.print(dropped)
}

// ---------------------------------------------------------------------------

type generator struct {
	base      string
	client    *http.Client
	ids       *idRing
	res       *results
	slowFrac  float64
	slowDelay time.Duration
}

func (g *generator) do(ctx context.Context, rng *rand.Rand) {
	kind := pick(rng)

	// Saturation traffic is drawn independently of the normal mix so that the
	// ordinary endpoints keep their shape and you can watch THEIR latency
	// degrade as the pool fills — which is the point of the exercise.
	if g.slowFrac > 0 && rng.Float64() < g.slowFrac {
		kind = opSlow
	}

	var (
		method, path string
		body         []byte
	)

	switch kind {
	case opCreate:
		method, path = http.MethodPost, "/orders"
		body, _ = json.Marshal(map[string]any{
			"customer_id":  fmt.Sprintf("cust-%04d", rng.Intn(500)),
			"amount_cents": int64(100 + rng.Intn(500_00)),
			"currency":     []string{"USD", "EUR", "GBP", "INR"}[rng.Intn(4)],
		})

	case opGet:
		id, ok := g.ids.random(rng)
		if !ok {
			// Nothing created yet — fall back to a create so the ring fills.
			g.doCreate(ctx, rng)
			return
		}
		method, path = http.MethodGet, "/orders/"+id

	case opList:
		statuses := []string{"", "&status=pending", "&status=paid", "&status=failed"}
		method = http.MethodGet
		path = fmt.Sprintf("/orders?limit=%d&offset=%d%s",
			10+rng.Intn(40), rng.Intn(3)*10, statuses[rng.Intn(len(statuses))])

	case opGetMissing:
		method, path = http.MethodGet, "/orders/"+randomUUID(rng)

	case opBadPayload:
		method, path = http.MethodPost, "/orders"
		body = []byte(`{"customer_id":"","amount_cents":0,"currency":"US"}`)

	case opBadUUID:
		method, path = http.MethodGet, "/orders/not-a-uuid"

	case opUnknownPath:
		method, path = http.MethodGet, "/does-not-exist"

	case opSlow:
		method, path = http.MethodGet, "/debug/slow?d="+g.slowDelay.String()
	}

	code, respBody, latency := g.request(ctx, method, path, body)
	g.res.record(kind, code, latency)

	if kind == opCreate && code == http.StatusCreated {
		g.ids.add(extractID(respBody))
	}
}

func (g *generator) doCreate(ctx context.Context, rng *rand.Rand) {
	body, _ := json.Marshal(map[string]any{
		"customer_id":  fmt.Sprintf("cust-%04d", rng.Intn(500)),
		"amount_cents": int64(100 + rng.Intn(500_00)),
		"currency":     "USD",
	})
	code, respBody, latency := g.request(ctx, http.MethodPost, "/orders", body)
	g.res.record(opCreate, code, latency)
	if code == http.StatusCreated {
		g.ids.add(extractID(respBody))
	}
}

func (g *generator) request(ctx context.Context, method, path string, body []byte) (int, []byte, time.Duration) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, g.base+path, rdr)
	if err != nil {
		return 0, nil, 0
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	start := time.Now()
	resp, err := g.client.Do(req)
	latency := time.Since(start)
	if err != nil {
		return 0, nil, latency // 0 == transport error (refused, timeout, reset)
	}
	defer resp.Body.Close()

	// Read the body fully even when we don't need it: otherwise the connection
	// cannot be returned to the idle pool and reuse silently stops working.
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, latency
}

func pick(rng *rand.Rand) opKind {
	total := 0
	for _, m := range mix {
		total += m.weight
	}
	n := rng.Intn(total)
	for _, m := range mix {
		if n < m.weight {
			return m.kind
		}
		n -= m.weight
	}
	return opCreate
}

func extractID(body []byte) string {
	var v struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &v)
	return v.ID
}

func randomUUID(rng *rand.Rand) string {
	var b [16]byte
	for i := range b {
		b[i] = byte(rng.Intn(256))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ---------------------------------------------------------------------------

// idRing keeps a bounded sample of created order IDs so GETs hit real rows
// without the generator's memory growing without bound.
type idRing struct {
	mu   sync.Mutex
	buf  []string
	next int
}

func newIDRing(n int) *idRing { return &idRing{buf: make([]string, 0, n)} }

func (r *idRing) add(id string) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) < cap(r.buf) {
		r.buf = append(r.buf, id)
		return
	}
	r.buf[r.next] = id
	r.next = (r.next + 1) % cap(r.buf)
}

func (r *idRing) random(rng *rand.Rand) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) == 0 {
		return "", false
	}
	return r.buf[rng.Intn(len(r.buf))], true
}

// ---------------------------------------------------------------------------

type results struct {
	mu        sync.Mutex
	byKind    map[opKind]*stat
	byCode    map[int]int
	latencies []time.Duration
}

type stat struct {
	count int
	codes map[int]int
}

func newResults() *results {
	return &results{
		byKind:    map[opKind]*stat{},
		byCode:    map[int]int{},
		latencies: make([]time.Duration, 0, 100_000),
	}
}

func (r *results) record(kind opKind, code int, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.byKind[kind]
	if !ok {
		s = &stat{codes: map[int]int{}}
		r.byKind[kind] = s
	}
	s.count++
	s.codes[code]++
	r.byCode[code]++
	r.latencies = append(r.latencies, d)
}

func (r *results) print(dropped int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fmt.Println("\n─── results ────────────────────────────────────────────────")

	kinds := make([]string, 0, len(r.byKind))
	for k := range r.byKind {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)

	total := 0
	for _, k := range kinds {
		s := r.byKind[opKind(k)]
		total += s.count
		codes := make([]int, 0, len(s.codes))
		for c := range s.codes {
			codes = append(codes, c)
		}
		sort.Ints(codes)
		parts := ""
		for _, c := range codes {
			label := fmt.Sprint(c)
			if c == 0 {
				label = "ERR"
			}
			parts += fmt.Sprintf(" %s=%d", label, s.codes[c])
		}
		fmt.Printf("  %-18s %6d %s\n", k, s.count, parts)
	}

	fmt.Printf("\n  total requests: %d", total)
	if dropped > 0 {
		fmt.Printf("   (dropped %d ticks — all workers busy)", dropped)
	}
	fmt.Println()

	if len(r.latencies) == 0 {
		return
	}
	sort.Slice(r.latencies, func(i, j int) bool { return r.latencies[i] < r.latencies[j] })
	fmt.Printf("  client-side latency: p50=%s  p90=%s  p95=%s  p99=%s  max=%s\n",
		pct(r.latencies, 0.50), pct(r.latencies, 0.90),
		pct(r.latencies, 0.95), pct(r.latencies, 0.99),
		r.latencies[len(r.latencies)-1].Round(time.Microsecond))
	fmt.Println("\n  Compare these against the server-side histogram in Phase 2 —")
	fmt.Println("  the gap between them is queueing and network, and it is real.")
	fmt.Println("────────────────────────────────────────────────────────────")
}

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i].Round(time.Microsecond)
}
