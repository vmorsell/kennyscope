// Package store persists parsed Kenny log events in SQLite.
// Lives are materialised as a separate table keyed by Kenny's life_id.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Event struct {
	ID          int64
	ContainerID string
	LifeID      sql.NullInt64
	Stream      string // "stdout" | "stderr"
	At          time.Time
	IngestedAt  time.Time
	Level       string
	Msg         string
	Raw         string
	Parsed      bool
}

type Life struct {
	LifeID       int64
	ContainerID  string
	StartedAt    time.Time
	EndedAt      sql.NullTime
	BootRaw      string
	ShutdownRaw  sql.NullString
	EventCount   int64
}

func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS events (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			container_id TEXT NOT NULL,
			life_id      INTEGER,
			stream       TEXT NOT NULL,
			at           DATETIME NOT NULL,
			ingested_at  DATETIME NOT NULL,
			level        TEXT NOT NULL DEFAULT '',
			msg          TEXT NOT NULL DEFAULT '',
			raw          TEXT NOT NULL,
			parsed       INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS events_life_idx ON events(life_id)`,
		`CREATE INDEX IF NOT EXISTS events_container_at_idx ON events(container_id, at)`,
		`CREATE INDEX IF NOT EXISTS events_msg_idx ON events(msg)`,

		`CREATE TABLE IF NOT EXISTS lives (
			life_id       INTEGER PRIMARY KEY,
			container_id  TEXT NOT NULL,
			started_at    DATETIME NOT NULL,
			ended_at      DATETIME,
			boot_raw      TEXT NOT NULL,
			shutdown_raw  TEXT
		)`,

		`CREATE TABLE IF NOT EXISTS cursors (
			container_id TEXT PRIMARY KEY,
			last_at      DATETIME NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: %s: %w", stmt, err)
		}
	}
	return nil
}

// InsertEvent writes one event. life_id is optional; pass sql.NullInt64{}
// to leave blank (observer will backfill from the current life in memory).
func (s *Store) InsertEvent(ctx context.Context, e Event) (int64, error) {
	if e.IngestedAt.IsZero() {
		e.IngestedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO events (container_id, life_id, stream, at, ingested_at, level, msg, raw, parsed)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ContainerID, e.LifeID, e.Stream, e.At, e.IngestedAt, e.Level, e.Msg, e.Raw, boolToInt(e.Parsed))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpsertLifeStart records a new life, keyed by life_id. If the life
// already exists (observer restart re-ingesting), the boot metadata is
// replaced but ended_at is preserved.
func (s *Store) UpsertLifeStart(ctx context.Context, lifeID int64, containerID string, startedAt time.Time, bootRaw string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO lives (life_id, container_id, started_at, boot_raw)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(life_id) DO UPDATE SET
		   container_id = excluded.container_id,
		   started_at   = excluded.started_at,
		   boot_raw     = excluded.boot_raw`,
		lifeID, containerID, startedAt, bootRaw)
	return err
}

// MarkLifeEnded writes the shutdown timestamp + last-words raw line.
// If the life has already ended, newer data wins.
func (s *Store) MarkLifeEnded(ctx context.Context, lifeID int64, endedAt time.Time, shutdownRaw string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE lives SET ended_at = ?, shutdown_raw = ? WHERE life_id = ?`,
		endedAt, shutdownRaw, lifeID)
	return err
}

// CurrentLifeForContainer returns the highest life_id recorded for the
// given container with no ended_at, or 0 if none.
func (s *Store) CurrentLifeForContainer(ctx context.Context, containerID string) (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(life_id) FROM lives WHERE container_id = ? AND ended_at IS NULL`,
		containerID).Scan(&id)
	if err != nil {
		return 0, err
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

// LatestLifeForContainer returns the highest life_id recorded for the
// given container regardless of whether it has ended. Used when the
// observer restarts and needs to pick up where it left off.
func (s *Store) LatestLifeForContainer(ctx context.Context, containerID string) (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(life_id) FROM lives WHERE container_id = ?`,
		containerID).Scan(&id)
	if err != nil {
		return 0, err
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

func (s *Store) ListLives(ctx context.Context, limit int) ([]Life, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT l.life_id, l.container_id, l.started_at, l.ended_at, l.boot_raw, l.shutdown_raw,
		       (SELECT COUNT(*) FROM events e WHERE e.life_id = l.life_id) AS event_count
		FROM lives l
		ORDER BY l.life_id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Life
	for rows.Next() {
		var l Life
		if err := rows.Scan(&l.LifeID, &l.ContainerID, &l.StartedAt, &l.EndedAt, &l.BootRaw, &l.ShutdownRaw, &l.EventCount); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) GetLife(ctx context.Context, lifeID int64) (*Life, error) {
	var l Life
	err := s.db.QueryRowContext(ctx, `
		SELECT life_id, container_id, started_at, ended_at, boot_raw, shutdown_raw,
		       (SELECT COUNT(*) FROM events e WHERE e.life_id = lives.life_id) AS event_count
		FROM lives
		WHERE life_id = ?`, lifeID).Scan(
		&l.LifeID, &l.ContainerID, &l.StartedAt, &l.EndedAt, &l.BootRaw, &l.ShutdownRaw, &l.EventCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func (s *Store) EventsByLife(ctx context.Context, lifeID int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, container_id, life_id, stream, at, ingested_at, level, msg, raw, parsed
		 FROM events WHERE life_id = ? ORDER BY id ASC`, lifeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var parsed int
		if err := rows.Scan(&e.ID, &e.ContainerID, &e.LifeID, &e.Stream, &e.At, &e.IngestedAt,
			&e.Level, &e.Msg, &e.Raw, &parsed); err != nil {
			return nil, err
		}
		e.Parsed = parsed != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) SetCursor(ctx context.Context, containerID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cursors (container_id, last_at) VALUES (?, ?)
		 ON CONFLICT(container_id) DO UPDATE SET last_at = excluded.last_at`,
		containerID, at)
	return err
}

func (s *Store) GetCursor(ctx context.Context, containerID string) (time.Time, bool, error) {
	var t time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT last_at FROM cursors WHERE container_id = ?`, containerID).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
