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

const (
	v4l2DegreesPerUnit = 3600

	axisPan  = "pan"
	axisTilt = "tilt"
	axisZoom = "zoom"
)

type ptzValues struct {
	Pan  int
	Tilt int
	Zoom int
}

func v4l2Set(ctx context.Context, dev, ctrl, value string) error {
	err := exec.CommandContext(ctx, "v4l2-ctl", "-d", dev, "--set-ctrl="+ctrl+"="+value).
		Run()
	if err != nil {
		return fmt.Errorf("v4l2Set %s=%s on %s: %w", ctrl, value, dev, err)
	}

	return nil
}

func v4l2SetMultiple(ctx context.Context, dev string, controls map[string]string) error {
	args := make([]string, 2, 2+len(controls))
	args[0] = "-d"
	args[1] = dev
	for ctrl, value := range controls {
		args = append(args, "--set-ctrl="+ctrl+"="+value)
	}

	err := exec.CommandContext(ctx, "v4l2-ctl", args...).Run()
	if err != nil {
		return fmt.Errorf("v4l2SetMultiple on %s: %w", dev, err)
	}

	return nil
}

// parsePTZLimits queries the driver for the actual min/max range of each
// PTZ control. UVC firmwares (and EMEET in particular) advertise narrower
// ranges than the daemon's hardcoded constants in some cases (e.g. zoom
// tops out at 150, not 400) and wider ones in others (tilt goes to +/-90,
// not +/-30). Pan/tilt values are converted from the v4l2 internal unit
// (degrees * 3600) back to degrees. A failure to invoke v4l2-ctl returns
// a zero-valued PTZLimits; callers should then fall back to the
// package-level lowercase constants.
func parsePTZLimits(ctx context.Context, dev string) pixy.PTZLimits {
	out, err := exec.CommandContext(
		ctx, "v4l2-ctl", "-d", dev, "--list-ctrls",
	).Output()
	if err != nil {
		//nolint:exhaustruct
		return pixy.PTZLimits{}
	}

	var lim pixy.PTZLimits
	for line := range strings.SplitSeq(string(out), "\n") {
		switch {
		case strings.Contains(line, "pan_absolute"):
			if lo, hi, ok := extractV4L2MinMax(line); ok {
				lim.Pan = [2]int{lo / v4l2DegreesPerUnit, hi / v4l2DegreesPerUnit}
			}
		case strings.Contains(line, "tilt_absolute"):
			if lo, hi, ok := extractV4L2MinMax(line); ok {
				lim.Tilt = [2]int{lo / v4l2DegreesPerUnit, hi / v4l2DegreesPerUnit}
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
// The function is split out so it can be unit-tested without spawning
// v4l2-ctl.
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

func parsePTZValues(ctx context.Context, dev string) ptzValues {
	out, err := exec.CommandContext(
		ctx, "v4l2-ctl", "-d", dev,
		"--get-ctrl=pan_absolute,tilt_absolute,zoom_absolute",
	).Output()
	if err != nil {
		//nolint:exhaustruct
		return ptzValues{}
	}

	var ptz ptzValues

	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}

		v, parseErr := strconv.Atoi(strings.TrimSpace(val))
		if parseErr != nil {
			continue
		}

		switch strings.TrimSpace(key) {
		case "pan_absolute":
			ptz.Pan = -v / v4l2DegreesPerUnit
		case "tilt_absolute":
			ptz.Tilt = v / v4l2DegreesPerUnit
		case "zoom_absolute":
			ptz.Zoom = v
		}
	}

	return ptz
}
