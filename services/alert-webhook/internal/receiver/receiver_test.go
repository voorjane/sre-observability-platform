package receiver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const validNotification = `{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"BudgetBurn","service":"order-api","severity":"critical"}}],"extra":"preserved"}`

func TestDeliverySurvivesRestartAndIDsContinue(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	handler := Handler(store, t.TempDir())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/alerts", strings.NewReader(validNotification)))
	if response.Code != http.StatusOK {
		t.Fatalf("delivery: %d %s", response.Code, response.Body.String())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	events, id := restored.Since(0)
	if id != 1 || len(events) != 1 || !bytes.Contains(events[0].Payload, []byte(`"extra":"preserved"`)) {
		t.Fatalf("journal was not restored: id=%d events=%v", id, events)
	}
	resolved := strings.ReplaceAll(validNotification, "firing", "resolved")
	event, err := restored.Append([]byte(resolved))
	if err != nil || event.ID != 2 {
		t.Fatalf("append after restart: %+v, %v", event, err)
	}
	events, _ = restored.Since(1)
	if len(events) != 1 || events[0].ID != 2 || events[0].ReceivedAt.IsZero() {
		t.Fatalf("cursor/timestamp: %v", events)
	}
}

func TestInvalidNotificationsAreNotAcknowledgedOrStored(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := Handler(store, t.TempDir())
	for _, test := range []struct {
		body string
		code int
	}{
		{`{`, 400}, {`{} {}`, 400}, {`{"status":"firing","alerts":[]}`, 400},
		{`{"status":"firing","alerts":[{"status":"bad","labels":{"alertname":"X"}}]}`, 400},
		{`{"status":"resolved","alerts":[{"status":"resolved","labels":{}}]}`, 400},
		{strings.Repeat("x", maxBody+1), 413},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/alerts", strings.NewReader(test.body)))
		if response.Code != test.code {
			t.Errorf("expected %d got %d", test.code, response.Code)
		}
	}
	if events, id := store.Since(0); len(events) != 0 || id != 0 {
		t.Fatalf("stored rejected delivery: %v", events)
	}
	store.Close()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/alerts", strings.NewReader(validNotification)))
	if response.Code != 500 {
		t.Fatalf("storage failure must permit Alertmanager retry, got %d", response.Code)
	}
}

func TestConcurrentDeliveriesHaveUniquePersistentIDs(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(Handler(store, t.TempDir()))
	defer server.Close()
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			response, err := http.Post(server.URL+"/alerts", "application/json", strings.NewReader(validNotification))
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Body.Close()
			io.Copy(io.Discard, response.Body)
			if response.StatusCode != 200 {
				t.Errorf("concurrent delivery returned %d", response.StatusCode)
			}
		}()
	}
	group.Wait()
	store.Close()
	restored, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	events, id := restored.Since(0)
	if len(events) != 12 || id != 12 {
		t.Fatalf("lost deliveries: %d, %d", len(events), id)
	}
	for i, event := range events {
		if event.ID != uint64(i+1) {
			t.Fatalf("nonmonotonic event: %+v", event)
		}
		var notification map[string]any
		if err := json.Unmarshal(event.Payload, &notification); err != nil {
			t.Fatal(err)
		}
	}
}
