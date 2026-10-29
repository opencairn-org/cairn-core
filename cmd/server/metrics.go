package main

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Observability: a Prometheus /metrics endpoint with HTTP request metrics plus
// the default Go runtime + process collectors. Registered on the default
// registry so go_* / process_* come for free.
var (
	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cairn_http_requests_total",
		Help: "Total HTTP requests by method, normalized path, and status class.",
	}, []string{"method", "path", "status"})

	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cairn_http_request_duration_seconds",
		Help:    "HTTP request latency by method and normalized path.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"method", "path"})

	buildInfoGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cairn_build_info",
		Help: "Build info; constant 1 with version/commit labels.",
	}, []string{"version", "commit"})
)

// setBuildInfoMetric stamps the build_info gauge (called once at startup).
func setBuildInfoMetric(version, commit string) {
	buildInfoGauge.WithLabelValues(version, commit).Set(1)
}

// metricsHandler serves the Prometheus exposition (default registry → includes
// go_* and process_* collectors).
func metricsHandler() http.Handler { return promhttp.Handler() }

// metricsMiddleware records request count + latency, labelling by a normalized
// path so per-id routes don't explode label cardinality.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		path := normalizePath(r.URL.Path)
		httpDuration.WithLabelValues(r.Method, path).Observe(time.Since(start).Seconds())
		httpRequests.WithLabelValues(r.Method, path, statusClass(sw.status)).Inc()
	})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

func statusClass(code int) string {
	return strconv.Itoa(code/100) + "xx"
}

var (
	uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	numRe  = regexp.MustCompile(`^[0-9]+$`)
)

// normalizePath collapses dynamic path segments (uuids, numbers) to :id so the
// metric's path label has bounded cardinality.
func normalizePath(p string) string {
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if uuidRe.MatchString(s) || numRe.MatchString(s) {
			segs[i] = ":id"
		}
	}
	// Signed content tokens are unique per URL; collapse them.
	if len(segs) > 2 && segs[1] == "content" {
		segs[2] = ":token"
	}
	out := strings.Join(segs, "/")
	if len(out) > 100 { // defensive cap
		out = out[:100]
	}
	return out
}
