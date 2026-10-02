# PRD — Local Assistant

**Codename:** `local-assistant-laya`
**Status:** Draft for review
**Owner:** geral
**Last updated:** 2026-10-02
**Target:** Single-user, single-host, offline-first personal assistant that understands plain-language
requests and fires an alert at the right moment.

---

## 1. Summary

A local Go application that turns a single line of plain language — *"count 20 min"*, *"remind me today
at 3 PM to email the invoice"* — into a scheduled task, then reliably notifies the user when it fires.

The distinguishing design constraint is **cost and reliability of the intelligence layer**. The app must
never *require* a paid API to function. It uses a four-tier parsing cascade: a deterministic Go parser
first, local decision models and a local LLM as fallback, and a near-free cloud decision model as an
optional upgrade. With every network and every API key removed, the app still handles the majority of
real input correctly and offline.

The UI is server-rendered with HTMX, typed Go components via `templ`, Tailwind for styling, and a
small amount of Alpine.js. There is no SPA build step and no Node toolchain in the runtime path.

---

## 2. Problem statement

Personal reminders today require either (a) a phone app that demands you re-learn its syntax, or
(b) a calendar you must maintain. Both fail the two highest-frequency cases:

- **Short countdown** — "time-box this for 20 minutes." Every task tracker handles this badly or not at
  all, because it is not an event, it is a duration.
- **Time-anchored intent** — "at 3 PM, send the invoice." The user already holds the intent; they
  should not have to navigate to a form to externalize it.

Natural language is the correct interface for both, but natural language parsing usually means an LLM
call, which means latency, cost, and a hard dependency on a network path. For a *timer*, both are
unacceptable: a 20-minute timer that takes 2 seconds to arm, or fails when the laptop is on a train,
is worse than a form.

Additionally, the user wants to avoid cloud spend and privacy exposure for something as trivial as
"tell me in 20 minutes." Any architecture that routes every timer through a paid frontier API fails the
requirement on its face.

---

## 3. Goals

| ID | Goal | Measure |
|----|------|---------|
| G1 | Zero-cost baseline operation | All Tier 0/1/2/3-local paths available at **$0/month**; app fully functional with no API keys set |
| G2 | Arm latency imperceptible | p95 end-to-end parse + schedule < 400 ms for accepted input |
| G3 | Timer arming is offline | T0 path requires no network. App degrades cleanly with all interfaces down |
| G4 | Alert reaches the phone | Web Push delivery confirmed end-to-end on iOS and Android |
| G5 | Never silently misfire | No task is scheduled without a recorded confidence; low-confidence parses require explicit confirmation |
| G6 | Single binary, any OS | Cross-compiles and runs on Windows, macOS, Linux with no cgo and no runtime install |
| G7 | Explainability | Every scheduled task records which tier parsed it, with what confidence, and from what raw input |

## 4. Non-goals

Explicitly out of scope for v1. Each is a defensible "no":

- **Sending email or any external action.** The app reminds; it does not act. Auto-sending email is a
  permissions, credentials, and irreversible-action problem that a local timer has no business solving.
- **Calendar / CalDAV / Google Calendar sync.** Bidirectional sync is a large surface with conflict
  semantics. v1 is a standalone list.
- **Multi-user, auth, remote access from untrusted networks.** Single user on the tailnet.
- **Task management, lists, subtasks, habits, project tracking.** Not a todo app.
- **Voice input.** Speech-to-text is a separate dependency and a separate privacy conversation.
- **A general-purpose agent with tools.** Jev/Laya output drives *routing and classification only*.
  There is no tool-calling loop in v1.
- **Cross-device sync of the task list.** The database is the single source of truth on the host.

---

## 5. Personas

**Primary — the operator (the user).** Technical, runs their own infrastructure, already operates local
models (Ollama on a home server, a Laya sidecar). Privacy-conscious to the point of running AI locally.
Wants a tool that is faster and more reliable than a phone app for two specific task shapes, and is
willing to accept configuration work (Tailscale, PWA install) in exchange for owning the data.

**Secondary — the phone.** Not a persona in the usual sense but a first-class surface: a small
installable PWA that only ever displays "what is coming up" and receives pushes. No input required on
the phone in v1.

---

## 6. Core user stories

### US-001 — Arm a duration timer from plain language
> As the user, I want to type "count 20 minutes" and have a countdown start immediately, so that I can
> work for a bounded block of time.

**Acceptance criteria**
1. Input `count 20 min`, `20 min timer`, `time-box 45m`, `countdown for 1h30` all arm a timer.
2. The UI shows the countdown ticking and the absolute fire time (local timezone) within 400 ms.
3. A sound and a Web Push notification fire at T+duration ± 2 s.
4. The task row records `source_tier`, `confidence`, and the verbatim raw input.

### US-002 — Arm a reminder at a wall-clock time from plain language
> As the user, I want to type "remind me today at 3 PM to email the invoice" and have it appear in my
> upcoming list, so that I do not have to open a form.

**Acceptance criteria**
1. `today at 3 PM`, `tomorrow 9am`, `at 15:00`, `Friday 17:30`, `in 2 hours` all resolve correctly.
2. If the named time has already passed today, the app resolves it to tomorrow and **says so** in the
   confirmation preview rather than silently choosing.
3. The free-text remainder after the time expression is captured as the task label verbatim.
4. The task appears in the timeline immediately and is sorted correctly against existing tasks.

### US-003 — Confirm before a low-confidence parse is trusted
> As the user, I want the app to show me how it interpreted my sentence before arming it, when it is not
> sure, so that a misparse does not become a silently missed appointment.

**Acceptance criteria**
1. Any parse whose decision confidence falls below the configured threshold renders a confirmation
   preview: the extracted duration/absolute time, the captured label, the resolved fire time.
2. Preview accepts with Enter / one click, or discards.
3. **A task is never scheduled directly from a below-threshold parse.**
4. Above-threshold parses schedule immediately without an interstitial.

### US-004 — Correct or cancel a scheduled task
> As the user, I want to fix or cancel anything the app armed, so that a bad parse costs one click, not
> a missed commitment.

**Acceptance criteria**
1. Each task row offers edit (time, label), snooze (+5/+15/+60), complete, delete.
2. All mutations are HTMX swaps; no full page reload.
3. A change to a task already in the past is rejected with a visible explanation.

### US-005 — Receive the alert on the phone, browser closed
> As the user, I want the notification to arrive on my phone even when the browser is closed and the
> phone is not on my home network, so that I can rely on it.

**Acceptance criteria**
1. The PWA is installable and registers a service worker over HTTPS on the tailnet.
2. With the browser fully closed and the phone on cellular, the push notification arrives.
3. On iOS this requires Home Screen installation; the app detects non-installed state and tells the user.

### US-006 — Never lose a task across sleep, hibernate, or reboot
> As the user, I want tasks to survive my laptop sleeping and survive a restart, so that I can trust the
> list.

**Acceptance criteria**
1. All state is committed to SQLite before the parse response is returned to the client.
2. After a suspend/hibernate/resume, overdue tasks produce **one** catch-up alert, not a burst.
3. After a cold boot, overdue tasks surface as a "missed while you were away" banner; tasks older than
   24 h are not re-notified, just listed.

### US-007 — See what the AI decided, and how confident it was
> As the user, I want to inspect the parse for any task, so that I can tell when to trust the app and
> when to check it myself.

**Acceptance criteria**
1. Every task row exposes the tier used, the confidence value, and the raw input.
2. A dedicated panel lists recent parses with their verdicts, forming a calibration log.

---

## 7. System architecture

Single Go binary, three long-lived concerns plus a stateless web tier.

```
┌──────────────────────────────────────────────────────────────┐
│  Browser (desktop + phone PWA over Tailscale HTTPS)          │
│  htmx · Alpine · service worker · Web Push client            │
└───────────────┬───────────────────────┬──────────────────────┘
                │ HTMX (HTML fragments) │ SSE (/events)
                ▼                       ▼
┌──────────────────────────────────────────────────────────────┐
│  internal/web — net/http mux, handlers, templ components     │
├──────────────────────────────────────────────────────────────┤
│  internal/parse — cascade orchestrator (T0 → T1/T2 → T3)     │
│    ├─ internal/parse/rules   deterministic Go lexer          │
│    └─ internal/ai            provider interface + clients    │
├──────────────────────────────────────────────────────────────┤
│  internal/scheduler — min-heap timers, wall-clock re-check   │
│  internal/store    — SQLite (WAL), migrations, repositories  │
│  internal/notify   — Web Push (VAPID) · SSE · host alerts    │
└───────────────┬──────────────────────────────────────────────┘
                │
        ┌───────┴────────┬──────────────┬─────────────────┐
        ▼                ▼              ▼                 ▼
   Jev (cloud)     Laya sidecar    Ollama (LAN)      Host (sound,
   $0.042/MTok in  127.0.0.1:8765  192.168.1.119     toast, log)
   free out        $0              :11434  $0
```

**Process model.** One process. The scheduler is a goroutine, not a separate service, because a single
SQLite file owned by one process eliminates an entire class of concurrency bugs.

**Repository layout** (consistent with `go-htmx-store` conventions):

```
cmd/localassistant/     entry point; serve | db migrate | keys generate
config/                 non-secret runtime config (defaults.yaml)
docs/                   this PRD + design notes
internal/
  ai/                   provider interface, Jev client, Laya client, Ollama client
  parse/                cascade orchestrator, rules lexer, confidence gate
  notify/               webpush, sse hub, host notifiers (per-OS)
  scheduler/            heap, tick loop, catch-up sweep
  store/                pgx-free sqlite pool + embedded migrations
    migrations/         *.up.sql / *.down.sql (embedded)
  web/                  mux, handlers, middleware
    templates/          *.templ
web/                    static: tailwind output, htmx, alpine, sw.js, manifest
```

---

## 8. Technology stack

| Concern | Choice | Rationale |
|---|---|---|
| Language | Go 1.26 | Requirement; single static binary |
| Router | `net/http` stdlib (`ServeMux` patterns) | Matches existing `go-htmx-store`; no framework tax |
| Components | `github.com/a-h/templ` | Type-checked Go templates; `templ generate` in build |
| Styling | Tailwind CSS, standalone CLI | No Node toolchain; committed output CSS |
| Client JS | HTMX + Alpine.js, vendored locally | No CDN — required for offline-first |
| Storage | `modernc.org/sqlite` | Pure Go, no cgo → true cross-OS cross-compile |
| Realtime | Server-Sent Events | One-directional server→client; no WS library |
| Push | VAPID keys generated on first run | `web-push-go` / stdlib-compatible client |
| HTTPS | `tailscale serve` (primary) or `tailscale cert` | Valid cert, no cert lifecycle management |
| Logging | `log/slog` JSON handler | Consistent with existing project |
| Config | env vars + `.env` via `joho/godotenv` | Consistent with existing project |

**Why not a Go web framework (Echo/Chi)?** The app has ~14 routes and one dependency-free job. The
stdlib mux with Go 1.22+ method-and-wildcard patterns is sufficient and removes upgrade surface.

**Why SQLite over PostgreSQL?** Single user, single writer, single file, zero ops. `modernc.org/sqlite`
avoids cgo, which is the difference between "compiles on my machine" and "cross-compiles to eleven
platforms."

---

## 9. Natural-language parsing and the AI tier cascade

This is the core of the product. The design constraint that shapes everything below: **neither of the
structured decision models can produce free-form text.**

- **Jev** (TypeSafe AI System One) deliberately gives up string generation in exchange for
  type-safe structured values that cannot be malformed and always carry calibrated probabilities.
  Latency 70 ms–500 ms; input $0.042/MTok, output free; early access; API at `docs.typesafe.ai`.
- **Laya** (Convai Innovations, Apache-2.0, ~0.4B, ModernBERT) returns `choice` / `score` / `noul`
  — a classification with P(true). It is not a text extractor.

Therefore **classification and extraction are different jobs and go to different tiers.** A decision
model can tell us *what kind of request this is* and *whether we are confident*; it cannot tell us
*what the user said the task was*. Conflating these is the most likely way to build the wrong system.

### 9.1 Tier definitions

| Tier | Component | Cost | Network | Job | Typical latency |
|---|---|---|---|---|---|
| **T0** | Go rules lexer | $0 | none | Parse duration, clock time, date expressions, and capture the verbatim remainder as the label | < 5 ms |
| **T1** | Jev (System One) | ~$0 (0.0002¢/typical call) | cloud | Structured decision: intent class, time anchoring, ambiguity flags, calibrated probability | 70–500 ms |
| **T2** | Laya sidecar | $0 | loopback | Same structured decision, fully offline. Tiebreak and availability fallback | 10–50 ms |
| **T3** | Ollama local LLM | $0 | LAN | Extract structured fields when T0 fails *and* a non-trivial free-text label or duration phrasing needs interpretation | 300–3000 ms |

### 9.2 Tier 0 — deterministic Go parser

Handles the majority of real input with zero network, zero cost, and deterministic behavior. Grammar:

- **Durations:** `20 min`, `20m`, `1h30`, `45 mins`, `1 hour 15 minutes`, `90 seconds`, `half an hour`.
- **Absolute clock times:** `3 PM`, `3:30pm`, `15:00`, `15h`, `at 9`, `noon`, `midnight`.
- **Day anchors:** `today`, `tomorrow`, `tonight`, `monday`…`sunday`, `next week`, `eod`, `eow`.
- **Relative offsets:** `in 2 hours`, `in 10 min`, `in a week`.
- **Bare:** a bare number with a unit, or a bare clock time, defaults to the `timer` / `today` intent.

Implementation notes:

- Compile the patterns once into a package-level `regexp.Regexp` set; never compile in a hot path.
- Order matters: relative offsets before absolute anchors; day anchors before bare clock times so
  "tomorrow 9am" does not degrade to "today 9am."
- The parser returns `(Parse, confidence, reason)`, not a bare struct. `reason` is surfaced in the
  confirmation preview so the user learns the app's grammar.
- Ambiguity is an explicit outcome, not a guess: `"remind me at 3 to call"` is ambiguous between a
  duration and a clock time. The parser must *say so* rather than defaulting.
- A hand-rolled lexer over normalized text (lowercased, whitespace-collapsed, unicode-normalized) is
  preferred over a large dependency for the subset of grammar above.

### 9.3 Tiers 1 and 2 — structured decisions

Both providers implement one interface, so they are interchangeable and independently testable:

```go
// internal/ai/provider.go
type Decision struct {
    Intent     Intent          // timer | reminder | ambiguous | unknown
    TimeKind   TimeKind        // duration | clock | relative | none
    Confidence float64         // calibrated P(correct), 0..1
    Tier       Tier            // T1 | T2
    Rationale  string          // human-readable, for the preview
    Latency    time.Duration
}

type Provider interface {
    Name() string
    Healthy(ctx context.Context) bool
    Decide(ctx context.Context, q Question) (Decision, error)
}
```

The `Question` is a set of typed, human-readable queries with declared response domains — the shape
both Laya (`questions`/`thresholds`) and Jev expect:

1. `is_timer` — "Does the user's text request a bounded block of time to work in?" → bool
2. `is_reminder` — "Does the user's text request to be reminded of something?" → bool
3. `has_clock_time` — "Does the user's text contain a time of day?" → bool
4. `needs_clarification` — "Is the user's intent ambiguous?" → bool

Routing policy:

- **T1 (Jev) preferred when `JEV_API_KEY` is set and the health probe passes.**
- **T2 (Laya) is never the sole arbiter of anything that matters.** The existing deployment notes for
  `paperclip-laya` record that Laya's `noul` is phrasing-sensitive — 0.18 versus 0.78 on
  near-identical posts. It is therefore used for: availability fallback when T1 is down, corroboration
  when T1's confidence lands in the ambiguous middle band, and as a signal to *lower* overall
  confidence when the two disagree.
- **Disagreement is a first-class outcome.** If T1 and T2 disagree beyond a margin, the app drops to
  the confirmation preview (US-003) rather than picking a winner. This is the single most important
  reliability rule in the system.
- Any provider error is non-fatal. The cascade records the failure, falls through, and the UI reports
  which tiers were attempted.

### 9.4 Tier 3 — local LLM extraction

Only invoked when T0 produced no parse *and* the structured tiers confidently identified a timer or
reminder intent *and* the remaining text still requires interpretation.

- Endpoint: `http://192.168.1.119:11434/v1` (OpenAI-compatible), models already provisioned locally
  include `llama3.2:latest` (3B) and `qwen3-vl:2b`.
- Small models are chosen deliberately: this is extraction, not reasoning, and the job is bounded.
- The prompt demands a single JSON object matching a Go struct, decoded with strict validation. Any
  decode failure, extra key, or out-of-range value is a hard error — never a partial acceptance.
- Tier 3 output **never** schedules a task directly. It carries `confidence = 0.5` by construction,
  which routes it into the mandatory confirmation preview. An extractor that guesses must always be
  confirmed by a human.

### 9.5 Cascade decision procedure

```
1. Run T0.
   ├─ parsed with confidence >= 0.85 ──────────────→ schedule immediately
   └─ otherwise, continue to step 2

2. Run T1 (Jev) if configured; concurrently ready T2 (Laya) if sidecar is up.
   ├─ T0 and a decision model agree, confidence >= threshold ──→ schedule immediately
   ├─ decision models disagree beyond margin ─────────────────→ confirmation preview (US-003)
   └─ all providers unavailable or unconfident ────────────────→ Tier 3 if a label needs
                                                                 interpretation, else
                                                                 "I didn't understand"
                                                                 + confirmation preview

3. Every accepted task is persisted before the HTTP response, with tier, confidence, rationale,
   raw input, and resolved absolute fire time.
```

Thresholds live in config, not code, so they can be calibrated (see §12, M5) without a rebuild.

### 9.6 Cost model

At 20 tasks/day, 25/day max:

| Configuration | Monthly spend | Notes |
|---|---|---|
| T0 only (offline) | **$0.00** | Covers the majority of input |
| + Laya (loopback) | **$0.00** | Fully local decisions |
| + Jev | **< $0.01** | 0.042¢/1K input tokens; a ~40-token query is a fraction of a cent |
| Paid frontier LLM (rejected) | ~$1–3/mo | Not required, not used |

There is no configuration in which this app requires a paid API.

---

## 10. Notification architecture

Four channels in priority order, each degrading independently.

| Tier | Channel | Mechanism | Delivers when |
|---|---|---|---|
| **L1** | Web Push | VAPID-signed payload to stored PushSubscription via browser push service | Browser **closed**, phone on cellular or any network |
| **L2** | Live UI (SSE) | `text/event-stream` on `/events`; Alpine updates countdown and shows a toast | Browser open on the tailnet URL |
| **L3** | Host alerts | Per-OS desktop toast, plus a sound, plus `slog` at WARN | Always; independent of phone |

### 10.1 Why Tailscale is load-bearing

Web Push requires a **secure context** to register a service worker. `http://192.168.1.x:8080` is not a
secure context; `http://localhost` is; `https://machine.tailnet.ts.net` is.

Critically, **Web Push does not need Tailscale at runtime.** Delivery goes
`server → browser vendor push service → device`, entirely over the public internet. Tailscale's role is
solely to make the UI reachable over valid HTTPS so the service worker is permitted to register at
all. This is why the phone receives alerts even when it is off the home network.

Two deployment options:

- **Preferred — `tailscale serve`.** Terminates TLS with an automatically provisioned certificate on
  the MagicDNS `*.ts.net` name and proxies to the local Go listener. No certificate lifecycle to
  manage.
- **Alternative — `tailscale cert <domain>` + Go's TLS listener** on `:8443`. Keeps the Go binary as
  the sole TLS terminator, at the cost of certificate renewal handling.

Hard rule: the UI carries no credentials and no remote-access control beyond possession of the
tailnet, so the application must never bind a plaintext listener to a tailnet address.

### 10.2 PWA requirements

- Web app manifest (`display: standalone`), icons at 192/512, theme colour.
- Service worker precaches the shell (HTMX, Alpine, CSS) so the app opens with no network.
- Push subscription is opt-in from the UI, stored per-device in SQLite; multiple devices supported.
- **iOS constraint (non-negotiable for the phone half):** Web Push on iOS requires iOS 16.4+ **and**
  that the PWA has been added to the Home Screen. If not installed, Web Push registration fails
  silently in some browser versions. The app must detect this state and surface explicit install
  instructions rather than letting the user believe they are subscribed.
- VAPID keypair is generated on first run and written to the platform config directory
  (`~/.config/local-assistant/` on Linux, `%APPDATA%\local-assistant\` on Windows,
  `~/Library/Application Support/local-assistant/` on macOS). Regeneration invalidates all
  subscriptions; the CLI command `localassistant keys rotate` exists for that recovery path.

### 10.3 Deferred option (P2)

If Home Screen installation proves too fragile for iOS, add an **ntfy** topic published to an
ntfy instance reachable over the tailnet. It gives real background push with no APNs dependency.
Explicitly not in v1: it adds a second always-on service to maintain.

---

## 11. Data model

SQLite, WAL mode, `busy_timeout` set. Migrations embedded in the binary.

```sql
CREATE TABLE tasks (
  id            INTEGER PRIMARY KEY,
  kind          TEXT    NOT NULL CHECK (kind IN ('timer','reminder')),
  label         TEXT    NOT NULL DEFAULT '',
  raw_input     TEXT    NOT NULL,
  -- exactly one of the following is set per kind
  duration_ms   INTEGER,
  due_at        TEXT,            -- RFC3339 with local offset, computed at parse time
  status        TEXT    NOT NULL CHECK (status IN
                  ('pending','fired','done','cancelled','missed')),
  source_tier   TEXT    NOT NULL,   -- T0 | T1 | T2 | T3
  confidence    REAL    NOT NULL,
  rationale     TEXT    NOT NULL DEFAULT '',
  needs_confirm INTEGER NOT NULL DEFAULT 0,
  notified_at   TEXT,
  created_at    TEXT    NOT NULL,
  updated_at    TEXT    NOT NULL
);
CREATE INDEX idx_tasks_status_due ON tasks (status, due_at);

CREATE TABLE push_subscriptions (
  id         INTEGER PRIMARY KEY,
  endpoint   TEXT NOT NULL UNIQUE,
  p256dh     TEXT NOT NULL,
  auth       TEXT NOT NULL,
  ua_hint    TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE parse_log (            -- calibration data; feeds §12 M5
  id            INTEGER PRIMARY KEY,
  raw_input     TEXT NOT NULL,
  chosen_tier   TEXT NOT NULL,
  confidence    REAL NOT NULL,
  rationale     TEXT NOT NULL,
  accepted      INTEGER NOT NULL,
  corrected_by_user INTEGER,       -- set when user edits; the training signal
  created_at    TEXT NOT NULL
);
```

Notes:

- `due_at` is stored with an explicit UTC offset. The scheduler compares in UTC; the UI renders in the
  host's local zone. A reminder for "3 PM" created on Tuesday means 15:00 *local on the parsed day* —
  and DST transitions are handled by re-resolving from the original anchor, not by adding 24 h.
- `needs_confirm` is persisted so that a reload between preview and acceptance cannot lose the
  obligation to confirm.
- `parse_log` is deliberately separate from `tasks`. It is the dataset that tells us, weeks later,
  whether the confidence thresholds are actually calibrated — the same shadow-mode discipline used in
  `paperclip-laya`.

---

## 12. Scheduler design

**Heap plus wall-clock re-check.** A single min-heap keyed by `due_at` provides O(log n) wakeup. The
heap alone is not sufficient, because it is unaware of wall-clock jumps, so:

1. A ticker runs every 15 s and compares the heap head against wall-clock now. The ticker, not the
   heap, is the authority — so a system clock jump, NTP correction, or resume-from-sleep cannot cause a
   missed or spurious fire.
2. On wake from suspend or hibernate, a **catch-up sweep** runs: all `pending` tasks with
   `due_at < now` are collected into a single "while you were away" notification containing the
   aggregate, each task is marked `missed`, and **no individual push is sent**. This directly prevents
   the failure mode where a laptop asleep for six hours fires six phone notifications on resume.
3. Tasks older than 24 h are listed as missed but are not re-notified, ever.
4. Firing is idempotent per task: `status` transitions under a conditional `UPDATE ... WHERE status =
   'pending'`, so a duplicate wake cannot double-notify.
5. On boot, the scheduler loads `pending` tasks, re-resolves any day-anchored reminders, and enqueues.
6. Durations are stored as an absolute `due_at` computed at arm time, so a duration survives a restart
   as a wall-clock deadline rather than as "20 minutes from whenever the process restarts."

---

## 13. HTTP surface

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/` | Timeline + NL input (full page) |
| `POST` | `/parse` | Run cascade; returns either a task fragment or a confirmation-preview fragment |
| `POST` | `/tasks/{id}/confirm` | Accept a previewed parse |
| `POST` | `/tasks/{id}` | Edit / snooze / complete / cancel (intent field) |
| `GET` | `/healthz` | Process + scheduler + provider health |
| `GET` | `/events` | SSE stream: ticks, task mutations, toasts |
| `GET` | `/sw.js`, `/manifest.webmanifest` | PWA shell (outside the auth-less tailnet scope) |
| `POST` | `/push/subscribe`, `/push/unsubscribe` | Manage device subscriptions |
| `GET` | `/calibration` | `parse_log` viewer (US-007) |

All mutating endpoints return HTML fragments for HTMX to swap. There is no JSON API in v1; the
`/push/*` endpoints are the sole exception because the service worker needs `application/json`.

---

## 14. Security and privacy

- **Bind to loopback or the Tailscale interface only.** Never `0.0.0.0` on a shared network.
- **All access is tailnet-scoped.** Tailscale ACLs are the authorization layer; the app itself
  implements no user accounts in v1. This is an accepted, documented consequence of single-user scope.
- **No outbound request contains task content** except the T1 (Jev) and T3 (Ollama) calls, whose
  payloads are logged and inspectable. When `JEV_API_KEY` is unset, nothing leaves the host.
- **API keys** read from environment, never written to the database, never rendered into templates.
- **HTML escaping** is `templ`'s default; any `templ.Raw` usage requires a documented justification.
- **No arbitrary code execution from parsed input.** Tiers produce typed values; string interpolation
  into shell commands is forbidden by review.
- **Destructive actions** (delete task, rotate keys) require explicit confirmation, not a single click.

---

## 15. Non-functional requirements

| Requirement | Target |
|---|---|
| Cold start to serving | < 1 s |
| T0 parse + persist | < 50 ms |
| Full cascade p95 | < 400 ms (T1); < 150 ms offline (T0/T2) |
| Fire accuracy | ≥ 99.9% of tasks within ±2 s of target |
| Memory footprint | < 80 MB RSS idle |
| Binary size | < 25 MB |
| Build | `CGO_ENABLED=0` cross-compile to windows/darwin/linux on amd64 + arm64 |
| Dependencies | No Node toolchain required to build or run |
| Offline | Full T0 functionality with all network interfaces down |
| Accessibility | Keyboard reachable, visible focus, respects `prefers-reduced-motion`, no colour-only state |

---

## 16. Risk register

| # | Risk | Impact | Likelihood | Mitigation |
|---|---|---|---|---|
| R1 | **Silent misparse schedules the wrong time** | High — user misses a commitment | Medium | Mandatory confidence gate (US-003); preview for anything below threshold; Tier 3 is *never* allowed to schedule unattended |
| R2 | **Laya phrasing-sensitivity** makes it a poor sole arbiter | Medium | High (observed) | T2 is fallback/corroboration only; disagreement forces a preview; never gates a decision alone |
| R3 | **Jev early-access instability or outage** | Medium | Medium | Optional by design; full offline operation without it; health probe with automatic T2 fallback |
| R4 | **Web Push silently not registered on iOS** (not Home Screen installed) | High — user believes they are covered and is not | High | Detect registration state and surface install instructions; explicit "subscribed / not subscribed" status in UI |
| R5 | **Secure-context failure** if served over LAN IP over HTTP | High — no push at all | Medium | Tailscale Serve documented as the deploy path; startup self-check warns if the origin is not a secure context |
| R6 | **Sleep/hibernate produces an alert storm** | Medium | Medium | Aggregate catch-up sweep (§12.2); idempotent fire via conditional update |
| R7 | **Confidence thresholds are guesses** | Medium | High | Thresholds in config; `parse_log` records user corrections as ground truth; calibrate before enabling unattended scheduling |
| R8 | **Ollama box on LAN is unreachable** | Low | Medium | T3 is last resort only; T0/T1/T2 cover the path; failure degrades to a clear "didn't understand" |
| R9 | **Templ/Tailwind build-tool drift** | Low | Medium | Pin tool versions; commit generated CSS so runtime needs no toolchain |
| R10 | **Scope creep into "general assistant"** | Medium | High | Non-goals §4 are binding; no tool-calling loop in v1 |

---

## 17. Milestones

**M1 — Skeleton and T0 loop** *(no AI required; proves the product is worth building)*
Templ + Tailwind + HTMX + Alpine shell; SQLite schema and migrations; min-heap scheduler with ticker
authority; duration and clock-time T0 parser; SSE countdown; host sound + desktop toast. US-001, US-004,
US-006 substantially complete.

**M2 — Calendar-grade T0 and the timeline**
Day/relative anchors, `in 2 hours`, DST-safe re-resolution, past-time rollover with explicit "moved to
tomorrow" messaging, edit/snooze/complete flows, missed-tasks banner. US-002, US-004, US-006 complete.

**M3 — Confidence gate and calibration log**
Decision interface, shadow-mode integration of T1/T2, preview fragment, `parse_log`, `/calibration`
page, threshold config. US-003, US-007 complete.

**M4 — PWA, Web Push, Tailscale deploy path**
Service worker, manifest, VAPID generation, subscription management, install-state detection,
Tailscale Serve instructions, end-to-end phone verification on iOS and Android. US-005 complete.

**M5 — Calibrate and enable unattended scheduling**
Run in shadow mode for two weeks of real use. Compare `parse_log` confidence against `corrected_by_user`
ground truth. Adjust thresholds. Only then relax the confirmation gate for high-confidence parses.

**M6 — Tier 3 extraction (optional)**
Ollama extractor, strict JSON validation, strict confidence ceiling. Only if M5 shows real inputs that
T0/T1/T2 cannot handle.

---

## 18. Open questions

1. **Alert granularity.** For a fire-on-wake catch-up, is one aggregate push per batch acceptable, or
   does the user want per-task pushes with a hard cap (e.g. first 3, then "and 6 more")?
2. **Recurrence.** v1 has no repeating tasks. Confirm this is acceptable — weekly reminders are a
   frequent real-world request and would add a recurrence rule engine to the data model.
3. **Snooze defaults.** Are 5/15/60 minutes the right set, or should snooze accept a natural-language
   duration through the same cascade?
4. **Sound choice.** Host alert sound and push sound — one audio asset per platform, or per-task-kind
   (timer vs reminder) so they are distinguishable without looking?
5. **Laya sidecar lifecycle.** Should the Go app supervise and health-check the Laya sidecar process, or
   assume it is managed externally (NSSM/Task Scheduler) as `paperclip-laya` does today?
6. **Retention.** `parse_log` grows forever. Acceptable volume, or a pruning policy needed?