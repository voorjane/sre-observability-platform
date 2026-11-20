package telemetry

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns a registry for one application instance, without global collectors.
type Metrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
}

func New(service string) *Metrics {
	labels := prometheus.Labels{"service": service}
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total", Help: "Completed HTTP requests.", ConstLabels: labels,
		}, []string{"method", "handler", "status_code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_request_duration_seconds", Help: "HTTP request duration in seconds.", ConstLabels: labels,
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"method", "handler", "status_code"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "http_in_flight_requests", Help: "Currently active HTTP requests, excluding metrics scrapes.", ConstLabels: labels,
		}),
	}
	m.registry.MustRegister(m.requests, m.duration, m.inFlight)
	registerer := prometheus.WrapRegistererWith(labels, m.registry)
	registerer.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Middleware surrounds routing so it includes validation, chaos, and dependency calls.
func (m *Metrics) Middleware(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			mux.ServeHTTP(w, r)
			return
		}
		// Go 1.22 does not expose Request.Pattern. Only method-qualified registered
		// patterns are used as labels; redirects with raw paths stay unmatched.
		_, pattern := mux.Handler(r)
		route := "unmatched"
		if registeredMethod, path, ok := strings.Cut(pattern, " "); ok &&
			(registeredMethod == r.Method || registeredMethod == http.MethodGet && r.Method == http.MethodHead) {
			route = path
		}
		method := metricMethod(r.Method)
		m.inFlight.Inc()
		defer m.inFlight.Dec()

		start := time.Now()
		writer, status := observeResponse(w)
		mux.ServeHTTP(writer, r)
		code := strconv.Itoa(status())
		m.requests.WithLabelValues(method, route, code).Inc()
		m.duration.WithLabelValues(method, route, code).Observe(time.Since(start).Seconds())
	})
}

func metricMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}
