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
	pixyVendorID  = "328f"
	pixyProductID = "00c0"
)

func isPixyName(name string) bool {
	return strings.Contains(name, "EMEET") ||
		strings.Contains(name, "Pixy") ||
		strings.Contains(name, "PIXY")
}

func readUSBVendorProduct(devicePath string) (vendor, product string) {
	if v, err := os.ReadFile(devicePath + "/id/vendor"); err == nil {
		if p, perr := os.ReadFile(devicePath + "/id/product"); perr == nil {
			return strings.TrimSpace(string(v)), strings.TrimSpace(string(p))
		}
	}

	for i := 0; i < 6; i++ {
		v, vErr := os.ReadFile(devicePath + "/idVendor")
		p, pErr := os.ReadFile(devicePath + "/idProduct")
		if vErr == nil && pErr == nil {
			return strings.TrimSpace(string(v)), strings.TrimSpace(string(p))
		}
		devicePath += "/.."
	}

	return "", ""
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

		vendor, product := readUSBVendorProduct(fmt.Sprintf("%s/%s/device", sysfsPath, name))

		if vendor == pixyVendorID && product == pixyProductID {
			return videoPath
		}
	}

	return ""
}

func hasPixyVendorProduct(ueventData []byte) bool {
	for line := range strings.SplitSeq(string(ueventData), "\n") {
		hidID, ok := strings.CutPrefix(line, "HID_ID=")
		if !ok {
			continue
		}

		parts := strings.Split(hidID, ":")
		if len(parts) != 3 {
			return false
		}

		vendor, vErr := strconv.ParseInt(parts[1], 16, 0)
		product, pErr := strconv.ParseInt(parts[2], 16, 0)
		expectedVendor, evErr := strconv.ParseInt(pixyVendorID, 16, 0)
		expectedProduct, epErr := strconv.ParseInt(pixyProductID, 16, 0)

		return vErr == nil && pErr == nil && evErr == nil && epErr == nil &&
			vendor == expectedVendor && product == expectedProduct
	}

	return false
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
				if isPixyName(hidName) && hasPixyVendorProduct(ueventData) {
					return hidrawPath
				}
			}
		}
	}

	return ""
}

func (d *Daemon) probeDevices() {
	d.videoDev = probeVideo4linux("/sys/class/video4linux")
	d.hidrawDev = probeHidraw("/sys/class/hidraw")

	if d.videoDev != "" && d.hidrawDev != "" {
		slog.Info("found PIXY device", "video", d.videoDev, "hidraw", d.hidrawDev)

		if d.state.Camera == pixy.StateOffline {
			d.state.Camera = pixy.StatePrivacy
		}

		d.refreshPTZLimits()
	} else {
		d.state.Camera = pixy.StateOffline
	}
}

// refreshPTZLimits queries the V4L2 driver for the actual PTZ ranges on
// the current video device and caches them. Bounded by a short timeout so
// a misbehaving driver cannot stall daemon probing.
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
// pixy package constants. Used by both the HTTP and command-grammar
// paths so the same range governs slider rendering and request
// clamping.
func (d *Daemon) effectivePTZLimits(axis string) (int, int) {
	d.ptzLimits.mu.RLock()
	lim := d.ptzLimits.values
	d.ptzLimits.mu.RUnlock()
	if lim.Has(axis) {
		return lim.For(axis)
	}
	return ptzLimits(axis)
}
