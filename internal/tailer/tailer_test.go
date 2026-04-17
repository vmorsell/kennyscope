package tailer

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmorsell/kennyscope/internal/docker"
	"github.com/vmorsell/kennyscope/internal/store"
)

func newTestTailer(t *testing.T) (*Tailer, *store.Store) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "observer.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(Config{Store: s, Logger: logger}), s
}

func TestHandleBootEventStartsLife(t *testing.T) {
	ctx := context.Background()
	tl, s := newTestTailer(t)

	at := time.Date(2026, 4, 16, 23, 23, 15, 0, time.UTC)
	tl.handle(ctx, "cid", docker.LogMessage{
		Stream: docker.Stdout,
		At:     at,
		Line:   `{"time":"2026-04-16T23:23:15Z","level":"INFO","msg":"kenny.boot","life_id":1}`,
	})

	lives, err := s.ListLives(ctx, 10)
	if err != nil {
		t.Fatalf("ListLives: %v", err)
	}
	if len(lives) != 1 || lives[0].LifeID != 1 {
		t.Fatalf("unexpected lives: %+v", lives)
	}
	if !lives[0].StartedAt.Equal(at) {
		t.Fatalf("StartedAt = %v, want %v", lives[0].StartedAt, at)
	}
}

func TestHandleBackfillsLifeIDFromCurrent(t *testing.T) {
	ctx := context.Background()
	tl, s := newTestTailer(t)

	at := time.Date(2026, 4, 16, 23, 0, 0, 0, time.UTC)
	tl.handle(ctx, "cid", docker.LogMessage{
		Stream: docker.Stdout, At: at,
		Line: `{"level":"INFO","msg":"kenny.boot","life_id":42}`,
	})
	tl.handle(ctx, "cid", docker.LogMessage{
		Stream: docker.Stdout, At: at.Add(time.Second),
		Line: `{"level":"INFO","msg":"claude.start"}`,
	})

	events, err := s.EventsByLife(ctx, 42)
	if err != nil {
		t.Fatalf("EventsByLife: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (boot + claude.start backfilled)", len(events))
	}
	if events[1].Msg != "claude.start" || !events[1].LifeID.Valid || events[1].LifeID.Int64 != 42 {
		t.Fatalf("claude.start event: %+v", events[1])
	}
}

func TestHandleShutdownEndsLife(t *testing.T) {
	ctx := context.Background()
	tl, s := newTestTailer(t)

	at := time.Date(2026, 4, 16, 23, 0, 0, 0, time.UTC)
	tl.handle(ctx, "cid", docker.LogMessage{
		Stream: docker.Stdout, At: at,
		Line: `{"msg":"kenny.boot","life_id":1}`,
	})
	tl.handle(ctx, "cid", docker.LogMessage{
		Stream: docker.Stdout, At: at.Add(59 * time.Minute),
		Line: `{"msg":"kenny.shutdown"}`,
	})

	l, err := s.GetLife(ctx, 1)
	if err != nil {
		t.Fatalf("GetLife: %v", err)
	}
	if !l.EndedAt.Valid {
		t.Fatalf("life didn't end: %+v", l)
	}
}

func TestHandleIgnoresNonJSONLines(t *testing.T) {
	ctx := context.Background()
	tl, s := newTestTailer(t)
	tl.handle(ctx, "cid", docker.LogMessage{
		Stream: docker.Stdout,
		At:     time.Now().UTC(),
		Line:   "some plain text here",
	})
	// No life, but event still inserted with Parsed=false and no life_id.
	lives, _ := s.ListLives(ctx, 10)
	if len(lives) != 0 {
		t.Fatalf("expected no lives; got %+v", lives)
	}
}
