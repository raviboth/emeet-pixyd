package main

import (
	"strings"
	"testing"
)

type frame struct {
	event string
	data  string
}

func collectFrames(t *testing.T, payload string) ([]frame, error) {
	t.Helper()
	var got []frame
	err := parseSSE(strings.NewReader(payload), func(ev, data string) {
		got = append(got, frame{ev, data})
	})
	return got, err
}

func TestParseSSE_SingleStateFrame(t *testing.T) {
	payload := "event: state\ndata: {\"camera\":\"tracking\"}\n\n"
	got, err := collectFrames(t, payload)
	if err == nil || err.Error() != "sse stream closed" {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("frame count = %d, want 1", len(got))
	}
	if got[0].event != "state" || got[0].data != `{"camera":"tracking"}` {
		t.Errorf("got %+v", got[0])
	}
}

func TestParseSSE_MultilineData(t *testing.T) {
	payload := "event: state\ndata: line1\ndata: line2\n\n"
	got, err := collectFrames(t, payload)
	if err == nil || err.Error() != "sse stream closed" {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 1 || got[0].data != "line1\nline2" {
		t.Errorf("got %+v", got)
	}
}

func TestParseSSE_DefaultEventTypeIsMessage(t *testing.T) {
	payload := "data: hi\n\n"
	got, _ := collectFrames(t, payload)
	if len(got) != 1 || got[0].event != "message" || got[0].data != "hi" {
		t.Errorf("got %+v", got)
	}
}

func TestParseSSE_CommentLineIgnored(t *testing.T) {
	payload := ": ping\nevent: state\ndata: x\n\n"
	got, _ := collectFrames(t, payload)
	if len(got) != 1 || got[0].event != "state" || got[0].data != "x" {
		t.Errorf("got %+v", got)
	}
}

func TestParseSSE_MultipleFrames(t *testing.T) {
	payload := "event: state\ndata: a\n\nevent: state\ndata: b\n\n"
	got, _ := collectFrames(t, payload)
	if len(got) != 2 {
		t.Fatalf("frame count = %d, want 2", len(got))
	}
	if got[0].data != "a" || got[1].data != "b" {
		t.Errorf("got %+v", got)
	}
}

func TestParseSSE_BlankBetweenFramesResetsEventType(t *testing.T) {
	payload := "event: state\ndata: a\n\ndata: b\n\n"
	got, _ := collectFrames(t, payload)
	if len(got) != 2 {
		t.Fatalf("frame count = %d, want 2", len(got))
	}
	if got[0].event != "state" || got[1].event != "message" {
		t.Errorf("event types reset wrong: %+v", got)
	}
}

func TestIconForOfflineWinsOverState(t *testing.T) {
	got := iconFor("tracking", false)
	if string(got) != string(iconOffline) {
		t.Errorf("offline override failed")
	}
}

func TestIconForKnownStates(t *testing.T) {
	cases := map[string][]byte{
		"tracking": iconTracking,
		"idle":     iconIdle,
		"privacy":  iconPrivacy,
		"active":   iconActive,
		"offline":  iconOffline,
		"unknown":  iconOffline,
		"":         iconOffline,
	}
	for state, want := range cases {
		if got := iconFor(state, true); string(got) != string(want) {
			t.Errorf("iconFor(%q) wrong icon", state)
		}
	}
}

func TestTooltipFor(t *testing.T) {
	got := tooltipFor(stateSnapshot{Camera: "tracking", Auto: "full"})
	want := "EMEET PIXY · tracking · auto=full"
	if got != want {
		t.Errorf("tooltipFor: got %q, want %q", got, want)
	}
	got = tooltipFor(stateSnapshot{})
	want = "EMEET PIXY · unknown · auto=off"
	if got != want {
		t.Errorf("tooltipFor empty: got %q, want %q", got, want)
	}
}
