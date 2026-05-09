package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	sseInitialBackoff = time.Second
	sseMaxBackoff     = 30 * time.Second
)

type stateSnapshot struct {
	Camera     string `json:"camera"`
	Audio      string `json:"audio"`
	Gesture    bool   `json:"gesture"`
	Auto       string `json:"auto"`
	InCall     bool   `json:"inCall"`
	Online     bool   `json:"online"`
	Device     string `json:"device"`
	LastSynced string `json:"lastSynced"`
}

func (t *tray) runSSE(ctx context.Context) {
	backoff := sseInitialBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		err := t.streamEvents(ctx, func() { backoff = sseInitialBackoff })
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Debug("SSE stream ended", "error", err, "retry_in", backoff)
		}
		t.applyState(stateSnapshot{Camera: "offline", Online: false})
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > sseMaxBackoff {
			backoff = sseMaxBackoff
		}
	}
}

// streamEvents opens an SSE connection and parses frames until the stream
// drops. onConnect is invoked once the daemon returns HTTP 200, so the caller
// can reset its retry backoff after a successful reconnect.
func (t *tray) streamEvents(ctx context.Context, onConnect func()) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.client.baseURL+"/api/events", http.NoBody)
	if err != nil {
		return fmt.Errorf("build sse request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	hc := &http.Client{Timeout: 0}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("dial sse: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sse status %d", resp.StatusCode)
	}
	if onConnect != nil {
		onConnect()
	}

	return parseSSE(resp.Body, t.handleEvent)
}

// parseSSE reads a Server-Sent Events stream from r, invoking emit for every
// completed frame. Returns the underlying read error, or a sentinel "stream
// closed" error on clean EOF.
func parseSSE(r io.Reader, emit func(eventType, data string)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var (
		eventType = "message"
		dataBuf   strings.Builder
	)

	dispatch := func() {
		if dataBuf.Len() == 0 {
			eventType = "message"
			return
		}
		emit(eventType, dataBuf.String())
		dataBuf.Reset()
		eventType = "message"
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			dispatch()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			eventType = value
		case "data":
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("sse scan: %w", err)
	}
	return errors.New("sse stream closed")
}

func (t *tray) handleEvent(eventType, data string) {
	if eventType != "state" {
		return
	}
	var snap stateSnapshot
	if err := json.Unmarshal([]byte(data), &snap); err != nil {
		slog.Debug("decode state event", "error", err, "data", data)
		return
	}
	slog.Debug("state event", "camera", snap.Camera, "auto", snap.Auto, "online", snap.Online)
	t.applyState(snap)
}
