package store

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // Pure-Go SQLite driver ("sqlite").

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/recipe"
)

//go:embed migrations/*.sql
var sqliteMigrationsFS embed.FS

// DefaultSQLiteFilename is the per-run SQLite database filename.
const DefaultSQLiteFilename = "store.db"

// SQLiteStore is a persistent local SQLite implementation of Store.
type SQLiteStore struct {
	clock clock.Clock
	db    *sql.DB
	path  string
	mu    sync.RWMutex
}

// NewSQLiteStore opens a SQLite-backed Store at path and applies migrations.
func NewSQLiteStore(path string, clk clock.Clock) (*SQLiteStore, error) {
	return OpenSQLite(path, clk)
}

// OpenSQLite opens a SQLite-backed Store at path and applies migrations.
func OpenSQLite(path string, clk clock.Clock) (*SQLiteStore, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: database path is required", ErrInvalidArgument)
	}
	if clk == nil {
		clk = clock.RealClock{}
	}
	if trimmed != ":memory:" && !strings.HasPrefix(trimmed, "file:") {
		dir := filepath.Dir(trimmed)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create sqlite directory %q: %w", dir, err)
			}
		}
	}

	db, err := sql.Open("sqlite", trimmed)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", trimmed, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &SQLiteStore{
		clock: clk,
		db:    db,
		path:  trimmed,
	}
	if err := s.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// RunDBPath returns the canonical database file path for runID under baseDir.
// If baseDir is empty, ~/.swarm is used.
func RunDBPath(baseDir, runID string) (string, error) {
	cleanRunID := strings.TrimSpace(runID)
	if cleanRunID == "" || cleanRunID == "." || cleanRunID == ".." || strings.ContainsAny(cleanRunID, `/\`) {
		return "", fmt.Errorf("%w: invalid runID %q", ErrInvalidArgument, runID)
	}
	if strings.TrimSpace(baseDir) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		baseDir = filepath.Join(home, ".swarm")
	}
	return filepath.Join(baseDir, cleanRunID, DefaultSQLiteFilename), nil
}

// OpenRunSQLite opens the per-run SQLite database under baseDir/<runID>/store.db.
func OpenRunSQLite(baseDir, runID string, clk clock.Clock) (*SQLiteStore, error) {
	dbPath, err := RunDBPath(baseDir, runID)
	if err != nil {
		return nil, err
	}
	return OpenSQLite(dbPath, clk)
}

func (s *SQLiteStore) init(ctx context.Context) error {
	pragmas := []string{
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA foreign_keys = ON;",
	}
	for _, p := range pragmas {
		if _, err := s.db.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("exec %q: %w", p, err)
		}
	}
	if err := s.applyMigrations(ctx); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // Seed and marker commit together, including across concurrent openers.
	result, err := tx.ExecContext(ctx, `INSERT INTO memory_seed_state(id) VALUES(1) ON CONFLICT(id) DO NOTHING`)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted != 0 {
		starter := StarterMemories()
		for i := range starter {
			v := &starter[i]
			_, err := tx.ExecContext(ctx, `INSERT INTO memories(id,name,description,type,scope,run_id,source,body,proposed_by,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, v.ID, v.Name, v.Description, v.Type, v.Scope, v.RunID, v.Source, v.Body, v.ProposedBy, formatTime(s.now()), formatTime(s.now()))
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *SQLiteStore) applyMigrations(ctx context.Context) error {
	const createMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL
);`
	if _, err := s.db.ExecContext(ctx, createMigrationsTable); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	entries, err := fs.ReadDir(sqliteMigrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int {
		return cmp.Compare(a.Name(), b.Name())
	})

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version := entry.Name()
		var existing string
		err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_migrations WHERE version = ?`, version).Scan(&existing)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check migration %q: %w", version, err)
		}

		sqlBytes, err := fs.ReadFile(sqliteMigrationsFS, "migrations/"+version)
		if err != nil {
			return fmt.Errorf("read migration %q: %w", version, err)
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %q: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %q: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, formatTime(s.now())); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %q: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %q: %w", version, err)
		}
	}
	return nil
}

// AppliedMigrations returns the ordered list of migration versions applied to this database.
func (s *SQLiteStore) AppliedMigrations(ctx context.Context) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Path returns the filesystem path or DSN used to open the SQLite database.
func (s *SQLiteStore) Path() string {
	return s.path
}

func (s *SQLiteStore) now() time.Time {
	return s.clock.Now().UTC()
}

func (s *SQLiteStore) Runs() RunStore { return (*sqliteRunStore)(s) }

func (s *SQLiteStore) Backlog() BacklogStore { return (*sqliteBacklogStore)(s) }

func (s *SQLiteStore) Lanes() LaneStore { return (*sqliteLaneStore)(s) }

func (s *SQLiteStore) Directives() DirectiveStore { return (*sqliteDirectiveStore)(s) }

func (s *SQLiteStore) Decisions() DecisionStore { return (*sqliteDecisionStore)(s) }

func (s *SQLiteStore) Events() EventStore { return (*sqliteEventStore)(s) }

func (s *SQLiteStore) Threads() ThreadStore { return (*sqliteThreadStore)(s) }

func (s *SQLiteStore) Recipes() RecipeStore       { return (*sqliteRecipeStore)(s) }
func (s *SQLiteStore) Memories() MemoryEntryStore { return (*sqliteMemoryStore)(s) }
func (s *SQLiteStore) Notes() NoteStore           { return (*sqliteNoteStore)(s) }

func (s *SQLiteStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

var _ Store = (*SQLiteStore)(nil)

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

type sqliteMemoryStore SQLiteStore

func (m *sqliteMemoryStore) s() *SQLiteStore { return (*SQLiteStore)(m) }
func (m *sqliteMemoryStore) Put(ctx context.Context, v MemoryEntry) (MemoryEntry, error) {
	s := m.s()
	if err := v.validate(); err != nil {
		return MemoryEntry{}, err
	}
	now := s.now()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = now
	}
	v.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO memories(id,name,description,type,scope,run_id,source,body,proposed_by,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,description=excluded.description,type=excluded.type,scope=excluded.scope,run_id=excluded.run_id,source=excluded.source,body=excluded.body,proposed_by=excluded.proposed_by,updated_at=excluded.updated_at`, v.ID, v.Name, v.Description, v.Type, v.Scope, v.RunID, v.Source, v.Body, v.ProposedBy, formatTime(v.CreatedAt), formatTime(v.UpdatedAt))
	return v, err
}
func (m *sqliteMemoryStore) Get(ctx context.Context, scope, id string) (*MemoryEntry, error) {
	s := m.s()
	var v MemoryEntry
	var ca, ua string
	err := s.db.QueryRowContext(ctx, `SELECT id,name,description,type,scope,run_id,source,body,proposed_by,created_at,updated_at FROM memories WHERE id=? AND (?='' OR scope=?)`, id, scope, scope).Scan(&v.ID, &v.Name, &v.Description, &v.Type, &v.Scope, &v.RunID, &v.Source, &v.Body, &v.ProposedBy, &ca, &ua)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: memory %q", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	v.CreatedAt, _ = time.Parse(time.RFC3339Nano, ca)
	v.UpdatedAt, _ = time.Parse(time.RFC3339Nano, ua)
	return &v, nil
}
func (m *sqliteMemoryStore) List(ctx context.Context, f MemoryFilter) ([]MemoryEntry, error) {
	s := m.s()
	q := `SELECT id,name,description,type,scope,run_id,source,body,proposed_by,created_at,updated_at FROM memories WHERE (?='' OR run_id='' OR run_id=?) AND (?='' OR scope=?) AND (?='' OR type=?) ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q, f.RunID, f.RunID, f.Scope, f.Scope, f.Type, f.Type)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MemoryEntry{}
	for rows.Next() {
		var v MemoryEntry
		var ca, ua string
		if err := rows.Scan(&v.ID, &v.Name, &v.Description, &v.Type, &v.Scope, &v.RunID, &v.Source, &v.Body, &v.ProposedBy, &ca, &ua); err != nil {
			return nil, err
		}
		v.CreatedAt, _ = time.Parse(time.RFC3339Nano, ca)
		v.UpdatedAt, _ = time.Parse(time.RFC3339Nano, ua)
		out = append(out, v)
	}
	return out, rows.Err()
}
func (m *sqliteMemoryStore) Delete(ctx context.Context, scope, id string) error {
	r, err := m.s().db.ExecContext(ctx, `DELETE FROM memories WHERE id=? AND (?='' OR scope=?)`, id, scope, scope)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: memory %q", ErrNotFound, id)
	}
	return nil
}

type sqliteNoteStore SQLiteStore

func (n *sqliteNoteStore) s() *SQLiteStore { return (*SQLiteStore)(n) }
func (n *sqliteNoteStore) Append(ctx context.Context, v RunNote) (RunNote, error) {
	s := n.s()
	if strings.TrimSpace(v.RunID) == "" || strings.TrimSpace(v.Text) == "" {
		return RunNote{}, fmt.Errorf("%w: note run_id and text required", ErrInvalidArgument)
	}
	if v.ID == "" {
		v.ID = "note-" + rand.Text()
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = s.now()
	}
	err := execInsertNew(ctx, s.db, fmt.Errorf("%w: note %q", ErrAlreadyExists, v.ID), `INSERT INTO run_notes(id,run_id,task_id,phase,text,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, v.ID, v.RunID, v.TaskID, v.Phase, v.Text, formatTime(v.CreatedAt))
	if err != nil {
		return RunNote{}, err
	}
	return v, nil
}
func (n *sqliteNoteStore) List(ctx context.Context, f NoteFilter) ([]RunNote, error) {
	rows, err := n.s().db.QueryContext(ctx, `SELECT id,run_id,task_id,phase,text,created_at FROM run_notes WHERE run_id=? AND (?='' OR task_id=?) ORDER BY rowid`, f.RunID, f.TaskID, f.TaskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunNote{}
	for rows.Next() {
		var v RunNote
		var ts string
		if err := rows.Scan(&v.ID, &v.RunID, &v.TaskID, &v.Phase, &v.Text, &ts); err != nil {
			return nil, err
		}
		v.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, v)
	}
	return out, rows.Err()
}

func parseTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func formatTimePtr(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return formatTime(*t)
}

func parseTimePtr(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	t, err := parseTime(raw)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func marshalJSONOrDefault(v any, def string) (string, error) {
	if v == nil {
		return def, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	s := string(b)
	if s == "null" {
		return def, nil
	}
	return s, nil
}

func unmarshalJSON(raw string, dest any) error {
	if raw == "" || raw == "[]" || raw == "{}" || raw == "null" {
		return nil
	}
	return json.Unmarshal([]byte(raw), dest)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

type rowScanner interface {
	Scan(dest ...any) error
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type execContext interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func execRequireRow(ctx context.Context, exec execContext, notFoundErr error, query string, args ...any) error {
	res, err := exec.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return notFoundErr
	}
	return nil
}

func execInsertNew(ctx context.Context, exec execContext, alreadyExistsErr error, query string, args ...any) error {
	res, err := exec.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return alreadyExistsErr
	}
	return nil
}

type sqliteRunStore SQLiteStore

func (r *sqliteRunStore) s() *SQLiteStore { return (*SQLiteStore)(r) }

const selectRunColumns = `
SELECT id, objective, plan_ref, default_recipe, created_at, updated_at,
       lease_holder_id, lease_acquired_at, lease_heartbeat_at, lease_expires_at
FROM runs`

func getRun(ctx context.Context, q queryRower, runID string) (*Run, error) {
	run, err := scanRun(q.QueryRowContext(ctx, selectRunColumns+` WHERE id = ?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: run %q", ErrNotFound, runID)
	}
	if err != nil {
		return nil, err
	}
	return run, nil
}

func (r *sqliteRunStore) Create(ctx context.Context, run Run) error {
	if strings.TrimSpace(run.ID) == "" {
		return fmt.Errorf("%w: run id is required", ErrInvalidArgument)
	}
	s := r.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	cp := run.Clone()
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = cp.CreatedAt
	}
	if cp.DefaultRecipe == "" {
		cp.DefaultRecipe = recipe.DefaultName
	}

	var holderID, acquiredAt, heartbeatAt, expiresAt string
	if cp.Lease != nil {
		holderID = cp.Lease.HolderID
		acquiredAt = formatTime(cp.Lease.AcquiredAt)
		heartbeatAt = formatTime(cp.Lease.HeartbeatAt)
		expiresAt = formatTime(cp.Lease.ExpiresAt)
	}

	return execInsertNew(ctx, s.db, fmt.Errorf("%w: run %q", ErrAlreadyExists, run.ID), `
INSERT INTO runs (
    id, objective, plan_ref, default_recipe, created_at, updated_at,
    lease_holder_id, lease_acquired_at, lease_heartbeat_at, lease_expires_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO NOTHING`,
		cp.ID, cp.Objective, cp.PlanRef, cp.DefaultRecipe,
		formatTime(cp.CreatedAt), formatTime(cp.UpdatedAt),
		holderID, acquiredAt, heartbeatAt, expiresAt,
	)
}

func scanRun(scanner rowScanner) (*Run, error) {
	var (
		run                                             Run
		createdStr, updatedStr                          string
		holderID, acquiredStr, heartbeatStr, expiresStr string
	)
	err := scanner.Scan(
		&run.ID, &run.Objective, &run.PlanRef, &run.DefaultRecipe,
		&createdStr, &updatedStr,
		&holderID, &acquiredStr, &heartbeatStr, &expiresStr,
	)
	if err != nil {
		return nil, err
	}
	if run.CreatedAt, err = parseTime(createdStr); err != nil {
		return nil, err
	}
	if run.UpdatedAt, err = parseTime(updatedStr); err != nil {
		return nil, err
	}
	if holderID != "" {
		acq, err := parseTime(acquiredStr)
		if err != nil {
			return nil, err
		}
		hb, err := parseTime(heartbeatStr)
		if err != nil {
			return nil, err
		}
		exp, err := parseTime(expiresStr)
		if err != nil {
			return nil, err
		}
		run.Lease = &Lease{
			RunID:       run.ID,
			HolderID:    holderID,
			AcquiredAt:  acq,
			HeartbeatAt: hb,
			ExpiresAt:   exp,
		}
	}
	return &run, nil
}

func (r *sqliteRunStore) Get(ctx context.Context, runID string) (*Run, error) {
	s := r.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return getRun(ctx, s.db, runID)
}

func (r *sqliteRunStore) UpdateObjective(ctx context.Context, runID, objective, planRef string) error {
	s := r.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	return execRequireRow(ctx, s.db, fmt.Errorf("%w: run %q", ErrNotFound, runID),
		`UPDATE runs SET objective = ?, plan_ref = ?, updated_at = ? WHERE id = ?`,
		objective, planRef, formatTime(s.now()), runID,
	)
}

func (r *sqliteRunStore) AcquireLease(ctx context.Context, runID, holderID string, ttl time.Duration) (*Lease, error) {
	if strings.TrimSpace(holderID) == "" || ttl <= 0 {
		return nil, fmt.Errorf("%w: holderID and positive ttl are required", ErrInvalidArgument)
	}
	s := r.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	run, err := getRun(ctx, tx, runID)
	if err != nil {
		return nil, err
	}

	now := s.now()
	if run.Lease != nil && !run.Lease.Expired(now) && run.Lease.HolderID != holderID {
		return nil, fmt.Errorf("%w: run %q held by %q until %s", ErrLeaseHeld, runID, run.Lease.HolderID, run.Lease.ExpiresAt.Format(time.RFC3339))
	}
	acquiredAt := now
	if run.Lease != nil && run.Lease.HolderID == holderID && !run.Lease.Expired(now) {
		acquiredAt = run.Lease.AcquiredAt
	}
	lease := Lease{
		RunID:       runID,
		HolderID:    holderID,
		AcquiredAt:  acquiredAt,
		HeartbeatAt: now,
		ExpiresAt:   now.Add(ttl),
	}
	_, err = tx.ExecContext(ctx, `
UPDATE runs
SET lease_holder_id = ?, lease_acquired_at = ?, lease_heartbeat_at = ?, lease_expires_at = ?, updated_at = ?
WHERE id = ?`,
		lease.HolderID, formatTime(lease.AcquiredAt), formatTime(lease.HeartbeatAt), formatTime(lease.ExpiresAt), formatTime(now), runID,
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &lease, nil
}

func (r *sqliteRunStore) HeartbeatLease(ctx context.Context, runID, holderID string, ttl time.Duration) (*Lease, error) {
	if strings.TrimSpace(holderID) == "" || ttl <= 0 {
		return nil, fmt.Errorf("%w: holderID and positive ttl are required", ErrInvalidArgument)
	}
	s := r.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	run, err := getRun(ctx, tx, runID)
	if err != nil {
		return nil, err
	}

	now := s.now()
	if run.Lease == nil || run.Lease.HolderID != holderID || run.Lease.Expired(now) {
		return nil, fmt.Errorf("%w: run %q by holder %q", ErrLeaseNotHeld, runID, holderID)
	}
	run.Lease.HeartbeatAt = now
	run.Lease.ExpiresAt = now.Add(ttl)

	_, err = tx.ExecContext(ctx, `
UPDATE runs
SET lease_heartbeat_at = ?, lease_expires_at = ?, updated_at = ?
WHERE id = ?`,
		formatTime(run.Lease.HeartbeatAt), formatTime(run.Lease.ExpiresAt), formatTime(now), runID,
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return run.Lease, nil
}

func (r *sqliteRunStore) ReleaseLease(ctx context.Context, runID, holderID string) error {
	s := r.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	run, err := getRun(ctx, tx, runID)
	if err != nil {
		return err
	}
	if run.Lease == nil || run.Lease.HolderID != holderID {
		return fmt.Errorf("%w: run %q by holder %q", ErrLeaseNotHeld, runID, holderID)
	}

	_, err = tx.ExecContext(ctx, `
UPDATE runs
SET lease_holder_id = '', lease_acquired_at = '', lease_heartbeat_at = '', lease_expires_at = '', updated_at = ?
WHERE id = ?`,
		formatTime(s.now()), runID,
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

type sqliteBacklogStore SQLiteStore

func (b *sqliteBacklogStore) s() *SQLiteStore { return (*SQLiteStore)(b) }

func (b *sqliteBacklogStore) Add(ctx context.Context, item BacklogItem) error {
	if strings.TrimSpace(item.RunID) == "" || strings.TrimSpace(item.ID) == "" {
		return fmt.Errorf("%w: backlog item requires run_id and id", ErrInvalidArgument)
	}
	if strings.TrimSpace(item.Title) == "" {
		return fmt.Errorf("%w: backlog item %q requires title", ErrInvalidArgument, item.ID)
	}
	cp := item.Clone()
	if cp.Priority == "" {
		cp.Priority = PriorityP2
	}
	if !cp.Priority.Valid() {
		return fmt.Errorf("%w: invalid priority %q", ErrInvalidArgument, cp.Priority)
	}
	if cp.Status == "" {
		cp.Status = BacklogOpen
	}
	if !cp.Status.Valid() {
		return fmt.Errorf("%w: invalid status %q", ErrInvalidArgument, cp.Status)
	}

	s := b.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = cp.CreatedAt
	}
	if len(cp.History) == 0 {
		cp.History = []BacklogHistoryEntry{
			{At: cp.CreatedAt, Actor: "system", Change: "created"},
		}
	}

	dependsJSON, err := marshalJSONOrDefault(cp.DependsOn, "[]")
	if err != nil {
		return err
	}
	tagsJSON, err := marshalJSONOrDefault(cp.Tags, "[]")
	if err != nil {
		return err
	}
	historyJSON, err := marshalJSONOrDefault(cp.History, "[]")
	if err != nil {
		return err
	}

	return execInsertNew(ctx, s.db, fmt.Errorf("%w: backlog item %q in run %q", ErrAlreadyExists, cp.ID, cp.RunID), `
INSERT INTO backlog_items (
    run_id, id, title, description, acceptance, priority, priority_rank, sort_order,
    status, lane_id, recipe_name, depends_on_json, tags_json, history_json, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (run_id, id) DO NOTHING`,
		cp.RunID, cp.ID, cp.Title, cp.Description, cp.Acceptance,
		string(cp.Priority), cp.Priority.Rank(), cp.Order,
		string(cp.Status), cp.LaneID, cp.RecipeName,
		dependsJSON, tagsJSON, historyJSON,
		formatTime(cp.CreatedAt), formatTime(cp.UpdatedAt),
	)
}

func scanBacklogItem(scanner rowScanner) (*BacklogItem, error) {
	var (
		item                            BacklogItem
		priorityStr, statusStr          string
		dependsJSON, tagsJSON, histJSON string
		createdStr, updatedStr          string
		priorityRank                    int
	)
	err := scanner.Scan(
		&item.RunID, &item.ID, &item.Title, &item.Description, &item.Acceptance,
		&priorityStr, &priorityRank, &item.Order,
		&statusStr, &item.LaneID, &item.RecipeName,
		&dependsJSON, &tagsJSON, &histJSON,
		&createdStr, &updatedStr,
	)
	if err != nil {
		return nil, err
	}
	item.Priority = Priority(priorityStr)
	item.Status = BacklogStatus(statusStr)
	if item.CreatedAt, err = parseTime(createdStr); err != nil {
		return nil, err
	}
	if item.UpdatedAt, err = parseTime(updatedStr); err != nil {
		return nil, err
	}
	if err := unmarshalJSON(dependsJSON, &item.DependsOn); err != nil {
		return nil, err
	}
	if err := unmarshalJSON(tagsJSON, &item.Tags); err != nil {
		return nil, err
	}
	if err := unmarshalJSON(histJSON, &item.History); err != nil {
		return nil, err
	}
	for i := range item.History {
		item.History[i].At = item.History[i].At.UTC()
	}
	return &item, nil
}

const selectBacklogColumns = `
SELECT run_id, id, title, description, acceptance, priority, priority_rank, sort_order,
       status, lane_id, recipe_name, depends_on_json, tags_json, history_json, created_at, updated_at
FROM backlog_items`

func getBacklogItem(ctx context.Context, q queryRower, runID, itemID string) (*BacklogItem, error) {
	item, err := scanBacklogItem(q.QueryRowContext(ctx, selectBacklogColumns+` WHERE run_id = ? AND id = ?`, runID, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: backlog item %q in run %q", ErrNotFound, itemID, runID)
	}
	if err != nil {
		return nil, err
	}
	return item, nil
}

func (b *sqliteBacklogStore) Get(ctx context.Context, runID, itemID string) (*BacklogItem, error) {
	s := b.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return getBacklogItem(ctx, s.db, runID, itemID)
}

func (b *sqliteBacklogStore) Update(ctx context.Context, item BacklogItem, actor, reason string) error {
	if !item.Priority.Valid() || !item.Status.Valid() {
		return fmt.Errorf("%w: invalid priority or status", ErrInvalidArgument)
	}
	s := b.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := getBacklogItem(ctx, tx, item.RunID, item.ID)
	if err != nil {
		return err
	}

	now := s.now()
	cp := item.Clone()
	cp.CreatedAt = existing.CreatedAt
	cp.UpdatedAt = now
	cp.History = append(slices.Clone(existing.History), BacklogHistoryEntry{
		At:     now,
		Actor:  actor,
		Change: reason,
	})

	dependsJSON, err := marshalJSONOrDefault(cp.DependsOn, "[]")
	if err != nil {
		return err
	}
	tagsJSON, err := marshalJSONOrDefault(cp.Tags, "[]")
	if err != nil {
		return err
	}
	historyJSON, err := marshalJSONOrDefault(cp.History, "[]")
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
UPDATE backlog_items
SET title = ?, description = ?, acceptance = ?, priority = ?, priority_rank = ?, sort_order = ?,
    status = ?, lane_id = ?, recipe_name = ?, depends_on_json = ?, tags_json = ?, history_json = ?, updated_at = ?
WHERE run_id = ? AND id = ?`,
		cp.Title, cp.Description, cp.Acceptance, string(cp.Priority), cp.Priority.Rank(), cp.Order,
		string(cp.Status), cp.LaneID, cp.RecipeName, dependsJSON, tagsJSON, historyJSON, formatTime(cp.UpdatedAt),
		cp.RunID, cp.ID,
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (b *sqliteBacklogStore) Reorder(ctx context.Context, runID string, itemIDs []string, actor string) error {
	s := b.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	existingItems := make([]*BacklogItem, len(itemIDs))
	for i, id := range itemIDs {
		item, err := getBacklogItem(ctx, tx, runID, id)
		if err != nil {
			return err
		}
		existingItems[i] = item
	}

	now := s.now()
	nowStr := formatTime(now)
	for idx, item := range existingItems {
		item.Order = idx + 1
		item.History = append(item.History, BacklogHistoryEntry{
			At:     now,
			Actor:  actor,
			Change: fmt.Sprintf("reordered to position %d", idx+1),
		})
		historyJSON, err := marshalJSONOrDefault(item.History, "[]")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE backlog_items SET sort_order = ?, history_json = ?, updated_at = ? WHERE run_id = ? AND id = ?`,
			item.Order, historyJSON, nowStr, runID, item.ID,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (b *sqliteBacklogStore) Close(ctx context.Context, runID, itemID string, status BacklogStatus, actor, reason string) error {
	if !status.Terminal() {
		return fmt.Errorf("%w: Close requires terminal status (done or dropped), got %q", ErrInvalidArgument, status)
	}
	s := b.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	item, err := getBacklogItem(ctx, tx, runID, itemID)
	if err != nil {
		return err
	}

	now := s.now()
	item.History = append(item.History, BacklogHistoryEntry{
		At:     now,
		Actor:  actor,
		Change: fmt.Sprintf("closed (%s): %s", status, reason),
	})
	historyJSON, err := marshalJSONOrDefault(item.History, "[]")
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE backlog_items SET status = ?, history_json = ?, updated_at = ? WHERE run_id = ? AND id = ?`,
		string(status), historyJSON, formatTime(now), runID, itemID,
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (b *sqliteBacklogStore) List(ctx context.Context, filter BacklogFilter) ([]BacklogItem, error) {
	s := b.s()
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, selectBacklogColumns+` WHERE run_id = ?`, filter.RunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []BacklogItem
	for rows.Next() {
		item, err := scanBacklogItem(rows)
		if err != nil {
			return nil, err
		}
		if filter.LaneID != "" && item.LaneID != filter.LaneID {
			continue
		}
		if len(filter.Statuses) > 0 && !slices.Contains(filter.Statuses, item.Status) {
			continue
		}
		out = append(out, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	slices.SortFunc(out, func(a, b BacklogItem) int {
		return cmp.Or(
			cmp.Compare(a.Priority.Rank(), b.Priority.Rank()),
			cmp.Compare(a.Order, b.Order),
			a.CreatedAt.Compare(b.CreatedAt),
			cmp.Compare(a.ID, b.ID),
		)
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

type sqliteLaneStore SQLiteStore

func (l *sqliteLaneStore) s() *SQLiteStore { return (*SQLiteStore)(l) }

const selectLaneColumns = `
SELECT run_id, id, item_id, title, branch, base_branch, worktree, status, phase,
       round, attempt, recipe_name, recipe_version, session_id, terminal_id,
       sessions_json, terminals_json, pr, pr_head, approval_sha, merge_sha,
       checks, last_event_id, last_event_at, created_at, updated_at
FROM lanes`

func scanLane(scanner rowScanner) (*Lane, error) {
	var (
		ln                                   Lane
		statusStr                            string
		sessionsJSON, terminalsJSON          string
		lastEventStr, createdStr, updatedStr string
	)
	err := scanner.Scan(
		&ln.RunID, &ln.ID, &ln.ItemID, &ln.Title, &ln.Branch, &ln.BaseBranch, &ln.Worktree,
		&statusStr, &ln.Phase, &ln.Round, &ln.Attempt, &ln.RecipeName, &ln.RecipeVersion,
		&ln.SessionID, &ln.TerminalID, &sessionsJSON, &terminalsJSON,
		&ln.PR, &ln.PRHead, &ln.ApprovalSHA, &ln.MergeSHA, &ln.Checks,
		&ln.LastEventID, &lastEventStr, &createdStr, &updatedStr,
	)
	if err != nil {
		return nil, err
	}
	ln.Status = LaneStatus(statusStr)
	if ln.LastEventAt, err = parseTime(lastEventStr); err != nil {
		return nil, err
	}
	if ln.CreatedAt, err = parseTime(createdStr); err != nil {
		return nil, err
	}
	if ln.UpdatedAt, err = parseTime(updatedStr); err != nil {
		return nil, err
	}
	if err := unmarshalJSON(sessionsJSON, &ln.Sessions); err != nil {
		return nil, err
	}
	if err := unmarshalJSON(terminalsJSON, &ln.Terminals); err != nil {
		return nil, err
	}
	return &ln, nil
}

func (l *sqliteLaneStore) Upsert(ctx context.Context, ln Lane) error {
	if err := ln.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	s := l.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := s.now()
	cp := ln.Clone()

	var existingCreatedStr string
	err = tx.QueryRowContext(ctx, `SELECT created_at FROM lanes WHERE run_id = ? AND id = ?`, cp.RunID, cp.ID).Scan(&existingCreatedStr)
	switch {
	case err == nil:
		if cp.CreatedAt.IsZero() {
			if cp.CreatedAt, err = parseTime(existingCreatedStr); err != nil {
				return err
			}
		}
	case errors.Is(err, sql.ErrNoRows):
		if cp.CreatedAt.IsZero() {
			cp.CreatedAt = now
		}
	default:
		return err
	}
	cp.UpdatedAt = now

	sessionsJSON, err := marshalJSONOrDefault(cp.Sessions, "{}")
	if err != nil {
		return err
	}
	terminalsJSON, err := marshalJSONOrDefault(cp.Terminals, "{}")
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO lanes (
    run_id, id, item_id, title, branch, base_branch, worktree, status, phase,
    round, attempt, recipe_name, recipe_version, session_id, terminal_id,
    sessions_json, terminals_json, pr, pr_head, approval_sha, merge_sha,
    checks, last_event_id, last_event_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (run_id, id) DO UPDATE SET
    item_id = excluded.item_id,
    title = excluded.title,
    branch = excluded.branch,
    base_branch = excluded.base_branch,
    worktree = excluded.worktree,
    status = excluded.status,
    phase = excluded.phase,
    round = excluded.round,
    attempt = excluded.attempt,
    recipe_name = excluded.recipe_name,
    recipe_version = excluded.recipe_version,
    session_id = excluded.session_id,
    terminal_id = excluded.terminal_id,
    sessions_json = excluded.sessions_json,
    terminals_json = excluded.terminals_json,
    pr = excluded.pr,
    pr_head = excluded.pr_head,
    approval_sha = excluded.approval_sha,
    merge_sha = excluded.merge_sha,
    checks = excluded.checks,
    last_event_id = excluded.last_event_id,
    last_event_at = excluded.last_event_at,
    created_at = excluded.created_at,
    updated_at = excluded.updated_at`,
		cp.RunID, cp.ID, cp.ItemID, cp.Title, cp.Branch, cp.BaseBranch, cp.Worktree,
		string(cp.Status), cp.Phase, cp.Round, cp.Attempt, cp.RecipeName, cp.RecipeVersion,
		cp.SessionID, cp.TerminalID, sessionsJSON, terminalsJSON,
		cp.PR, cp.PRHead, cp.ApprovalSHA, cp.MergeSHA, cp.Checks,
		cp.LastEventID, formatTime(cp.LastEventAt), formatTime(cp.CreatedAt), formatTime(cp.UpdatedAt),
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (l *sqliteLaneStore) Get(ctx context.Context, runID, laneID string) (*Lane, error) {
	s := l.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	row := s.db.QueryRowContext(ctx, selectLaneColumns+` WHERE run_id = ? AND id = ?`, runID, laneID)
	ln, err := scanLane(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: lane %q in run %q", ErrNotFound, laneID, runID)
	}
	if err != nil {
		return nil, err
	}
	return ln, nil
}

func (l *sqliteLaneStore) List(ctx context.Context, filter LaneFilter) ([]Lane, error) {
	s := l.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx, selectLaneColumns+` WHERE run_id = ? ORDER BY id ASC`, filter.RunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Lane
	for rows.Next() {
		ln, err := scanLane(rows)
		if err != nil {
			return nil, err
		}
		if len(filter.Statuses) > 0 && !slices.Contains(filter.Statuses, ln.Status) {
			continue
		}
		out = append(out, *ln)
	}
	return out, rows.Err()
}

func (l *sqliteLaneStore) Delete(ctx context.Context, runID, laneID string) error {
	s := l.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	return execRequireRow(ctx, s.db, fmt.Errorf("%w: lane %q in run %q", ErrNotFound, laneID, runID),
		`DELETE FROM lanes WHERE run_id = ? AND id = ?`, runID, laneID,
	)
}

type sqliteDirectiveStore SQLiteStore

func (d *sqliteDirectiveStore) s() *SQLiteStore { return (*SQLiteStore)(d) }

const selectDirectiveColumns = `
SELECT run_id, id, kind, source, scope, text, ref, status, expires_at, created_at
FROM directives`

func scanDirective(scanner rowScanner) (*Directive, error) {
	var (
		dir                           Directive
		kindStr, sourceStr, statusStr string
		expiresStr, createdStr        string
	)
	err := scanner.Scan(
		&dir.RunID, &dir.ID, &kindStr, &sourceStr, &dir.Scope,
		&dir.Text, &dir.Ref, &statusStr, &expiresStr, &createdStr,
	)
	if err != nil {
		return nil, err
	}
	dir.Kind = DirectiveKind(kindStr)
	dir.Source = DirectiveSource(sourceStr)
	dir.Status = DirectiveStatus(statusStr)
	if dir.ExpiresAt, err = parseTimePtr(expiresStr); err != nil {
		return nil, err
	}
	if dir.CreatedAt, err = parseTime(createdStr); err != nil {
		return nil, err
	}
	return &dir, nil
}

func (d *sqliteDirectiveStore) Add(ctx context.Context, dir Directive) error {
	if strings.TrimSpace(dir.RunID) == "" || strings.TrimSpace(dir.ID) == "" {
		return fmt.Errorf("%w: directive requires run_id and id", ErrInvalidArgument)
	}
	if strings.TrimSpace(dir.Text) == "" && strings.TrimSpace(dir.Ref) == "" {
		return fmt.Errorf("%w: directive %q requires text or ref", ErrInvalidArgument, dir.ID)
	}
	s := d.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	cp := dir.Clone()
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = s.now()
	}
	if cp.Status == "" {
		cp.Status = DirectiveActive
	}
	if cp.Kind == "" {
		cp.Kind = DirectiveStanding
	}
	if cp.Source == "" {
		cp.Source = DirectiveSourceOwner
	}

	return execInsertNew(ctx, s.db, fmt.Errorf("%w: directive %q in run %q", ErrAlreadyExists, dir.ID, dir.RunID), `
INSERT INTO directives (run_id, id, kind, source, scope, text, ref, status, expires_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (run_id, id) DO NOTHING`,
		cp.RunID, cp.ID, string(cp.Kind), string(cp.Source), cp.Scope,
		cp.Text, cp.Ref, string(cp.Status), formatTimePtr(cp.ExpiresAt), formatTime(cp.CreatedAt),
	)
}

func (d *sqliteDirectiveStore) Get(ctx context.Context, runID, directiveID string) (*Directive, error) {
	s := d.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	row := s.db.QueryRowContext(ctx, selectDirectiveColumns+` WHERE run_id = ? AND id = ?`, runID, directiveID)
	dir, err := scanDirective(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: directive %q in run %q", ErrNotFound, directiveID, runID)
	}
	if err != nil {
		return nil, err
	}
	return dir, nil
}

func (d *sqliteDirectiveStore) UpdateStatus(ctx context.Context, runID, directiveID string, status DirectiveStatus) error {
	s := d.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	return execRequireRow(ctx, s.db, fmt.Errorf("%w: directive %q in run %q", ErrNotFound, directiveID, runID),
		`UPDATE directives SET status = ? WHERE run_id = ? AND id = ?`, string(status), runID, directiveID,
	)
}

func (d *sqliteDirectiveStore) List(ctx context.Context, filter DirectiveFilter) ([]Directive, error) {
	s := d.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.now()

	rows, err := s.db.QueryContext(ctx, selectDirectiveColumns+` WHERE run_id = ?`, filter.RunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Directive
	for rows.Next() {
		dir, err := scanDirective(rows)
		if err != nil {
			return nil, err
		}
		if filter.ActiveOnly && !dir.IsActive(now) {
			continue
		}
		if filter.Scope != "" && dir.Scope != filter.Scope {
			continue
		}
		if len(filter.Kinds) > 0 && !slices.Contains(filter.Kinds, dir.Kind) {
			continue
		}
		out = append(out, *dir)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b Directive) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

type sqliteDecisionStore SQLiteStore

func (d *sqliteDecisionStore) s() *SQLiteStore { return (*SQLiteStore)(d) }

func (d *sqliteDecisionStore) Append(ctx context.Context, dec Decision) (Decision, error) {
	if strings.TrimSpace(dec.RunID) == "" || strings.TrimSpace(dec.Tool) == "" {
		return Decision{}, fmt.Errorf("%w: decision requires run_id and tool", ErrInvalidArgument)
	}
	s := d.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Decision{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var nextSeq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM decisions WHERE run_id = ?`, dec.RunID).Scan(&nextSeq); err != nil {
		return Decision{}, err
	}

	cp := dec.Clone()
	cp.Seq = nextSeq
	if cp.ID == "" {
		cp.ID = fmt.Sprintf("dec-%d", nextSeq)
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = s.now()
	}
	if cp.Category == "" {
		cp.Category = DecisionAuto
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO decisions (
    run_id, seq, id, category, actor, thread_id, turn_id, lane_id, tool,
    args_json, reason, result, vetted, input_tokens, output_tokens, cost_usd, latency_ms, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cp.RunID, cp.Seq, cp.ID, string(cp.Category), cp.Actor, cp.ThreadID, cp.TurnID, cp.LaneID, cp.Tool,
		string(cp.Args), cp.Reason, cp.Result, boolToInt(cp.Vetted),
		cp.InputTokens, cp.OutputTokens, cp.CostUSD, cp.LatencyMs, formatTime(cp.CreatedAt),
	)
	if err != nil {
		return Decision{}, err
	}
	if err := tx.Commit(); err != nil {
		return Decision{}, err
	}
	return cp, nil
}

func (d *sqliteDecisionStore) List(ctx context.Context, filter DecisionFilter) ([]Decision, error) {
	s := d.s()
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
SELECT run_id, seq, id, category, actor, thread_id, turn_id, lane_id, tool,
       args_json, reason, result, vetted, input_tokens, output_tokens, cost_usd, latency_ms, created_at
FROM decisions
WHERE run_id = ? AND seq > ?
ORDER BY seq ASC`, filter.RunID, filter.SinceSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Decision
	for rows.Next() {
		var (
			dec                      Decision
			catStr, argsStr, timeStr string
			vettedInt                int
		)
		if err := rows.Scan(
			&dec.RunID, &dec.Seq, &dec.ID, &catStr, &dec.Actor, &dec.ThreadID, &dec.TurnID, &dec.LaneID, &dec.Tool,
			&argsStr, &dec.Reason, &dec.Result, &vettedInt, &dec.InputTokens, &dec.OutputTokens, &dec.CostUSD, &dec.LatencyMs, &timeStr,
		); err != nil {
			return nil, err
		}
		dec.Category = DecisionCategory(catStr)
		dec.Vetted = vettedInt != 0
		if argsStr != "" {
			dec.Args = json.RawMessage(argsStr)
		}
		if dec.CreatedAt, err = parseTime(timeStr); err != nil {
			return nil, err
		}

		if filter.SinceTime != nil && dec.CreatedAt.Before(*filter.SinceTime) {
			continue
		}
		if filter.LaneID != "" && dec.LaneID != filter.LaneID {
			continue
		}
		if filter.Actor != "" && dec.Actor != filter.Actor {
			continue
		}
		if filter.Category != "" && dec.Category != filter.Category {
			continue
		}
		out = append(out, dec)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, rows.Err()
}

type sqliteEventStore SQLiteStore

func (e *sqliteEventStore) s() *SQLiteStore { return (*SQLiteStore)(e) }

func (e *sqliteEventStore) Append(ctx context.Context, ev Event) (Event, error) {
	if strings.TrimSpace(ev.RunID) == "" || strings.TrimSpace(string(ev.Kind)) == "" {
		return Event{}, fmt.Errorf("%w: event requires run_id and kind", ErrInvalidArgument)
	}
	s := e.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var nextSeq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE run_id = ?`, ev.RunID).Scan(&nextSeq); err != nil {
		return Event{}, err
	}

	cp := ev.Clone()
	cp.Seq = nextSeq
	if cp.ID == "" {
		cp.ID = fmt.Sprintf("ev-%d", nextSeq)
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = s.now()
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO events (
    run_id, seq, id, kind, class, lane_id, source, summary, payload_json, urgent, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cp.RunID, cp.Seq, cp.ID, string(cp.Kind), string(cp.Class), cp.LaneID,
		cp.Source, cp.Summary, string(cp.Payload), boolToInt(cp.Urgent), formatTime(cp.CreatedAt),
	)
	if err != nil {
		return Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return Event{}, err
	}
	return cp, nil
}

func (e *sqliteEventStore) List(ctx context.Context, filter EventFilter) ([]Event, error) {
	s := e.s()
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
SELECT run_id, seq, id, kind, class, lane_id, source, summary, payload_json, urgent, created_at
FROM events
WHERE run_id = ? AND seq > ?
ORDER BY seq ASC`, filter.RunID, filter.AfterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var (
			ev                                Event
			kindStr, classStr, payload, cTime string
			urgentInt                         int
		)
		if err := rows.Scan(
			&ev.RunID, &ev.Seq, &ev.ID, &kindStr, &classStr, &ev.LaneID,
			&ev.Source, &ev.Summary, &payload, &urgentInt, &cTime,
		); err != nil {
			return nil, err
		}
		ev.Kind = EventKind(kindStr)
		ev.Class = EventClass(classStr)
		ev.Urgent = urgentInt != 0
		if payload != "" {
			ev.Payload = json.RawMessage(payload)
		}
		if ev.CreatedAt, err = parseTime(cTime); err != nil {
			return nil, err
		}

		if filter.LaneID != "" && ev.LaneID != filter.LaneID {
			continue
		}
		if len(filter.Kinds) > 0 && !slices.Contains(filter.Kinds, ev.Kind) {
			continue
		}
		out = append(out, ev)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, rows.Err()
}

func (e *sqliteEventStore) GetCursor(ctx context.Context, runID, consumer string) (EventCursor, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(consumer) == "" {
		return EventCursor{}, fmt.Errorf("%w: runID and consumer are required", ErrInvalidArgument)
	}
	s := e.s()
	s.mu.RLock()
	defer s.mu.RUnlock()

	var (
		cur        EventCursor
		updatedStr string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT run_id, consumer, seq, updated_at FROM event_cursors WHERE run_id = ? AND consumer = ?`,
		runID, consumer,
	).Scan(&cur.RunID, &cur.Consumer, &cur.Seq, &updatedStr)
	if errors.Is(err, sql.ErrNoRows) {
		return EventCursor{
			RunID:    runID,
			Consumer: consumer,
			Seq:      0,
		}, nil
	}
	if err != nil {
		return EventCursor{}, err
	}
	if cur.UpdatedAt, err = parseTime(updatedStr); err != nil {
		return EventCursor{}, err
	}
	return cur, nil
}

func (e *sqliteEventStore) SetCursor(ctx context.Context, runID, consumer string, seq int64) error {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(consumer) == "" || seq < 0 {
		return fmt.Errorf("%w: valid runID, consumer, and non-negative seq are required", ErrInvalidArgument)
	}
	s := e.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
INSERT INTO event_cursors (run_id, consumer, seq, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (run_id, consumer) DO UPDATE SET
    seq = MAX(event_cursors.seq, excluded.seq),
    updated_at = excluded.updated_at`,
		runID, consumer, seq, formatTime(s.now()),
	)
	return err
}

type sqliteThreadStore SQLiteStore

func (t *sqliteThreadStore) s() *SQLiteStore { return (*SQLiteStore)(t) }

const selectThreadColumns = `
SELECT id, run_id, role, goal, parent_thread_id, forked_at_turn_id, status,
       queued_steers_json, input_tokens, output_tokens, cost_usd, created_at, updated_at
FROM threads`

func scanThread(scanner rowScanner) (*Thread, error) {
	var (
		th                     Thread
		roleStr, statusStr     string
		queuedJSON             string
		createdStr, updatedStr string
	)
	err := scanner.Scan(
		&th.ID, &th.RunID, &roleStr, &th.Goal, &th.ParentThreadID, &th.ForkedAtTurnID,
		&statusStr, &queuedJSON, &th.InputTokens, &th.OutputTokens, &th.CostUSD,
		&createdStr, &updatedStr,
	)
	if err != nil {
		return nil, err
	}
	th.Role = ThreadRole(roleStr)
	th.Status = ThreadStatus(statusStr)
	if th.CreatedAt, err = parseTime(createdStr); err != nil {
		return nil, err
	}
	if th.UpdatedAt, err = parseTime(updatedStr); err != nil {
		return nil, err
	}
	if err := unmarshalJSON(queuedJSON, &th.QueuedSteers); err != nil {
		return nil, err
	}
	return &th, nil
}

func getThread(ctx context.Context, q queryRower, threadID string) (*Thread, error) {
	th, err := scanThread(q.QueryRowContext(ctx, selectThreadColumns+` WHERE id = ?`, threadID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: thread %q", ErrNotFound, threadID)
	}
	if err != nil {
		return nil, err
	}
	return th, nil
}

func requireThreadExists(ctx context.Context, q queryRower, threadID string) error {
	var exists string
	if err := q.QueryRowContext(ctx, `SELECT id FROM threads WHERE id = ?`, threadID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: thread %q", ErrNotFound, threadID)
	} else if err != nil {
		return err
	}
	return nil
}

const selectTurnColumns = `
SELECT thread_id, id, run_id, seq, status, input, error, steers_json,
       input_tokens, output_tokens, cost_usd, started_at, completed_at
FROM turns`

func scanTurn(scanner rowScanner) (*Turn, error) {
	var (
		tr                       Turn
		statusStr, steersJSON    string
		startedStr, completedStr string
	)
	err := scanner.Scan(
		&tr.ThreadID, &tr.ID, &tr.RunID, &tr.Seq, &statusStr, &tr.Input, &tr.Error, &steersJSON,
		&tr.InputTokens, &tr.OutputTokens, &tr.CostUSD, &startedStr, &completedStr,
	)
	if err != nil {
		return nil, err
	}
	tr.Status = TurnStatus(statusStr)
	if tr.StartedAt, err = parseTime(startedStr); err != nil {
		return nil, err
	}
	if tr.CompletedAt, err = parseTimePtr(completedStr); err != nil {
		return nil, err
	}
	if err := unmarshalJSON(steersJSON, &tr.Steers); err != nil {
		return nil, err
	}
	return &tr, nil
}

func getTurn(ctx context.Context, q queryRower, threadID, turnID string) (*Turn, error) {
	tr, err := scanTurn(q.QueryRowContext(ctx, selectTurnColumns+` WHERE thread_id = ? AND id = ?`, threadID, turnID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: turn %q in thread %q", ErrNotFound, turnID, threadID)
	}
	if err != nil {
		return nil, err
	}
	return tr, nil
}

func insertTurn(ctx context.Context, exec execContext, tr Turn) error {
	steersJSON, err := marshalJSONOrDefault(tr.Steers, "[]")
	if err != nil {
		return err
	}
	_, err = exec.ExecContext(ctx, `
INSERT INTO turns (
    thread_id, id, run_id, seq, status, input, error, steers_json,
    input_tokens, output_tokens, cost_usd, started_at, completed_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tr.ThreadID, tr.ID, tr.RunID, tr.Seq, string(tr.Status), tr.Input, tr.Error, steersJSON,
		tr.InputTokens, tr.OutputTokens, tr.CostUSD, formatTime(tr.StartedAt), formatTimePtr(tr.CompletedAt),
	)
	return err
}

const selectItemColumns = `
SELECT thread_id, seq, id, turn_id, kind, name, call_id, content, payload_json, is_error, created_at
FROM thread_items`

func scanItem(scanner rowScanner) (*Item, error) {
	var (
		it                            Item
		kindStr, payloadStr, cTimeStr string
		isErrInt                      int
	)
	err := scanner.Scan(
		&it.ThreadID, &it.Seq, &it.ID, &it.TurnID, &kindStr,
		&it.Name, &it.CallID, &it.Content, &payloadStr, &isErrInt, &cTimeStr,
	)
	if err != nil {
		return nil, err
	}
	it.Kind = ItemKind(kindStr)
	it.IsError = isErrInt != 0
	if payloadStr != "" {
		it.Payload = json.RawMessage(payloadStr)
	}
	if it.CreatedAt, err = parseTime(cTimeStr); err != nil {
		return nil, err
	}
	return &it, nil
}

func insertItem(ctx context.Context, exec execContext, it Item) error {
	_, err := exec.ExecContext(ctx, `
INSERT INTO thread_items (
    thread_id, seq, id, turn_id, kind, name, call_id, content, payload_json, is_error, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		it.ThreadID, it.Seq, it.ID, it.TurnID, string(it.Kind),
		it.Name, it.CallID, it.Content, string(it.Payload), boolToInt(it.IsError), formatTime(it.CreatedAt),
	)
	return err
}

func (t *sqliteThreadStore) StartThread(ctx context.Context, th Thread) (Thread, error) {
	if strings.TrimSpace(th.ID) == "" || strings.TrimSpace(th.RunID) == "" || strings.TrimSpace(string(th.Role)) == "" {
		return Thread{}, fmt.Errorf("%w: thread requires id, run_id, and role", ErrInvalidArgument)
	}
	s := t.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	cp := th.Clone()
	if cp.Status == "" {
		cp.Status = ThreadStatusIdle
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = cp.CreatedAt
	}
	queuedJSON, err := marshalJSONOrDefault(cp.QueuedSteers, "[]")
	if err != nil {
		return Thread{}, err
	}

	err = execInsertNew(ctx, s.db, fmt.Errorf("%w: thread %q", ErrAlreadyExists, th.ID), `
INSERT INTO threads (
    id, run_id, role, goal, parent_thread_id, forked_at_turn_id, status,
    queued_steers_json, input_tokens, output_tokens, cost_usd, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO NOTHING`,
		cp.ID, cp.RunID, string(cp.Role), cp.Goal, cp.ParentThreadID, cp.ForkedAtTurnID,
		string(cp.Status), queuedJSON, cp.InputTokens, cp.OutputTokens, cp.CostUSD,
		formatTime(cp.CreatedAt), formatTime(cp.UpdatedAt),
	)
	if err != nil {
		return Thread{}, err
	}
	return cp, nil
}

func (t *sqliteThreadStore) GetThread(ctx context.Context, threadID string) (*Thread, error) {
	s := t.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return getThread(ctx, s.db, threadID)
}

func (t *sqliteThreadStore) ListThreads(ctx context.Context, filter ThreadFilter) ([]Thread, error) {
	s := t.s()
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx,
		selectThreadColumns+` WHERE (? = '' OR run_id = ?) AND (? = '' OR role = ?)`,
		filter.RunID, filter.RunID, string(filter.Role), string(filter.Role),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Thread
	for rows.Next() {
		th, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		if len(filter.Statuses) > 0 && !slices.Contains(filter.Statuses, th.Status) {
			continue
		}
		out = append(out, *th)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b Thread) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

func (t *sqliteThreadStore) UpdateThreadStatus(ctx context.Context, threadID string, status ThreadStatus) error {
	s := t.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	return execRequireRow(ctx, s.db, fmt.Errorf("%w: thread %q", ErrNotFound, threadID),
		`UPDATE threads SET status = ?, updated_at = ? WHERE id = ?`, string(status), formatTime(s.now()), threadID,
	)
}

func (t *sqliteThreadStore) SetThreadGoal(ctx context.Context, threadID, goal string) error {
	s := t.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	return execRequireRow(ctx, s.db, fmt.Errorf("%w: thread %q", ErrNotFound, threadID),
		`UPDATE threads SET goal = ?, updated_at = ? WHERE id = ?`, goal, formatTime(s.now()), threadID,
	)
}

func (t *sqliteThreadStore) ForkThread(ctx context.Context, parentThreadID, newThreadID, upToTurnID string) (Thread, error) {
	if strings.TrimSpace(newThreadID) == "" {
		return Thread{}, fmt.Errorf("%w: newThreadID is required", ErrInvalidArgument)
	}
	s := t.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Thread{}, err
	}
	defer func() { _ = tx.Rollback() }()

	parent, err := getThread(ctx, tx, parentThreadID)
	if err != nil {
		return Thread{}, err
	}

	turnRows, err := tx.QueryContext(ctx, selectTurnColumns+` WHERE thread_id = ? ORDER BY seq ASC`, parentThreadID)
	if err != nil {
		return Thread{}, err
	}
	includedTurns := make(map[string]bool)
	var copiedTurns []Turn
	foundUpTo := upToTurnID == ""
	for turnRows.Next() {
		tr, err := scanTurn(turnRows)
		if err != nil {
			_ = turnRows.Close()
			return Thread{}, err
		}
		tr.ThreadID = newThreadID
		copiedTurns = append(copiedTurns, *tr)
		includedTurns[tr.ID] = true
		if upToTurnID != "" && tr.ID == upToTurnID {
			foundUpTo = true
			break
		}
	}
	if err := turnRows.Err(); err != nil {
		_ = turnRows.Close()
		return Thread{}, err
	}
	_ = turnRows.Close()

	if !foundUpTo {
		return Thread{}, fmt.Errorf("%w: turn %q in parent thread %q", ErrNotFound, upToTurnID, parentThreadID)
	}

	itemRows, err := tx.QueryContext(ctx, selectItemColumns+` WHERE thread_id = ? ORDER BY seq ASC`, parentThreadID)
	if err != nil {
		return Thread{}, err
	}
	var copiedItems []Item
	for itemRows.Next() {
		it, err := scanItem(itemRows)
		if err != nil {
			_ = itemRows.Close()
			return Thread{}, err
		}
		if !includedTurns[it.TurnID] {
			continue
		}
		it.ThreadID = newThreadID
		copiedItems = append(copiedItems, *it)
	}
	if err := itemRows.Err(); err != nil {
		_ = itemRows.Close()
		return Thread{}, err
	}
	_ = itemRows.Close()

	now := s.now()
	forked := Thread{
		ID:             newThreadID,
		RunID:          parent.RunID,
		Role:           parent.Role,
		Goal:           parent.Goal,
		ParentThreadID: parentThreadID,
		ForkedAtTurnID: upToTurnID,
		Status:         ThreadStatusIdle,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	err = execInsertNew(ctx, tx, fmt.Errorf("%w: thread %q", ErrAlreadyExists, newThreadID), `
INSERT INTO threads (
    id, run_id, role, goal, parent_thread_id, forked_at_turn_id, status,
    queued_steers_json, input_tokens, output_tokens, cost_usd, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, '[]', 0, 0, 0, ?, ?)
ON CONFLICT (id) DO NOTHING`,
		forked.ID, forked.RunID, string(forked.Role), forked.Goal,
		forked.ParentThreadID, forked.ForkedAtTurnID, string(forked.Status),
		formatTime(forked.CreatedAt), formatTime(forked.UpdatedAt),
	)
	if err != nil {
		return Thread{}, err
	}

	for i := range copiedTurns {
		if err := insertTurn(ctx, tx, copiedTurns[i]); err != nil {
			return Thread{}, err
		}
	}

	for i := range copiedItems {
		if err := insertItem(ctx, tx, copiedItems[i]); err != nil {
			return Thread{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return Thread{}, err
	}
	return forked, nil
}

func (t *sqliteThreadStore) StartTurn(ctx context.Context, turn Turn) (Turn, error) {
	if strings.TrimSpace(turn.ID) == "" || strings.TrimSpace(turn.ThreadID) == "" {
		return Turn{}, fmt.Errorf("%w: turn requires id and thread_id", ErrInvalidArgument)
	}
	s := t.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Turn{}, err
	}
	defer func() { _ = tx.Rollback() }()

	th, err := getThread(ctx, tx, turn.ThreadID)
	if err != nil {
		return Turn{}, err
	}

	var existingID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM turns WHERE thread_id = ? AND id = ?`, turn.ThreadID, turn.ID).Scan(&existingID)
	if err == nil {
		return Turn{}, fmt.Errorf("%w: turn %q in thread %q", ErrAlreadyExists, turn.ID, turn.ThreadID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Turn{}, err
	}

	var nextSeq int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM turns WHERE thread_id = ?`, turn.ThreadID).Scan(&nextSeq); err != nil {
		return Turn{}, err
	}

	now := s.now()
	cp := turn.Clone()
	cp.RunID = th.RunID
	cp.Seq = nextSeq
	cp.Status = TurnStatusRunning
	if cp.StartedAt.IsZero() {
		cp.StartedAt = now
	}
	if len(th.QueuedSteers) > 0 {
		cp.Steers = append(slices.Clone(th.QueuedSteers), cp.Steers...)
	}

	if err := insertTurn(ctx, tx, cp); err != nil {
		return Turn{}, err
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE threads SET status = ?, queued_steers_json = '[]', updated_at = ? WHERE id = ?`,
		string(ThreadStatusRunning), formatTime(now), turn.ThreadID,
	)
	if err != nil {
		return Turn{}, err
	}
	if err := tx.Commit(); err != nil {
		return Turn{}, err
	}
	return cp, nil
}

func (t *sqliteThreadStore) SteerTurn(ctx context.Context, threadID, turnID, message string) error {
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("%w: steer message is required", ErrInvalidArgument)
	}
	s := t.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	th, err := getThread(ctx, tx, threadID)
	if err != nil {
		return err
	}

	now := s.now()
	nowStr := formatTime(now)
	if turnID != "" {
		tr, err := scanTurn(tx.QueryRowContext(ctx, selectTurnColumns+` WHERE thread_id = ? AND id = ?`, threadID, turnID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if tr != nil && tr.Status == TurnStatusRunning {
			tr.Steers = append(tr.Steers, message)
			steersJSON, err := marshalJSONOrDefault(tr.Steers, "[]")
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE turns SET steers_json = ? WHERE thread_id = ? AND id = ?`, steersJSON, threadID, turnID); err != nil {
				return err
			}

			var nextItemSeq int
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM thread_items WHERE thread_id = ?`, threadID).Scan(&nextItemSeq); err != nil {
				return err
			}
			err = insertItem(ctx, tx, Item{
				ID:        fmt.Sprintf("item-%d", nextItemSeq),
				ThreadID:  threadID,
				TurnID:    turnID,
				Seq:       nextItemSeq,
				Kind:      ItemKindSteer,
				Content:   message,
				CreatedAt: now,
			})
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE threads SET updated_at = ? WHERE id = ?`, nowStr, threadID); err != nil {
				return err
			}
			return tx.Commit()
		}
	}

	th.QueuedSteers = append(th.QueuedSteers, message)
	queuedJSON, err := marshalJSONOrDefault(th.QueuedSteers, "[]")
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET queued_steers_json = ?, updated_at = ? WHERE id = ?`, queuedJSON, nowStr, threadID); err != nil {
		return err
	}
	return tx.Commit()
}

func (t *sqliteThreadStore) CompleteTurn(ctx context.Context, threadID, turnID string, status TurnStatus, usage model.Usage, errMsg string) error {
	if status == TurnStatusRunning || status == "" {
		return fmt.Errorf("%w: CompleteTurn requires a non-running status", ErrInvalidArgument)
	}
	s := t.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	th, err := getThread(ctx, tx, threadID)
	if err != nil {
		return err
	}

	tr, err := getTurn(ctx, tx, threadID, turnID)
	if err != nil {
		return err
	}

	now := s.now()
	nowStr := formatTime(now)
	_, err = tx.ExecContext(ctx, `
UPDATE turns
SET status = ?, completed_at = ?, error = ?, input_tokens = ?, output_tokens = ?, cost_usd = ?
WHERE thread_id = ? AND id = ?`,
		string(status), nowStr, errMsg,
		tr.InputTokens+usage.InputTokens, tr.OutputTokens+usage.OutputTokens, tr.CostUSD+usage.CostUSD,
		threadID, turnID,
	)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
UPDATE threads
SET status = ?, input_tokens = ?, output_tokens = ?, cost_usd = ?, updated_at = ?
WHERE id = ?`,
		string(ThreadStatusIdle),
		th.InputTokens+usage.InputTokens, th.OutputTokens+usage.OutputTokens, th.CostUSD+usage.CostUSD,
		nowStr, threadID,
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (t *sqliteThreadStore) GetTurn(ctx context.Context, threadID, turnID string) (*Turn, error) {
	s := t.s()
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err := requireThreadExists(ctx, s.db, threadID); err != nil {
		return nil, err
	}
	return getTurn(ctx, s.db, threadID, turnID)
}

func (t *sqliteThreadStore) ListTurns(ctx context.Context, threadID string) ([]Turn, error) {
	s := t.s()
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err := requireThreadExists(ctx, s.db, threadID); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, selectTurnColumns+` WHERE thread_id = ? ORDER BY seq ASC`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Turn
	for rows.Next() {
		tr, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *tr)
	}
	return out, rows.Err()
}

func (t *sqliteThreadStore) AppendItem(ctx context.Context, item Item) (Item, error) {
	if strings.TrimSpace(item.ThreadID) == "" || strings.TrimSpace(item.TurnID) == "" || strings.TrimSpace(string(item.Kind)) == "" {
		return Item{}, fmt.Errorf("%w: item requires thread_id, turn_id, and kind", ErrInvalidArgument)
	}
	s := t.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Item{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if err := requireThreadExists(ctx, tx, item.ThreadID); err != nil {
		return Item{}, err
	}
	var exists string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM turns WHERE thread_id = ? AND id = ?`, item.ThreadID, item.TurnID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return Item{}, fmt.Errorf("%w: turn %q in thread %q", ErrNotFound, item.TurnID, item.ThreadID)
	} else if err != nil {
		return Item{}, err
	}

	var nextSeq int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM thread_items WHERE thread_id = ?`, item.ThreadID).Scan(&nextSeq); err != nil {
		return Item{}, err
	}

	cp := item.Clone()
	cp.Seq = nextSeq
	if cp.ID == "" {
		cp.ID = fmt.Sprintf("item-%d", nextSeq)
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = s.now()
	}

	if err := insertItem(ctx, tx, cp); err != nil {
		return Item{}, err
	}
	if err := tx.Commit(); err != nil {
		return Item{}, err
	}
	return cp, nil
}

func (t *sqliteThreadStore) ListItems(ctx context.Context, threadID, turnID string, limit, offset int) ([]Item, error) {
	s := t.s()
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err := requireThreadExists(ctx, s.db, threadID); err != nil {
		return nil, err
	}

	limitArg := -1
	if limit > 0 {
		limitArg = limit
	}
	offsetArg := max(offset, 0)

	var (
		rows *sql.Rows
		err  error
	)
	if turnID != "" {
		rows, err = s.db.QueryContext(ctx, selectItemColumns+` WHERE thread_id = ? AND turn_id = ? ORDER BY seq ASC LIMIT ? OFFSET ?`, threadID, turnID, limitArg, offsetArg)
	} else {
		rows, err = s.db.QueryContext(ctx, selectItemColumns+` WHERE thread_id = ? ORDER BY seq ASC LIMIT ? OFFSET ?`, threadID, limitArg, offsetArg)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

type sqliteRecipeStore SQLiteStore

func (r *sqliteRecipeStore) s() *SQLiteStore { return (*SQLiteStore)(r) }

const selectRecipeColumns = `
SELECT run_id, name, version, source, base_recipe, base_version,
       phases_json, transitions_json, unmapped_json, merge_policy_json, limits_json, created_at
FROM recipes`

func scanRecipe(scanner rowScanner) (*Recipe, error) {
	var (
		rec                                               Recipe
		phasesJSON, transJSON, unmapJSON, mergeJSON, limJ string
		createdStr                                        string
	)
	err := scanner.Scan(
		&rec.RunID, &rec.Name, &rec.Version, &rec.Source, &rec.BaseRecipe, &rec.BaseVersion,
		&phasesJSON, &transJSON, &unmapJSON, &mergeJSON, &limJ, &createdStr,
	)
	if err != nil {
		return nil, err
	}
	if rec.CreatedAt, err = parseTime(createdStr); err != nil {
		return nil, err
	}
	if err := unmarshalJSON(phasesJSON, &rec.Phases); err != nil {
		return nil, err
	}
	if err := unmarshalJSON(transJSON, &rec.Transitions); err != nil {
		return nil, err
	}
	if err := unmarshalJSON(unmapJSON, &rec.Unmapped); err != nil {
		return nil, err
	}
	if err := unmarshalJSON(mergeJSON, &rec.MergePolicy); err != nil {
		return nil, err
	}
	if err := unmarshalJSON(limJ, &rec.Limits); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (r *sqliteRecipeStore) Save(ctx context.Context, rec Recipe) (Recipe, error) {
	if strings.TrimSpace(rec.RunID) == "" {
		return Recipe{}, fmt.Errorf("%w: recipe run_id is required", ErrInvalidArgument)
	}
	if err := rec.Validate(); err != nil {
		return Recipe{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	s := r.s()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Recipe{}, err
	}
	defer func() { _ = tx.Rollback() }()

	cp := rec.Clone()
	if cp.Version == 0 {
		var nextVer int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM recipes WHERE run_id = ? AND name = ?`, cp.RunID, cp.Name).Scan(&nextVer); err != nil {
			return Recipe{}, err
		}
		cp.Version = nextVer
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = s.now()
	}

	phasesJSON, err := marshalJSONOrDefault(cp.Phases, "[]")
	if err != nil {
		return Recipe{}, err
	}
	transJSON, err := marshalJSONOrDefault(cp.Transitions, "[]")
	if err != nil {
		return Recipe{}, err
	}
	unmapJSON, err := marshalJSONOrDefault(cp.Unmapped, "[]")
	if err != nil {
		return Recipe{}, err
	}
	mergeJSON, err := marshalJSONOrDefault(cp.MergePolicy, "{}")
	if err != nil {
		return Recipe{}, err
	}
	limitsJSON, err := marshalJSONOrDefault(cp.Limits, "{}")
	if err != nil {
		return Recipe{}, err
	}

	err = execInsertNew(ctx, tx, fmt.Errorf("%w: recipe %q v%d in run %q", ErrAlreadyExists, cp.Name, cp.Version, cp.RunID), `
INSERT INTO recipes (
    run_id, name, version, source, base_recipe, base_version,
    phases_json, transitions_json, unmapped_json, merge_policy_json, limits_json, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (run_id, name, version) DO NOTHING`,
		cp.RunID, cp.Name, cp.Version, cp.Source, cp.BaseRecipe, cp.BaseVersion,
		phasesJSON, transJSON, unmapJSON, mergeJSON, limitsJSON, formatTime(cp.CreatedAt),
	)
	if err != nil {
		return Recipe{}, err
	}
	if err := tx.Commit(); err != nil {
		return Recipe{}, err
	}
	return cp, nil
}

func (r *sqliteRecipeStore) Get(ctx context.Context, runID, name string, version int) (*Recipe, error) {
	s := r.s()
	s.mu.RLock()
	defer s.mu.RUnlock()

	var row *sql.Row
	if version <= 0 {
		row = s.db.QueryRowContext(ctx, selectRecipeColumns+` WHERE run_id = ? AND name = ? ORDER BY version DESC LIMIT 1`, runID, name)
	} else {
		row = s.db.QueryRowContext(ctx, selectRecipeColumns+` WHERE run_id = ? AND name = ? AND version = ?`, runID, name, version)
	}
	rec, err := scanRecipe(row)
	if errors.Is(err, sql.ErrNoRows) {
		if version <= 0 {
			return nil, fmt.Errorf("%w: recipe %q in run %q", ErrNotFound, name, runID)
		}
		return nil, fmt.Errorf("%w: recipe %q v%d in run %q", ErrNotFound, name, version, runID)
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func (r *sqliteRecipeStore) ListVersions(ctx context.Context, runID, name string) ([]Recipe, error) {
	s := r.s()
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, selectRecipeColumns+` WHERE run_id = ? AND name = ? ORDER BY version ASC`, runID, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Recipe
	for rows.Next() {
		rec, err := scanRecipe(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

func (r *sqliteRecipeStore) ListLatest(ctx context.Context, runID string) ([]Recipe, error) {
	s := r.s()
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
SELECT r.run_id, r.name, r.version, r.source, r.base_recipe, r.base_version,
       r.phases_json, r.transitions_json, r.unmapped_json, r.merge_policy_json, r.limits_json, r.created_at
FROM recipes r
INNER JOIN (
    SELECT run_id, name, MAX(version) AS max_version
    FROM recipes
    WHERE run_id = ?
    GROUP BY run_id, name
) latest ON r.run_id = latest.run_id AND r.name = latest.name AND r.version = latest.max_version
ORDER BY r.name ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Recipe
	for rows.Next() {
		rec, err := scanRecipe(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}
