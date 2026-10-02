// Package notify delivers alerts on the machine the app runs on.
//
// This is the lowest tier of the notification stack in the PRD: it works even
// when the phone is dead, the browser is closed, and the network is down. Web
// Push and SSE layer on top of it in later milestones.
//
// Every notifier here is best-effort by design. A failed toast must never stop
// the scheduler, and must never be silent either: failures are logged and the
// slog WARN record is itself the final fallback.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/geral/local-assistant-laya/internal/store"
)

// Options configures the host notifier.
type Options struct {
	// Sound plays an audible alert. Recommended: a phone push is silent.
	Sound bool
	// Toast shows an OS-level desktop notification.
	Toast bool
	// Logger receives delivery results. Defaults to slog.Default().
	Logger *slog.Logger
}

// HostNotifier alerts through the local desktop.
type HostNotifier struct {
	sound bool
	toast bool
	log   *slog.Logger
}

// NewHost builds the local-desktop notifier.
func NewHost(opts Options) *HostNotifier {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &HostNotifier{sound: opts.Sound, toast: opts.Toast, log: log}
}

// Fire delivers one on-time alert.
func (h *HostNotifier) Fire(ctx context.Context, t *store.Task) error {
	title, body := describe(t)
	h.deliver(title, body)
	return nil
}

// CatchUp delivers exactly one aggregate alert for a batch missed while the
// host was asleep. Never one alert per task.
func (h *HostNotifier) CatchUp(ctx context.Context, tasks []*store.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	if len(tasks) == 1 {
		title, body := describe(tasks[0])
		h.deliver("Missed while you were away", title+" - "+body)
		return nil
	}

	// Cap the visible list. A phone buzz with 40 lines is noise.
	const maxShown = 5
	var b strings.Builder
	fmt.Fprintf(&b, "%d tasks came due while the app was not running:\n", len(tasks))
	for i, t := range tasks {
		if i == maxShown {
			fmt.Fprintf(&b, "...and %d more.", len(tasks)-maxShown)
			break
		}
		_, line := describe(t)
		fmt.Fprintf(&b, "- %s", line)
		if i < len(tasks)-1 {
			b.WriteString("\n")
		}
	}
	h.deliver("Missed while you were away", b.String())
	return nil
}

// deliver fans out to the enabled channels. Each channel is independent: a
// broken toast must not suppress the sound.
func (h *HostNotifier) deliver(title, body string) {
	if h.toast {
		if err := toast(title, body); err != nil {
			h.log.Warn("notify: toast failed", "error", err, "title", title)
		}
	}
	if h.sound {
		if err := sound(); err != nil {
			h.log.Warn("notify: sound failed", "error", err)
		}
	}
	// The log line is the guaranteed-last-resort record.
	h.log.Warn("ALERT", "title", title, "body", body, "at", time.Now().Format(time.RFC3339))
}

// describe turns a task into a human title and body.
func describe(t *store.Task) (string, string) {
	if t.Label != "" {
		if t.Kind == store.KindTimer {
			return "Timer finished", t.Label
		}
		return t.Label, "Reminder"
	}
	if t.Kind == store.KindTimer {
		return "Timer finished", t.RawInput
	}
	return "Reminder", t.RawInput
}

// Chain fans a single event out to several notifiers. The push notifier joins
// this chain in M4 without touching the scheduler.
type Chain struct {
	mu        sync.Mutex
	notifiers []Notifier
}

// Notifier is the interface the scheduler depends on.
type Notifier interface {
	Fire(ctx context.Context, t *store.Task) error
	CatchUp(ctx context.Context, tasks []*store.Task) error
}

// NewChain builds a fan-out notifier.
func NewChain(notifiers ...Notifier) *Chain {
	return &Chain{notifiers: notifiers}
}

// Add appends a notifier to the chain.
func (c *Chain) Add(n Notifier) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notifiers = append(c.notifiers, n)
}

// Fire delivers to every notifier. Errors are collected, not returned, so one
// broken channel cannot suppress another.
func (c *Chain) Fire(ctx context.Context, t *store.Task) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for _, n := range c.notifiers {
		if err := n.Fire(ctx, t); err != nil {
			errs = append(errs, err)
		}
	}
	return join(errs)
}

// CatchUp delivers to every notifier as a single aggregate event.
func (c *Chain) CatchUp(ctx context.Context, tasks []*store.Task) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for _, n := range c.notifiers {
		if err := n.CatchUp(ctx, tasks); err != nil {
			errs = append(errs, err)
		}
	}
	return join(errs)
}

func join(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		return fmt.Errorf("notifier chain: %d failures, first: %w", len(errs), errs[0])
	}
}
