package telemetry

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

func findMetric(t *testing.T, m *Metrics, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			if len(metric.Label) != len(labels) {
				continue
			}
			matched := true
			for _, label := range metric.Label {
				if value, ok := labels[label.GetName()]; !ok || value != label.GetValue() {
					matched = false
				}
			}
			if matched {
				return metric
			}
		}
	}
	t.Fatalf("metric %s with labels %v not found", name, labels)
	return nil
}

func TestResponseStatusAndHistogram(t *testing.T) {
	tests := []struct {
		name   string
		status string
		handle http.HandlerFunc
	}{
		{"implicit body", "200", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }},
		{"empty response", "200", func(w http.ResponseWriter, r *http.Request) {}},
		{"first final status", "201", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			w.WriteHeader(http.StatusInternalServerError)
		}},
		{"body before status", "200", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("ok"))
			w.WriteHeader(http.StatusInternalServerError)
		}},
		{"flush before status", "200", func(w http.ResponseWriter, r *http.Request) {
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Error("Flusher interface was lost")
				return
			}
			flusher.Flush()
			w.WriteHeader(http.StatusInternalServerError)
		}},
		{"client error", "400", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "bad request", http.StatusBadRequest) }},
		{"server error", "500", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "failed", http.StatusInternalServerError) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New("test-service")
			mux := http.NewServeMux()
			mux.HandleFunc("GET /operation", tt.handle)
			m.Middleware(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/operation?request_id=unique", nil))
			labels := map[string]string{"service": "test-service", "method": "GET", "handler": "/operation", "status_code": tt.status}
			if got := findMetric(t, m, "http_requests_total", labels).GetCounter().GetValue(); got != 1 {
				t.Errorf("expected one request, got %v", got)
			}
			histogram := findMetric(t, m, "http_request_duration_seconds", labels).GetHistogram()
			if histogram.GetSampleCount() != 1 || histogram.GetSampleSum() <= 0 {
				t.Errorf("unexpected histogram: %v", histogram)
			}
			foundBoundary := false
			for _, bucket := range histogram.Bucket {
				if bucket.GetUpperBound() == 0.25 {
					foundBoundary = true
				}
			}
			if !foundBoundary {
				t.Error("missing 250 ms histogram boundary")
			}
			if got := findMetric(t, m, "http_in_flight_requests", map[string]string{"service": "test-service"}).GetGauge().GetValue(); got != 0 {
				t.Errorf("in-flight did not return to zero: %v", got)
			}
		})
	}
}

func TestBoundedRouteAndMethodLabels(t *testing.T) {
	m := New("test-service")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "" {
			t.Error("route path values were lost")
		}
	})
	mux.HandleFunc("GET /tree/{id}/", func(http.ResponseWriter, *http.Request) {})
	router := m.Middleware(mux)
	for _, path := range []string{"/items/first?customer_id=one", "/items/second?customer_id=two"} {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	for _, path := range []string{"/missing/first", "/missing/second"} {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	// ServeMux returns a raw redirect path here, including an untrusted space.
	for _, path := range []string{"/tree/first%20secret", "/tree/second%20private"} {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodHead, "/items/third", nil))
	for _, method := range []string{"CUSTOM-ONE", "CUSTOM-TWO"} {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, "/missing/third", nil))
	}
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items/first", nil))
	for _, expected := range []struct {
		method, handler, status string
		count                   float64
	}{
		{"GET", "/items/{id}", "200", 2},
		{"GET", "unmatched", "404", 2},
		{"OTHER", "unmatched", "404", 2},
		{"POST", "unmatched", "405", 1},
		{"GET", "unmatched", "301", 2},
		{"HEAD", "/items/{id}", "200", 1},
	} {
		labels := map[string]string{"service": "test-service", "method": expected.method, "handler": expected.handler, "status_code": expected.status}
		if got := findMetric(t, m, "http_requests_total", labels).GetCounter().GetValue(); got != expected.count {
			t.Errorf("labels %v: expected %v, got %v", labels, expected.count, got)
		}
	}
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "http_requests_total" && len(family.Metric) != 6 {
			t.Errorf("unexpected cardinality: %d time series", len(family.Metric))
		}
	}
}

func TestInFlightWhileRequestIsActive(t *testing.T) {
	m := New("test-service")
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /blocked", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	})
	go func() {
		defer close(done)
		m.Middleware(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/blocked", nil))
	}()
	defer func() { unblock(); <-done }()
	<-entered
	labels := map[string]string{"service": "test-service"}
	if got := findMetric(t, m, "http_in_flight_requests", labels).GetGauge().GetValue(); got != 1 {
		t.Errorf("expected one active request, got %v", got)
	}
	unblock()
	<-done
	if got := findMetric(t, m, "http_in_flight_requests", labels).GetGauge().GetValue(); got != 0 {
		t.Errorf("expected zero active requests, got %v", got)
	}
}

func TestPanicReleasesInFlight(t *testing.T) {
	m := New("test-service")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /panic", func(http.ResponseWriter, *http.Request) { panic("handler failed") })
	func() {
		defer func() {
			if recover() == nil {
				t.Error("middleware swallowed panic")
			}
		}()
		m.Middleware(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", nil))
	}()
	if got := findMetric(t, m, "http_in_flight_requests", map[string]string{"service": "test-service"}).GetGauge().GetValue(); got != 0 {
		t.Errorf("panic leaked an in-flight request: %v", got)
	}
}

func TestMetricsScrapesAreExcluded(t *testing.T) {
	for _, service := range []string{"first", "second"} {
		m := New(service)
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", m.Handler())
		for i := 0; i < 2; i++ {
			rr := httptest.NewRecorder()
			m.Middleware(mux).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics?source=test", nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("scrape failed: %d %s", rr.Code, rr.Body.String())
			}
		}
		families, err := m.registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		foundGo, foundProcess := false, false
		for _, family := range families {
			switch family.GetName() {
			case "http_requests_total", "http_request_duration_seconds":
				t.Errorf("metrics scrape affected application metrics: %s", family.GetName())
			case "go_goroutines":
				foundGo = true
			case "process_cpu_seconds_total":
				foundProcess = true
			}
		}
		if !foundGo || !foundProcess {
			t.Error("missing Go runtime or process metrics")
		}
		if got := findMetric(t, m, "http_in_flight_requests", map[string]string{"service": service}).GetGauge().GetValue(); got != 0 {
			t.Errorf("scrape leaked an in-flight request: %v", got)
		}
	}
}
