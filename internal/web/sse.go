package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/geral/local-assistant-laya/internal/store"
)

// Event is one server-sent message.
type Event struct {
	Name string
	Data any
}

// FiredPayload is what the browser receives when a task fires.
type FiredPayload struct {
	TaskID int64  `json:"task_id"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Body   string `json:"body"`
	Title  string `json:"title"`
}

// RefreshPayload tells the browser its task list is stale.
type RefreshPayload struct {
	Reason string `json:"reason"`
}

// Hub fans server-sent events out to every connected client.
//
// A phone that is asleep is not here. It is reached by Web Push in M4. This
// hub is the live channel: it drives the on-screen countdown refresh and the
// in-page alert.
type Hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
	log     *slog.Logger
	closed  bool
}

// NewHub builds an event hub.
func NewHub(log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{clients: make(map[chan []byte]struct{}), log: log}
}

// Subscribe registers a client and returns its channel plus an unsubscribe
// function. The buffer is deliberately shallow: a stalled client must never
// grow the server's memory without bound.
func (h *Hub) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 8)

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	h.clients[ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if _, ok := h.clients[ch]; ok {
			delete(h.clients, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// Broadcast sends an event to every client. Slow clients are skipped rather
// than allowed to block the scheduler.
func (h *Hub) Broadcast(ev Event) {
	payload, err := json.Marshal(ev.Data)
	if err != nil {
		h.log.Error("sse: marshal failed", "event", ev.Name, "error", err)
		return
	}
	frame := fmt.Appendf(nil, "event: %s\ndata: %s\n\n", ev.Name, payload)

	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- frame:
		default:
			h.log.Warn("sse: dropping event for a slow client", "event", ev.Name)
		}
	}
}

// Close disconnects every client.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for ch := range h.clients {
		delete(h.clients, ch)
		close(ch)
	}
}

// Clients reports the current connection count, for the health endpoint.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// sseNotifier bridges scheduler events onto the hub. It satisfies
// notify.Notifier so it can join the same chain as the desktop alerts.
type sseNotifier struct {
	hub *Hub
}

// BroadcastNotifier wraps a hub as a notifier.
func BroadcastNotifier(hub *Hub) *sseNotifier { return &sseNotifier{hub: hub} }

// Fire announces a single fired task and asks clients to refresh.
func (s *sseNotifier) Fire(_ context.Context, t *store.Task) error {
	title, body := describeTask(t)
	s.hub.Broadcast(Event{Name: "fired", Data: FiredPayload{
		TaskID: t.ID,
		Kind:   t.Kind,
		Label:  t.Label,
		Title:  title,
		Body:   body,
	}})
	s.hub.Broadcast(Event{Name: "refresh", Data: RefreshPayload{Reason: "fired"}})
	return nil
}

// CatchUp announces one aggregate alert and asks clients to refresh.
func (s *sseNotifier) CatchUp(_ context.Context, tasks []*store.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	title, body := describeBatch(tasks)
	s.hub.Broadcast(Event{Name: "fired", Data: FiredPayload{
		Kind:  "catchup",
		Title: title,
		Body:  body,
	}})
	s.hub.Broadcast(Event{Name: "refresh", Data: RefreshPayload{Reason: "catchup"}})
	return nil
}

func describeTask(t *store.Task) (title, body string) {
	label := t.Label
	if label == "" {
		label = t.RawInput
	}
	if t.Kind == store.KindTimer {
		return "Timer finished", label
	}
	if label == "" {
		return "Reminder", ""
	}
	return label, "Reminder"
}

func describeBatch(tasks []*store.Task) (title, body string) {
	const maxShown = 5
	if len(tasks) == 1 {
		t, b := describeTask(tasks[0])
		return "Missed while you were away", t + " - " + b
	}
	var out string
	for i, t := range tasks {
		if i == maxShown {
			out += fmt.Sprintf("\n...and %d more.", len(tasks)-maxShown)
			break
		}
		_, line := describeTask(t)
		if i > 0 {
			out += "\n"
		}
		out += "- " + line
	}
	return fmt.Sprintf("%d tasks came due while the app was not running", len(tasks)), out
}

// handleEvents streams the hub to one browser.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Proxies that buffer would defeat the whole point of this endpoint.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch, unsubscribe := s.hub.Subscribe()
	defer unsubscribe()

	// An immediate comment frame tells the browser the stream is live, so
	// htmx/Alpine can stop showing a connecting state.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	// A heartbeat keeps intermediaries from reaping an idle connection.
	beat := time.NewTicker(25 * time.Second)
	defer beat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case frame, open := <-ch:
			if !open {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		case <-beat.C:
			if _, err := fmt.Fprint(w, ": beat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
