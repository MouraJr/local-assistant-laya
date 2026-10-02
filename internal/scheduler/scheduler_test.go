package scheduler

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/geral/local-assistant-laya/internal/store"
)

// fakeNotifier records what the scheduler asked it to deliver.
type fakeNotifier struct {
	fired   []*store.Task
	catchUp [][]*store.Task
}

func (f *fakeNotifier) Fire(_ context.Context, t *store.Task) error {
	f.fired = append(f.fired, t)
	return nil
}

func (f *fakeNotifier) CatchUp(_ context.Context, tasks []*store.Task) error {
	f.catchUp = append(f.catchUp, tasks)
	return nil
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seed(t *testing.T, st *store.Store, due time.Time, label string) *store.Task {
	t.Helper()
	ctx := context.Background()
	task := &store.Task{
		Kind:       store.KindReminder,
		Label:      label,
		RawInput:   label,
		DueAt:      due,
		HasDue:     true,
		SourceTier: "T0",
		Confidence: 0.95,
		Status:     store.StatusPending,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, nil))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// The scheduler sleeps for real time but its notion of "now" is injected, so
// these tests run instantly and cover hours of elapsed time.
func fixedClock(start time.Time) func() time.Time {
	cur := start
	return func() time.Time { return cur }
}

func TestFiresOverdueTasksIndividually(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	clock := fixedClock(now)

	seed(t, st, now.Add(-time.Minute), "first")
	seed(t, st, now.Add(-time.Second), "second")

	n := &fakeNotifier{}
	s := New(st, n, Options{Tick: 15 * time.Second, Now: clock, Logger: quietLogger()})

	// One tick, no elapsed gap: this is a live fire, not a catch-up.
	s.setLast(now)
	now = now.Add(15 * time.Second)
	s.sweep(ctx, now, false)

	if len(n.fired) != 2 {
		t.Fatalf("fired %d tasks, want 2", len(n.fired))
	}
	if len(n.catchUp) != 0 {
		t.Fatalf("catchUp called %d times, want 0 for a live fire", len(n.catchUp))
	}

	pending, _ := st.ListPending(ctx)
	if len(pending) != 0 {
		t.Errorf("%d tasks still pending, want 0", len(pending))
	}
}

// The whole point of the catch-up sweep: a laptop asleep for six hours must
// not fire six phone notifications on resume.
func TestSleepProducesOneAggregateNotABurst(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	clock := fixedClock(now)

	for i := 0; i < 6; i++ {
		seed(t, st, now.Add(time.Duration(i)*time.Hour), "task")
	}

	n := &fakeNotifier{}
	s := New(st, n, Options{Tick: 15 * time.Second, Now: clock, Logger: quietLogger()})

	s.setLast(now)
	now = now.Add(6 * time.Hour) // resume from suspend
	s.sweep(ctx, now, false)

	if len(n.fired) != 0 {
		t.Errorf("fired %d tasks individually, want 0 after a sleep", len(n.fired))
	}
	if len(n.catchUp) != 1 {
		t.Fatalf("catchUp called %d times, want exactly 1", len(n.catchUp))
	}
	if got := len(n.catchUp[0]); got != 6 {
		t.Errorf("catch-up carried %d tasks, want 6", got)
	}

	missed, _ := st.ListMissed(ctx, 100)
	if len(missed) != 6 {
		t.Errorf("%d tasks marked missed, want 6", len(missed))
	}
}

// An old task belongs in the "while you were away" list but must never
// re-notify the user.
func TestStaleTasksAreListedButNotNotified(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	clock := fixedClock(now)

	seed(t, st, now.Add(-2*time.Hour), "recent")
	seed(t, st, now.Add(-48*time.Hour), "ancient")

	n := &fakeNotifier{}
	s := New(st, n, Options{Tick: 15 * time.Second, Now: clock, Logger: quietLogger()})

	s.setLast(now)
	now = now.Add(3 * time.Hour)
	s.sweep(ctx, now, false)

	if len(n.catchUp) != 1 {
		t.Fatalf("catchUp called %d times, want 1", len(n.catchUp))
	}
	if got := len(n.catchUp[0]); got != 1 {
		t.Errorf("catch-up carried %d tasks, want 1 (the 48h-old task must be dropped)", got)
	}

	// Both are still listed as missed, so nothing silently disappears.
	missed, _ := st.ListMissed(ctx, 100)
	if len(missed) != 2 {
		t.Errorf("%d tasks listed as missed, want 2", len(missed))
	}
}

// Duplicate wakes must not double-notify.
func TestFiringIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	clock := fixedClock(now)

	seed(t, st, now.Add(-time.Minute), "once")

	n := &fakeNotifier{}
	s := New(st, n, Options{Tick: 15 * time.Second, Now: clock, Logger: quietLogger()})

	s.setLast(now)
	now = now.Add(15 * time.Second)
	s.sweep(ctx, now, false)
	s.sweep(ctx, now, false)
	s.sweep(ctx, now, false)

	if len(n.fired) != 1 {
		t.Fatalf("fired %d times, want exactly 1", len(n.fired))
	}
}

// Booting with overdue work is a catch-up even when no time has passed inside
// the process.
func TestBootTreatsOverdueAsCatchUp(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	clock := fixedClock(now)

	seed(t, st, now.Add(-30*time.Minute), "survived a reboot")

	n := &fakeNotifier{}
	s := New(st, n, Options{Tick: 15 * time.Second, Now: clock, Logger: quietLogger()})

	s.setLast(now)
	s.sweep(ctx, now, true)

	if len(n.fired) != 0 {
		t.Errorf("fired %d tasks on boot, want a catch-up instead", len(n.fired))
	}
	if len(n.catchUp) != 1 || len(n.catchUp[0]) != 1 {
		t.Fatalf("catchUp = %v, want one call carrying one task", n.catchUp)
	}
}

// A task waiting on the user for confirmation is not a deadline yet.
func TestTasksAwaitingConfirmationAreNotScheduled(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	clock := fixedClock(now)

	task := seed(t, st, now.Add(30*time.Second), "needs a yes")
	yes := true
	if err := st.Update(ctx, task.ID, store.UpdateFields{NeedsConfirm: &yes}); err != nil {
		t.Fatalf("update: %v", err)
	}

	n := &fakeNotifier{}
	s := New(st, n, Options{Tick: 15 * time.Second, Now: clock, Logger: quietLogger()})
	s.reload(ctx)

	if len(s.queue) != 0 {
		t.Errorf("heap holds %d items, want 0 for a task awaiting confirmation", len(s.queue))
	}
	if wait := s.NextWake(); wait != s.tick {
		t.Errorf("NextWake = %v, want the full tick %v", wait, s.tick)
	}

	// The heap is only an optimisation; the sweep is the authority. Past its
	// deadline, an unconfirmed task must still not fire.
	s.setLast(now.Add(time.Minute))
	s.sweep(ctx, now.Add(time.Minute), false)
	if len(n.fired) != 0 || len(n.catchUp) != 0 {
		t.Errorf("unconfirmed task fired: fired=%d catchUp=%d", len(n.fired), len(n.catchUp))
	}
	after, _ := st.Get(ctx, task.ID)
	if after.Status != store.StatusPending {
		t.Errorf("Status = %q, want pending until the user confirms", after.Status)
	}
}

// NextWake must never sleep past the earliest deadline.
func TestNextWakeRespectsEarliestDeadline(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	clock := fixedClock(now)

	seed(t, st, now.Add(90*time.Second), "soon")
	seed(t, st, now.Add(10*time.Second), "sooner")

	s := New(st, &fakeNotifier{}, Options{Tick: 15 * time.Second, Now: clock, Logger: quietLogger()})
	s.reload(ctx)

	if got := s.NextWake(); got != 10*time.Second {
		t.Errorf("NextWake = %v, want 10s", got)
	}
}

// A past deadline must not produce a negative or zero sleep.
func TestNextWakeClampsOverdueToZero(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	clock := fixedClock(now)

	seed(t, st, now.Add(-time.Hour), "overdue")

	s := New(st, &fakeNotifier{}, Options{Tick: 15 * time.Second, Now: clock, Logger: quietLogger()})
	s.reload(ctx)

	if got := s.NextWake(); got != 0 {
		t.Errorf("NextWake = %v, want 0", got)
	}
}
