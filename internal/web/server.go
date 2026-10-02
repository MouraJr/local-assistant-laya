// Package web serves the HTMX interface and the server-sent event stream.
//
// Every mutating route returns an HTML fragment for htmx to swap. There is no
// JSON API in v1; the browser is the only client.
package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/geral/local-assistant-laya/internal/parse"
	"github.com/geral/local-assistant-laya/internal/store"
	"github.com/geral/local-assistant-laya/internal/web/templates"
)

// Options configures the server.
type Options struct {
	// Store is the task repository.
	Store *store.Store
	// Hub broadcasts scheduler events to connected browsers.
	Hub *Hub
	// ConfirmThreshold is the confidence at or above which a parse may be
	// scheduled without asking the user to confirm it.
	ConfirmThreshold float64
	// Now is injectable for tests.
	Now func() time.Time
	// Logger receives request and parse events.
	Logger *slog.Logger
	// Tier identifies the parser in use, shown in the UI.
	Tier string
}

// Server owns the HTTP routes.
type Server struct {
	store     *store.Store
	hub       *Hub
	threshold float64
	now       func() time.Time
	log       *slog.Logger
	tier      string
	queued    enqueuer
}

// New builds the server.
func New(opts Options) *Server {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	tier := opts.Tier
	if tier == "" {
		tier = parse.TierRules
	}
	return &Server{
		store:     opts.Store,
		hub:       opts.Hub,
		threshold: opts.ConfirmThreshold,
		now:       nowFn,
		log:       log,
		tier:      tier,
	}
}

// Routes returns the configured mux.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /timeline", s.handleTimeline)
	mux.HandleFunc("POST /parse", s.handleParse)
	mux.HandleFunc("POST /tasks/{id}", s.handleEdit)
	mux.HandleFunc("POST /tasks/{id}/confirm", s.handleConfirm)
	mux.HandleFunc("POST /tasks/{id}/done", s.handleDone)
	mux.HandleFunc("POST /tasks/{id}/cancel", s.handleCancel)
	mux.HandleFunc("POST /tasks/{id}/snooze", s.handleSnooze)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("GET /static/", s.staticHandler())

	return s.withLogging(mux)
}

// withLogging records every request at debug level.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Debug("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"took", time.Since(start).Round(time.Microsecond).String(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer so SSE keeps working behind the
// logging middleware.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) staticHandler() http.Handler {
	// Static assets are embedded so the binary is genuinely self-contained and
	// works with no files beside it.
	return http.FileServer(http.FS(staticFS))
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	pending, missed, err := s.loadTasks(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not load tasks")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.Layout(pending, missed, s.health(r.Context())).Render(r.Context(), w); err != nil {
		s.log.Error("render index failed", "error", err)
	}
}

// handleTimeline re-renders the task list. The browser calls it when the
// event stream reports that something fired.
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	s.renderTimeline(w, r)
}

// renderTimeline returns the whole sorted list, so a new task lands in its
// correct position instead of replacing everything else on screen.
func (s *Server) renderTimeline(w http.ResponseWriter, r *http.Request) {
	pending, missed, err := s.loadTasks(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not load tasks")
		return
	}
	s.render(w, r, http.StatusOK, templates.Timeline(pending, missed))
}

// handleParse is the whole product: one line of plain language in, one
// scheduled task out.
func (s *Server) handleParse(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	raw := strings.TrimSpace(r.FormValue("q"))
	if raw == "" {
		s.notice(w, "Type something, for example: count 20 min")
		return
	}

	res := parse.Parse(raw, s.now())
	if !res.OK {
		// Tier 0 found nothing. In M3 this is where the cascade escalates to
		// Jev and Laya; for now the honest answer is to say so.
		s.logParse(raw, 0, res.Rationale, false)
		s.notice(w, "No time found in "+strconv.Quote(raw)+
			". Try: count 20 min, remind me at 3 PM to email, tomorrow 9am.")
		return
	}

	task := &store.Task{
		Kind:       string(res.Kind),
		Label:      res.Label,
		RawInput:   raw,
		DueAt:      res.DueAt,
		HasDue:     true,
		SourceTier: res.Tier,
		Confidence: res.Confidence,
		Rationale:  res.Rationale,
		RolledOver: res.RolledOver,
	}
	if res.Kind == parse.KindTimer {
		task.DurationMS = res.Duration.Milliseconds()
	}

	// The gate. Anything the parser is not sure about is persisted but held
	// for confirmation, so a reload cannot silently lose the obligation.
	task.NeedsConfirm = res.Confidence < s.threshold

	if err := s.store.CreateTask(ctx, task); err != nil {
		s.fail(w, http.StatusInternalServerError, "could not save the task")
		return
	}
	s.logParse(raw, res.Confidence, res.Rationale, true)

	s.log.Info("parsed",
		"input", raw,
		"tier", res.Tier,
		"confidence", res.Confidence,
		"needs_confirm", task.NeedsConfirm,
		"due", task.DueAt.Format(time.RFC3339),
	)

	s.nudge()
	s.renderTimeline(w, r)
}

func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := s.taskID(w, r)
	if !ok {
		return
	}
	no := false
	if err := s.store.Update(ctx, id, store.UpdateFields{NeedsConfirm: &no}); err != nil {
		s.fail(w, http.StatusInternalServerError, "could not confirm the task")
		return
	}
	task, err := s.store.Get(ctx, id)
	if err != nil {
		s.fail(w, http.StatusNotFound, "task not found")
		return
	}
	s.nudge()
	s.render(w, r, http.StatusOK, templates.TaskRow(*task))
}

func (s *Server) handleDone(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taskID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	task, err := s.store.Get(ctx, id)
	if err != nil {
		s.fail(w, http.StatusNotFound, "task not found")
		return
	}
	if err := s.store.SetStatus(ctx, id, store.StatusDone); err != nil {
		s.fail(w, http.StatusInternalServerError, "could not update the task")
		return
	}
	s.nudge()
	label := task.Label
	if label == "" {
		label = task.RawInput
	}
	s.render(w, r, http.StatusOK, templates.DoneFragment(label))
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taskID(w, r)
	if !ok {
		return
	}
	if err := s.store.Delete(r.Context(), id); err != nil {
		s.fail(w, http.StatusInternalServerError, "could not delete the task")
		return
	}
	s.nudge()
	s.render(w, r, http.StatusOK, templates.DoneFragment("discarded"))
}

// handleSnooze pushes a pending task back by a fixed number of minutes,
// without asking for new natural language. It applies to both kinds: a 3 PM
// reminder that turns out to be inconvenient is as common as a timer that ran
// short.
func (s *Server) handleSnooze(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := s.taskID(w, r)
	if !ok {
		return
	}
	minutes, err := strconv.Atoi(r.URL.Query().Get("minutes"))
	if err != nil || minutes <= 0 || minutes > 24*60 {
		s.fail(w, http.StatusBadRequest, "snooze needs a minutes value between 1 and 1440")
		return
	}
	task, ok := s.editableTask(w, r, id)
	if !ok {
		return
	}

	// Push from the later of now and the current deadline, so a task that is
	// overdue but not yet swept moves relative to now rather than staying in
	// the past.
	base := task.DueAt
	if now := s.now(); base.Before(now) {
		base = now
	}
	newDue := base.Add(time.Duration(minutes) * time.Minute)
	if err := s.store.Update(ctx, id, store.UpdateFields{DueAt: &newDue}); err != nil {
		s.fail(w, http.StatusInternalServerError, "could not snooze the task")
		return
	}
	s.nudge()
	s.renderRow(w, r, id)
}

// handleEdit applies a user correction to the label and/or due time. It is
// the ground-truth signal for calibration, so it is flagged as a correction.
func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taskID(w, r)
	if !ok {
		return
	}
	if _, ok := s.editableTask(w, r, id); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, http.StatusBadRequest, "could not read the form")
		return
	}

	f := store.UpdateFields{Corrected: true}
	if _, set := r.PostForm["label"]; set {
		label := strings.TrimSpace(r.PostForm.Get("label"))
		if len(label) > maxLabelLen {
			s.fail(w, http.StatusBadRequest, fmt.Sprintf("label is too long (%d characters max)", maxLabelLen))
			return
		}
		f.Label = &label
	}
	if v := strings.TrimSpace(r.PostForm.Get("due")); v != "" {
		// datetime-local carries no zone. It is the user's wall clock, read in
		// the same zone the UI renders in.
		due, err := time.ParseInLocation("2006-01-02T15:04", v, s.now().Location())
		if err != nil {
			s.fail(w, http.StatusBadRequest, "time must look like 2026-10-02T15:00")
			return
		}
		if !due.After(s.now()) {
			s.fail(w, http.StatusBadRequest, "that time has already passed; pick a time in the future")
			return
		}
		f.DueAt = &due
	}
	if f.Label == nil && f.DueAt == nil {
		s.fail(w, http.StatusBadRequest, "nothing to change")
		return
	}
	// The user stated the details themselves, so there is nothing left for
	// the parser to be unsure about.
	no := false
	f.NeedsConfirm = &no

	if err := s.store.Update(r.Context(), id, f); err != nil {
		s.fail(w, http.StatusInternalServerError, "could not save the change")
		return
	}
	s.nudge()
	s.renderRow(w, r, id)
}

// maxLabelLen bounds user-supplied labels on edit.
const maxLabelLen = 500

// editableTask loads a task and rejects changes to anything no longer pending
// (US-004 AC3). The rejection is rendered for the user; ok is false when the
// caller should stop.
func (s *Server) editableTask(w http.ResponseWriter, r *http.Request, id int64) (*store.Task, bool) {
	task, err := s.store.Get(r.Context(), id)
	if err != nil {
		s.fail(w, http.StatusNotFound, "task not found")
		return nil, false
	}
	if task.Status != store.StatusPending {
		s.fail(w, http.StatusConflict, "this task is already "+task.Status+" and can no longer be changed")
		return nil, false
	}
	return task, true
}

func (s *Server) renderRow(w http.ResponseWriter, r *http.Request, id int64) {
	task, err := s.store.Get(r.Context(), id)
	if err != nil {
		s.fail(w, http.StatusNotFound, "task not found")
		return
	}
	s.render(w, r, http.StatusOK, templates.TaskRow(*task))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h := s.health(r.Context())
	status := http.StatusOK
	if !h.StoreUp || !h.SchedulerUp {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"ok":%t,"store":%t,"scheduler":%t,"sse_clients":%d,"tier":%q,"confirm_threshold":%v}`+"\n",
		h.StoreUp && h.SchedulerUp, h.StoreUp, h.SchedulerUp, h.SSEClients, h.Tier, h.Threshold)
}

func (s *Server) health(ctx context.Context) templates.Health {
	h := templates.Health{Tier: s.tier, Threshold: s.threshold, SSEClients: s.hub.Clients()}
	h.StoreUp = s.store.Healthy(ctx) == nil
	h.SchedulerUp = true
	return h
}

func (s *Server) loadTasks(ctx context.Context) ([]store.Task, []store.Task, error) {
	pending, err := s.store.ListPending(ctx)
	if err != nil {
		return nil, nil, err
	}
	missed, err := s.store.ListMissed(ctx, 20)
	if err != nil {
		return nil, nil, err
	}
	return deref(pending), deref(missed), nil
}

func deref(in []*store.Task) []store.Task {
	out := make([]store.Task, 0, len(in))
	for _, t := range in {
		out = append(out, *t)
	}
	return out
}

// nudge tells the scheduler a task changed, so the wake-up heap is rebuilt
// without waiting for the next tick.
type enqueuer interface{ Enqueue() }

// SetScheduler attaches the scheduler used for Enqueue nudges. Kept separate
// from New to avoid an import cycle between web and scheduler.
func (s *Server) SetScheduler(e enqueuer) { s.queued = e }

func (s *Server) nudge() {
	if s.queued != nil {
		s.queued.Enqueue()
	}
}

func (s *Server) taskID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.fail(w, http.StatusBadRequest, "invalid task id")
		return 0, false
	}
	return id, true
}

func (s *Server) logParse(raw string, confidence float64, rationale string, accepted bool) {
	if _, err := s.store.LogParse(context.Background(), raw, s.tier, confidence, rationale, accepted); err != nil {
		// Calibration bookkeeping must never break a request.
		s.log.Warn("parse log write failed", "error", err)
	}
}

func (s *Server) render(w http.ResponseWriter, _ *http.Request, status int, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := c.Render(context.Background(), w); err != nil {
		s.log.Error("fragment render failed", "error", err)
	}
}

// fail reports an error. The message is retargeted to the notice slot so a
// rejected action never replaces the row or the list it was aimed at.
func (s *Server) fail(w http.ResponseWriter, status int, msg string) {
	if status >= http.StatusInternalServerError {
		s.log.Error("request failed", "status", status, "message", msg)
	} else {
		s.log.Info("request rejected", "status", status, "message", msg)
	}
	retarget(w)
	s.render(w, nil, status, templates.ErrorFragment(msg))
}

// notice answers 200 with guidance for input that was not understood as a
// task. Like fail, it never touches the timeline.
func (s *Server) notice(w http.ResponseWriter, msg string) {
	retarget(w)
	s.render(w, nil, http.StatusOK, templates.ErrorFragment(msg))
}

func retarget(w http.ResponseWriter) {
	w.Header().Set("HX-Retarget", "#notice")
	w.Header().Set("HX-Reswap", "innerHTML")
}
