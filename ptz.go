//go:build linux

package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/LarsArtmann/emeet-pixyd/internal/pixy"
)

const v4l2ctl = "v4l2-ctl"

// v4l2UnitsPerDegree is the V4L2 internal unit: 1 degree = 3600 V4L2 units.
const v4l2UnitsPerDegree = 3600

type ptzAxisInfo struct {
	Min        int
	Max        int
	Label      string
	Unit       string
	V4L2Ctrl   string
	Multiplier int
	// Invert negates the value at the v4l2 boundary. On EMEET PIXY
	// the V4L2 pan_absolute sign convention is the opposite of the
	// camera's physical movement: a positive pan_absolute drives the
	// camera left. Inverting at the boundary lets the daemon present
	// "slider-right = camera-right" everywhere above this layer.
	Invert bool
}

//nolint:gochecknoglobals
var ptzAxes = map[string]ptzAxisInfo{
	pixy.AxisPan: {
		Min: pixy.PanMin, Max: pixy.PanMax, Label: "Pan", Unit: "\u00b0",
		V4L2Ctrl: "pan_absolute", Multiplier: v4l2UnitsPerDegree, Invert: true,
	},
	pixy.AxisTilt: {
		Min: pixy.TiltMin, Max: pixy.TiltMax, Label: "Tilt", Unit: "\u00b0",
		V4L2Ctrl: "tilt_absolute", Multiplier: v4l2UnitsPerDegree,
	},
	pixy.AxisZoom: {
		Min: pixy.ZoomMin, Max: pixy.ZoomMax, Label: "Zoom", Unit: "x",
		V4L2Ctrl: "zoom_absolute", Multiplier: 1,
	},
}

// ptzAxisOrder defines the deterministic order for V4L2 control listing.
//
//nolint:gochecknoglobals
var ptzAxisOrder = []string{pixy.AxisPan, pixy.AxisTilt, pixy.AxisZoom}

// v4l2CtrlToAxis maps V4L2 control names back to PTZ axis names.
//
//nolint:gochecknoglobals
var v4l2CtrlToAxis = buildCtrlToAxis()

func buildCtrlToAxis() map[string]string {
	m := make(map[string]string, len(ptzAxes))
	for axis, info := range ptzAxes {
		m[info.V4L2Ctrl] = axis
	}

	return m
}

func ptzAxisValid(axis string) bool {
	_, ok := ptzAxes[axis]

	return ok
}

func ptzAxisValue(axis string, status webStatus) int {
	return status.Get(axis)
}

func clampInt(v, lo, hi int) int {
	return max(lo, min(hi, v))
}

func v4l2Set(ctx context.Context, dev, ctrl, value string) error {
	err := exec.CommandContext(ctx, v4l2ctl, "-d", dev, "--set-ctrl="+ctrl+"="+value).
		Run()
	if err != nil {
		return fmt.Errorf("v4l2Set %s=%s on %s: %w", ctrl, value, dev, err)
	}

	return nil
}

// v4l2GetCtrlList returns the comma-separated list of V4L2 control names for v4l2-ctl --get-ctrl.
func v4l2GetCtrlList() string {
	ctrls := make([]string, 0, len(ptzAxisOrder))
	for _, axis := range ptzAxisOrder {
		ctrls = append(ctrls, ptzAxes[axis].V4L2Ctrl)
	}

	return strings.Join(ctrls, ",")
}

func parsePTZValues(ctx context.Context, dev string) pixy.PTZValues {
	out, err := exec.CommandContext(
		ctx, v4l2ctl, "-d", dev,
		"--get-ctrl="+v4l2GetCtrlList(),
	).Output()
	if err != nil {
		//nolint:exhaustruct
		return pixy.PTZValues{}
	}

	var ptz pixy.PTZValues

	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		key, rawVal, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}

		v, parseErr := strconv.Atoi(strings.TrimSpace(rawVal))
		if parseErr != nil {
			continue
		}

		axis, found := v4l2CtrlToAxis[strings.TrimSpace(key)]
		if !found {
			continue
		}

		info := ptzAxes[axis]
		val := v / info.Multiplier
		if info.Invert {
			val = -val
		}
		ptz = ptz.Set(axis, val)
	}

	return ptz
}

// parsePTZLimits queries the driver for the actual min/max range of each
// PTZ control. UVC firmwares (and EMEET in particular) advertise narrower
// ranges than the daemon's hardcoded constants in some cases (e.g. zoom
// tops out at 150, not 400) and wider ones in others (tilt goes to +/-90,
// not +/-30). Pan/tilt values are converted from the v4l2 internal unit
// (degrees * v4l2UnitsPerDegree) back to degrees. A failure to invoke
// v4l2-ctl returns a zero-valued PTZLimits; callers fall back to the
// ptzAxes map's Min/Max.
func parsePTZLimits(ctx context.Context, dev string) pixy.PTZLimits {
	out, err := exec.CommandContext(
		ctx, v4l2ctl, "-d", dev, "--list-ctrls",
	).Output()
	if err != nil {
		//nolint:exhaustruct
		return pixy.PTZLimits{}
	}

	//nolint:exhaustruct
	var lim pixy.PTZLimits
	for line := range strings.SplitSeq(string(out), "\n") {
		switch {
		case strings.Contains(line, "pan_absolute"):
			if lo, hi, ok := extractV4L2MinMax(line); ok {
				lim.Pan = [2]int{lo / v4l2UnitsPerDegree, hi / v4l2UnitsPerDegree}
			}
		case strings.Contains(line, "tilt_absolute"):
			if lo, hi, ok := extractV4L2MinMax(line); ok {
				lim.Tilt = [2]int{lo / v4l2UnitsPerDegree, hi / v4l2UnitsPerDegree}
			}
		case strings.Contains(line, "zoom_absolute"):
			if lo, hi, ok := extractV4L2MinMax(line); ok {
				lim.Zoom = [2]int{lo, hi}
			}
		}
	}

	return lim
}

// extractV4L2MinMax pulls the "min=N max=N" pair from a single v4l2-ctl
// --list-ctrls output line. Lines look like:
//
//	pan_absolute 0x009a0908 (int) : min=-540000 max=540000 step=3600 ...
//
// Split out so it can be unit-tested without spawning v4l2-ctl.
func extractV4L2MinMax(line string) (int, int, bool) {
	lo, ok := extractV4L2Int(line, "min=")
	if !ok {
		return 0, 0, false
	}
	hi, ok := extractV4L2Int(line, "max=")
	if !ok {
		return 0, 0, false
	}

	return lo, hi, true
}

func extractV4L2Int(line, prefix string) (int, bool) {
	idx := strings.Index(line, prefix)
	if idx < 0 {
		return 0, false
	}
	rest := line[idx+len(prefix):]
	end := strings.IndexAny(rest, " \t")
	if end < 0 {
		end = len(rest)
	}
	v, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, false
	}

	return v, true
}
