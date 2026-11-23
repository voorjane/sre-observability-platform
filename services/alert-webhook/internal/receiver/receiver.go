// Package receiver stores local Alertmanager webhook deliveries without external forwarding.
package receiver

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const maxBody = 1 << 20
const retainedEvents = 1000

type Event struct {
	ID         uint64          `json:"id"`
	ReceivedAt time.Time       `json:"received_at"`
	Payload    json.RawMessage `json:"payload"`
}

type Store struct {
	mu     sync.Mutex
	file   *os.File
	events []Event
	lastID uint64
}

func Open(directory string) (*Store, error) {
	if err := os.MkdirAll(directory, 0750); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, "notifications.jsonl"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0640)
	if err != nil {
		return nil, err
	}
	store := &Store{file: file}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 2*maxBody)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || event.ID <= store.lastID {
			file.Close()
			return nil, fmt.Errorf("invalid journal entry after ID %d", store.lastID)
		}
		store.keep(event)
	}
	if err := scanner.Err(); err != nil {
		file.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) keep(event Event) {
	s.lastID = event.ID
	s.events = append(s.events, event)
	if len(s.events) > retainedEvents {
		s.events = append([]Event(nil), s.events[len(s.events)-retainedEvents:]...)
	}
}

func (s *Store) Append(payload []byte) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	event := Event{ID: s.lastID + 1, ReceivedAt: time.Now().UTC(), Payload: append(json.RawMessage(nil), payload...)}
	line, err := json.Marshal(event)
	if err != nil {
		return Event{}, err
	}
	if _, err := s.file.Write(append(line, '\n')); err != nil {
		return Event{}, err
	}
	// A failed sync may still leave a complete entry on disk. Never reuse its ID
	// when Alertmanager retries the unacknowledged delivery.
	s.lastID = event.ID
	if err := s.file.Sync(); err != nil {
		return Event{}, err
	}
	s.keep(event)
	return event, nil
}

func (s *Store) Since(after uint64) ([]Event, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := make([]Event, 0)
	for _, event := range s.events {
		if event.ID > after {
			events = append(events, event)
		}
	}
	return events, s.lastID
}

func (s *Store) Close() error { return s.file.Close() }

func Handler(store *Store, runbooks string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		after := uint64(0)
		if raw := r.URL.Query().Get("after"); raw != "" {
			var err error
			after, err = strconv.ParseUint(raw, 10, 64)
			if err != nil {
				respond(w, http.StatusBadRequest, map[string]string{"error": "invalid after ID"})
				return
			}
		}
		events, lastID := store.Since(after)
		respond(w, http.StatusOK, map[string]any{"events": events, "last_id": lastID})
	})
	mux.HandleFunc("POST /alerts", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			var sizeError *http.MaxBytesError
			code := http.StatusBadRequest
			if errors.As(err, &sizeError) {
				code = http.StatusRequestEntityTooLarge
			}
			respond(w, code, map[string]string{"error": "cannot read notification"})
			return
		}
		var notification struct {
			Status string `json:"status"`
			Alerts []struct {
				Status string            `json:"status"`
				Labels map[string]string `json:"labels"`
			} `json:"alerts"`
		}
		if err := json.Unmarshal(body, &notification); err != nil || !validStatus(notification.Status) || len(notification.Alerts) == 0 {
			respond(w, http.StatusBadRequest, map[string]string{"error": "invalid Alertmanager notification"})
			return
		}
		for _, alert := range notification.Alerts {
			if !validStatus(alert.Status) || alert.Labels["alertname"] == "" {
				respond(w, http.StatusBadRequest, map[string]string{"error": "invalid alert"})
				return
			}
		}
		event, err := store.Append(body)
		if err != nil {
			slog.Error("store notification", "error", err)
			respond(w, http.StatusInternalServerError, map[string]string{"error": "notification not stored"})
			return
		}
		slog.Info("notification received", "id", event.ID, "status", notification.Status, "alerts", len(notification.Alerts))
		respond(w, http.StatusOK, map[string]uint64{"id": event.ID})
	})
	mux.Handle("GET /runbooks/", http.StripPrefix("/runbooks/", http.FileServer(http.Dir(runbooks))))
	return mux
}

func validStatus(status string) bool { return status == "firing" || status == "resolved" }

func respond(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
