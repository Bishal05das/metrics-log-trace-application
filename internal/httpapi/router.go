package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/bishal05das/metrics-log-trace-application/internal/store"
)

// InstrumentedRoutes lists {method, route, success code} for every endpoint
// that carries request metrics. It pre-initialises those series at zero so
// dashboards and alerts have something to match before the first request.
//
// The success code is explicit rather than assumed to be 200. Getting it wrong
// is not harmless: it creates a series pinned at zero forever while the real
// traffic accumulates on a different one, so a panel filtered on code="200"
// shows a flat line through a fully healthy service.
//
// Health endpoints are absent deliberately: the metrics middleware skips them.
var InstrumentedRoutes = [][3]string{
	{http.MethodPost, "/orders", "201"},
	{http.MethodGet, "/orders", "200"},
	{http.MethodGet, "/orders/{id}", "200"},
}

// Options controls optional route groups.
type Options struct {
	// EnableDebugRoutes exposes fault-injection endpoints (/debug/slow).
	//
	// Deliberate fault injection behind a flag is a real production practice —
	// it is how you verify that your alerts actually fire before an incident
	// does it for you. It is gated on APP_ENV=dev here because an unauthenticated
	// "hold a database connection for N seconds" endpoint is a denial-of-service
	// primitive handed to the internet.
	EnableDebugRoutes bool
}

// NewRouter wires the routes and the middleware chain.
//
// We use the standard library's ServeMux (Go 1.22+ method-and-wildcard
// patterns). That is a deliberate choice: since Go 1.23, http.Request.Pattern
// holds the *pattern that matched* — "GET /orders/{id}" rather than
// "/orders/8f3c...". ServeMux sets it in place on the request, so an outer
// middleware can read it after next.ServeHTTP returns. That field is the
// `route` label on our request metrics and the main defence against a
// cardinality explosion. No third-party router needed.
//
// mw[0] ends up outermost; see chain().
// Deps are the router's dependencies. A struct rather than a growing parameter
// list, so adding one does not churn every call site.
type Deps struct {
	Store   *store.Store
	Log     *slog.Logger
	Events  OrderEvents // optional; nil disables business events
	Options Options
}

func NewRouter(d Deps, mw ...Middleware) http.Handler {
	s, log, opts := d.Store, d.Log, d.Options
	api := NewAPI(s, log, d.Events)

	mux := http.NewServeMux()

	if opts.EnableDebugRoutes {
		mux.HandleFunc("GET /debug/slow", api.slowQuery)
		log.Warn("debug routes enabled", "routes", []string{"GET /debug/slow"})
	}

	mux.HandleFunc("POST /orders", api.createOrder)
	mux.HandleFunc("GET /orders", api.listOrders)
	mux.HandleFunc("GET /orders/{id}", api.getOrder)

	mux.HandleFunc("GET /healthz", api.healthz)
	mux.HandleFunc("GET /readyz", api.readyz)

	// Catch-all. Registering it explicitly matters for metrics: every unknown
	// path matches this pattern, so r.Pattern is "/" rather than empty, and
	// routeLabel() folds it into the single "unmatched" series.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, codeMethodUnknown, "no such endpoint", "")
	})

	return chain(mux, mw...)
}
