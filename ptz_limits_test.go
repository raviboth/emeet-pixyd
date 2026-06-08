//go:build linux

package main

import (
	"testing"

	"github.com/LarsArtmann/emeet-pixyd/internal/pixy"
)

func TestExtractV4L2MinMax(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		line string
		lo   int
		hi   int
		ok   bool
	}{
		{
			name: "pan",
			line: "                   pan_absolute 0x009a0908 (int)    : min=-540000 max=540000 step=3600 default=0 value=0 flags=has-min-max",
			lo:   -540000, hi: 540000, ok: true,
		},
		{
			name: "zoom narrow",
			line: "                  zoom_absolute 0x009a090d (int)    : min=100 max=150 step=1 default=100 value=136 flags=has-min-max",
			lo:   100, hi: 150, ok: true,
		},
		{
			name: "missing fields",
			line: "                   pan_absolute (int) : default=0 value=0",
			ok:   false,
		},
		{
			name: "garbage",
			line: "",
			ok:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lo, hi, ok := extractV4L2MinMax(tc.line)
			if ok != tc.ok || (ok && (lo != tc.lo || hi != tc.hi)) {
				t.Errorf("extractV4L2MinMax = (%d, %d, %v), want (%d, %d, %v)",
					lo, hi, ok, tc.lo, tc.hi, tc.ok)
			}
		})
	}
}

func TestPTZLimitsHasAndFor(t *testing.T) {
	t.Parallel()

	lim := pixy.PTZLimits{
		Pan:  [2]int{-150, 150},
		Tilt: [2]int{0, 0},
		Zoom: [2]int{100, 150},
	}

	if !lim.Has(pixy.AxisPan) || !lim.Has(pixy.AxisZoom) {
		t.Error("expected pan and zoom to be set")
	}
	if lim.Has(pixy.AxisTilt) {
		t.Error("tilt range is zero; Has should return false")
	}
	if lo, hi := lim.For(pixy.AxisPan); lo != -150 || hi != 150 {
		t.Errorf("pan = (%d, %d), want (-150, 150)", lo, hi)
	}
	if lo, hi := lim.For("nope"); lo != 0 || hi != 0 {
		t.Errorf("unknown axis = (%d, %d), want (0, 0)", lo, hi)
	}
}

func TestEffectivePTZLimits_FallbackToAxesMap(t *testing.T) {
	t.Parallel()

	// A daemon with no probed limits falls back to ptzAxes[axis].Min/Max
	// so first-boot behaviour (before probe) matches the historical
	// contract.
	//nolint:exhaustruct
	d := &Daemon{}

	lo, hi := d.effectivePTZLimits(pixy.AxisZoom)
	if lo != ptzAxes[pixy.AxisZoom].Min || hi != ptzAxes[pixy.AxisZoom].Max {
		t.Errorf("zoom fallback = (%d, %d), want (%d, %d)",
			lo, hi, ptzAxes[pixy.AxisZoom].Min, ptzAxes[pixy.AxisZoom].Max)
	}
}

func TestEffectivePTZLimits_PrefersProbedValues(t *testing.T) {
	t.Parallel()

	//nolint:exhaustruct
	d := &Daemon{}
	d.ptzLimits.values = pixy.PTZLimits{
		Pan:  [2]int{-150, 150},
		Tilt: [2]int{-90, 90},
		Zoom: [2]int{100, 150},
	}

	lo, hi := d.effectivePTZLimits(pixy.AxisZoom)
	if lo != 100 || hi != 150 {
		t.Errorf("zoom probed = (%d, %d), want (100, 150)", lo, hi)
	}
	lo, hi = d.effectivePTZLimits(pixy.AxisTilt)
	if lo != -90 || hi != 90 {
		t.Errorf("tilt probed = (%d, %d), want (-90, 90)", lo, hi)
	}
}
