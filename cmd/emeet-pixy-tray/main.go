// Command emeet-pixy-tray renders a KDE/StatusNotifierItem tray indicator
// for the running emeet-pixyd daemon. It subscribes to /api/events for live
// state updates and exposes the most-used controls as menu actions.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"fyne.io/systray"
)

const (
	defaultDaemonURL = "http://127.0.0.1:8090"
	heartbeatPeriod  = 60 * time.Second
)

type tray struct {
	client *apiClient

	mTrack   *systray.MenuItem
	mIdle    *systray.MenuItem
	mPrivacy *systray.MenuItem
	mCenter  *systray.MenuItem
	mReset   *systray.MenuItem
	mSync    *systray.MenuItem
	mWebUI   *systray.MenuItem
	mQuit    *systray.MenuItem
}

func main() {
	var (
		daemonURL = flag.String("url", defaultDaemonURL, "daemon base URL")
		logLevel  = flag.String("log", "info", "log level (debug, info, warn, error)")
	)
	flag.Parse()

	configureLogger(*logLevel)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	t := &tray{client: newAPIClient(*daemonURL)}
	systray.Run(t.onReady(ctx), t.onExit)
}

func (t *tray) onReady(ctx context.Context) func() {
	return func() {
		systray.SetIcon(iconOffline)
		systray.SetTooltip("EMEET PIXY · connecting...")
		systray.SetTitle("EMEET PIXY")

		t.buildMenu()

		go t.runSSE(ctx)
		go t.runHeartbeat(ctx)
		go t.handleMenuClicks(ctx)

		go func() {
			<-ctx.Done()
			systray.Quit()
		}()
	}
}

func (t *tray) onExit() {
	slog.Info("tray exiting")
}

func (t *tray) buildMenu() {
	t.mTrack = systray.AddMenuItem("Track", "Engage face tracking")
	t.mIdle = systray.AddMenuItem("Idle", "Stop tracking, keep camera awake")
	t.mPrivacy = systray.AddMenuItem("Privacy", "Close shutter")
	systray.AddSeparator()
	t.mCenter = systray.AddMenuItem("Center", "Center pan/tilt/zoom")
	t.mReset = systray.AddMenuItem("Reset", "Reset to defaults")
	systray.AddSeparator()
	t.mSync = systray.AddMenuItem("Sync state", "Resync daemon from device")
	t.mWebUI = systray.AddMenuItem("Open Web UI", "Open the daemon web panel")
	systray.AddSeparator()
	t.mQuit = systray.AddMenuItem("Quit", "Exit the tray (daemon keeps running)")
}

func (t *tray) handleMenuClicks(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.mTrack.ClickedCh:
			t.post(ctx, "/api/track")
		case <-t.mIdle.ClickedCh:
			t.post(ctx, "/api/idle")
		case <-t.mPrivacy.ClickedCh:
			t.post(ctx, "/api/privacy")
		case <-t.mCenter.ClickedCh:
			t.post(ctx, "/api/center")
		case <-t.mReset.ClickedCh:
			t.post(ctx, "/api/reset")
		case <-t.mSync.ClickedCh:
			t.post(ctx, "/api/sync")
		case <-t.mWebUI.ClickedCh:
			openBrowser(t.client.baseURL + "/")
		case <-t.mQuit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

func (t *tray) post(ctx context.Context, path string) {
	go func() {
		if err := t.client.post(ctx, path); err != nil {
			slog.Warn("post failed", "path", path, "error", err)
		}
	}()
}

func (t *tray) runHeartbeat(ctx context.Context) {
	tick := time.NewTicker(heartbeatPeriod)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := t.client.ping(ctx); err != nil {
				slog.Debug("heartbeat ping failed", "error", err)
				t.applyState(stateSnapshot{Camera: "offline", Online: false})
			}
		}
	}
}

func (t *tray) applyState(s stateSnapshot) {
	systray.SetIcon(iconFor(s.Camera, s.Online))
	systray.SetTooltip(tooltipFor(s))
}

func tooltipFor(s stateSnapshot) string {
	state := s.Camera
	if state == "" {
		state = "unknown"
	}
	auto := s.Auto
	if auto == "" {
		auto = "off"
	}
	return "EMEET PIXY · " + state + " · auto=" + auto
}

func configureLogger(level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))
}
