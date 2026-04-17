package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "observer.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestInsertEventAndQuery(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	at := time.Date(2026, 4, 16, 23, 23, 15, 0, time.UTC)
	if err := s.UpsertLifeStart(ctx, 1, "abc123", at, `{"msg":"kenny.boot"}`); err != nil {
		t.Fatalf("UpsertLifeStart: %v", err)
	}
	if _, err := s.InsertEvent(ctx, Event{
		ContainerID: "abc123",
		LifeID:      sql.NullInt64{Int64: 1, Valid: true},
		Stream:      "stdout",
		At:          at.Add(time.Second),
		Level:       "INFO",
		Msg:         "claude.start",
		Raw:         `{"msg":"claude.start"}`,
		Parsed:      true,
	}); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	lives, err := s.ListLives(ctx, 10)
	if err != nil || len(lives) != 1 {
		t.Fatalf("ListLives: %v, len=%d", err, len(lives))
	}
	if lives[0].LifeID != 1 || lives[0].EventCount != 1 {
		t.Fatalf("unexpected life: %+v", lives[0])
	}

	events, err := s.EventsByLife(ctx, 1)
	if err != nil || len(events) != 1 {
		t.Fatalf("EventsByLife: %v, len=%d", err, len(events))
	}
	if events[0].Msg != "claude.start" {
		t.Fatalf("event msg = %q", events[0].Msg)
	}
}

func TestLifeEndingUpdatesRow(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	at := time.Now().UTC()
	if err := s.UpsertLifeStart(ctx, 7, "c", at, `{"msg":"kenny.boot"}`); err != nil {
		t.Fatalf("UpsertLifeStart: %v", err)
	}
	end := at.Add(time.Hour)
	if err := s.MarkLifeEnded(ctx, 7, end, `{"msg":"kenny.shutdown"}`); err != nil {
		t.Fatalf("MarkLifeEnded: %v", err)
	}
	l, err := s.GetLife(ctx, 7)
	if err != nil {
		t.Fatalf("GetLife: %v", err)
	}
	if !l.EndedAt.Valid || !l.EndedAt.Time.Equal(end) {
		t.Fatalf("EndedAt not set correctly: %+v", l.EndedAt)
	}
	if !l.ShutdownRaw.Valid {
		t.Fatalf("ShutdownRaw not set")
	}
}

func TestCurrentLifeForContainer(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	now := time.Now().UTC()
	_ = s.UpsertLifeStart(ctx, 1, "c", now, "boot1")
	_ = s.UpsertLifeStart(ctx, 2, "c", now.Add(time.Minute), "boot2")
	_ = s.MarkLifeEnded(ctx, 1, now.Add(time.Second), "end1")

	id, err := s.CurrentLifeForContainer(ctx, "c")
	if err != nil || id != 2 {
		t.Fatalf("Current = %d, err=%v, want 2", id, err)
	}

	latest, err := s.LatestLifeForContainer(ctx, "c")
	if err != nil || latest != 2 {
		t.Fatalf("Latest = %d, err=%v, want 2", latest, err)
	}
}

func TestCursor(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ts := time.Now().UTC()
	if _, ok, err := s.GetCursor(ctx, "c"); err != nil || ok {
		t.Fatalf("no cursor: ok=%v err=%v", ok, err)
	}
	if err := s.SetCursor(ctx, "c", ts); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}
	got, ok, err := s.GetCursor(ctx, "c")
	if err != nil || !ok || !got.Equal(ts) {
		t.Fatalf("GetCursor: ok=%v err=%v got=%v want=%v", ok, err, got, ts)
	}
}
