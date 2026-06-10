//go:build linux

package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"strconv"
	"time"

	"github.com/LarsArtmann/emeet-pixyd/internal/events"
	"github.com/LarsArtmann/emeet-pixyd/internal/pixy"
	"github.com/a-h/templ"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	maxStreamBufferSize = 10 * 1024 * 1024
	maxBodyBytes        = 1 << 10

	staticCacheMaxAge = 7 * 24 * time.Hour

	toastTypeSuccess = "success"
	toastTypeInfo    = "info"
	toastTypeError   = "error"

	ptzCacheTTL = 2 * time.Second

	toastTrackingEnabled = "Tracking enabled"
	toastCameraIdle      = "Camera idle"
	toastPrivacyOn       = "Privacy mode on"
	toastCameraCentered  = "Camera centered"
	toastCameraReset     = "Camera reset"
	toastStateSynced     = "State synced"
	toastProbedDevices   = "Probed devices"
	toastAudioChanged    = "Audio mode changed"
	toastGestureToggled  = "Gesture toggled"
	toastAutoToggled     = "Auto mode toggled"
)

type actionToastInfo struct {
	msg  string
	kind string
}

//nolint:gochecknoglobals
var actionToasts = map[string]actionToastInfo{
	cmdTrack:         {toastTrackingEnabled, toastTypeSuccess},
	cmdIdle:          {toastCameraIdle, toastTypeSuccess},
	cmdPrivacy:       {toastPrivacyOn, toastTypeSuccess},
	cmdCenter:        {toastCameraCentered, toastTypeSuccess},
	cmdReset:         {toastCameraReset, toastTypeSuccess},
	cmdSync:          {toastStateSynced, toastTypeSuccess},
	cmdProbe:         {toastProbedDevices, toastTypeSuccess},
	cmdToggleGesture: {toastGestureToggled, toastTypeInfo},
	cmdToggleAuto:    {toastAutoToggled, toastTypeInfo},
}

//go:embed static
var staticFS embed.FS

func formatLastSynced(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	elapsed := time.Since(t)
	if elapsed < time.Minute {
		return "just now"
	}

	if elapsed < time.Hour {
		return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
	}

	return t.Format("15:04")
}

type webServer struct {
	daemon *Daemon
}

func (s *webServer) getWebStatus() webStatus {
	s.daemon.mu.RLock()
	defer s.daemon.mu.RUnlock()
	//nolint:exhaustruct
	status := webStatus{
		PTZValues:  pixy.PTZValues{},
		Camera:     s.daemon.state.Camera,
		Audio:      s.daemon.state.Audio,
		Gesture:    s.daemon.state.Gesture,
		InCall:     s.daemon.state.InCall,
		Auto:       s.daemon.state.AutoMode,
		Online:     s.daemon.videoDev != "",
		Device:     s.daemon.videoDev,
		Error:      errStr(s.daemon.autoError),
		LastSynced: formatLastSynced(s.daemon.lastSyncedAt),
		Version:    buildVersion,
	}
	if status.Online {
		status.Zoom = pixy.ZoomDefault
	}
	status.PanLo, status.PanHi = s.daemon.effectivePTZLimits(pixy.AxisPan)
	status.TiltLo, status.TiltHi = s.daemon.effectivePTZLimits(pixy.AxisTilt)
	status.ZoomLo, status.ZoomHi = s.daemon.effectivePTZLimits(pixy.AxisZoom)

	return status
}

func (s *webServer) getWebStatusWithPTZ(ctx context.Context) webStatus {
	status := s.getWebStatus()
	if !status.Online {
		return status
	}

	dev := status.Device

	if values, valid := s.daemon.ptzCache.Get(); valid {
		status.Pan = values.Pan
		status.Tilt = values.Tilt
		status.Zoom = values.Zoom

		return status
	}

	ptz := parsePTZValues(ctx, dev)
	s.daemon.ptzCache.Set(ptz, ptzCacheTTL)

	status.Pan = ptz.Pan
	status.Tilt = ptz.Tilt
	status.Zoom = ptz.Zoom

	return status
}

func (s *webServer) handleIndex(responseWriter http.ResponseWriter, request *http.Request) {
	status := s.getWebStatusWithPTZ(request.Context())
	templ.Handler(page(status)).ServeHTTP(responseWriter, request) //nolint:contextcheck
}

func (s *webServer) handleHealth(responseWriter http.ResponseWriter, _ *http.Request) {
	s.daemon.mu.RLock()
	online := s.daemon.videoDev != ""
	camera := s.daemon.state.Camera
	s.daemon.mu.RUnlock()

	responseWriter.Header().Set("Content-Type", "application/json")

	if !online {
		responseWriter.WriteHeader(http.StatusServiceUnavailable)
	}

	data, err := json.Marshal(healthResponse{
		Status:  boolStr(online, "ok", "offline"),
		Camera:  camera,
		Version: buildVersion,
	})
	if err != nil {
		slog.Error("failed to marshal health response", "error", err)

		return
	}

	_, _ = responseWriter.Write(data)
}

type healthResponse struct {
	Status  string           `json:"status"`
	Camera  pixy.CameraState `json:"camera"`
	Version string           `json:"version"`
}

func (s *webServer) snapshotEventBody() []byte {
	status := s.getWebStatus()
	body, err := json.Marshal(struct {
		Camera     string `json:"camera"`
		Audio      string `json:"audio"`
		Gesture    bool   `json:"gesture"`
		Auto       string `json:"auto"`
		InCall     bool   `json:"inCall"`
		Online     bool   `json:"online"`
		Device     string `json:"device"`
		LastSynced string `json:"lastSynced"`
	}{
		Camera:     string(status.Camera),
		Audio:      string(status.Audio),
		Gesture:    status.Gesture,
		Auto:       status.Auto.String(),
		InCall:     status.InCall,
		Online:     status.Online,
		Device:     status.Device,
		LastSynced: status.LastSynced,
	})
	if err != nil {
		slog.Debug("snapshotEventBody marshal", "error", err)

		return []byte("{}")
	}

	return body
}

func writeSSEFrame(w http.ResponseWriter, eventType events.Type, body []byte) error {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, body); err != nil {
		return fmt.Errorf("write sse: %w", err)
	}

	return nil
}

// sseHeartbeatInterval is the cadence at which the SSE handler emits a
// keepalive comment so intermediate proxies do not close idle
// connections. 20s is well under the common 30/60s idle timeout
// without adding noticeable byte traffic.
const sseHeartbeatInterval = 20 * time.Second

func (s *webServer) handleEvents(responseWriter http.ResponseWriter, request *http.Request) {
	flusher, ok := responseWriter.(http.Flusher)
	if !ok {
		http.Error(responseWriter, "streaming not supported", http.StatusInternalServerError)

		return
	}

	rc := http.NewResponseController(responseWriter)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		slog.Warn("could not clear write deadline; SSE may be cut off by server timeout", "error", err)
	}

	responseWriter.Header().Set("Content-Type", "text/event-stream")
	responseWriter.Header().Set("Cache-Control", "no-store")
	responseWriter.Header().Set("Connection", "keep-alive")
	responseWriter.Header().Set("X-Accel-Buffering", "no")

	if err := writeSSEFrame(responseWriter, events.TypeState, s.snapshotEventBody()); err != nil {
		return
	}
	flusher.Flush()

	sub, unsub := s.daemon.events.Subscribe()
	defer unsub()

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-request.Context().Done():
			return
		case ev, open := <-sub:
			if !open {
				return
			}
			if err := writeSSEFrame(responseWriter, ev.Type, ev.Body); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := io.WriteString(responseWriter, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *webServer) handleStatusPanel(responseWriter http.ResponseWriter, request *http.Request) {
	status := s.getWebStatusWithPTZ(request.Context())
	templ.Handler(statusPanel(status)).ServeHTTP(responseWriter, request) //nolint:contextcheck
}

func (s *webServer) action(command string) http.HandlerFunc {
	return func(responseWriter http.ResponseWriter, request *http.Request) {
		request.Body = http.MaxBytesReader(responseWriter, request.Body, maxBodyBytes)

		result := s.daemon.handleCommand(request.Context(), command)

		slog.Debug("web action", "cmd", command, "response", result.String())

		// center / reset write directly to v4l2 controls. The
		// PTZ cache reflects the position before the write, so
		// the freshly-rendered panel would show stale sliders
		// until the 2s TTL expired. Force a re-fetch.
		if command == cmdCenter || command == cmdReset {
			s.invalidatePTZCache()
		}

		status := s.getWebStatusWithPTZ(request.Context())
		toast, toastType := actionToast(command)
		applyResultToStatus(result, &status, toast, toastType)

		templ.Handler(statusPanel(status)).ServeHTTP(responseWriter, request) //nolint:contextcheck
	}
}

func actionToast(command string) (string, string) {
	info, ok := actionToasts[command]
	if !ok {
		return "", ""
	}

	return info.msg, info.kind
}

func applyResultToStatus(result CommandResult, status *webStatus, toast, toastType string) {
	if result.IsError() {
		status.Error = result.String()
	} else {
		status.Toast = toast
		status.ToastType = toastType
	}
}

func (s *webServer) handleAudio(responseWriter http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(responseWriter, request.Body, maxBodyBytes)
	mode := request.FormValue("mode")

	cmd := cmdAudio
	if mode != "" {
		cmd = cmdAudio + " " + mode
	}

	result := s.daemon.handleCommand(request.Context(), cmd)
	slog.Debug("web audio", "cmd", cmd, "response", result.String())

	status := s.getWebStatusWithPTZ(request.Context())

	toast := toastAudioChanged
	if !result.IsError() {
		toast = "Audio: " + string(status.Audio)
	}

	applyResultToStatus(result, &status, toast, toastTypeSuccess)
	templ.Handler(statusPanel(status)).ServeHTTP(responseWriter, request) //nolint:contextcheck
}

func (s *webServer) handlePTZ(responseWriter http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(responseWriter, request.Body, maxBodyBytes)
	axis := request.PathValue("axis")

	val := request.FormValue("value")
	if axis == "" || val == "" {
		http.Error(responseWriter, "missing axis or value", http.StatusBadRequest)

		return
	}

	if !ptzAxisValid(axis) {
		http.Error(responseWriter, "invalid axis", http.StatusBadRequest)

		return
	}

	intVal, err := strconv.Atoi(val)
	if err != nil {
		http.Error(responseWriter, "invalid value", http.StatusBadRequest)

		return
	}

	info := ptzAxes[axis]
	lo, hi := s.daemon.effectivePTZLimits(axis)
	intVal = clampInt(intVal, lo, hi)
	result := s.daemon.handleCommand(request.Context(), axis+" "+strconv.Itoa(intVal))
	slog.Debug("web ptz", "axis", axis, "val", intVal, "response", result.String())

	if result.IsError() {
		status := s.getWebStatusWithPTZ(request.Context())
		sliderVal := ptzAxisValue(axis, status)
		templ.Handler(ptzSliderWithToast( //nolint:contextcheck
			info.Label, axis, lo, hi, sliderVal, info.Unit,
			result.String(), toastTypeError,
		)).ServeHTTP(responseWriter, request)

		return
	}

	s.invalidatePTZCache()

	templ.Handler(ptzSliderWithToast( //nolint:contextcheck
		info.Label, axis, lo, hi, intVal, info.Unit,
		"", "",
	)).ServeHTTP(responseWriter, request)
}

func (s *webServer) invalidatePTZCache() {
	s.daemon.ptzCache.Invalidate()
}

func (s *webServer) checkDevice(responseWriter http.ResponseWriter) (webStatus, bool) {
	status := s.getWebStatus()
	if status.Device == "" {
		http.Error(responseWriter, "no camera device", http.StatusServiceUnavailable)

		return status, false
	}

	return status, true
}

func newWebMux(server *webServer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", cachingFS{handler: http.FileServer(http.FS(staticFS))})
	mux.HandleFunc("GET /{$}", server.handleIndex)
	mux.HandleFunc("GET /panel", server.handleStatusPanel)
	mux.HandleFunc("GET /api/health", server.handleHealth)
	mux.HandleFunc("POST /api/track", server.action(cmdTrack))
	mux.HandleFunc("POST /api/"+cmdIdle, server.action(cmdIdle))
	mux.HandleFunc("POST /api/privacy", server.action(cmdPrivacy))
	mux.HandleFunc("POST /api/toggle-privacy", server.action(cmdTogglePrivacy))
	mux.HandleFunc("POST /api/audio", server.handleAudio)
	mux.HandleFunc("POST /api/gesture", server.action(cmdToggleGesture))
	mux.HandleFunc("POST /api/auto", server.action(cmdToggleAuto))
	mux.HandleFunc("POST /api/center", server.action(cmdCenter))
	mux.HandleFunc("POST /api/reset", server.action(cmdReset))
	mux.HandleFunc("POST /api/sync", server.action(cmdSync))
	mux.HandleFunc("POST /api/probe", server.action(cmdProbe))
	mux.HandleFunc("POST /api/ptz/{axis}", server.handlePTZ)
	mux.HandleFunc("POST /api/ptz/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing axis", http.StatusBadRequest)
	})
	mux.HandleFunc("GET /api/snapshot", server.handleSnapshot)
	mux.HandleFunc("GET /api/stream", server.handleStream)
	mux.HandleFunc("GET /api/events", server.handleEvents)
	mux.Handle("GET /metrics", promhttp.Handler())

	if server.daemon.config.Debug {
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	}

	return mux
}
