//go:build linux

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/LarsArtmann/emeet-pixyd/internal/pixy"
)

const (
	pixyVendorIDInt  = 0x328f
	pixyProductIDInt = 0x00c0
)

func isPixyName(name string) bool {
	return strings.Contains(name, "EMEET") ||
		strings.Contains(name, "Pixy") ||
		strings.Contains(name, "PIXY")
}

func matchesPixyID(ueventData []byte, prefix, sep string, vendorIdx, productIdx int) bool {
	for line := range strings.SplitSeq(string(ueventData), "\n") {
		value, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}

		parts := strings.Split(value, sep)
		if len(parts) <= max(vendorIdx, productIdx) {
			return false
		}

		vendor, vErr := strconv.ParseInt(parts[vendorIdx], 16, 0)
		product, pErr := strconv.ParseInt(parts[productIdx], 16, 0)

		return vErr == nil && pErr == nil &&
			vendor == int64(pixyVendorIDInt) && product == int64(pixyProductIDInt)
	}

	return false
}

func probeVideo4linux(sysfsPath string) string {
	entries, err := os.ReadDir(sysfsPath)
	if err != nil {
		return ""
	}

	for _, entry := range entries {
		name := entry.Name()

		videoPath := "/dev/" + name

		indexFile := fmt.Sprintf("%s/%s/index", sysfsPath, name)

		indexData, iErr := os.ReadFile(indexFile)
		if iErr == nil && strings.TrimSpace(string(indexData)) != "0" {
			continue
		}

		ueventFile := fmt.Sprintf("%s/%s/device/uevent", sysfsPath, name)

		ueventData, uErr := os.ReadFile(ueventFile)
		if uErr != nil {
			slog.Warn("video4linux probe: failed to read uevent", "path", ueventFile, "error", uErr)

			continue
		}

		if matchesPixyID(ueventData, "PRODUCT=", "/", 0, 1) {
			return videoPath
		}
	}

	return ""
}

func probeHidraw(sysfsPath string) string {
	entries, err := os.ReadDir(sysfsPath)
	if err != nil {
		return ""
	}

	for _, entry := range entries {
		name := entry.Name()

		hidrawPath := "/dev/" + name

		ueventFile := fmt.Sprintf("%s/%s/device/uevent", sysfsPath, name)

		ueventData, uErr := os.ReadFile(ueventFile)
		if uErr != nil {
			continue
		}

		for line := range strings.SplitSeq(string(ueventData), "\n") {
			if hidName, ok := strings.CutPrefix(line, "HID_NAME="); ok {
				if isPixyName(hidName) && matchesPixyID(ueventData, "HID_ID=", ":", 1, 2) {
					return hidrawPath
				}
			}
		}
	}

	return ""
}

type probeResult struct {
	VideoDev  string
	HidrawDev string
}

func probeDevices() probeResult {
	recordProbe()

	result := probeResult{
		VideoDev:  probeVideo4linux("/sys/class/video4linux"),
		HidrawDev: probeHidraw("/sys/class/hidraw"),
	}
	switch {
	case result.VideoDev != "" && result.HidrawDev != "":
		slog.Info("found PIXY device", "video", result.VideoDev, "hidraw", result.HidrawDev)
	case result.VideoDev != "" && result.HidrawDev == "":
		slog.Warn("partial PIXY device: video found but no hidraw", "video", result.VideoDev)
	case result.VideoDev == "" && result.HidrawDev != "":
		slog.Warn("partial PIXY device: hidraw found but no video", "hidraw", result.HidrawDev)
	}

	return result
}

func (d *Daemon) applyProbeResult(r probeResult) {
	d.videoDev = r.VideoDev
	d.hidrawDev = r.HidrawDev

	if r.HidrawDev != "" {
		d.hidDev = newHIDRawDevice(r.HidrawDev)
	} else {
		d.hidDev = nil
	}

	if r.VideoDev != "" && r.HidrawDev != "" {
		d.hidFailCount = 0
		if d.state.Camera == pixy.StateOffline {
			d.state.Camera = pixy.StatePrivacy
		}

		d.refreshPTZLimits()
	} else {
		d.state.Camera = pixy.StateOffline
	}
}

// refreshPTZLimits queries the V4L2 driver for the actual PTZ ranges on
// the current video device and caches them. Bounded by a short timeout
// so a misbehaving driver cannot stall daemon probing.
func (d *Daemon) refreshPTZLimits() {
	if d.videoDev == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lim := parsePTZLimits(ctx, d.videoDev)

	d.ptzLimits.mu.Lock()
	d.ptzLimits.values = lim
	d.ptzLimits.mu.Unlock()

	slog.Info("ptz limits probed",
		"device", d.videoDev,
		"pan", lim.Pan, "tilt", lim.Tilt, "zoom", lim.Zoom,
	)
}

// effectivePTZLimits returns the (min, max) for an axis, preferring the
// driver-reported values cached at probe time and falling back to the
// ptzAxes map's Min/Max. Used by both the HTTP and command-grammar
// paths so the same range governs slider rendering and request
// clamping.
func (d *Daemon) effectivePTZLimits(axis string) (int, int) {
	d.ptzLimits.mu.RLock()
	lim := d.ptzLimits.values
	d.ptzLimits.mu.RUnlock()
	if lim.Has(axis) {
		return lim.For(axis)
	}

	info, ok := ptzAxes[axis]
	if !ok {
		return 0, 0
	}

	return info.Min, info.Max
}
