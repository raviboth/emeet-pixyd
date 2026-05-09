package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
)

const iconSize = 24

var (
	iconTracking = mustIcon("tracking")
	iconIdle     = mustIcon("idle")
	iconPrivacy  = mustIcon("privacy")
	iconActive   = mustIcon("active")
	iconOffline  = mustIcon("offline")
)

func mustIcon(state string) []byte {
	img := image.NewRGBA(image.Rect(0, 0, iconSize, iconSize))
	c := stateColor(state)
	drawCameraGlyph(img, c, state == "privacy")

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return solidPNG(c)
	}
	return buf.Bytes()
}

func stateColor(state string) color.RGBA {
	switch state {
	case "tracking":
		return color.RGBA{R: 0x2e, G: 0xcc, B: 0x71, A: 0xff}
	case "idle":
		return color.RGBA{R: 0xf3, G: 0x9c, B: 0x12, A: 0xff}
	case "privacy":
		return color.RGBA{R: 0xe7, G: 0x4c, B: 0x3c, A: 0xff}
	case "active":
		return color.RGBA{R: 0x34, G: 0x98, B: 0xdb, A: 0xff}
	default:
		return color.RGBA{R: 0x95, G: 0xa5, B: 0xa6, A: 0xff}
	}
}

// drawCameraGlyph renders a stylised camera silhouette onto img. The body is a
// rounded rectangle with a circular lens; for privacy a diagonal slash is laid
// across the lens to suggest a closed shutter.
func drawCameraGlyph(img *image.RGBA, fg color.RGBA, slash bool) {
	bg := color.RGBA{0, 0, 0, 0}
	for y := 0; y < iconSize; y++ {
		for x := 0; x < iconSize; x++ {
			img.SetRGBA(x, y, bg)
		}
	}

	// Body rect: x in [3, 21), y in [7, 19).
	for y := 7; y < 19; y++ {
		for x := 3; x < 21; x++ {
			img.SetRGBA(x, y, fg)
		}
	}
	// Viewfinder bump on top.
	for y := 4; y < 7; y++ {
		for x := 8; x < 14; x++ {
			img.SetRGBA(x, y, fg)
		}
	}
	// Lens hole (background) centred at (12, 13) radius 4.
	hole := color.RGBA{0, 0, 0, 0}
	const cx, cy, r = 12, 13, 4
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= r*r {
				img.SetRGBA(x, y, hole)
			}
		}
	}
	// Inner lens dot (foreground) radius 1.
	for y := cy - 1; y <= cy+1; y++ {
		for x := cx - 1; x <= cx+1; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= 1 {
				img.SetRGBA(x, y, fg)
			}
		}
	}
	if slash {
		// Diagonal slash across the lens.
		for i := -7; i <= 7; i++ {
			x, y := cx+i, cy+i
			if x >= 0 && x < iconSize && y >= 0 && y < iconSize {
				img.SetRGBA(x, y, fg)
			}
			if x+1 < iconSize {
				img.SetRGBA(x+1, y, fg)
			}
		}
	}
}

func solidPNG(c color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, iconSize, iconSize))
	for y := 0; y < iconSize; y++ {
		for x := 0; x < iconSize; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func iconFor(state string, online bool) []byte {
	if !online {
		return iconOffline
	}
	switch state {
	case "tracking":
		return iconTracking
	case "idle":
		return iconIdle
	case "privacy":
		return iconPrivacy
	case "active":
		return iconActive
	}
	return iconOffline
}
