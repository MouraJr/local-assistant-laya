// Package scheduler decides when a task fires.
//
// Two mechanisms cooperate, and the split matters:
//
//   - A min-heap ordered by due time tells the run loop when it can safely
//     sleep. It is only a wake-up optimisation.
//   - A ticker is the authority on what has expired. Every wake-up re-queries
//     the database rather than trusting the heap, so a wall-clock jump, an NTP
//     correction, or a resume from suspend cannot cause a miss or a spurious
//     fire.
//
// After a sleep or a reboot, overdue tasks do not each buzz the phone. They
// are collected into one catch-up notification, because six alerts on waking
// up is not helpful.
package scheduler

import (
	"container/heap"
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/geral/local-assistant-laya/internal/store"
)

// staleWindow is how far back a task may be before it is listed but never
// re-notified.
const staleWindow = 24 * time.Hour

// Notifier delivers alerts. It is an interface so the scheduler can be tested
// without touching a desktop, a sound card, or a push service.
type Notifier interface {
	// Fire delivers a single on-time alert.
	Fire(ctx context.Context, t *store.Task) error
	// CatchUp delivers one aggregate alert for everything missed while the
	// host was asleep.
	CatchUp(ctx context.Context, tasks []*store.Task) error
}

// item is one entry in the wake-up heap.
type item struct {
	due time.Time
	id  int64
}

type itemHeap []item

func (h itemHeap) Len() int           { return len(h) }
func (h itemHeap) Less(i, j int) bool { return h[i].due.Before(h[j].due) }
func (h itemHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *itemHeap) Push(x any)        { *h = append(*h, x.(item)) }
func (h *itemHeap) Pop() any          { old := *h; n := len(old); it := old[n-1]; *h = old[:n-1]; return it }

// Options configures a Scheduler.
type Options struct {
	// Tick is the wall-clock re-check interval.
	Tick time.Duration
	// Now is injectable for tests.
	Now func() time.Time
	// Logger receives fire and sweep events.
	Logger *slog.Logger
}

// Scheduler fires tasks and survives sleep.
type Scheduler struct {
	store    *store.Store
	notifier Notifier
	tick     time.Duration
	now      func() time.Time
	log      *slog.Logger

	mu    sync.Mutex
	queue itemHeap
	last  time.Time

	wake chan struct{}
}

// New builds a scheduler. Logger may be nil.
func New(st *store.Store, n Notifier, opts Options) *Scheduler {
	tick := opts.Tick
	if tick <= 0 {
		tick = 15 * time.Second
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	heap.Init(&itemHeap{})
	return &Scheduler{
		store:    st,
		notifier: n,
		tick:     tick,
		now:      nowFn,
		log:      log,
		wake:     make(chan struct{}, 1),
	}
}

// Enqueue nudges the loop after a task is created or edited, so a new task
// due in 90 seconds does not wait for the next tick to be noticed.
func (s *Scheduler) Enqueue() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// NextWake reports how long the loop may sleep. Exported for tests.
func (s *Scheduler) NextWake() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return s.tick
	}
	until := s.queue[0].due.Sub(s.now())
	if until < 0 {
		return 0
	}
	if until < s.tick {
		return until
	}
	return s.tick
}

// Run blocks until the context is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	s.last = s.now()
	s.mu.Unlock()

	// Anything already overdue at boot is a catch-up, not a live fire.
	s.sweep(ctx, s.now(), true)
	s.reload(ctx)

	for {
		timer := time.NewTimer(s.NextWake())
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-s.wake:
			timer.Stop()
		case <-timer.C:
		}

		now := s.now()
		s.sweep(ctx, now, false)
		s.reload(ctx)
	}
}

// sweep fires or aggregates everything now overdue.
//
// The database query is the sole source of truth for what is overdue. The
// heap never decides this, which is precisely why a clock jump is harmless.
func (s *Scheduler) sweep(ctx context.Context, now time.Time, booting bool) {
	overdue, err := s.store.DueBefore(ctx, now)
	if err != nil {
		s.log.Error("scheduler: overdue query failed", "error", err)
		s.setLast(now)
		return
	}
	if len(overdue) == 0 {
		s.setLast(now)
		return
	}

	s.mu.Lock()
	gap := now.Sub(s.last)
	s.mu.Unlock()

	// A gap much larger than one tick means the host was asleep or the clock
	// moved. Either way, one aggregate beats a burst of individual alerts.
	slept := booting || gap > 2*s.tick

	if slept {
		var fresh []*store.Task
		var stale int
		for _, t := range overdue {
			// An old task still belongs in the "while you were away" list, but
			// re-notifying about something 48 hours old is noise, not help.
			isStale := t.DueAt.Before(now.Add(-staleWindow))
			if isStale {
				stale++
			}
			moved, err := s.store.MarkMissed(ctx, t.ID)
			if err != nil {
				s.log.Error("scheduler: mark missed failed", "id", t.ID, "error", err)
				continue
			}
			if moved && !isStale {
				fresh = append(fresh, t)
			}
		}
		s.log.Warn("scheduler: catch-up after sleep or clock change",
			"missed", len(fresh), "stale", stale, "gap", gap.String())
		if len(fresh) > 0 {
			if err := s.notifier.CatchUp(ctx, fresh); err != nil {
				s.log.Error("scheduler: catch-up notify failed", "error", err)
			}
		}
	} else {
		for _, t := range overdue {
			fired, err := s.store.MarkFired(ctx, t.ID, now)
			if err != nil {
				s.log.Error("scheduler: mark fired failed", "id", t.ID, "error", err)
				continue
			}
			if !fired {
				// Already handled. This is what makes firing idempotent.
				continue
			}
			if err := s.notifier.Fire(ctx, t); err != nil {
				s.log.Error("scheduler: notify failed", "id", t.ID, "error", err)
			}
		}
		s.log.Info("scheduler: fired overdue tasks", "count", len(overdue))
	}

	s.setLast(now)
}

func (s *Scheduler) setLast(t time.Time) {
	s.mu.Lock()
	s.last = t
	s.mu.Unlock()
}

// reload rebuilds the wake-up heap from the database. The heap holds no state
// that cannot be recovered, so it is always safe to discard.
func (s *Scheduler) reload(ctx context.Context) {
	pending, err := s.store.ListPending(ctx)
	if err != nil {
		s.log.Error("scheduler: reload failed", "error", err)
		return
	}
	next := make(itemHeap, 0, len(pending))
	for _, t := range pending {
		if t.NeedsConfirm {
			// Waiting on the user. It is not a deadline yet.
			continue
		}
		if !t.HasDue {
			continue
		}
		next = append(next, item{due: t.DueAt, id: t.ID})
	}
	heap.Init(&next)

	s.mu.Lock()
	s.queue = next
	s.mu.Unlock()
}
