// Package tailer consumes Docker logs from matching containers, parses
// each line as Kenny's structured slog output, and writes the result
// into the store. Lifecycle events (kenny.boot, kenny.shutdown) update
// the lives table; everything else is recorded as an event belonging
// to the current life.
package tailer

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/vmorsell/kennyscope/internal/docker"
	"github.com/vmorsell/kennyscope/internal/store"
)

type Tailer struct {
	client        *docker.Client
	store         *store.Store
	match         string
	logger        *slog.Logger
	backoffShort  time.Duration
	backoffLong   time.Duration

	mu          sync.Mutex
	currentLife map[string]int64
}

type Config struct {
	Client *docker.Client
	Store  *store.Store
	Match  string // substring match on container name
	Logger *slog.Logger
}

func New(cfg Config) *Tailer {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Tailer{
		client:       cfg.Client,
		store:        cfg.Store,
		match:        cfg.Match,
		logger:       logger,
		backoffShort: 2 * time.Second,
		backoffLong:  10 * time.Second,
		currentLife:  map[string]int64{},
	}
}

// Run blocks, discovering and tailing matching containers until ctx is cancelled.
func (t *Tailer) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		containers, err := t.client.ListRunningContainers(ctx, t.match)
		if err != nil {
			t.logger.Warn("list containers", slog.String("err", err.Error()))
			if sleepOrDone(ctx, t.backoffLong) {
				return
			}
			continue
		}
		if len(containers) == 0 {
			t.logger.Debug("no matching containers")
			if sleepOrDone(ctx, t.backoffShort) {
				return
			}
			continue
		}
		for _, c := range containers {
			t.tailOne(ctx, c.ID)
			if ctx.Err() != nil {
				return
			}
		}
		if sleepOrDone(ctx, t.backoffShort) {
			return
		}
	}
}

func (t *Tailer) tailOne(ctx context.Context, containerID string) {
	t.ensureCurrentLife(ctx, containerID)

	cursor, _, _ := t.store.GetCursor(ctx, containerID)
	var since int64
	if !cursor.IsZero() {
		// +1 so we don't re-ingest the last seen record.
		since = cursor.Unix() + 1
	}

	err := t.client.TailLogs(ctx, containerID, since, func(m docker.LogMessage) {
		t.handle(ctx, containerID, m)
	})
	if err != nil && ctx.Err() == nil {
		t.logger.Warn("tail logs",
			slog.String("container", containerID),
			slog.String("err", err.Error()))
	}
}

// ensureCurrentLife populates the in-memory current life_id for a
// container from persistent state on first sight.
func (t *Tailer) ensureCurrentLife(ctx context.Context, containerID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.currentLife[containerID]; ok {
		return
	}
	id, err := t.store.LatestLifeForContainer(ctx, containerID)
	if err != nil {
		t.logger.Warn("latest life",
			slog.String("container", containerID),
			slog.String("err", err.Error()))
		return
	}
	t.currentLife[containerID] = id
}

func (t *Tailer) handle(ctx context.Context, containerID string, m docker.LogMessage) {
	parsed, ok := parseLine(m.Line)

	eventTime := m.At
	if eventTime.IsZero() && ok {
		if parsed.Time != "" {
			if parsedTs, err := time.Parse(time.RFC3339Nano, parsed.Time); err == nil {
				eventTime = parsedTs
			}
		}
	}
	if eventTime.IsZero() {
		eventTime = time.Now().UTC()
	}

	// Lifecycle events update the lives table and current-life cache.
	if ok && parsed.Msg == "kenny.boot" && parsed.LifeID != nil && *parsed.LifeID > 0 {
		if err := t.store.UpsertLifeStart(ctx, *parsed.LifeID, containerID, eventTime, m.Line); err != nil {
			t.logger.Warn("upsert life start", slog.String("err", err.Error()))
		}
		t.mu.Lock()
		t.currentLife[containerID] = *parsed.LifeID
		t.mu.Unlock()
	}

	var eventLife int64
	if ok && parsed.LifeID != nil && *parsed.LifeID > 0 {
		eventLife = *parsed.LifeID
	} else {
		t.mu.Lock()
		eventLife = t.currentLife[containerID]
		t.mu.Unlock()
	}
	var nullLife sql.NullInt64
	if eventLife > 0 {
		nullLife = sql.NullInt64{Int64: eventLife, Valid: true}
	}

	ev := store.Event{
		ContainerID: containerID,
		LifeID:      nullLife,
		Stream:      string(m.Stream),
		At:          eventTime,
		Level:       parsed.Level,
		Msg:         parsed.Msg,
		Raw:         m.Line,
		Parsed:      ok,
	}
	if _, err := t.store.InsertEvent(ctx, ev); err != nil {
		t.logger.Warn("insert event", slog.String("err", err.Error()))
	}

	if ok && parsed.Msg == "kenny.shutdown" && eventLife > 0 {
		if err := t.store.MarkLifeEnded(ctx, eventLife, eventTime, m.Line); err != nil {
			t.logger.Warn("mark life ended", slog.String("err", err.Error()))
		}
	}

	if err := t.store.SetCursor(ctx, containerID, eventTime); err != nil {
		t.logger.Debug("set cursor", slog.String("err", err.Error()))
	}
}

type parsedLine struct {
	Time   string `json:"time"`
	Level  string `json:"level"`
	Msg    string `json:"msg"`
	LifeID *int64 `json:"life_id,omitempty"`
}

func parseLine(line string) (parsedLine, bool) {
	var p parsedLine
	if err := json.Unmarshal([]byte(line), &p); err != nil {
		return parsedLine{}, false
	}
	// Require at least a msg or level to count as "parsed slog output."
	if p.Msg == "" && p.Level == "" {
		return parsedLine{}, false
	}
	return p, true
}

func sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}
