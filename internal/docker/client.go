// Package docker is a minimal client for the Docker Engine HTTP API.
// Talks to docker-socket-proxy (tecnativa/docker-socket-proxy) over TCP
// rather than a raw unix socket, so only the LOGS and CONTAINERS
// endpoints need to be enabled proxy-side.
package docker

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	base string
	http *http.Client
}

// New constructs a client pointing at base, e.g. "http://docker-socket-proxy:2375".
func New(base string) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{
			// No timeout on the http.Client because log streams are long-lived.
			// Per-request timeouts are enforced via ctx.
		},
	}
}

type Container struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	Image  string   `json:"Image"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
}

// ListRunningContainers returns all running containers whose name contains
// nameMatch (substring match). nameMatch may be empty to list everything.
func (c *Client) ListRunningContainers(ctx context.Context, nameMatch string) ([]Container, error) {
	u := c.base + "/containers/json"
	if nameMatch != "" {
		filters, _ := json.Marshal(map[string][]string{"name": {nameMatch}})
		u += "?filters=" + url.QueryEscape(string(filters))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("list containers: %s: %s", resp.Status, string(body))
	}
	var out []Container
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

type StreamType string

const (
	Stdout StreamType = "stdout"
	Stderr StreamType = "stderr"
)

type LogMessage struct {
	Stream StreamType
	At     time.Time
	Line   string
	Raw    []byte // the bytes as received, before any parsing
}

// TailLogs streams logs for a container, calling onMessage for each line.
// Blocks until ctx is cancelled or the stream closes. since is a unix
// timestamp (seconds); pass 0 to start from the earliest available log.
func (c *Client) TailLogs(ctx context.Context, containerID string, since int64, onMessage func(LogMessage)) error {
	u := fmt.Sprintf("%s/containers/%s/logs?stdout=1&stderr=1&follow=1&timestamps=1&since=%d",
		c.base, url.PathEscape(containerID), since)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("logs: %s: %s", resp.Status, string(body))
	}

	// Docker framed-stream protocol: 8-byte header per chunk, then payload.
	// Header: [stream_type, 0, 0, 0, size_uint32_be].
	reader := bufio.NewReader(resp.Body)
	header := make([]byte, 8)

	for {
		if _, err := io.ReadFull(reader, header); err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read header: %w", err)
		}
		streamByte := header[0]
		size := binary.BigEndian.Uint32(header[4:8])
		if size == 0 {
			continue
		}
		if size > 16*1024*1024 {
			return fmt.Errorf("log chunk too large: %d bytes", size)
		}

		payload := make([]byte, size)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return fmt.Errorf("read payload: %w", err)
		}

		stream := Stdout
		if streamByte == 2 {
			stream = Stderr
		}

		// Payload may contain multiple newline-separated lines. When
		// timestamps=1 is set each line is prefixed with an RFC3339Nano
		// timestamp followed by a space.
		for _, line := range bytes_SplitLines(payload) {
			ts, rest := splitTimestamp(line)
			onMessage(LogMessage{
				Stream: stream,
				At:     ts,
				Line:   rest,
				Raw:    line,
			})
		}
	}
}

func bytes_SplitLines(b []byte) [][]byte {
	// Split on \n, preserving order and dropping the trailing empty.
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// splitTimestamp pulls the RFC3339Nano prefix off a log line. If parsing
// fails, the whole line is returned as the body with a zero timestamp.
func splitTimestamp(line []byte) (time.Time, string) {
	idx := -1
	for i, c := range line {
		if c == ' ' {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return time.Time{}, string(line)
	}
	tsStr := string(line[:idx])
	t, err := time.Parse(time.RFC3339Nano, tsStr)
	if err != nil {
		return time.Time{}, string(line)
	}
	return t, string(line[idx+1:])
}
