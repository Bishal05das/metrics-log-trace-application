package logging

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// LevelHandler exposes the log level for reading and changing at runtime.
//
//	GET  /loglevel            -> {"level":"INFO"}
//	PUT  /loglevel?level=debug -> {"level":"DEBUG"}
//
// Why this exists: the moment you actually want debug logs is during an
// incident, and "change an env var, ship it, wait for a rolling restart" is not
// an acceptable answer then — the restart also destroys the in-memory state you
// were trying to observe. slog.LevelVar is atomic and read on every record, so
// flipping it takes effect on the very next line with no restart.
//
// This is mounted on the ADMIN port (9100), never the public API. It changes
// the behaviour of the process and, at debug, can substantially increase log
// volume and cost — an unauthenticated internet-facing switch for that would be
// a denial-of-wallet primitive. Same reasoning as /metrics living there, and
// the same payoff from the two-listener design in Phase 1.
//
// Remember to turn it back down. A forgotten debug level is a bill.
func LevelHandler(levelVar *slog.LevelVar, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(map[string]string{"level": levelVar.Level().String()})

		case http.MethodPut, http.MethodPost:
			raw := r.URL.Query().Get("level")
			if raw == "" {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{
					"error": "missing ?level= (debug|info|warn|error)",
				})
				return
			}

			var lvl slog.Level
			if err := lvl.UnmarshalText([]byte(raw)); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{
					"error": "unknown level " + raw + " (want debug|info|warn|error)",
				})
				return
			}

			old := levelVar.Level()
			levelVar.Set(lvl)

			// Log the change at WARN so it is visible even at a raised level,
			// and so the audit trail of "who turned on debug at 3am" exists.
			log.Warn("log level changed at runtime",
				"from", old.String(), "to", lvl.String(),
				"remote_addr", r.RemoteAddr)

			json.NewEncoder(w).Encode(map[string]string{"level": lvl.String()})

		default:
			w.Header().Set("Allow", "GET, PUT")
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}
