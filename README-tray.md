# emeet-pixy-tray

A KDE Plasma StatusNotifierItem (system-tray) indicator for the running
`emeet-pixyd` daemon. Shows the current camera state with a coloured icon and
exposes the most-used controls (Track / Idle / Privacy / Center / Reset / Sync)
as menu actions, without requiring the web UI.

## Build

```sh
go build -o emeet-pixy-tray ./cmd/emeet-pixy-tray
```

No CGO required. Pulls in `fyne.io/systray`, which talks to the panel over
`org.kde.StatusNotifierItem` D-Bus, so it works natively on Wayland under
KDE Plasma 6.

## Install

```sh
sudo install -m 755 emeet-pixy-tray /usr/local/bin/
install -m 644 contrib/emeet-pixy-tray.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now emeet-pixy-tray.service
```

The unit is `PartOf=graphical-session.target` so it stops cleanly on logout.

## Run manually

```sh
emeet-pixy-tray --url http://127.0.0.1:8090 --log info
```

Flags:

- `--url`: daemon base URL (default `http://127.0.0.1:8090`).
- `--log`: `debug`, `info`, `warn`, `error` (default `info`).

## How it works

- Subscribes to `/api/events` (Server-Sent Events) for live state updates.
  On every `state` event the tray icon and tooltip are refreshed.
- Falls back to a 60s `GET /panel` heartbeat so a silent SSE drop still flips
  the icon to "offline" until the SSE reconnect succeeds.
- Reconnect uses exponential backoff (1s starting, 30s cap).
- All menu callbacks fire HTTP POSTs in their own goroutines so the UI thread
  never blocks on a slow daemon response.

## Icon palette

| State    | Colour |
|----------|--------|
| tracking | green  |
| idle     | amber  |
| privacy  | red, with shutter slash |
| active   | blue   |
| offline  | grey   |

Icons are generated programmatically at startup as 24x24 PNGs (KDE's SNI
implementation expects raster pixmaps, not SVG).
