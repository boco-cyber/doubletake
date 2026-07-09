# Screen selection design

Date: 2026-07-08

## Problem

doubletake currently offers no control over *which* screen gets captured:

- On X11, `startX11Capture` always auto-crops to whatever `xrandr` reports as
  the "primary" output (or the first connected one) — see
  `internal/airplay/capture.go:214-231`.
- On Wayland, the xdg-desktop-portal shows its own native picker on first
  connect, then persists that choice via a restore token, so subsequent runs
  silently reuse the same monitor/window forever — `capture.go:702-733`.

Neither path lets a user pick a specific monitor on a multi-monitor system,
and neither offers a way to stream to a *virtual* monitor — a headless
display that extends the desktop purely so the Apple TV can act as a second
screen (Sidecar-style), rather than a mirror of an existing one.

## Goal

Add a **screen selection** setting: choose a specific physical monitor by
name, or request a virtual monitor that extends the desktop and gets
captured instead. This is a single daemon-wide setting, not per-target —
capture is already shared across all connected AirPlay receivers via
`BroadcastCapture` (`internal/daemon/daemon.go:708-778`), so one capture
source serves every active stream.

**Scope:** X11 gets full support — physical selection and virtual monitor
creation. Wayland gets physical-monitor reselection (forcing the OS picker
to reappear); true virtual-monitor creation on Wayland (via KWin) is called
out as follow-up work requiring its own investigation spike, not fully
speced here.

## CLI flags (`cmd/doubletake/main.go`)

| Flag | Default | Description |
|------|---------|-------------|
| `-screen` | `""` | Screen to capture. Empty = today's auto-detect-primary behavior (X11) / portal picker (Wayland), unchanged. An xrandr output name (e.g. `DP-3`) selects that physical monitor. The literal `virtual` requests a virtual monitor. |
| `-virtual-position` | `right` | Where the virtual monitor sits relative to the primary: `right`, `left`, `above`, or `below`. Only meaningful with `-screen virtual`. Kept as a separate flag rather than bundled into `-screen`'s value, to keep `-screen`'s grammar single-purpose. |
| `-list-screens` | `false` | Print available physical outputs (name, resolution, primary flag) and whether a virtual candidate was detected, then exit. |

## X11: physical monitor selection

Extends the existing xrandr-parsing code in `capture.go:337-420`:

- `parseXrandrGeometry` currently discards the output name. It's extended to
  also capture it, producing `MonitorInfo{Name, X, Y, Width, Height, Primary bool}`.
- New `listX11Monitors(display) []MonitorInfo` replaces the ad-hoc
  primary/first-connected scan and backs both `-list-screens` and monitor
  lookup by name.
- `startX11Capture` reads `ScreenID` from `CaptureConfig`:
  - Empty → unchanged auto-detect (today's primary/first-connected logic).
  - Named → looks up that exact connected output. If not found, capture
    fails immediately with an error listing the outputs that *are*
    available (no silent fallback to primary).

## X11: virtual monitor creation

- `findVirtualCandidate(display) (outputName string, ok bool)` scans
  `xrandr --query` for a **disconnected** output matching `^VIRTUAL\d+$`
  (NVIDIA's built-in convention — exposed by default on the proprietary
  driver) or `^DUMMY\d+$` (common `xf86-video-dummy` naming). First match
  wins.
- If no candidate is found, capture fails with an explicit error:
  *"No virtual/dummy output available. On NVIDIA this should be automatic
  (VIRTUAL1-8); on Intel/AMD you need `xf86-video-dummy` configured in
  xorg.conf (requires an X restart) — see README."* doubletake never writes
  to xorg.conf or prompts for a restart itself — that's out of scope.
- If found, resolution/fps mirror the capture's own FPS setting and the
  existing 1920x1080 default (matching the assumption already baked into
  auto-bitrate sizing, `capture.go:614`). A modeline is computed via `cvt`
  (standard, commonly already present), then applied with `xrandr --newmode`,
  `--addmode`, and `--output <name> --mode <mode> --pos <computed from
  -virtual-position + primary geometry>`.
- **Rollback on partial failure:** if any step in the setup sequence fails
  (e.g. `--addmode` succeeds but `--output` fails), doubletake runs
  best-effort teardown of whatever was already applied before returning the
  error, so a failed attempt never leaves a half-configured output behind.
- **Teardown** happens on last disconnect, mirroring `BroadcastCapture`'s
  existing create-on-demand/teardown-on-last-disconnect lifecycle
  (`daemon.go:708-778`): `xrandr --output <name> --off`, then
  `--delmode`/`--rmmode` to remove the added mode, restoring the output to
  its original disconnected state.
- The virtual output is then captured via `ximagesrc` exactly like a real
  monitor, using its geometry for cropping — no new capture-path code needed
  beyond monitor selection.

## Wayland: physical selection (this pass), virtual monitor (follow-up)

- The portal's `SelectSources` call (`capture.go:702-733`) doesn't accept a
  target monitor name — it always shows the OS's native picker, by
  sandboxing design. `-screen <name>` cannot force a specific monitor on
  Wayland the way it can on X11.
- What it *can* do: any non-empty `-screen` value (including `virtual`)
  clears the saved restore token before requesting the screencast, forcing
  the native picker to reappear so the user can reselect — versus today's
  silent reuse of the last saved choice. This is a documented platform gap,
  not a bug.
- `-screen virtual` on Wayland returns a clear "not yet supported on
  Wayland" error in v1. Follow-up work will investigate KWin's
  `kscreen-doctor` / D-Bus virtual-output management APIs; this is
  explicitly out of scope for this design.

## Daemon + `doubletake-ctl` protocol

- `daemon.Config` (`internal/daemon/daemon.go:71-83`) gains `ScreenID
  string` and `VirtualPosition string`, set once from CLI flags at startup —
  same pattern as `FPS`/`Bitrate`/`HWAccel` today.
- New request `Cmd: "screens"` lists physical outputs, virtual-candidate
  availability, and the currently configured `ScreenID`, mirroring the
  existing `devices` command's response shape.
- New request `Cmd: "screen-set", Target: "<name|virtual|auto>"` mutates
  `d.cfg.ScreenID` at runtime. Capture only reads `cfg.ScreenID` when
  starting a *new* `BroadcastCapture` (i.e. when no streams are active,
  `getOrStartBroadcastLocked`, `daemon.go:711`), so this is naturally safe:
  - No streams active → takes effect immediately for the next connect.
  - Streams active → **rejected** with an error ("stop streaming before
    changing screen"), rather than silently deferred or force-disconnecting.
    Predictable behavior, no surprise interruption of an active AirPlay
    session.
- `doubletake-ctl` gets two new subcommands: `screens` (list) and
  `screen-set <name>`.

## Plasmoid UI (`plasmoid/contents/ui/main.qml`)

This is the plasmoid's first settings surface (today it has none — no
`config.qml`).

- A screen-picker control in the header area of `fullRepresentation`
  (alongside the existing refresh/stop/mute buttons), showing the current
  screen choice. Populated by a new `runCtl(["screens"], "screens")` call
  added to the existing status/devices polling timer (`main.qml:203-213`).
- Selecting an entry calls `screen-set`. If the daemon rejects it (streaming
  active), the existing error-banner pattern (`main.qml:360-380`) surfaces
  the message.
- The dropdown lists physical outputs by name/resolution, plus — when a
  virtual candidate was detected — a "Virtual Screen (extend desktop)"
  entry.

## Error handling summary

| Situation | Behavior |
|---|---|
| `-screen <name>` on X11, name not connected | Capture fails immediately, error lists available outputs |
| `-screen virtual` on X11, no VIRTUAL*/DUMMY* candidate | Capture fails, error explains NVIDIA vs Intel/AMD requirements |
| `-screen virtual` on Wayland | Fails with "not yet supported on Wayland" |
| xrandr virtual setup fails partway | Best-effort rollback of already-applied xrandr state, then error |
| `screen-set` while streaming | Rejected with "stop streaming before changing screen" |

## Testing

- Unit tests for `parseXrandrGeometry`/`listX11Monitors` name extraction
  against sample `xrandr --query` output (connected, disconnected, primary,
  multi-monitor cases).
- Unit tests for `findVirtualCandidate` against sample xrandr output with
  and without `VIRTUAL*`/`DUMMY*` disconnected outputs present.
- Unit tests for the modeline/position computation given a primary geometry
  and each `-virtual-position` value.
- Daemon-level test for `screen-set` rejection while a stream is active vs.
  acceptance while idle.
- Manual verification on real hardware for the xrandr create/teardown
  sequence (not feasible to fully simulate in CI without an X server with a
  dummy/virtual driver available).
