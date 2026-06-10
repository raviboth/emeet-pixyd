//go:build linux

// Package main implements the emeet-pixyd daemon for the EMEET PIXY webcam.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/LarsArtmann/emeet-pixyd/internal/events"
	"github.com/LarsArtmann/emeet-pixyd/internal/pixy"
	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/larsartmann/httputil"
)

// Build info, overridden via -ldflags.
//
//nolint:gochecknoglobals
var buildVersion = "dev"

type Daemon struct {
	mu        sync.RWMutex
	cmdMu     sync.Mutex
	state     pixy.State
	config    pixy.Config
	videoDev  string
	hidrawDev string
	hidDev    HIDDevice

	debounceInUse int
	debounceIdle  int
	hidFailCount  int
	autoError     error
	lastSyncedAt  time.Time

	lastFrame lastFrameCache

	ptzCache ptzCache

	// ptzLimits caches per-axis driver-reported ranges. Populated by
	// refreshPTZLimits after applyProbeResult finds a video device. A
	// zero-valued PTZLimits means probing failed or has not happened
	// yet and callers fall back to the ptzAxes map's Min/Max.
	ptzLimits struct {
		mu     sync.RWMutex
		values pixy.PTZLimits
	}

	// previewPaused is the user-asserted "release /dev/videoN" flag.
	// When true, the previewSection renders a paused placeholder and
	// /api/stream returns 503 so another app (e.g. a browser tab
	// using getUserMedia, or a meeting client) can grab the device.
	// Guarded by mu.
	previewPaused bool

	streamSema chan struct{}

	// events fans state/PTZ/online changes out to connected SSE
	// clients. nil when the broadcaster failed to initialize; all
	// publish helpers check before use so the daemon still runs.
	events *events.Broadcaster

	deps Dependencies
}

const hidCircuitBreakerThreshold = 3

const (
	httpReadHeaderTimeout = 5 * time.Second
	httpReadTimeout       = 10 * time.Second
	httpWriteTimeout      = 30 * time.Second
	httpIdleTimeout       = 60 * time.Second
	httpMaxHeaderBytes    = 1 << 20
	ueventChBufSize       = 8
	shutdownTimeout       = 5 * time.Second
)

func NewDaemon(cfg pixy.Config) (*Daemon, error) {
	err := cfg.Validate()
	if err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	//nolint:exhaustruct // remaining fields set below or zero-valued
	d := &Daemon{
		config:     cfg,
		state:      pixy.DefaultState(),
		streamSema: make(chan struct{}, 1),
		events:     events.New(),
	}
	//nolint:exhaustruct // remaining deps set below (circular ref on d.setTracking etc)
	d.deps = Dependencies{
		isCameraInUse: isCameraInUse,
		findSource:    findPixySource,
		setSource:     setDefaultSource,
		notify:        notify,
	}
	d.deps.setTracking = d.setTracking
	d.deps.setAudio = d.setAudio
	d.deps.setGesture = d.setGesture
	d.deps.centerCamera = d.centerCamera
	d.deps.resetCamera = d.resetCamera
	d.deps.v4l2Set = v4l2Set
	d.deps.parsePTZ = parsePTZValues
	// Config values override defaults before loading persisted state;
	// persisted state (if valid) wins, ensuring user overrides survive restarts.
	d.state.AutoMode = cfg.AutoMode
	d.state.Audio = cfg.DefaultAudio

	registerMetrics()
	d.loadState()
	d.applyProbeResult(probeDevices())
	checkExternalDeps()

	return d, nil
}

// publishState captures the current state under RLock, marshals it,
// and fans out a TypeState event. Must NOT be called while holding
// d.mu. No-op when the broadcaster failed to initialize (events == nil).
func (d *Daemon) publishState() {
	if d.events == nil {
		return
	}
	d.mu.RLock()
	snapshot := struct {
		Camera     pixy.CameraState `json:"camera"`
		Audio      pixy.AudioMode   `json:"audio"`
		Gesture    bool             `json:"gesture"`
		Auto       pixy.AutoMode    `json:"auto"`
		InCall     bool             `json:"inCall"`
		Online     bool             `json:"online"`
		Device     string           `json:"device"`
		LastSynced time.Time        `json:"lastSynced"`
	}{
		Camera:     d.state.Camera,
		Audio:      d.state.Audio,
		Gesture:    d.state.Gesture,
		Auto:       d.state.AutoMode,
		InCall:     d.state.InCall,
		Online:     d.videoDev != "",
		Device:     d.videoDev,
		LastSynced: d.lastSyncedAt,
	}
	d.mu.RUnlock()

	body, err := json.Marshal(snapshot)
	if err != nil {
		slog.Debug("publishState marshal", "error", err)

		return
	}
	d.events.Publish(events.Event{Type: events.TypeState, Body: body})
}

// publishPTZ fans out a TypePTZ event. The body is empty by design;
// clients re-fetch /panel which already serves fresh PTZ values via
// the cache.
func (d *Daemon) publishPTZ() {
	if d.events == nil {
		return
	}
	d.events.Publish(events.Event{Type: events.TypePTZ, Body: []byte(`{}`)})
}

// publishOnline fans out a TypeOnline event indicating online/offline
// transitions. Caller must NOT hold d.mu.
func (d *Daemon) publishOnline(online bool) {
	if d.events == nil {
		return
	}
	body, err := json.Marshal(struct {
		Online bool `json:"online"`
	}{Online: online})
	if err != nil {
		slog.Debug("publishOnline marshal", "error", err)

		return
	}
	d.events.Publish(events.Event{Type: events.TypeOnline, Body: body})
}

func checkExternalDeps() {
	for _, dep := range []struct {
		binary string
		impact string
	}{
		{ffmpegBin, "MJPEG streaming unavailable"},
		{v4l2ctl, "PTZ control unavailable"},
		{wpctl, "PipeWire source switching unavailable"},
		{notifySend, "desktop notifications unavailable"},
	} {
		_, err := exec.LookPath(dep.binary)
		if err != nil {
			slog.Warn("optional dependency not found", "binary", dep.binary, "impact", dep.impact)
		}
	}
}

func sdNotify(state string) {
	sent, err := daemon.SdNotify(false, state)
	if err != nil {
		slog.Debug("sd_notify failed", "error", err)
	} else if !sent {
		slog.Debug("sd_notify not sent (no NOTIFY_SOCKET)")
	}
}

func (d *Daemon) newHTTPServer() *http.Server {
	webSrv := &webServer{daemon: d}
	mux := newWebMux(webSrv)
	//nolint:exhaustruct
	return &http.Server{
		Addr: d.config.WebAddr,
		Handler: httputil.Chain(
			mux, securityMiddleware, loggingMiddleware, requestIDMiddleware,
		),
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
		MaxHeaderBytes:    httpMaxHeaderBytes,
	}
}

func (d *Daemon) Run() {
	createErr := os.MkdirAll(d.config.StateDir, pixy.PermissionStateDir)
	if createErr != nil {
		slog.Error("failed to create state dir", "error", createErr)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		listenErr := d.listenUnix(ctx)
		if listenErr != nil {
			slog.Error("unix socket error", "error", listenErr)
		}
	}()

	httpSrv := d.startHTTPServer()

	slog.Info("EMEET PIXY daemon started")
	sdNotify("READY=1")
	d.mu.Lock()
	slog.Info(
		"initial state",
		"camera",
		d.state.Camera,
		"audio",
		d.state.Audio,
		"auto",
		d.state.AutoMode,
	)
	d.mu.Unlock()

	d.eventLoop(ctx, cancel, sigs, httpSrv)
}

func (d *Daemon) startHTTPServer() *http.Server {
	if d.config.WebAddr == "" {
		return nil
	}

	httpSrv := d.newHTTPServer()

	go func() {
		slog.Info("web UI starting", "addr", d.config.WebAddr)

		listenErr := httpSrv.ListenAndServe()
		if listenErr != nil && !errors.Is(listenErr, http.ErrServerClosed) {
			slog.Error("web server error", "error", listenErr)
		}
	}()

	return httpSrv
}

func (d *Daemon) handleShutdown(cancel context.CancelFunc, httpSrv *http.Server) {
	sdNotify("STOPPING=1")
	slog.Info("shutting down")
	d.mu.Lock()
	d.saveStateOrLog("failed to save state on shutdown")
	d.mu.Unlock()
	cancel()

	_ = os.Remove(d.config.SocketPath())

	if httpSrv != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		_ = httpSrv.Shutdown(shutdownCtx)

		shutdownCancel()
	}
}

// hardwareSyncInterval is the period at which the daemon re-queries
// the camera for its actual tracking / audio state. The poll is gated
// by cmdMu so it never races an in-flight write; the cost is two short
// HID reads per tick (gesture readback is intentionally swallowed by
// syncState, see device.go). Tuned for "I waved my hand and the UI
// should reflect the new tracking state within a few seconds" without
// hammering the HID interface.
const hardwareSyncInterval = 3 * time.Second

func (d *Daemon) eventLoop(
	ctx context.Context,
	cancel context.CancelFunc,
	sigs <-chan os.Signal,
	httpSrv *http.Server,
) {
	ticker := time.NewTicker(d.config.PollInterval)
	defer ticker.Stop()

	// Hardware-state sync ticker. The camera can flip its own
	// tracking/audio state independently of the daemon: the physical
	// privacy slider, the hand-gesture trigger when gesture detection
	// is enabled, and the EMEET utility on another OS can all do
	// this. Without a periodic re-query d.state drifts from the
	// device and the UI shows stale buttons until the user clicks
	// Sync.
	hwSyncTicker := time.NewTicker(hardwareSyncInterval)
	defer hwSyncTicker.Stop()

	ueventCh := make(chan struct{}, ueventChBufSize)
	go d.listenUevents(ctx, ueventCh)

	for {
		select {
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				slog.Info("received SIGHUP, saving state")
				d.mu.Lock()
				d.saveStateOrLog("failed to save state on SIGHUP")
				d.mu.Unlock()

				continue
			}

			d.handleShutdown(cancel, httpSrv) //nolint:contextcheck // shutdown cancels the parent context

			return
		case <-ueventCh:
			slog.Info("device event detected, re-probing")
			d.cmdMu.Lock()
			d.mu.Lock()
			oldVideo := d.videoDev
			d.applyProbeResult(probeDevices()) //nolint:contextcheck
			newVideo := d.videoDev
			d.mu.Unlock()

			if oldVideo != newVideo {
				d.publishOnline(newVideo != "")
				d.publishState()
			}

			if oldVideo == "" && newVideo != "" {
				slog.Info("device appeared, syncing state")

				_ = d.syncState(ctx)
			}
			d.cmdMu.Unlock()
		case <-ticker.C:
			d.autoManage(ctx)
			sdNotify("WATCHDOG=1")
		case <-hwSyncTicker.C:
			d.mu.RLock()
			videoDev := d.videoDev
			d.mu.RUnlock()
			if videoDev == "" {
				continue
			}
			d.cmdMu.Lock()
			_ = d.syncState(ctx)
			d.cmdMu.Unlock()
		}
	}
}

func exitWithDaemonError(err error) {
	if err != nil {
		_, dieErr := fmt.Fprintf(os.Stderr, "Error: %v\nIs emeet-pixyd running?\n", err)
		_ = dieErr

		os.Exit(1)
	}
}

func handleFlag() bool {
	if len(os.Args) < 2 {
		return false
	}

	switch os.Args[1] {
	case "--version", "-v":
		_, printErr := fmt.Fprintln(os.Stdout, "emeet-pixyd", buildVersion)
		if printErr != nil {
			slog.Debug("failed to print version", "error", printErr)
		}

		return true
	case "--help", "-h":
		_, _ = fmt.Fprintln(
			os.Stdout,
			"Usage: emeet-pixyd [command]\n\nRun without arguments to start the daemon.\nRun with a command to send it to a running daemon via Unix socket.\n\nCommands:\n  status            Show current camera status\n  waybar            Output waybar-compatible JSON\n  version           Print version\n  sync              Sync state from hardware\n  probe             Re-probe for device\n  Show device paths\n  track             Enable tracking mode\n  idle              Set idle mode\n  privacy           Enable privacy mode\n  toggle-privacy    Toggle privacy on/off\n  center            Center camera (pan/tilt/zoom reset)\n  audio [mode]      Cycle or set audio mode (nc, live, org/original)\n  gesture-on        Enable gesture control\n  gesture-off       Disable gesture control\n  toggle-gesture    Toggle gesture control\n  auto              Show current auto mode\n  auto-on           Enable auto mode (full)\n  auto-off          Disable auto mode\n  toggle-auto       Toggle auto mode\n  pan <degrees>     Set pan position\n  tilt <degrees>    Set tilt position\n  zoom <value>      Set zoom level",
		)

		return true
	default:
		return false
	}
}

func main() {
	cfg := pixy.ConfigFromEnv()

	if cfg.Debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	if handleFlag() {
		return
	}

	if len(os.Args) > 1 {
		cmd := strings.Join(os.Args[1:], " ")

		resp, err := sendCommand(cfg, cmd)
		exitWithDaemonError(err)

		_, printErr := fmt.Fprintln(os.Stdout, resp)
		if printErr != nil {
			slog.Debug("failed to print response", "error", printErr)
		}

		return
	}

	d, err := NewDaemon(cfg)
	if err != nil {
		slog.Error("daemon init failed", "error", err)
		os.Exit(1)
	}

	d.Run()
}
