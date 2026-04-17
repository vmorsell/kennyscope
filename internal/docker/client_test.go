package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestListRunningContainers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/json" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("filters"); got == "" {
			t.Errorf("missing filters")
		}
		_ = json.NewEncoder(w).Encode([]Container{
			{ID: "abc", Names: []string{"/kenny-app-1"}, State: "running"},
		})
	}))
	defer srv.Close()

	c := New(srv.URL)
	containers, err := c.ListRunningContainers(context.Background(), "kenny")
	if err != nil {
		t.Fatalf("ListRunningContainers: %v", err)
	}
	if len(containers) != 1 || containers[0].ID != "abc" {
		t.Fatalf("unexpected result: %+v", containers)
	}
}

// writeFrame encodes a single Docker framed log chunk.
func writeFrame(buf *bytes.Buffer, stream byte, payload []byte) {
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:8], uint32(len(payload)))
	buf.Write(header)
	buf.Write(payload)
}

func TestTailLogsParsesStream(t *testing.T) {
	body := &bytes.Buffer{}
	ts := time.Date(2026, 4, 16, 23, 23, 15, 491684686, time.UTC)
	line1 := ts.Format(time.RFC3339Nano) + ` {"msg":"kenny.boot"}` + "\n"
	line2 := ts.Add(time.Second).Format(time.RFC3339Nano) + ` {"msg":"claude.start"}` + "\n"
	writeFrame(body, 1, []byte(line1))
	writeFrame(body, 2, []byte(line2))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		_, _ = w.Write(body.Bytes())
	}))
	defer srv.Close()

	c := New(srv.URL)
	var got []LogMessage
	var mu sync.Mutex
	err := c.TailLogs(context.Background(), "abc", 0, func(m LogMessage) {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("TailLogs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(got), got)
	}
	if got[0].Stream != Stdout || got[1].Stream != Stderr {
		t.Fatalf("stream types wrong: %v %v", got[0].Stream, got[1].Stream)
	}
	if got[0].Line != `{"msg":"kenny.boot"}` {
		t.Fatalf("line 0 = %q", got[0].Line)
	}
	if !got[0].At.Equal(ts) {
		t.Fatalf("timestamp 0 = %v, want %v", got[0].At, ts)
	}
}
