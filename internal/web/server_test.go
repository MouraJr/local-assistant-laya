package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/geral/local-assistant-laya/internal/notify"
	"github.com/geral/local-assistant-laya/internal/parse"
	"github.com/geral/local-assistant-laya/internal/store"
)

var now = time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(Options{
		Store:            st,
		Hub:              NewHub(log),
		ConfirmThreshold: 0.85,
		Now:              func() time.Time { return now },
		Logger:           log,
		Tier:             parse.TierRules,
	})
	return srv, st
}

func post(t *testing.T, h http.Handler, path, form string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The headline feature: one line of plain language becomes a scheduled task.
func TestParseCreatesATimer(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Routes()

	rec := post(t, h, "/parse", "q=count+20+min")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Timer") {
		t.Errorf("response missing the timer badge:\n%s", body)
	}

	pending, err := st.ListPending(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("stored %d tasks, want 1", len(pending))
	}
	got := pending[0]
	if got.Kind != store.KindTimer {
		t.Errorf("Kind = %q, want timer", got.Kind)
	}
	if got.DurationMS != (20 * time.Minute).Milliseconds() {
		t.Errorf("DurationMS = %d, want %d", got.DurationMS, (20 * time.Minute).Milliseconds())
	}
	if want := now.Add(20 * time.Minute); !got.DueAt.Equal(want) {
		t.Errorf("DueAt = %v, want %v", got.DueAt, want)
	}
	if got.NeedsConfirm {
		t.Error("NeedsConfirm = true for an explicit, unambiguous timer")
	}
}

func TestParseCreatesAReminder(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Routes()

	post(t, h, "/parse", "q=remind+me+at+3+PM+to+email+the+invoice")

	pending, _ := st.ListPending(context.Background())
	if len(pending) != 1 {
		t.Fatalf("stored %d tasks, want 1", len(pending))
	}
	got := pending[0]
	if got.Kind != store.KindReminder {
		t.Errorf("Kind = %q, want reminder", got.Kind)
	}
	if want := time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC); !got.DueAt.Equal(want) {
		t.Errorf("DueAt = %v, want %v", got.DueAt, want)
	}
	if got.Label != "email the invoice" {
		t.Errorf("Label = %q, want %q", got.Label, "email the invoice")
	}
}

// The safety property from US-003: a low-confidence parse is persisted but
// held for confirmation, and the UI must say why.
func TestLowConfidenceParseRequiresConfirmation(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Routes()

	rec := post(t, h, "/parse", "q=in+2+hours")
	body := rec.Body.String()

	if !strings.Contains(body, "Not sure") {
		t.Errorf("response missing the confirmation prompt:\n%s", body)
	}
	if !strings.Contains(body, "Yes, that") {
		t.Error("response missing the confirm button")
	}

	pending, _ := st.ListPending(context.Background())
	if len(pending) != 1 {
		t.Fatalf("stored %d tasks, want 1", len(pending))
	}
	if !pending[0].NeedsConfirm {
		t.Error("NeedsConfirm = false: an ambiguous parse must not schedule unattended")
	}
	if pending[0].Rationale == "" {
		t.Error("Rationale is empty; the user must be told why it is unsure")
	}
}

func TestConfirmClearsTheGate(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Routes()

	post(t, h, "/parse", "q=in+2+hours")
	pending, _ := st.ListPending(context.Background())
	id := pending[0].ID

	rec := post(t, h, "/tasks/"+itoa(id)+"/confirm", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Not sure") {
		t.Error("still showing the confirmation prompt after confirming")
	}

	after, _ := st.Get(context.Background(), id)
	if after.NeedsConfirm {
		t.Error("NeedsConfirm still true after confirm")
	}
}

// A time that already passed must be surfaced, not silently moved.
func TestRolloverIsSurfacedInTheResponse(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Routes()

	rec := post(t, h, "/parse", "q=remind+me+at+9+AM+to+stretch")
	if !strings.Contains(rec.Body.String(), "already passed today") {
		t.Errorf("rollover not surfaced in the response:\n%s", rec.Body.String())
	}

	pending, _ := st.ListPending(context.Background())
	if !pending[0].RolledOver {
		t.Error("RolledOver = false, want true")
	}
	if want := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC); !pending[0].DueAt.Equal(want) {
		t.Errorf("DueAt = %v, want %v", pending[0].DueAt, want)
	}
}

func TestUnparseableInputIsHonest(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Routes()

	rec := post(t, h, "/parse", "q=hello+world")
	if !strings.Contains(rec.Body.String(), "No time found") {
		t.Errorf("expected a clear failure, got:\n%s", rec.Body.String())
	}
	pending, _ := st.ListPending(context.Background())
	if len(pending) != 0 {
		t.Errorf("stored %d tasks for unparseable input, want 0", len(pending))
	}
}

func TestEmptyInputIsRejected(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := post(t, srv.Routes(), "/parse", "q=")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with an inline message", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Type something") {
		t.Error("missing the empty-input message")
	}
}

func TestDoneAndCancel(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Routes()

	post(t, h, "/parse", "q=count+5+min")
	pending, _ := st.ListPending(context.Background())
	id := pending[0].ID

	post(t, h, "/tasks/"+itoa(id)+"/done", "")
	after, _ := st.Get(context.Background(), id)
	if after.Status != store.StatusDone {
		t.Errorf("Status = %q, want done", after.Status)
	}

	post(t, h, "/parse", "q=count+9+min")
	pending, _ = st.ListPending(context.Background())
	id = pending[0].ID
	post(t, h, "/tasks/"+itoa(id)+"/cancel", "")

	if _, err := st.Get(context.Background(), id); err == nil {
		t.Error("task still present after cancel")
	}
}

// Snooze applies to both kinds (US-004) and pushes the existing deadline
// back, matching the "+15m" button's promise.
func TestSnoozePushesBackBothKinds(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Routes()

	post(t, h, "/parse", "q=count+5+min")
	post(t, h, "/parse", "q=tomorrow+9am")
	pending, _ := st.ListPending(context.Background())
	if len(pending) != 2 {
		t.Fatalf("stored %d tasks, want 2", len(pending))
	}

	for _, task := range pending {
		rec := post(t, h, "/tasks/"+itoa(task.ID)+"/snooze?minutes=15", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("snooze %s: status = %d, want 200", task.Kind, rec.Code)
		}
		after, _ := st.Get(context.Background(), task.ID)
		if want := task.DueAt.Add(15 * time.Minute); !after.DueAt.Equal(want) {
			t.Errorf("%s DueAt = %v, want %v", task.Kind, after.DueAt, want)
		}
	}
}

// An overdue task that the scheduler has not swept yet must snooze from now,
// not stay in the past.
func TestSnoozeOverdueMovesFromNow(t *testing.T) {
	srv, st := newTestServer(t)
	task := &store.Task{
		Kind: store.KindReminder, RawInput: "late", DueAt: now.Add(-10 * time.Minute),
		HasDue: true, SourceTier: "T0", Confidence: 0.95,
	}
	if err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatalf("create: %v", err)
	}
	post(t, srv.Routes(), "/tasks/"+itoa(task.ID)+"/snooze?minutes=5", "")
	after, _ := st.Get(context.Background(), task.ID)
	if want := now.Add(5 * time.Minute); !after.DueAt.Equal(want) {
		t.Errorf("DueAt = %v, want %v", after.DueAt, want)
	}
}

func TestSnoozeRejectsNonsense(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Routes()

	post(t, h, "/parse", "q=count+5+min")
	pending, _ := st.ListPending(context.Background())
	id := itoa(pending[0].ID)

	for _, q := range []string{"", "?minutes=0", "?minutes=abc", "?minutes=99999"} {
		rec := post(t, h, "/tasks/"+id+"/snooze"+q, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("snooze%s returned %d, want 400", q, rec.Code)
		}
	}
}

func TestIndexRendersPage(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Local Assistant", "/parse", "count 20 min", "htmx.min.js", "alpine.min.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func TestHealthEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("health body = %s", rec.Body.String())
	}
}

// HTML escaping matters here: a task label is attacker-influenced input that
// ends up in a page.
func TestLabelIsEscaped(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := post(t, srv.Routes(), "/parse", "q="+url.QueryEscape("remind me at 3 PM to <script>alert(1)</script>"))
	if strings.Contains(rec.Body.String(), "<script>alert(1)</script>") {
		t.Error("label was not HTML-escaped")
	}
	if !strings.Contains(rec.Body.String(), "&lt;script&gt;") {
		t.Errorf("expected escaped label in:\n%s", rec.Body.String())
	}
}

func TestStaticAssetsAreServed(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Routes()

	for _, path := range []string{
		"/static/tailwind.css",
		"/static/vendor/htmx.min.js",
		"/static/vendor/alpine.min.js",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

// A task that needs confirmation must not be reachable by the scheduler as a
// deadline, which is what keeps an unconfirmed parse from ever firing.
func TestNeedsConfirmSatisfiesNotifierInterface(t *testing.T) {
	// Guards the wiring: the scheduler's Notifier is notify.Notifier, and the
	// web hub notifier must satisfy it too, so the chain compiles and the
	// events reach browsers.
	var _ notify.Notifier = BroadcastNotifier(NewHub(slog.Default()))
	var _ notify.Notifier = notify.NewHost(notify.Options{})
}

// Regression: due times are stored as UTC instants but must render in the
// user's own zone. Before this was fixed, "remind me at 3 PM" came back
// showing "20:00 UTC".
func TestDueTimeRendersInLocalZone(t *testing.T) {
	if time.Local == time.UTC {
		t.Skip("host is on UTC, so local and UTC rendering are identical")
	}
	srv, _ := newTestServer(t)
	rec := post(t, srv.Routes(), "/parse", "q=remind+me+at+3+PM+to+email")
	body := rec.Body.String()

	due := time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC)
	if strings.Contains(body, due.Format("Mon 2 Jan 15:04:05 MST")) {
		t.Errorf("rendered the raw UTC stamp instead of the local one:\n%s", body)
	}
	if !strings.Contains(body, due.Local().Format("15:04")) {
		t.Errorf("missing the local wall-clock time %s in:\n%s",
			due.Local().Format("15:04"), body)
	}
}

// A bare timer used to render the kind badge and then repeat "Timer" as its
// label. The label now falls back to the raw input.
func TestBareTimerLabelIsNotDuplicated(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := post(t, srv.Routes(), "/parse", "q=in+2+hours")
	if got := strings.Count(rec.Body.String(), ">Timer<"); got != 1 {
		t.Errorf("kind badge rendered %d times, want exactly 1:\n%s", got, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), ">in 2 hours<") {
		t.Error("label should fall back to the raw input")
	}
}

// Regression: /parse used to replace the whole timeline with the one new row,
// so every other task disappeared from the screen until a reload.
func TestParseReturnsTheWholeTimeline(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Routes()

	post(t, h, "/parse", "q=remind+me+at+3+PM+to+email+the+invoice")
	rec := post(t, h, "/parse", "q=count+20+min+stretch")
	body := rec.Body.String()
	for _, want := range []string{"email the invoice", "stretch", "Upcoming"} {
		if !strings.Contains(body, want) {
			t.Errorf("timeline missing %q:\n%s", want, body)
		}
	}
	// Sorted by due time: the 20-minute timer comes before the 3 PM reminder.
	if strings.Index(body, "stretch") > strings.Index(body, "email the invoice") {
		t.Error("timeline is not sorted by due time")
	}
}

func TestTimelineEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Routes()
	post(t, h, "/parse", "q=count+20+min+stretch")

	req := httptest.NewRequest(http.MethodGet, "/timeline", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "stretch") {
		t.Errorf("GET /timeline = %d:\n%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "<html") {
		t.Error("GET /timeline returned a full page, want a fragment")
	}
}

// Messages must not land in the timeline: a typo would otherwise wipe the
// list from the screen.
func TestMessagesAreRetargetedToTheNoticeSlot(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Routes()

	for _, tc := range []struct{ path, form string }{
		{"/parse", "q=hello+world"},
		{"/parse", "q="},
		{"/tasks/999/snooze?minutes=5", ""},
	} {
		rec := post(t, h, tc.path, tc.form)
		if got := rec.Header().Get("HX-Retarget"); got != "#notice" {
			t.Errorf("%s %s: HX-Retarget = %q, want #notice", tc.path, tc.form, got)
		}
	}
}

func TestEditChangesLabelAndTime(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Routes()

	post(t, h, "/parse", "q=in+2+hours")
	pending, _ := st.ListPending(context.Background())
	id := pending[0].ID

	form := url.Values{"label": {"call the bank"}, "due": {"2026-10-02T16:30"}}
	rec := post(t, h, "/tasks/"+itoa(id), form.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", rec.Code, rec.Body.String())
	}

	after, _ := st.Get(context.Background(), id)
	if after.Label != "call the bank" {
		t.Errorf("Label = %q, want %q", after.Label, "call the bank")
	}
	if want := time.Date(2026, 10, 2, 16, 30, 0, 0, time.UTC); !after.DueAt.Equal(want) {
		t.Errorf("DueAt = %v, want %v", after.DueAt, want)
	}
	if after.NeedsConfirm {
		t.Error("NeedsConfirm still true after the user set the details themselves")
	}
}

func TestEditRejectsPastAndFinishedTasks(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Routes()

	post(t, h, "/parse", "q=count+20+min")
	pending, _ := st.ListPending(context.Background())
	id := itoa(pending[0].ID)

	rec := post(t, h, "/tasks/"+id, url.Values{"due": {"2026-10-02T09:00"}}.Encode())
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already passed") {
		t.Errorf("past time: status %d, body:\n%s", rec.Code, rec.Body.String())
	}

	for _, bad := range []string{"tomorrow", "2026-13-40T99:99"} {
		rec = post(t, h, "/tasks/"+id, url.Values{"due": {bad}}.Encode())
		if rec.Code != http.StatusBadRequest {
			t.Errorf("due=%q: status %d, want 400", bad, rec.Code)
		}
	}

	rec = post(t, h, "/tasks/"+id, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty edit: status %d, want 400", rec.Code)
	}

	post(t, h, "/tasks/"+id+"/done", "")
	rec = post(t, h, "/tasks/"+id, url.Values{"label": {"too late"}}.Encode())
	if rec.Code != http.StatusConflict {
		t.Errorf("edit after done: status %d, want 409", rec.Code)
	}
	rec = post(t, h, "/tasks/"+id+"/snooze?minutes=5", "")
	if rec.Code != http.StatusConflict {
		t.Errorf("snooze after done: status %d, want 409", rec.Code)
	}
}

// Calibration integrity: only a real edit is a correction. Confirming or
// snoozing a parse means it was right, and must not be logged as wrong.
func TestOnlyEditsCountAsCorrections(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Routes()
	ctx := context.Background()

	post(t, h, "/parse", "q=in+2+hours")
	pending, _ := st.ListPending(ctx)
	id := itoa(pending[0].ID)

	post(t, h, "/tasks/"+id+"/confirm", "")
	post(t, h, "/tasks/"+id+"/snooze?minutes=5", "")
	log, _ := st.ParseAccuracy(ctx)
	if len(log) != 1 || log[0].CorrectedByUser {
		t.Fatalf("confirm/snooze flagged as a correction: %+v", log)
	}

	post(t, h, "/tasks/"+id, url.Values{"label": {"fixed"}}.Encode())
	log, _ = st.ParseAccuracy(ctx)
	if !log[0].CorrectedByUser {
		t.Error("an edit was not recorded as a correction")
	}
}

func TestDeleteAsksFirst(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := post(t, srv.Routes(), "/parse", "q=count+20+min")
	if !strings.Contains(rec.Body.String(), "hx-confirm=") {
		t.Error("delete button has no confirmation step")
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
