// Command alertsink receives Alertmanager webhooks and prints them.
//
// In production the receiver is Slack, PagerDuty or Opsgenie. This stands in
// for them so you can watch grouping, routing, inhibition and resolution
// happen without needing any external credentials — the Alertmanager logic
// exercised is identical.
//
// It also keeps the last N notifications in memory and serves them at / so you
// can review what arrived while you were watching something else.
//
//	go run ./cmd/alertsink -addr :9101
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// payload is Alertmanager's webhook format (version 4).
type payload struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	Status            string            `json:"status"` // firing | resolved
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []alert           `json:"alerts"`
}

type alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

const (
	reset  = "\033[0m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	dim    = "\033[2m"
	bold   = "\033[1m"
)

type sink struct {
	mu      sync.Mutex
	history []record
	max     int
}

type record struct {
	At       time.Time
	Route    string
	Status   string
	Receiver string
	GroupKey string
	Count    int
	Names    []string
}

func main() {
	addr := flag.String("addr", ":9101", "listen address")
	flag.Parse()

	s := &sink{max: 200}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /alerts", s.handle)
	mux.HandleFunc("GET /", s.list)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		fmt.Printf("\n%sshutting down%s\n", dim, reset)
		_ = srv.Close()
	}()

	fmt.Printf("%salertsink listening on %s%s\n", bold, *addr, reset)
	fmt.Printf("%swaiting for Alertmanager webhooks... (GET / for history)%s\n\n", dim, reset)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func (s *sink) handle(w http.ResponseWriter, r *http.Request) {
	var p payload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)

	route := r.URL.Query().Get("route")
	if route == "" {
		route = "default"
	}

	colour, marker := green, "RESOLVED"
	if p.Status == "firing" {
		colour, marker = red, "FIRING"
		if route == "ticket" {
			colour = yellow
		}
	}

	fmt.Printf("%s%s[%s]%s %sreceiver=%s route=%s  %d alert(s)%s\n",
		bold, colour, marker, reset, dim, p.Receiver, route, len(p.Alerts), reset)

	if len(p.GroupLabels) > 0 {
		fmt.Printf("  %sgrouped by:%s %s\n", dim, reset, fmtLabels(p.GroupLabels))
	}

	names := make([]string, 0, len(p.Alerts))
	for _, a := range p.Alerts {
		name := a.Labels["alertname"]
		names = append(names, name)

		fmt.Printf("  %s%-34s%s %s\n", bold, name, reset, a.Annotations["summary"])

		if d := a.Annotations["description"]; d != "" {
			fmt.Printf("    %s%s%s\n", dim, d, reset)
		}
		if rb := a.Annotations["runbook_url"]; rb != "" {
			fmt.Printf("    %srunbook: %s%s\n", dim, rb, reset)
		}

		extra := map[string]string{}
		for k, v := range a.Labels {
			if k != "alertname" && k != "job" && k != "service" {
				extra[k] = v
			}
		}
		if len(extra) > 0 {
			fmt.Printf("    %slabels: %s%s\n", dim, fmtLabels(extra), reset)
		}

		if a.Status == "firing" {
			fmt.Printf("    %sfiring for %s%s\n", dim, time.Since(a.StartsAt).Round(time.Second), reset)
		}
	}
	fmt.Println()

	s.mu.Lock()
	s.history = append(s.history, record{
		At: time.Now(), Route: route, Status: p.Status,
		Receiver: p.Receiver, GroupKey: p.GroupKey,
		Count: len(p.Alerts), Names: names,
	})
	if len(s.history) > s.max {
		s.history = s.history[len(s.history)-s.max:]
	}
	s.mu.Unlock()
}

func (s *sink) list(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if len(s.history) == 0 {
		fmt.Fprintln(w, "no notifications received yet")
		return
	}
	fmt.Fprintf(w, "%-21s %-9s %-9s %-8s %s\n", "TIME", "STATUS", "ROUTE", "ALERTS", "NAMES")
	for i := len(s.history) - 1; i >= 0; i-- {
		r := s.history[i]
		fmt.Fprintf(w, "%-21s %-9s %-9s %-8d %s\n",
			r.At.Format("2006-01-02 15:04:05"), r.Status, r.Route, r.Count,
			strings.Join(dedupe(r.Names), ", "))
	}
}

func fmtLabels(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
