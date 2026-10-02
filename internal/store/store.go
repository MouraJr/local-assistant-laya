package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrNotFound is returned when a task id does not exist.
var ErrNotFound = errors.New("task not found")

// Task status values.
const (
	StatusPending   = "pending"
	StatusFired     = "fired"
	StatusDone      = "done"
	StatusCancelled = "cancelled"
	StatusMissed    = "missed"
)

// Kind values.
const (
	KindTimer    = "timer"
	KindReminder = "reminder"
)

// Task is one scheduled obligation.
type Task struct {
	ID           int64
	Kind         string
	Label        string
	RawInput     string
	DurationMS   int64
	DueAt        time.Time
	HasDue       bool
	Status       string
	SourceTier   string
	Confidence   float64
	Rationale    string
	NeedsConfirm bool
	// RolledOver records that the user named a time which had already passed,
	// and the app moved it to the following day. Surfaced, never silent.
	RolledOver bool
	NotifiedAt time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ParseEntry is one row of the calibration log.
type ParseEntry struct {
	ID              int64
	RawInput        string
	ChosenTier      string
	Confidence      float64
	Rationale       string
	Accepted        bool
	CorrectedByUser bool
	CreatedAt       time.Time
}

// Store is the SQLite-backed repository. One process owns the file.
type Store struct {
	db *sql.DB
}

// Open connects to SQLite with WAL and applies migrations.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite tolerates one writer. Keeping the pool at one connection removes
	// SQLITE_BUSY from the failure surface entirely.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.Glob(migrationFS, "migrations/*.up.sql")
	if err != nil {
		return fmt.Errorf("glob migrations: %w", err)
	}
	sort.Strings(entries)

	for _, name := range entries {
		var seen int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&seen)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if seen > 0 {
			continue
		}
		body, err := migrationFS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
			name, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

func ts(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTS(s sql.NullString) time.Time {
	if !s.Valid || s.String == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return time.Time{}
	}
	return t
}

const taskCols = `id, kind, label, raw_input, duration_ms, due_at, status,
	source_tier, confidence, rationale, needs_confirm, rolled_over, notified_at, created_at, updated_at`

func scanTask(row interface{ Scan(...any) error }) (*Task, error) {
	var (
		t          Task
		durationMS sql.NullInt64
		dueAt      sql.NullString
		notifiedAt sql.NullString
		createdAt  sql.NullString
		updatedAt  sql.NullString
		needsConf  int
		rolledOver int
	)
	if err := row.Scan(&t.ID, &t.Kind, &t.Label, &t.RawInput, &durationMS, &dueAt,
		&t.Status, &t.SourceTier, &t.Confidence, &t.Rationale, &needsConf, &rolledOver,
		&notifiedAt, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	t.DurationMS = durationMS.Int64
	t.HasDue = dueAt.Valid
	t.DueAt = parseTS(dueAt)
	t.NotifiedAt = parseTS(notifiedAt)
	t.CreatedAt = parseTS(createdAt)
	t.UpdatedAt = parseTS(updatedAt)
	t.NeedsConfirm = needsConf == 1
	t.RolledOver = rolledOver == 1
	return &t, nil
}

// CreateTask persists a task. Callers must treat a returned error as fatal to
// the request: the response is only sent after this succeeds.
func (s *Store) CreateTask(ctx context.Context, t *Task) error {
	now := time.Now().UTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	if t.Status == "" {
		t.Status = StatusPending
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks (kind, label, raw_input, duration_ms, due_at, status,
			source_tier, confidence, rationale, needs_confirm, rolled_over, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Kind, t.Label, t.RawInput, nullableInt(t.DurationMS), ts(t.DueAt), t.Status,
		t.SourceTier, t.Confidence, t.Rationale, boolInt(t.NeedsConfirm), boolInt(t.RolledOver),
		ts(t.CreatedAt), ts(t.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	t.ID = id
	return nil
}

func nullableInt(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Get returns a single task.
func (s *Store) Get(ctx context.Context, id int64) (*Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get task %d: %w", id, err)
	}
	return t, nil
}

// ListPending returns every pending task ordered by due time. Tasks needing
// confirmation are included so they can be re-rendered after a reload.
func (s *Store) ListPending(ctx context.Context) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+`
		FROM tasks WHERE status = ? ORDER BY due_at ASC, id ASC`, StatusPending)
	if err != nil {
		return nil, fmt.Errorf("list pending: %w", err)
	}
	defer rows.Close()
	return collect(rows)
}

// ListMissed returns overdue tasks that were swept while the host was away.
func (s *Store) ListMissed(ctx context.Context, limit int) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+`
		FROM tasks WHERE status = ? ORDER BY due_at ASC LIMIT ?`, StatusMissed, limit)
	if err != nil {
		return nil, fmt.Errorf("list missed: %w", err)
	}
	defer rows.Close()
	return collect(rows)
}

// ListRecent returns the most recently finished tasks.
func (s *Store) ListRecent(ctx context.Context, limit int) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+`
		FROM tasks WHERE status IN (?, ?, ?) ORDER BY due_at DESC LIMIT ?`,
		StatusFired, StatusDone, StatusCancelled, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent: %w", err)
	}
	defer rows.Close()
	return collect(rows)
}

func collect(rows *sql.Rows) ([]*Task, error) {
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DueBefore returns pending tasks whose due time has passed. This is the
// catch-up sweep input: the ticker, not the heap, decides what expired.
//
// Tasks still awaiting confirmation are excluded. A below-threshold parse is
// not an obligation until the user accepts it, so it must never fire (US-003).
func (s *Store) DueBefore(ctx context.Context, now time.Time) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+`
		FROM tasks WHERE status = ? AND needs_confirm = 0
		AND due_at IS NOT NULL AND due_at < ?
		ORDER BY due_at ASC`, StatusPending, ts(now))
	if err != nil {
		return nil, fmt.Errorf("due before: %w", err)
	}
	defer rows.Close()
	return collect(rows)
}

// MarkFired transitions a pending task to fired exactly once. A second call
// returns affected=false, which is what makes firing idempotent.
func (s *Store) MarkFired(ctx context.Context, id int64, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET status = ?, notified_at = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		StatusFired, ts(at), ts(time.Now().UTC()), id, StatusPending)
	if err != nil {
		return false, fmt.Errorf("mark fired %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkMissed transitions a pending task to missed. Returns false when the task
// was already handled, which keeps the catch-up sweep safe to re-run.
func (s *Store) MarkMissed(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		StatusMissed, ts(time.Now().UTC()), id, StatusPending)
	if err != nil {
		return false, fmt.Errorf("mark missed %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpdateFields is the mutable subset of a task.
type UpdateFields struct {
	Label        *string
	DueAt        *time.Time
	DurationMS   *int64
	NeedsConfirm *bool
	// Corrected marks this edit as the user fixing what the parser produced.
	// Confirming or snoozing is not a correction: counting those would teach
	// the calibration log that every accepted parse was wrong.
	Corrected bool
}

// Update applies an edit. When f.Corrected is set, the matching calibration
// log row is flagged: that correction is the only ground truth available for
// tuning confidence thresholds.
func (s *Store) Update(ctx context.Context, id int64, f UpdateFields) error {
	sets := []string{"updated_at = ?"}
	args := []any{ts(time.Now().UTC())}
	if f.Label != nil {
		sets = append(sets, "label = ?")
		args = append(args, *f.Label)
	}
	if f.DueAt != nil {
		sets = append(sets, "due_at = ?")
		args = append(args, ts(*f.DueAt))
	}
	if f.DurationMS != nil {
		sets = append(sets, "duration_ms = ?")
		args = append(args, nullableInt(*f.DurationMS))
	}
	if f.NeedsConfirm != nil {
		sets = append(sets, "needs_confirm = ?")
		args = append(args, boolInt(*f.NeedsConfirm))
	}
	args = append(args, id)

	if _, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return fmt.Errorf("update task %d: %w", id, err)
	}
	if !f.Corrected {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE parse_log SET corrected_by_user = 1
		WHERE id = (
			SELECT id FROM parse_log WHERE raw_input = (
				SELECT raw_input FROM tasks WHERE id = ?
			) ORDER BY id DESC LIMIT 1
		)`, id)
	if err != nil {
		// Calibration bookkeeping must never block a user edit.
		return nil
	}
	return nil
}

// SetStatus moves a task out of pending into a terminal state.
func (s *Store) SetStatus(ctx context.Context, id int64, status string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?`,
		status, ts(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("set status %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a task outright.
func (s *Store) Delete(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete task %d: %w", id, err)
	}
	return nil
}

// LogParse appends to the calibration log.
func (s *Store) LogParse(ctx context.Context, rawInput, tier string, confidence float64, rationale string, accepted bool) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO parse_log (raw_input, chosen_tier, confidence, rationale, accepted, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		rawInput, tier, confidence, rationale, boolInt(accepted), ts(time.Now().UTC()))
	if err != nil {
		return 0, fmt.Errorf("log parse: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// ParseAccuracy summarises the calibration log: how often the user had to
// correct a parse, split by the confidence band that was claimed.
func (s *Store) ParseAccuracy(ctx context.Context) ([]ParseEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, raw_input, chosen_tier, confidence, rationale, accepted, corrected_by_user, created_at
		FROM parse_log ORDER BY id DESC LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("parse accuracy: %w", err)
	}
	defer rows.Close()

	var out []ParseEntry
	for rows.Next() {
		var (
			e         ParseEntry
			accepted  int
			corrected sql.NullInt64
			createdAt sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.RawInput, &e.ChosenTier, &e.Confidence,
			&e.Rationale, &accepted, &corrected, &createdAt); err != nil {
			return nil, fmt.Errorf("scan parse log: %w", err)
		}
		e.Accepted = accepted == 1
		e.CorrectedByUser = corrected.Valid && corrected.Int64 == 1
		e.CreatedAt = parseTS(createdAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Healthy reports whether the database answers.
func (s *Store) Healthy(ctx context.Context) error {
	return s.db.PingContext(ctx)
}
