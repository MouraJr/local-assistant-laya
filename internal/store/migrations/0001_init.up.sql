CREATE TABLE tasks (
  id            INTEGER PRIMARY KEY,
  kind          TEXT    NOT NULL CHECK (kind IN ('timer','reminder')),
  label         TEXT    NOT NULL DEFAULT '',
  raw_input     TEXT    NOT NULL,
  duration_ms   INTEGER,
  due_at        TEXT,
  status        TEXT    NOT NULL CHECK (status IN
                  ('pending','fired','done','cancelled','missed')),
  source_tier   TEXT    NOT NULL,
  confidence    REAL    NOT NULL,
  rationale     TEXT    NOT NULL DEFAULT '',
  needs_confirm INTEGER NOT NULL DEFAULT 0,
  rolled_over   INTEGER NOT NULL DEFAULT 0,
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

-- Calibration data. corrected_by_user is the only ground truth we have; it is
-- what turns guessed confidence thresholds into measured ones.
CREATE TABLE parse_log (
  id               INTEGER PRIMARY KEY,
  raw_input        TEXT NOT NULL,
  chosen_tier      TEXT NOT NULL,
  confidence       REAL NOT NULL,
  rationale        TEXT NOT NULL,
  accepted         INTEGER NOT NULL,
  corrected_by_user INTEGER,
  created_at       TEXT NOT NULL
);