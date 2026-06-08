//go:build linux

package main

import "github.com/LarsArtmann/emeet-pixyd/internal/pixy"

type webStatus struct {
	pixy.PTZValues

	// PanLo/PanHi (and the tilt/zoom equivalents) carry the per-axis
	// driver-reported PTZ limits so the template can render slider
	// min/max attributes that match what the daemon will actually
	// accept. When the device has not been probed yet these stay
	// zero and the template falls back to the package-level
	// pixy.PanMin / pixy.PanMax constants.
	PanLo  int
	PanHi  int
	TiltLo int
	TiltHi int
	ZoomLo int
	ZoomHi int

	Camera     pixy.CameraState
	Audio      pixy.AudioMode
	Gesture    bool
	InCall     bool
	Auto       pixy.AutoMode
	Online     bool
	Device     string
	Error      string
	LastSynced string
	Toast      string
	ToastType  string
	Version    string
}
