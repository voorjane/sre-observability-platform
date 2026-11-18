package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"payment-worker/internal/config"
)

func startTestApp(t *testing.T, cfg *config.Config, customize func(*App)) (string, func() time.Duration) {
	t.Helper()
	cfg.Port = "0"
	application := New(cfg)
	if customize != nil {
		customize(application)
	}
	address := make(chan string, 1)
	application.httpServer.BaseContext = func(listener net.Listener) context.Context {
		address <- listener.Addr().String()
		return context.Background()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = application.Run(ctx)
	}()
	stop := func() time.Duration {
		start := time.Now()
		cancel()
		select {
		case <-done:
			if runErr != nil {
				t.Errorf("application shutdown: %v", runErr)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("application exceeded shutdown test deadline")
		}
		return time.Since(start)
	}
	t.Cleanup(func() { stop() })
	select {
	case bound := <-address:
		_, port, err := net.SplitHostPort(bound)
		if err != nil {
			t.Fatal(err)
		}
		return "http://127.0.0.1:" + port, stop
	case <-done:
		t.Fatalf("application did not start: %v", runErr)
	case <-time.After(2 * time.Second):
		t.Fatal("application startup timed out")
	}
	return "", stop
}

func TestRunFlushesStartupSpan(t *testing.T) {
	exported := make(chan struct{}, 4)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path != "/v1/traces" {
			t.Errorf("unexpected OTLP path: %s", r.URL.Path)
		}
		exported <- struct{}{}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer collector.Close()
	cfg := config.Load()
	cfg.ShutdownTimeout = 500 * time.Millisecond
	cfg.Tracing = config.TracingConfig{Endpoint: collector.URL + "/v1/traces", ExportTimeout: 200 * time.Millisecond, SampleRatio: 1}
	url, stop := startTestApp(t, cfg, nil)
	response, err := http.Get(url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("unexpected health status: %d", response.StatusCode)
	}
	if elapsed := stop(); elapsed > cfg.ShutdownTimeout+100*time.Millisecond {
		t.Errorf("shutdown exceeded its budget: %s", elapsed)
	}
	select {
	case <-exported:
	default:
		t.Fatal("startup span was not flushed on shutdown")
	}
}

func TestRunSharesShutdownBudgetWithTracing(t *testing.T) {
	exportStarted, releaseExport := make(chan struct{}), make(chan struct{})
	var exportOnce sync.Once
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		exportOnce.Do(func() { close(exportStarted) })
		<-releaseExport
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer collector.Close()
	defer close(releaseExport)
	cfg := config.Load()
	cfg.ShutdownTimeout = 400 * time.Millisecond
	cfg.Tracing = config.TracingConfig{Endpoint: collector.URL + "/v1/traces", ExportTimeout: 3 * time.Second, SampleRatio: 1}
	requestStarted, releaseRequest := make(chan struct{}), make(chan struct{})
	var requestOnce sync.Once
	release := func() { requestOnce.Do(func() { close(releaseRequest) }) }
	defer release()
	url, stop := startTestApp(t, cfg, func(application *App) {
		next := application.httpServer.Handler
		application.httpServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/slow" {
				next.ServeHTTP(w, r)
				return
			}
			close(requestStarted)
			<-releaseRequest
			w.WriteHeader(http.StatusOK)
		})
	})
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, err := http.Get(url + "/slow")
		if err != nil {
			t.Error(err)
			return
		}
		response.Body.Close()
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("slow request did not start")
	}
	time.AfterFunc(250*time.Millisecond, release)
	elapsed := stop()
	if elapsed > cfg.ShutdownTimeout+150*time.Millisecond {
		t.Errorf("HTTP drain and tracing used separate shutdown budgets: %s", elapsed)
	}
	<-requestDone
	select {
	case <-exportStarted:
	default:
		t.Fatal("tracing flush was not attempted after draining HTTP")
	}
}
