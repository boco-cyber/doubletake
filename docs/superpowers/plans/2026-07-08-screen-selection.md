# Screen Selection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let users choose which screen doubletake broadcasts — a specific physical monitor by name, or a virtual monitor that extends the desktop — via a `-screen` flag, `doubletake-ctl` subcommands, and a plasmoid picker.

**Architecture:** `CaptureConfig.ScreenID`/`VirtualPosition` flow from CLI flags (standalone mode) or `daemon.Config` (daemon mode) into `internal/airplay/capture.go`'s X11 capture path, which resolves the requested screen to a crop region — auto-detecting, looking up a named xrandr output, or creating/tearing down a virtual output via `xrandr`/`cvt`. Wayland gets a narrower change: forcing the portal's picker to reappear on screen-change requests, since the portal API has no by-name selection. The daemon exposes the setting over its existing JSON/Unix-socket protocol (`screens`, `screen-set` commands), which `doubletake-ctl` and the plasmoid consume.

**Tech Stack:** Go 1.25, GStreamer (`gst-launch-1.0`), X11 `xrandr` + `cvt` CLI tools, xdg-desktop-portal over D-Bus (existing), QML/Kirigami (Plasma 6 plasmoid).

## Global Constraints

- Spec: `docs/superpowers/specs/2026-07-08-screen-selection-design.md`.
- Scope: X11 gets full support (physical selection + virtual monitor). Wayland gets physical-monitor reselection only in this plan; Wayland virtual-monitor creation is explicitly out of scope (returns an error).
- Virtual monitor resolution is fixed at 1920x1080 (`virtualScreenWidth`/`virtualScreenHeight` constants) and refresh rate mirrors the capture's own FPS setting — not independently configurable in this plan.
- Virtual monitor candidates are detected by matching disconnected xrandr outputs named `VIRTUAL\d+` (NVIDIA default) or `DUMMY\d+` (`xf86-video-dummy` convention). doubletake never writes to `xorg.conf` or prompts for an X restart.
- If virtual-monitor xrandr setup fails partway, already-applied xrandr state is rolled back before returning the error.
- `-virtual-position` accepts exactly `left`, `right`, `above`, `below`; default `right`. It is a separate flag from `-screen`, not bundled into its value.
- Screen selection is one setting per daemon (capture is already shared across all targets via `BroadcastCapture`), not per-target.
- `doubletake-ctl screen-set` is rejected with an error ("stop streaming before changing screen") whenever any stream is active — it never force-disconnects.
- Named-screen lookups that don't match a connected output fail immediately with an error listing available names — no silent fallback to auto-detect.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/airplay/capture.go` | `MonitorInfo` type, xrandr parsing/enumeration, named/virtual screen resolution, virtual monitor create/teardown, `CaptureConfig` fields, X11 + Wayland capture wiring |
| `internal/airplay/capture_test.go` | Unit tests for all pure parsing/computation helpers added above |
| `cmd/doubletake/main.go` | `-screen`/`-virtual-position`/`-list-screens` flags, `printAvailableScreens`, `runDaemon` plumbing |
| `internal/daemon/daemon.go` | `Config.ScreenID`/`VirtualPosition`, `ScreenInfo` type, `Response.Screens`/`CurrentScreen`, `screens`/`screen-set` command handlers |
| `internal/daemon/daemon_test.go` (new) | Unit tests for the new daemon handlers |
| `internal/daemon/daemonclient/client.go` | `Screens()`/`ScreenSet()` client methods |
| `internal/daemon/daemonclient/daemonclient_test.go` (new) | Integration test: real daemon + client round trip for the new commands |
| `cmd/doubletake-ctl/main.go` | `screens`/`screen-set` subcommands + usage text |
| `plasmoid/contents/ui/main.qml` | Screen-picker menu, `screens` polling, `screen-set` action |

---

### Task 1: X11 monitor enumeration

**Files:**
- Modify: `internal/airplay/capture.go:333-420` (replace `detectPrimaryMonitor` body, keep `parseXrandrGeometry` as-is, add new code above it)
- Test: `internal/airplay/capture_test.go`

**Interfaces:**
- Consumes: existing `parseXrandrGeometry(line string) (xOffset, yOffset, width, height int, ok bool)` (unchanged, `capture.go:388-420`)
- Produces: `type MonitorInfo struct { Name string; Connected bool; Primary bool; X, Y, Width, Height int }`, `func ListX11Monitors(display string) ([]MonitorInfo, error)`, unexported `func primaryOrFirstConnected(monitors []MonitorInfo) (MonitorInfo, bool)` — both used by later tasks.

- [ ] **Step 1: Write the failing tests**

Add to `internal/airplay/capture_test.go`:

```go
func TestParseXrandrOutputs(t *testing.T) {
	sample := `Screen 0: minimum 320 x 200, current 3840 x 1080, maximum 16384 x 16384
DP-3 connected primary 1920x1080+0+0 (normal left inverted right x axis y axis) 531mm x 299mm
   1920x1080     60.00*+  59.94
DP-1 connected 1920x1080+1920+0 (normal left inverted right x axis y axis) 531mm x 299mm
   1920x1080     60.00 +
HDMI-1 disconnected (normal left inverted right x axis y axis)
VIRTUAL1 disconnected (normal left inverted right x axis y axis)
`
	monitors := parseXrandrOutputs(sample)

	want := []MonitorInfo{
		{Name: "DP-3", Connected: true, Primary: true, X: 0, Y: 0, Width: 1920, Height: 1080},
		{Name: "DP-1", Connected: true, Primary: false, X: 1920, Y: 0, Width: 1920, Height: 1080},
		{Name: "HDMI-1", Connected: false},
		{Name: "VIRTUAL1", Connected: false},
	}
	if len(monitors) != len(want) {
		t.Fatalf("got %d monitors, want %d: %+v", len(monitors), len(want), monitors)
	}
	for i, w := range want {
		if monitors[i] != w {
			t.Fatalf("monitor %d = %+v, want %+v", i, monitors[i], w)
		}
	}
}

func TestPrimaryOrFirstConnected(t *testing.T) {
	// Primary present picks primary even if listed second.
	monitors := []MonitorInfo{
		{Name: "DP-1", Connected: true, X: 1920, Y: 0, Width: 1920, Height: 1080},
		{Name: "DP-3", Connected: true, Primary: true, X: 0, Y: 0, Width: 1920, Height: 1080},
	}
	got, ok := primaryOrFirstConnected(monitors)
	if !ok || got.Name != "DP-3" {
		t.Fatalf("primaryOrFirstConnected() = %+v, %v, want DP-3, true", got, ok)
	}

	// No primary falls back to first connected.
	monitors = []MonitorInfo{
		{Name: "HDMI-1", Connected: false},
		{Name: "DP-1", Connected: true, X: 0, Y: 0, Width: 2560, Height: 1440},
	}
	got, ok = primaryOrFirstConnected(monitors)
	if !ok || got.Name != "DP-1" {
		t.Fatalf("primaryOrFirstConnected() = %+v, %v, want DP-1, true", got, ok)
	}

	// Nothing connected.
	if _, ok := primaryOrFirstConnected(nil); ok {
		t.Fatalf("primaryOrFirstConnected(nil) ok = true, want false")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/airplay/ -run 'TestParseXrandrOutputs|TestPrimaryOrFirstConnected' -v`
Expected: FAIL with `undefined: parseXrandrOutputs` / `undefined: MonitorInfo` / `undefined: primaryOrFirstConnected`

- [ ] **Step 3: Implement**

In `internal/airplay/capture.go`, insert the following immediately before the existing `detectPrimaryMonitor` function (currently at line 333):

```go
// MonitorInfo describes one X11 RandR output as reported by xrandr.
type MonitorInfo struct {
	Name      string
	Connected bool
	Primary   bool
	X, Y      int
	Width, Height int
}

// ListX11Monitors queries xrandr for every output (connected and
// disconnected) on the given X display.
func ListX11Monitors(display string) ([]MonitorInfo, error) {
	out, err := exec.Command("xrandr", "--display", display, "--query").Output()
	if err != nil {
		return nil, fmt.Errorf("xrandr query: %w", err)
	}
	return parseXrandrOutputs(string(out)), nil
}

// parseXrandrOutputs parses the text of `xrandr --query` into one
// MonitorInfo per output line, skipping mode lines and the leading
// "Screen 0: ..." line.
func parseXrandrOutputs(output string) []MonitorInfo {
	var monitors []MonitorInfo
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		switch {
		case strings.Contains(line, " connected"):
			mi := MonitorInfo{Name: name, Connected: true, Primary: strings.Contains(line, " primary ")}
			if x, y, w, h, ok := parseXrandrGeometry(line); ok {
				mi.X, mi.Y, mi.Width, mi.Height = x, y, w, h
			}
			monitors = append(monitors, mi)
		case strings.Contains(line, " disconnected"):
			monitors = append(monitors, MonitorInfo{Name: name, Connected: false})
		}
	}
	return monitors
}

// primaryOrFirstConnected returns the primary connected monitor with known
// geometry, or failing that the first connected monitor with known
// geometry.
func primaryOrFirstConnected(monitors []MonitorInfo) (MonitorInfo, bool) {
	for _, m := range monitors {
		if m.Connected && m.Primary && m.Width > 0 && m.Height > 0 {
			return m, true
		}
	}
	for _, m := range monitors {
		if m.Connected && m.Width > 0 && m.Height > 0 {
			return m, true
		}
	}
	return MonitorInfo{}, false
}
```

Then replace the body of `detectPrimaryMonitor` (the function currently spanning `capture.go:333-384`) with:

```go
// detectPrimaryMonitor queries xrandr to find the primary monitor's geometry.
// Returns (startX, startY, endX, endY) bounding the primary monitor, where
// endX = startX + monitor_width and endY = startY + monitor_height. If
// detection fails it returns all zeros, meaning no cropping should be applied.
func detectPrimaryMonitor(display string) (startX, startY, endX, endY int) {
	monitors, err := ListX11Monitors(display)
	if err != nil {
		dbg("[CAPTURE] xrandr failed: %v, skipping monitor crop", err)
		return 0, 0, 0, 0
	}
	m, ok := primaryOrFirstConnected(monitors)
	if !ok {
		dbg("[CAPTURE] couldn't parse xrandr output, skipping monitor crop")
		return 0, 0, 0, 0
	}
	dbg("[CAPTURE] primary monitor: %dx%d at +%d+%d", m.Width, m.Height, m.X, m.Y)
	return m.X, m.Y, m.X + m.Width, m.Y + m.Height
}
```

Leave `parseXrandrGeometry` (now the following function) untouched.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/airplay/ -run 'TestParseXrandrOutputs|TestPrimaryOrFirstConnected' -v`
Expected: PASS

- [ ] **Step 5: Run the full package test suite to check nothing broke**

Run: `go test ./internal/airplay/ -v`
Expected: PASS (all existing tests, e.g. `TestRecommendedBitrateKbps`, `TestBuildWaylandGstArgsSkipsMissingVapostproc`, still pass)

- [ ] **Step 6: Commit**

```bash
git add internal/airplay/capture.go internal/airplay/capture_test.go
git commit -m "airplay: extract MonitorInfo/ListX11Monitors from detectPrimaryMonitor"
```

---

### Task 2: X11 named-screen selection

**Files:**
- Modify: `internal/airplay/capture.go` (`CaptureConfig` struct; new `findMonitorByName`, `describeConnectedNames`, `resolveX11CaptureRegion`; wire into `startX11Capture`)
- Test: `internal/airplay/capture_test.go`

**Interfaces:**
- Consumes: `MonitorInfo`, `ListX11Monitors`, `primaryOrFirstConnected` (Task 1), `detectPrimaryMonitor` (existing)
- Produces: `CaptureConfig.ScreenID string`, `CaptureConfig.VirtualPosition string` (fields — `VirtualPosition` unused until Task 5, but added now to avoid a second struct edit), `func resolveX11CaptureRegion(display string, cfg CaptureConfig) (startX, startY, endX, endY int, err error)` — Task 5 will change this signature to add a cleanup return value.

- [ ] **Step 1: Write the failing tests**

Add to `internal/airplay/capture_test.go`:

```go
func TestFindMonitorByName(t *testing.T) {
	monitors := []MonitorInfo{
		{Name: "DP-3", Connected: true, Primary: true, X: 0, Y: 0, Width: 1920, Height: 1080},
		{Name: "DP-1", Connected: true, X: 1920, Y: 0, Width: 2560, Height: 1440},
		{Name: "HDMI-1", Connected: false},
	}

	m, ok := findMonitorByName(monitors, "DP-1")
	if !ok {
		t.Fatalf("findMonitorByName(DP-1) not found")
	}
	if m.X != 1920 || m.Y != 0 || m.Width != 2560 || m.Height != 1440 {
		t.Fatalf("findMonitorByName(DP-1) = %+v, unexpected geometry", m)
	}

	if _, ok := findMonitorByName(monitors, "HDMI-1"); ok {
		t.Fatalf("findMonitorByName(HDMI-1) unexpectedly found (disconnected)")
	}
	if _, ok := findMonitorByName(monitors, "DP-9"); ok {
		t.Fatalf("findMonitorByName(DP-9) unexpectedly found (nonexistent)")
	}
}

func TestDescribeConnectedNames(t *testing.T) {
	monitors := []MonitorInfo{
		{Name: "DP-3", Connected: true},
		{Name: "HDMI-1", Connected: false},
		{Name: "DP-1", Connected: true},
	}
	if got := describeConnectedNames(monitors); got != "DP-3, DP-1" {
		t.Fatalf("describeConnectedNames() = %q, want %q", got, "DP-3, DP-1")
	}
	if got := describeConnectedNames(nil); got != "(none detected)" {
		t.Fatalf("describeConnectedNames(nil) = %q, want %q", got, "(none detected)")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/airplay/ -run 'TestFindMonitorByName|TestDescribeConnectedNames' -v`
Expected: FAIL with `undefined: findMonitorByName` / `undefined: describeConnectedNames`

- [ ] **Step 3: Implement**

In `internal/airplay/capture.go`, add `ScreenID` and `VirtualPosition` to `CaptureConfig` (currently `capture.go:18-26`):

```go
// CaptureConfig holds screen capture settings.
type CaptureConfig struct {
	FPS     int
	Bitrate int    // Video bitrate in kbps (0 = auto)
	HWAccel string // "auto", "vaapi", "none"

	// ScreenID selects what to capture: "" auto-detects the primary monitor
	// (X11) or shows the portal's picker (Wayland); a name selects that
	// connected X11 output; "virtual" requests a virtual extended-desktop
	// monitor (X11 only).
	ScreenID string
	// VirtualPosition places a "virtual" ScreenID relative to the primary
	// monitor: "left", "right", "above", or "below". Defaults to "right".
	VirtualPosition string

	RestoreToken     string
	SaveRestoreToken func(string) error
}
```

Add the following new functions after `primaryOrFirstConnected` (added in Task 1):

```go
// findMonitorByName returns the connected monitor with the given output
// name, if any.
func findMonitorByName(monitors []MonitorInfo, name string) (MonitorInfo, bool) {
	for _, m := range monitors {
		if m.Connected && m.Name == name {
			return m, true
		}
	}
	return MonitorInfo{}, false
}

// describeConnectedNames returns a comma-separated list of connected output
// names, for error messages when a requested screen isn't found.
func describeConnectedNames(monitors []MonitorInfo) string {
	var names []string
	for _, m := range monitors {
		if m.Connected {
			names = append(names, m.Name)
		}
	}
	if len(names) == 0 {
		return "(none detected)"
	}
	return strings.Join(names, ", ")
}

// resolveX11CaptureRegion determines which region of the X screen to
// capture based on cfg.ScreenID: empty auto-detects the primary monitor
// (preserving prior behavior), any other value selects that connected
// output by name (Task 5 adds a "virtual" case here).
func resolveX11CaptureRegion(display string, cfg CaptureConfig) (startX, startY, endX, endY int, err error) {
	if cfg.ScreenID == "" {
		startX, startY, endX, endY = detectPrimaryMonitor(display)
		return startX, startY, endX, endY, nil
	}

	monitors, err := ListX11Monitors(display)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("list screens: %w", err)
	}

	m, ok := findMonitorByName(monitors, cfg.ScreenID)
	if !ok {
		return 0, 0, 0, 0, fmt.Errorf("screen %q not found; available screens: %s", cfg.ScreenID, describeConnectedNames(monitors))
	}
	return m.X, m.Y, m.X + m.Width, m.Y + m.Height, nil
}
```

Now wire it into `startX11Capture`. Replace this block (currently `capture.go:214-218`):

```go
	// Detect primary monitor geometry — ximagesrc captures the full X screen
	// (all monitors combined). On multi-monitor setups this wastes CPU on pixels
	// we don't need, so crop to the primary monitor. The encoded resolution is
	// then the primary monitor's native resolution (no rescaling).
	startX, startY, endX, endY := detectPrimaryMonitor(display)
```

with:

```go
	// Determine which region of the X screen to capture. ximagesrc captures
	// the full X screen (all monitors combined) by default, so we crop to a
	// single monitor's geometry — auto-detected (empty ScreenID, preserving
	// the original behavior) or explicitly named. The encoded resolution is
	// then that monitor's native resolution (no rescaling).
	startX, startY, endX, endY, err := resolveX11CaptureRegion(display, cfg)
	if err != nil {
		cancel()
		return nil, err
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/airplay/ -run 'TestFindMonitorByName|TestDescribeConnectedNames' -v`
Expected: PASS

- [ ] **Step 5: Build and run the full package test suite**

Run: `go build ./... && go test ./internal/airplay/ -v`
Expected: build succeeds, all tests PASS. (`startX11Capture` now shadows `err` via `:=` — confirm no "err declared and not used" or redeclaration errors; `go build` catches this.)

- [ ] **Step 6: Commit**

```bash
git add internal/airplay/capture.go internal/airplay/capture_test.go
git commit -m "airplay: support selecting an X11 screen by name via CaptureConfig.ScreenID"
```

---

### Task 3: Virtual monitor candidate detection

**Files:**
- Modify: `internal/airplay/capture.go` (add `regexp` import, `FindVirtualCandidate`)
- Test: `internal/airplay/capture_test.go`

**Interfaces:**
- Consumes: `MonitorInfo` (Task 1)
- Produces: `func FindVirtualCandidate(monitors []MonitorInfo) (string, bool)` — used by Task 5 (capture wiring), Task 7 (`-list-screens`), and Task 8 (`screens` daemon command).

- [ ] **Step 1: Write the failing tests**

Add to `internal/airplay/capture_test.go`:

```go
func TestFindVirtualCandidate(t *testing.T) {
	tests := []struct {
		name     string
		monitors []MonitorInfo
		want     string
		wantOK   bool
	}{
		{
			name:     "nvidia virtual output",
			monitors: []MonitorInfo{{Name: "DP-3", Connected: true}, {Name: "VIRTUAL1", Connected: false}},
			want:     "VIRTUAL1",
			wantOK:   true,
		},
		{
			name:     "dummy driver output",
			monitors: []MonitorInfo{{Name: "DUMMY0", Connected: false}},
			want:     "DUMMY0",
			wantOK:   true,
		},
		{
			name:     "connected virtual-named output is not a candidate",
			monitors: []MonitorInfo{{Name: "VIRTUAL1", Connected: true}},
			wantOK:   false,
		},
		{
			name:     "unrelated disconnected output is not a candidate",
			monitors: []MonitorInfo{{Name: "HDMI-1", Connected: false}},
			wantOK:   false,
		},
		{
			name:     "no monitors",
			monitors: nil,
			wantOK:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := FindVirtualCandidate(tt.monitors)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("FindVirtualCandidate(%+v) = %q, %v, want %q, %v", tt.monitors, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/airplay/ -run TestFindVirtualCandidate -v`
Expected: FAIL with `undefined: FindVirtualCandidate`

- [ ] **Step 3: Implement**

Add `"regexp"` to the import block at the top of `internal/airplay/capture.go` (alongside the existing `"os/exec"`, `"strconv"` imports).

Add, after `describeConnectedNames` (Task 2):

```go
// virtualCandidatePattern matches disconnected output names that are known,
// pre-existing "spare" outputs a driver can turn into a virtual monitor:
// VIRTUAL1-8 (NVIDIA proprietary driver default) or DUMMY* (xf86-video-dummy
// convention). xrandr cannot fabricate a new output, only enable one the
// driver already exposes.
var virtualCandidatePattern = regexp.MustCompile(`^(VIRTUAL|DUMMY)\d+$`)

// FindVirtualCandidate returns the name of the first disconnected output
// that looks like a usable virtual/dummy monitor, if any.
func FindVirtualCandidate(monitors []MonitorInfo) (string, bool) {
	for _, m := range monitors {
		if !m.Connected && virtualCandidatePattern.MatchString(m.Name) {
			return m.Name, true
		}
	}
	return "", false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/airplay/ -run TestFindVirtualCandidate -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/airplay/capture.go internal/airplay/capture_test.go
git commit -m "airplay: detect NVIDIA VIRTUAL*/xf86-video-dummy DUMMY* candidates"
```

---

### Task 4: Virtual monitor placement and modeline computation (pure logic)

**Files:**
- Modify: `internal/airplay/capture.go` (add `computeVirtualPosition`, `parseCvtOutput`, `virtualScreenWidth`/`virtualScreenHeight` constants)
- Test: `internal/airplay/capture_test.go`

**Interfaces:**
- Consumes: `MonitorInfo` (Task 1)
- Produces: `func computeVirtualPosition(primary MonitorInfo, position string, vw, vh int) (x, y int)`, `func parseCvtOutput(output string) (modeName string, params []string, err error)`, `const virtualScreenWidth = 1920`, `const virtualScreenHeight = 1080` — all consumed by Task 5's `createVirtualMonitor`/`computeModeline`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/airplay/capture_test.go`:

```go
func TestComputeVirtualPosition(t *testing.T) {
	primary := MonitorInfo{X: 0, Y: 0, Width: 1920, Height: 1080}
	tests := []struct {
		position string
		wantX    int
		wantY    int
	}{
		{"right", 1920, 0},
		{"", 1920, 0}, // unrecognized/default falls back to "right"
		{"left", -1920, 0},
		{"above", 0, -1080},
		{"below", 0, 1080},
	}
	for _, tt := range tests {
		t.Run(tt.position, func(t *testing.T) {
			x, y := computeVirtualPosition(primary, tt.position, 1920, 1080)
			if x != tt.wantX || y != tt.wantY {
				t.Fatalf("computeVirtualPosition(%q) = (%d, %d), want (%d, %d)", tt.position, x, y, tt.wantX, tt.wantY)
			}
		})
	}

	// A non-origin primary should offset correctly too.
	primary = MonitorInfo{X: 1920, Y: 0, Width: 2560, Height: 1440}
	x, y := computeVirtualPosition(primary, "right", 1920, 1080)
	if x != 4480 || y != 0 {
		t.Fatalf("computeVirtualPosition(right) with offset primary = (%d, %d), want (4480, 0)", x, y)
	}
}

func TestParseCvtOutput(t *testing.T) {
	sample := `# 1920x1080 29.97 Hz (CVT 2.07M9) hsync: 33.72 kHz; pclk: 138.50 MHz
Modeline "1920x1080_30.00"  138.50  1920 2040 2248 2576  1080 1083 1088 1118 -hsync +vsync
`
	name, params, err := parseCvtOutput(sample)
	if err != nil {
		t.Fatalf("parseCvtOutput() error = %v", err)
	}
	if name != "1920x1080_30.00" {
		t.Fatalf("parseCvtOutput() name = %q, want %q", name, "1920x1080_30.00")
	}
	wantParams := []string{"138.50", "1920", "2040", "2248", "2576", "1080", "1083", "1088", "1118", "-hsync", "+vsync"}
	if len(params) != len(wantParams) {
		t.Fatalf("parseCvtOutput() params = %v, want %v", params, wantParams)
	}
	for i := range wantParams {
		if params[i] != wantParams[i] {
			t.Fatalf("parseCvtOutput() params[%d] = %q, want %q", i, params[i], wantParams[i])
		}
	}
}

func TestParseCvtOutputNoModeline(t *testing.T) {
	if _, _, err := parseCvtOutput("garbage output\n"); err == nil {
		t.Fatalf("parseCvtOutput() error = nil, want error for missing Modeline")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/airplay/ -run 'TestComputeVirtualPosition|TestParseCvtOutput' -v`
Expected: FAIL with `undefined: computeVirtualPosition` / `undefined: parseCvtOutput`

- [ ] **Step 3: Implement**

Add, after `FindVirtualCandidate` (Task 3):

```go
const (
	// virtualScreenWidth/virtualScreenHeight is the fixed resolution used
	// for a created virtual monitor; it mirrors the 1080p assumption
	// already baked into auto-bitrate sizing (captureBitrateKbps).
	virtualScreenWidth  = 1920
	virtualScreenHeight = 1080
)

// computeVirtualPosition returns the top-left coordinate for a virtual
// monitor of size vw x vh, placed relative to the primary monitor.
// Unrecognized position values (including "") default to "right".
func computeVirtualPosition(primary MonitorInfo, position string, vw, vh int) (x, y int) {
	switch position {
	case "left":
		return primary.X - vw, primary.Y
	case "above":
		return primary.X, primary.Y - vh
	case "below":
		return primary.X, primary.Y + primary.Height
	default:
		return primary.X + primary.Width, primary.Y
	}
}

// parseCvtOutput extracts the mode name and xrandr --newmode parameters
// from the output of the `cvt` utility, e.g.:
//
//	# 1920x1080 59.96 Hz (CVT 2.07M9) hsync: 67.16 kHz; pclk: 173.00 MHz
//	Modeline "1920x1080_30.00"  173.00  1920 2048 2248 2576  1080 1083 1088 1120 -hsync +vsync
func parseCvtOutput(output string) (modeName string, params []string, err error) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Modeline") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		return strings.Trim(fields[1], `"`), fields[2:], nil
	}
	return "", nil, fmt.Errorf("cvt output did not contain a Modeline: %s", output)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/airplay/ -run 'TestComputeVirtualPosition|TestParseCvtOutput' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/airplay/capture.go internal/airplay/capture_test.go
git commit -m "airplay: add virtual monitor placement and cvt modeline parsing"
```

---

### Task 5: Virtual monitor creation/teardown, wired into X11 capture

**Files:**
- Modify: `internal/airplay/capture.go` (`computeModeline`, `createVirtualMonitor`; rewrite `resolveX11CaptureRegion` signature; rewrite `startX11Capture`; add `virtualCleanup` to `ScreenCapture` and its `Stop()`)

**Interfaces:**
- Consumes: `FindVirtualCandidate`, `primaryOrFirstConnected` (Task 3, Task 1), `computeVirtualPosition`, `parseCvtOutput`, `virtualScreenWidth`/`virtualScreenHeight` (Task 4)
- Produces: `resolveX11CaptureRegion(display string, cfg CaptureConfig) (startX, startY, endX, endY int, cleanup func() error, err error)` (signature change from Task 2 — adds `cleanup`), `ScreenCapture.virtualCleanup func() error` field, consumed by `Stop()`.

This task has no new pure-logic unit tests of its own — `createVirtualMonitor` and `computeModeline` shell out to real `xrandr`/`cvt`, which aren't available in a CI sandbox without an X server and a dummy/NVIDIA virtual output. The pure helpers it calls (`computeVirtualPosition`, `parseCvtOutput`, `FindVirtualCandidate`) are already tested in Tasks 3-4. This task is verified by a full-package build plus the manual hardware check in Step 4.

- [ ] **Step 1: Implement `computeModeline` and `createVirtualMonitor`**

Add to `internal/airplay/capture.go`, after `parseCvtOutput` (Task 4):

```go
// computeModeline runs `cvt` to compute an xrandr modeline for the given
// resolution and refresh rate.
func computeModeline(width, height, fps int) (modeName string, params []string, err error) {
	out, err := exec.Command("cvt", strconv.Itoa(width), strconv.Itoa(height), strconv.Itoa(fps)).Output()
	if err != nil {
		return "", nil, fmt.Errorf("cvt: %w", err)
	}
	return parseCvtOutput(string(out))
}

// createVirtualMonitor enables the given disconnected output as a new
// virtual monitor at (x, y) with the given resolution/refresh rate, via
// xrandr --newmode/--addmode/--output. If any step fails, it rolls back
// whatever steps already succeeded before returning the error. On success,
// the returned cleanup function reverses all of it (turns the output back
// off and removes the added mode), restoring the output to its original
// disconnected state.
func createVirtualMonitor(display, output string, width, height, fps, x, y int) (cleanup func() error, err error) {
	modeName, params, err := computeModeline(width, height, fps)
	if err != nil {
		return nil, fmt.Errorf("compute modeline: %w", err)
	}

	var applied []func()
	rollback := func() {
		for i := len(applied) - 1; i >= 0; i-- {
			applied[i]()
		}
	}

	newmodeArgs := append([]string{"--display", display, "--newmode", modeName}, params...)
	if out, err := exec.Command("xrandr", newmodeArgs...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("xrandr --newmode: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	applied = append(applied, func() {
		exec.Command("xrandr", "--display", display, "--rmmode", modeName).Run()
	})

	if out, err := exec.Command("xrandr", "--display", display, "--addmode", output, modeName).CombinedOutput(); err != nil {
		rollback()
		return nil, fmt.Errorf("xrandr --addmode: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	applied = append(applied, func() {
		exec.Command("xrandr", "--display", display, "--delmode", output, modeName).Run()
	})

	posArg := fmt.Sprintf("%dx%d", x, y)
	if out, err := exec.Command("xrandr", "--display", display,
		"--output", output, "--mode", modeName, "--pos", posArg).CombinedOutput(); err != nil {
		rollback()
		return nil, fmt.Errorf("xrandr --output: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	return func() error {
		exec.Command("xrandr", "--display", display, "--output", output, "--off").Run()
		exec.Command("xrandr", "--display", display, "--delmode", output, modeName).Run()
		exec.Command("xrandr", "--display", display, "--rmmode", modeName).Run()
		return nil
	}, nil
}
```

- [ ] **Step 2: Rewrite `resolveX11CaptureRegion` to add the virtual case**

Replace the `resolveX11CaptureRegion` function added in Task 2 with:

```go
// resolveX11CaptureRegion determines which region of the X screen to
// capture based on cfg.ScreenID: empty auto-detects the primary monitor
// (preserving prior behavior), "virtual" creates a virtual monitor
// extending the desktop, and any other value selects that connected
// output by name. The returned cleanup, if non-nil, must be called when
// capture stops to tear down a created virtual monitor.
func resolveX11CaptureRegion(display string, cfg CaptureConfig) (startX, startY, endX, endY int, cleanup func() error, err error) {
	if cfg.ScreenID == "" {
		startX, startY, endX, endY = detectPrimaryMonitor(display)
		return startX, startY, endX, endY, nil, nil
	}

	monitors, err := ListX11Monitors(display)
	if err != nil {
		return 0, 0, 0, 0, nil, fmt.Errorf("list screens: %w", err)
	}

	if cfg.ScreenID == "virtual" {
		outputName, ok := FindVirtualCandidate(monitors)
		if !ok {
			return 0, 0, 0, 0, nil, fmt.Errorf("no virtual/dummy output available; on NVIDIA this should be automatic (VIRTUAL1-8), on Intel/AMD configure xf86-video-dummy in xorg.conf (requires an X restart) — see README")
		}
		primary, ok := primaryOrFirstConnected(monitors)
		if !ok {
			return 0, 0, 0, 0, nil, fmt.Errorf("no connected monitor found to position the virtual monitor relative to")
		}
		fps := cfg.FPS
		if fps <= 0 {
			fps = 30
		}
		position := cfg.VirtualPosition
		if position == "" {
			position = "right"
		}
		x, y := computeVirtualPosition(primary, position, virtualScreenWidth, virtualScreenHeight)
		vcleanup, err := createVirtualMonitor(display, outputName, virtualScreenWidth, virtualScreenHeight, fps, x, y)
		if err != nil {
			return 0, 0, 0, 0, nil, fmt.Errorf("create virtual monitor: %w", err)
		}
		return x, y, x + virtualScreenWidth, y + virtualScreenHeight, vcleanup, nil
	}

	m, ok := findMonitorByName(monitors, cfg.ScreenID)
	if !ok {
		return 0, 0, 0, 0, nil, fmt.Errorf("screen %q not found; available screens: %s", cfg.ScreenID, describeConnectedNames(monitors))
	}
	return m.X, m.Y, m.X + m.Width, m.Y + m.Height, nil, nil
}
```

- [ ] **Step 3: Wire cleanup through `ScreenCapture` and `startX11Capture`**

Add a field to `ScreenCapture` (currently `capture.go:40-49`):

```go
// ScreenCapture manages screen capture via GStreamer.
type ScreenCapture struct {
	cmd      *exec.Cmd // gst-launch-1.0 process
	stdout   io.ReadCloser
	cancel   context.CancelFunc
	pwNodeID uint32
	dbusConn *dbus.Conn    // portal session D-Bus connection (must stay open for Wayland)
	waitCh   chan struct{} // closed when process exits
	waitErr  error         // set before waitCh is closed
	stopped  bool

	virtualCleanup func() error // tears down a created virtual monitor, if any; nil otherwise
}
```

Replace the entire `startX11Capture` function (currently `capture.go:198-287`) with:

```go
func startX11Capture(ctx context.Context, cfg CaptureConfig) (*ScreenCapture, error) {
	if err := exec.Command("gst-inspect-1.0", "ximagesrc").Run(); err != nil {
		return nil, fmt.Errorf("GStreamer 'ximagesrc' plugin not found; install gst-plugins-good")
	}

	captureCtx, cancel := context.WithCancel(ctx)

	fps := cfg.FPS
	if fps <= 0 {
		fps = 30
	}

	display := os.Getenv("DISPLAY")

	encoder := detectGstEncoder(cfg)

	// Determine which region of the X screen to capture. ximagesrc captures
	// the full X screen (all monitors combined) by default, so we crop to a
	// single monitor's geometry — auto-detected (empty ScreenID, preserving
	// the original behavior), a created virtual monitor, or an explicitly
	// named output. The encoded resolution is that monitor's native
	// resolution (no rescaling).
	startX, startY, endX, endY, virtualCleanup, err := resolveX11CaptureRegion(display, cfg)
	if err != nil {
		cancel()
		return nil, err
	}

	ximageSrcArgs := []string{
		"ximagesrc", fmt.Sprintf("display-name=%s", display), "use-damage=false",
	}
	if endX > startX && endY > startY {
		ximageSrcArgs = append(ximageSrcArgs,
			fmt.Sprintf("startx=%d", startX),
			fmt.Sprintf("starty=%d", startY),
			fmt.Sprintf("endx=%d", endX-1),
			fmt.Sprintf("endy=%d", endY-1),
		)
		dbg("[CAPTURE] cropping ximagesrc to x=%d..%d y=%d..%d", startX, endX-1, startY, endY-1)
	}

	gstArgs := []string{"--quiet"}
	gstArgs = append(gstArgs, ximageSrcArgs...)
	gstArgs = append(gstArgs,
		"!", fmt.Sprintf("video/x-raw,framerate=%d/1", fps),
		"!", "queue", "max-size-buffers=1", "max-size-bytes=0", "max-size-time=0", "leaky=downstream",
		"!", "videoconvert",
	)
	if encoder.needsVulkan {
		gstArgs = append(gstArgs, "!", "vulkanupload")
	}
	gstArgs = append(gstArgs, "!")
	gstArgs = append(gstArgs, encoder.parts...)
	gstArgs = append(gstArgs,
		"!", "h264parse", "config-interval=-1",
		"!", "video/x-h264,stream-format=byte-stream,alignment=au",
		"!", "fdsink", "fd=1", "sync=false", "async=false",
	)

	dbg("[CAPTURE] gst-launch-1.0 (x11) %s", strings.Join(gstArgs, " "))
	cmd := exec.CommandContext(captureCtx, "gst-launch-1.0", gstArgs...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		if virtualCleanup != nil {
			virtualCleanup()
		}
		return nil, fmt.Errorf("gst stdout pipe: %w", err)
	}
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		// If Vulkan encoder failed, retry with software fallback
		if encoder.needsVulkan {
			log.Printf("[CAPTURE] vulkanh264enc pipeline failed, falling back to x264enc")
			cancel()
			if virtualCleanup != nil {
				virtualCleanup()
			}
			cfg.HWAccel = "none"
			return startX11Capture(ctx, cfg)
		}
		cancel()
		if virtualCleanup != nil {
			virtualCleanup()
		}
		return nil, fmt.Errorf("start gst-launch: %w", err)
	}

	go logStderr("GST", stderr)

	capture := &ScreenCapture{
		cmd:            cmd,
		stdout:         stdout,
		cancel:         cancel,
		waitCh:         make(chan struct{}),
		virtualCleanup: virtualCleanup,
	}
	go func() {
		capture.waitErr = cmd.Wait()
		close(capture.waitCh)
	}()

	return capture, nil
}
```

Add cleanup invocation at the end of `Stop()` (currently `capture.go:301-331`), after the existing `select` block that waits for the process to exit:

```go
func (sc *ScreenCapture) Stop() {
	if sc.stopped {
		return
	}
	sc.stopped = true
	if sc.cancel != nil {
		sc.cancel()
	}

	// Close stdout to unblock any pending Read() call.
	if sc.stdout != nil {
		sc.stdout.Close()
	}

	if sc.dbusConn != nil {
		sc.dbusConn.Close()
	}

	if sc.cmd != nil && sc.cmd.Process != nil {
		_ = sc.cmd.Process.Signal(os.Interrupt)
	}

	select {
	case <-sc.waitCh:
	case <-time.After(2 * time.Second):
		if sc.cmd != nil && sc.cmd.Process != nil {
			_ = sc.cmd.Process.Kill()
		}
		<-sc.waitCh
	}

	if sc.virtualCleanup != nil {
		if err := sc.virtualCleanup(); err != nil {
			log.Printf("[CAPTURE] warning: failed to tear down virtual monitor: %v", err)
		}
	}
}
```

- [ ] **Step 4: Build, run the full test suite, and manually verify on real X11 hardware**

Run: `go build ./... && go test ./internal/airplay/ -v`
Expected: build succeeds, all tests PASS.

Manual check (requires an X11 session with either an NVIDIA GPU or a pre-configured `xf86-video-dummy` output, and `cvt` installed — `x11-xserver-utils` on Debian/Ubuntu):

```sh
xrandr --query | grep -E 'VIRTUAL|DUMMY'   # confirm a disconnected candidate exists
go build -o bin/doubletake ./cmd/doubletake
./bin/doubletake -target <apple-tv-ip> -screen virtual -virtual-position right -test
```

Expected: a new virtual monitor appears in `xrandr --query` to the right of the primary while streaming (confirm with `xrandr --query` in a second terminal), and disappears (`xrandr --query` shows it disconnected again) after `Ctrl-C`.

- [ ] **Step 5: Commit**

```bash
git add internal/airplay/capture.go
git commit -m "airplay: create and tear down a virtual monitor for -screen virtual"
```

---

### Task 6: Wayland screen-change handling

**Files:**
- Modify: `internal/airplay/capture.go` (`startWaylandCapture`, new `waylandRequestToken`)
- Test: `internal/airplay/capture_test.go`

**Interfaces:**
- Consumes: `CaptureConfig` (Task 2)
- Produces: `func waylandRequestToken(cfg CaptureConfig) string`, used only within `startWaylandCapture`.

- [ ] **Step 1: Write the failing test**

Add to `internal/airplay/capture_test.go`:

```go
func TestWaylandRequestToken(t *testing.T) {
	tests := []struct {
		name string
		cfg  CaptureConfig
		want string
	}{
		{"no screen override reuses saved token", CaptureConfig{RestoreToken: "saved-token"}, "saved-token"},
		{"named screen forces fresh picker", CaptureConfig{RestoreToken: "saved-token", ScreenID: "DP-3"}, ""},
		{"virtual screen forces fresh picker", CaptureConfig{RestoreToken: "saved-token", ScreenID: "virtual"}, ""},
		{"no saved token, no override", CaptureConfig{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := waylandRequestToken(tt.cfg); got != tt.want {
				t.Fatalf("waylandRequestToken(%+v) = %q, want %q", tt.cfg, got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/airplay/ -run TestWaylandRequestToken -v`
Expected: FAIL with `undefined: waylandRequestToken`

- [ ] **Step 3: Implement**

Add, near `startWaylandCapture` in `internal/airplay/capture.go`:

```go
// waylandRequestToken returns the restore token to pass to the portal's
// SelectSources call: the saved token normally, or empty when a screen
// change was explicitly requested. The xdg-desktop-portal has no API to
// select a monitor by name — passing an empty token forces its native
// picker to reappear so the user can choose a different monitor/window
// themselves, instead of silently reusing whatever was chosen last time.
func waylandRequestToken(cfg CaptureConfig) string {
	if cfg.ScreenID != "" {
		return ""
	}
	return cfg.RestoreToken
}
```

Modify the start of `startWaylandCapture` (currently `capture.go:64-79`) from:

```go
func startWaylandCapture(ctx context.Context, cfg CaptureConfig) (*ScreenCapture, error) {
	// Check dependencies
	if err := exec.Command("gst-inspect-1.0", "pipewiresrc").Run(); err != nil {
		return nil, fmt.Errorf("GStreamer 'pipewiresrc' plugin not found; install gst-pipewire")
	}

	nodeID, pwFd, dbusConn, restoreToken, err := requestScreencast(ctx, cfg.RestoreToken)
```

to:

```go
func startWaylandCapture(ctx context.Context, cfg CaptureConfig) (*ScreenCapture, error) {
	if cfg.ScreenID == "virtual" {
		return nil, fmt.Errorf("virtual screen capture is not yet supported on Wayland")
	}

	// Check dependencies
	if err := exec.Command("gst-inspect-1.0", "pipewiresrc").Run(); err != nil {
		return nil, fmt.Errorf("GStreamer 'pipewiresrc' plugin not found; install gst-pipewire")
	}

	requestToken := waylandRequestToken(cfg)
	nodeID, pwFd, dbusConn, restoreToken, err := requestScreencast(ctx, requestToken)
```

The rest of `startWaylandCapture` is unchanged.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/airplay/ -run TestWaylandRequestToken -v`
Expected: PASS

- [ ] **Step 5: Build and run the full package test suite**

Run: `go build ./... && go test ./internal/airplay/ -v`
Expected: build succeeds, all tests PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/airplay/capture.go internal/airplay/capture_test.go
git commit -m "airplay: force portal picker to reappear on Wayland when -screen is set"
```

---

### Task 7: CLI flags for standalone mode

**Files:**
- Modify: `cmd/doubletake/main.go`

**Interfaces:**
- Consumes: `airplay.CaptureConfig.ScreenID`/`VirtualPosition` (Task 2), `airplay.ListX11Monitors`, `airplay.FindVirtualCandidate` (Task 1, Task 3)
- Produces: `-screen`, `-virtual-position`, `-list-screens` flags; `func printAvailableScreens()`. Task 8 separately changes the `runDaemon` call/signature in this same file.

No automated tests: `cmd/doubletake` has no existing test file, and `main()`/`printAvailableScreens` are side-effecting (env vars, process exec, stdout) in a way the rest of this package already leaves to manual/build verification (see `internal/airplay` and `internal/daemon` for where the real logic — and its tests — lives).

- [ ] **Step 1: Add the new flags**

In `cmd/doubletake/main.go`, insert after the `hwaccel := ...` line (currently `main.go:60`) and before `testMode := ...`:

```go
	screen := flag.String("screen", "", "Screen to capture: empty = auto-detect (X11) / portal picker (Wayland), an xrandr output name, or \"virtual\" for a virtual extended-desktop monitor")
	virtualPosition := flag.String("virtual-position", "right", "Position of the virtual monitor relative to the primary: left, right, above, or below (only used with -screen virtual)")
	listScreens := flag.Bool("list-screens", false, "List available screens (physical outputs and virtual monitor availability) and exit")
```

- [ ] **Step 2: Validate `-virtual-position` and handle `-list-screens` right after `flag.Parse()`**

Immediately after `flag.Parse()` (currently `main.go:69`), insert:

```go
	switch *virtualPosition {
	case "left", "right", "above", "below":
	default:
		log.Fatalf("invalid -virtual-position %q (want left, right, above, or below)", *virtualPosition)
	}

	if *listScreens {
		printAvailableScreens()
		return
	}
```

- [ ] **Step 3: Plumb the flags into the standalone-mode `CaptureConfig`**

Replace this block (currently `main.go:281-286`):

```go
		captureCfg := airplay.CaptureConfig{
			FPS:     *fps,
			Bitrate: *bitrate,
			HWAccel: *hwaccel,
		}
```

with:

```go
		captureCfg := airplay.CaptureConfig{
			FPS:             *fps,
			Bitrate:         *bitrate,
			HWAccel:         *hwaccel,
			ScreenID:        *screen,
			VirtualPosition: *virtualPosition,
		}
```

- [ ] **Step 4: Add `printAvailableScreens`**

Add near `selectDevice` (currently `main.go:335-370`):

```go
// printAvailableScreens prints the screens doubletake can currently target
// with -screen, for use with -list-screens.
func printAvailableScreens() {
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		fmt.Println("screen listing is not available on Wayland; the desktop portal shows its own picker when you connect.")
		fmt.Println("pass any -screen value to force that picker to reappear instead of reusing a saved choice.")
		return
	}
	display := os.Getenv("DISPLAY")
	if display == "" {
		fmt.Println("no display server detected (neither WAYLAND_DISPLAY nor DISPLAY is set)")
		return
	}
	monitors, err := airplay.ListX11Monitors(display)
	if err != nil {
		fmt.Printf("failed to query screens: %v\n", err)
		return
	}
	fmt.Println("available screens:")
	for _, m := range monitors {
		if !m.Connected {
			continue
		}
		marker := ""
		if m.Primary {
			marker = " (primary)"
		}
		fmt.Printf("  %s: %dx%d+%d+%d%s\n", m.Name, m.Width, m.Height, m.X, m.Y, marker)
	}
	if name, ok := airplay.FindVirtualCandidate(monitors); ok {
		fmt.Printf("  virtual: available (would use output %s)\n", name)
	} else {
		fmt.Println("  virtual: not available (no VIRTUAL*/DUMMY* output detected; see README for setup)")
	}
}
```

- [ ] **Step 5: Build and smoke-test**

Run: `go build ./... && go vet ./...`
Expected: no errors.

Run: `go build -o bin/doubletake ./cmd/doubletake && ./bin/doubletake -list-screens`
Expected: prints either the Wayland message or a list of X11 outputs plus a `virtual: ...` line, depending on the environment this is run in — no crash.

Run: `./bin/doubletake -virtual-position sideways -list-screens`
Expected: exits immediately with `invalid -virtual-position "sideways" (want left, right, above, or below)` (fails before reaching the screen listing, confirming validation runs first).

- [ ] **Step 6: Commit**

```bash
git add cmd/doubletake/main.go
git commit -m "cmd/doubletake: add -screen, -virtual-position, -list-screens flags"
```

---

### Task 8: Daemon config, protocol, and handlers

**Files:**
- Modify: `internal/daemon/daemon.go` (`Config`, `ScreenInfo`, `Response`, `handleRequest`, `handleScreens`, `handleScreenSet`, `getOrStartBroadcastLocked`)
- Modify: `cmd/doubletake/main.go` (`runDaemon` signature + call site)
- Test: `internal/daemon/daemon_test.go` (new file)

**Interfaces:**
- Consumes: `airplay.ListX11Monitors`, `airplay.FindVirtualCandidate` (Task 1, Task 3), `airplay.CaptureConfig.ScreenID`/`VirtualPosition` (Task 2)
- Produces: `daemon.Config.ScreenID string`, `daemon.Config.VirtualPosition string`, `type ScreenInfo struct { Name string; Width, Height int; Primary, IsVirtual, Available bool }`, `Response.Screens []ScreenInfo`, `Response.CurrentScreen string`, `Cmd: "screens"` and `Cmd: "screen-set"` requests — consumed by Task 9's `daemonclient`.

- [ ] **Step 1: Write the failing tests**

Create `internal/daemon/daemon_test.go`:

```go
package daemon

import (
	"path/filepath"
	"testing"
)

func newTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	credFile := filepath.Join(t.TempDir(), "credentials.json")
	d, err := New(Config{CredBackend: "file", CredFile: credFile})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return d
}

func TestHandleScreensNoDisplay(t *testing.T) {
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", "")

	d := newTestDaemon(t)
	resp := d.handleScreens()

	if !resp.OK {
		t.Fatalf("handleScreens() OK = false, error = %q", resp.Error)
	}
	if resp.CurrentScreen != "auto" {
		t.Fatalf("handleScreens() CurrentScreen = %q, want %q", resp.CurrentScreen, "auto")
	}
	if len(resp.Screens) != 0 {
		t.Fatalf("handleScreens() Screens = %v, want empty (no display server in test environment)", resp.Screens)
	}
}

func TestHandleScreenSetIdle(t *testing.T) {
	d := newTestDaemon(t)

	resp := d.handleScreenSet(Request{Cmd: "screen-set", Target: "DP-3"})
	if !resp.OK {
		t.Fatalf("handleScreenSet() OK = false, error = %q", resp.Error)
	}
	if resp.CurrentScreen != "DP-3" {
		t.Fatalf("handleScreenSet() CurrentScreen = %q, want %q", resp.CurrentScreen, "DP-3")
	}
	d.mu.Lock()
	got := d.cfg.ScreenID
	d.mu.Unlock()
	if got != "DP-3" {
		t.Fatalf("d.cfg.ScreenID = %q, want %q", got, "DP-3")
	}
}

func TestHandleScreenSetAutoClearsScreenID(t *testing.T) {
	d := newTestDaemon(t)
	d.mu.Lock()
	d.cfg.ScreenID = "DP-3"
	d.mu.Unlock()

	resp := d.handleScreenSet(Request{Cmd: "screen-set", Target: "auto"})
	if !resp.OK {
		t.Fatalf("handleScreenSet() OK = false, error = %q", resp.Error)
	}
	if resp.CurrentScreen != "auto" {
		t.Fatalf("handleScreenSet() CurrentScreen = %q, want %q", resp.CurrentScreen, "auto")
	}
	d.mu.Lock()
	got := d.cfg.ScreenID
	d.mu.Unlock()
	if got != "" {
		t.Fatalf("d.cfg.ScreenID = %q, want empty", got)
	}
}

func TestHandleScreenSetRejectedWhileStreaming(t *testing.T) {
	d := newTestDaemon(t)
	d.mu.Lock()
	d.streams["10.0.0.5"] = &activeStream{deviceIP: "10.0.0.5", state: StateStreaming}
	d.mu.Unlock()

	resp := d.handleScreenSet(Request{Cmd: "screen-set", Target: "DP-3"})
	if resp.OK {
		t.Fatalf("handleScreenSet() OK = true while streaming, want rejection")
	}
	if resp.Error == "" {
		t.Fatalf("handleScreenSet() Error empty, want a rejection message")
	}

	d.mu.Lock()
	got := d.cfg.ScreenID
	d.mu.Unlock()
	if got != "" {
		t.Fatalf("d.cfg.ScreenID = %q, want unchanged (empty)", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/daemon/ -v`
Expected: FAIL to compile — `d.handleScreens undefined`, `d.handleScreenSet undefined`, `Config` has no field `ScreenID`.

- [ ] **Step 3: Implement**

In `internal/daemon/daemon.go`, add fields to `Config` (currently `daemon.go:71-83`):

```go
// Config holds daemon configuration.
type Config struct {
	SocketPath      string
	CredFile        string
	CredBackend     string
	FPS             int
	Bitrate         int
	HWAccel         string
	ScreenID        string
	VirtualPosition string
	Debug           bool
	TestMode        bool
	NoEncrypt       bool
	DirectKey       bool
	NoAudio         bool
}
```

Add `ScreenInfo` after `DeviceInfo` (currently `daemon.go:61-68`):

```go
// ScreenInfo describes one screen available to broadcast: either a physical
// monitor or the synthetic "virtual" entry representing an on-demand
// virtual monitor.
type ScreenInfo struct {
	Name      string `json:"name"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Primary   bool   `json:"primary,omitempty"`
	IsVirtual bool   `json:"is_virtual,omitempty"`
	Available bool   `json:"available"`
}
```

Add fields to `Response` (currently `daemon.go:48-59`):

```go
// Response is returned to the caller for every request.
type Response struct {
	OK            bool         `json:"ok"`
	State         State        `json:"state"`
	Device        string       `json:"device,omitempty"`
	DeviceIP      string       `json:"device_ip,omitempty"`
	HasAudio      bool         `json:"has_audio"`
	AudioMuted    bool         `json:"audio_muted"`
	NeedsPIN      bool         `json:"needs_pin,omitempty"`
	Error         string       `json:"error,omitempty"`
	Devices       []DeviceInfo `json:"devices,omitempty"`
	Streams       []StreamInfo `json:"streams,omitempty"`
	Screens       []ScreenInfo `json:"screens,omitempty"`
	CurrentScreen string       `json:"current_screen,omitempty"`
}
```

Add cases to `handleRequest` (currently `daemon.go:293-312`), inserted after the `"devices"` case:

```go
	case "devices":
		return d.handleDevices()
	case "screens":
		return d.handleScreens()
	case "screen-set":
		return d.handleScreenSet(req)
	case "connect":
```

Add handlers after `handleDevices` (currently `daemon.go:400-408`):

```go
// screenIDForDisplay normalizes an internal ScreenID for display/API
// purposes: the empty string (auto-detect) is shown as "auto".
func screenIDForDisplay(id string) string {
	if id == "" {
		return "auto"
	}
	return id
}

func (d *Daemon) handleScreens() Response {
	d.mu.Lock()
	current := screenIDForDisplay(d.cfg.ScreenID)
	overall := d.overallStateLocked()
	d.mu.Unlock()

	var screens []ScreenInfo
	if os.Getenv("WAYLAND_DISPLAY") == "" && os.Getenv("DISPLAY") != "" {
		monitors, err := airplay.ListX11Monitors(os.Getenv("DISPLAY"))
		if err != nil {
			return Response{OK: false, State: overall, Error: fmt.Sprintf("list screens: %v", err)}
		}
		for _, m := range monitors {
			if !m.Connected {
				continue
			}
			screens = append(screens, ScreenInfo{
				Name:      m.Name,
				Width:     m.Width,
				Height:    m.Height,
				Primary:   m.Primary,
				Available: true,
			})
		}
		_, virtualOK := airplay.FindVirtualCandidate(monitors)
		screens = append(screens, ScreenInfo{Name: "virtual", IsVirtual: true, Available: virtualOK})
	}

	return Response{OK: true, State: overall, Screens: screens, CurrentScreen: current}
}

func (d *Daemon) handleScreenSet(req Request) Response {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.streams) > 0 {
		return Response{OK: false, State: d.overallStateLocked(), Error: "stop streaming before changing screen"}
	}

	screen := req.Target
	if screen == "auto" {
		screen = ""
	}
	d.cfg.ScreenID = screen

	return Response{OK: true, State: d.overallStateLocked(), CurrentScreen: screenIDForDisplay(d.cfg.ScreenID)}
}
```

Update `getOrStartBroadcastLocked` (currently `daemon.go:711-734`) to snapshot `ScreenID`/`VirtualPosition` under the lock (they're now mutable at runtime via `handleScreenSet`, unlike `FPS`/`Bitrate`/`HWAccel` which are still set once at startup) and pass them through:

```go
func (d *Daemon) getOrStartBroadcastLocked(restoreToken, deviceID string) (*airplay.BroadcastSink, error) {
	d.mu.Lock()
	bc := d.broadcast
	screenID := d.cfg.ScreenID
	virtualPosition := d.cfg.VirtualPosition
	d.mu.Unlock()

	if bc != nil {
		// Capture already running — add a new sink.
		sink := bc.AddSink()
		return sink, nil
	}

	// Start a fresh screen capture.
	capCfg := airplay.CaptureConfig{
		FPS:             d.cfg.FPS,
		Bitrate:         d.cfg.Bitrate,
		HWAccel:         d.cfg.HWAccel,
		ScreenID:        screenID,
		VirtualPosition: virtualPosition,
		RestoreToken:    restoreToken,
	}
```

(The rest of `getOrStartBroadcastLocked` is unchanged.)

In `cmd/doubletake/main.go`, update the `runDaemon` call (currently `main.go:75-78`):

```go
	if *daemonize {
		runDaemon(*socketPath, *credFile, *credBackend, *fps, *bitrate, *hwaccel, *screen, *virtualPosition, *debug, *testMode, *noEncrypt, *directKey, *noAudio)
		return
	}
```

and the `runDaemon` function itself (currently `main.go:398-436`), updating its signature and `daemon.Config` construction:

```go
func runDaemon(socketPath, credFile, credBackend string, fps, bitrate int, hwaccel, screen, virtualPosition string, debug, testMode, noEncrypt, directKey, noAudio bool) {
	cfg := daemon.Config{
		SocketPath:      socketPath,
		CredFile:        credFile,
		CredBackend:     credBackend,
		FPS:             fps,
		Bitrate:         bitrate,
		HWAccel:         hwaccel,
		ScreenID:        screen,
		VirtualPosition: virtualPosition,
		Debug:           debug,
		TestMode:        testMode,
		NoEncrypt:       noEncrypt,
		DirectKey:       directKey,
		NoAudio:         noAudio,
	}
```

(The rest of `runDaemon` is unchanged.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -v`
Expected: PASS

- [ ] **Step 5: Build everything**

Run: `go build ./... && go vet ./...`
Expected: no errors.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/daemon.go internal/daemon/daemon_test.go cmd/doubletake/main.go
git commit -m "daemon: add screens/screen-set commands and ScreenID/VirtualPosition config"
```

---

### Task 9: `daemonclient` + `doubletake-ctl` subcommands

**Files:**
- Modify: `internal/daemon/daemonclient/client.go`
- Modify: `cmd/doubletake-ctl/main.go`
- Test: `internal/daemon/daemonclient/daemonclient_test.go` (new file)

**Interfaces:**
- Consumes: `daemon.Request{Cmd: "screens"}`, `daemon.Request{Cmd: "screen-set", Target: string}`, `daemon.Response.Screens`/`CurrentScreen` (Task 8)
- Produces: `func (c *Client) Screens() (*daemon.Response, error)`, `func (c *Client) ScreenSet(name string) (*daemon.Response, error)` — consumed by Task 10 (plasmoid runs `doubletake-ctl screens`/`screen-set` as subprocesses, not this Go API directly, but this task's `doubletake-ctl` output format is what the plasmoid parses).

- [ ] **Step 1: Write the failing test**

Create `internal/daemon/daemonclient/daemonclient_test.go`:

```go
package daemonclient

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"doubletake/internal/daemon"
)

func startTestDaemon(t *testing.T) *Client {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "doubletake.sock")
	credFile := filepath.Join(t.TempDir(), "credentials.json")

	d, err := daemon.New(daemon.Config{
		SocketPath:  socketPath,
		CredFile:    credFile,
		CredBackend: "file",
	})
	if err != nil {
		t.Fatalf("daemon.New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		d.Shutdown()
		<-runErr
	})

	client := New(socketPath)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := client.Status(); err == nil && resp.OK {
			return client
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("daemon did not become ready in time")
	return nil
}

func TestScreensAndScreenSetRoundTrip(t *testing.T) {
	client := startTestDaemon(t)

	resp, err := client.Screens()
	if err != nil {
		t.Fatalf("Screens() error = %v", err)
	}
	if !resp.OK {
		t.Fatalf("Screens() OK = false, error = %q", resp.Error)
	}
	if resp.CurrentScreen != "auto" {
		t.Fatalf("Screens() CurrentScreen = %q, want %q", resp.CurrentScreen, "auto")
	}

	resp, err = client.ScreenSet("DP-3")
	if err != nil {
		t.Fatalf("ScreenSet() error = %v", err)
	}
	if !resp.OK {
		t.Fatalf("ScreenSet() OK = false, error = %q", resp.Error)
	}
	if resp.CurrentScreen != "DP-3" {
		t.Fatalf("ScreenSet() CurrentScreen = %q, want %q", resp.CurrentScreen, "DP-3")
	}

	resp, err = client.Screens()
	if err != nil {
		t.Fatalf("second Screens() error = %v", err)
	}
	if resp.CurrentScreen != "DP-3" {
		t.Fatalf("second Screens() CurrentScreen = %q, want %q", resp.CurrentScreen, "DP-3")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/daemonclient/ -v`
Expected: FAIL to compile — `client.Screens undefined`, `client.ScreenSet undefined`

- [ ] **Step 3: Implement**

Add to `internal/daemon/daemonclient/client.go`, after `UnmuteTarget` (currently `client.go:73-75`):

```go
// Screens lists available screens (physical outputs and virtual monitor
// availability) and the currently configured screen.
func (c *Client) Screens() (*daemon.Response, error) {
	return c.send(daemon.Request{Cmd: "screens"})
}

// ScreenSet changes which screen the daemon broadcasts: a physical output
// name, "virtual", or "auto" to restore auto-detection. Fails if any
// stream is currently active.
func (c *Client) ScreenSet(name string) (*daemon.Response, error) {
	return c.send(daemon.Request{Cmd: "screen-set", Target: name})
}
```

Add subcommands to `cmd/doubletake-ctl/main.go`, in the `switch cmd` block (currently `main.go:35-80`), inserted after the `"unmute"` case:

```go
	case "unmute":
		if len(args) >= 2 {
			resp, err = client.UnmuteTarget(args[1])
		} else {
			resp, err = client.Unmute()
		}
	case "screens":
		resp, err = client.Screens()
	case "screen-set":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: doubletake-ctl screen-set <name|virtual|auto>\n")
			os.Exit(1)
		}
		resp, err = client.ScreenSet(args[1])
	default:
```

Update `usage()` (currently `main.go:96-98`):

```go
func usage() {
	fmt.Fprintf(os.Stderr, "Usage: doubletake-ctl [-socket path] <command> [args]\n\nCommands:\n  status                      Show daemon state and all active streams\n  discover                    Discover AirPlay devices on the network\n  devices                     List cached discovered devices\n  connect [target] [pin]      Start mirroring (to target IP, or first free device)\n  pin <4-digit-PIN>           Submit PIN for a device waiting for pairing\n  disconnect [target]         Stop mirroring (all streams, or only the given IP)\n  mute [target]               Mute mirrored audio (all streams, or only the given IP)\n  unmute [target]             Unmute mirrored audio (all streams, or only the given IP)\n  screens                     List available screens and the current selection\n  screen-set <name>           Change which screen is broadcast (name, \"virtual\", or \"auto\"); fails while streaming\n\nFlags:\n  -socket path                Override daemon socket path (default: %s)\n", daemon.DefaultSocketPath())
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/daemon/daemonclient/ -v`
Expected: PASS

- [ ] **Step 5: Build everything and run the full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: no errors, all tests PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/daemonclient/client.go internal/daemon/daemonclient/daemonclient_test.go cmd/doubletake-ctl/main.go
git commit -m "doubletake-ctl: add screens and screen-set subcommands"
```

---

### Task 10: Plasmoid screen picker

**Files:**
- Modify: `plasmoid/contents/ui/main.qml`

**Interfaces:**
- Consumes: `doubletake-ctl screens` / `doubletake-ctl screen-set <name>` JSON output (Task 9): `{ok, screens: [{name, width, height, primary, is_virtual, available}], current_screen, error}`
- Produces: none (leaf UI change)

No automated tests: the plasmoid has no test framework today (verified: `plasmoid/` contains only `metadata.json`, `README.md`, `contents/ui/main.qml`). Verified manually via `kpackagetool6`, matching the existing project convention documented in `plasmoid/README.md`.

- [ ] **Step 1: Add state properties**

In `plasmoid/contents/ui/main.qml`, add after `property var pendingCommands: (new Object())` (currently line 22):

```qml
    property var screenList: []
    property string currentScreen: "auto"
```

- [ ] **Step 2: Add a label helper**

Add after the `isDeviceAudioMuted` function (currently `main.qml:110-113`):

```qml
    function screenLabelFor(name) {
        if (name === "auto" || name === "") return "Auto"
        if (name === "virtual") return "Virtual Screen"
        return name
    }
```

- [ ] **Step 3: Poll `screens` alongside `status`/`devices`**

Replace the `statusTimer` block (currently `main.qml:203-213`):

```qml
    Timer {
        id: statusTimer
        interval: 3000
        running: true
        repeat: true
        triggeredOnStart: true
        onTriggered: {
            root.runCtl(["status"], "status")
            root.runCtl(["devices"], "discover")
        }
    }
```

with:

```qml
    Timer {
        id: statusTimer
        interval: 3000
        running: true
        repeat: true
        triggeredOnStart: true
        onTriggered: {
            root.runCtl(["status"], "status")
            root.runCtl(["devices"], "discover")
            root.runCtl(["screens"], "screens")
        }
    }
```

- [ ] **Step 4: Handle the `screens` and `screen-set` responses**

In `handleResponse` (currently `main.qml:145-200`), insert two new branches between the existing `"discover"` and `"connect" || "pin"` branches:

```qml
        } else if (action === "discover") {
            if (resp.ok && resp.devices) {
                root.deviceList = resp.devices
            } else {
                root.errorText = resp.error || "Discovery failed"
            }
            root.runCtl(["status"], "status")
        } else if (action === "screens") {
            if (resp.ok) {
                root.screenList = resp.screens || []
                root.currentScreen = resp.current_screen || "auto"
            }
        } else if (action === "screen-set") {
            if (!resp.ok) {
                root.errorText = resp.error || "Failed to change screen"
            }
            root.runCtl(["screens"], "screens")
        } else if (action === "connect" || action === "pin") {
```

- [ ] **Step 5: Add the screen-picker button and menu**

In the header `RowLayout` (currently `main.qml:258-301`), insert a new `Controls.ToolButton` right after the existing refresh button (which ends at `main.qml:277`):

```qml
                Controls.ToolButton {
                    icon.name: "view-refresh"
                    display: Controls.ToolButton.IconOnly
                    Controls.ToolTip.text: "Refresh devices"
                    Controls.ToolTip.visible: hovered
                    enabled: !root.isBusy
                    onClicked: {
                        root.runCtl(["discover"], "discover")
                    }
                }
                Controls.ToolButton {
                    id: screenButton
                    icon.name: "video-display-symbolic"
                    display: Controls.ToolButton.IconOnly
                    Controls.ToolTip.text: "Screen: " + root.screenLabelFor(root.currentScreen)
                    Controls.ToolTip.visible: hovered
                    enabled: !root.isBusy
                    onClicked: screenMenu.popup()

                    Controls.Menu {
                        id: screenMenu

                        Controls.MenuItem {
                            text: "Auto (primary)"
                            checkable: true
                            checked: root.currentScreen === "auto"
                            onTriggered: root.runCtl(["screen-set", "auto"], "screen-set")
                        }

                        Repeater {
                            model: root.screenList
                            delegate: Controls.MenuItem {
                                text: modelData.is_virtual
                                      ? ("Virtual Screen (extend desktop)" + (modelData.available ? "" : " — unavailable"))
                                      : (modelData.name + " (" + modelData.width + "x" + modelData.height + ")" + (modelData.primary ? " · primary" : ""))
                                enabled: !modelData.is_virtual || modelData.available
                                checkable: true
                                checked: root.currentScreen === modelData.name
                                onTriggered: root.runCtl(["screen-set", modelData.name], "screen-set")
                            }
                        }
                    }
                }
```

- [ ] **Step 6: Manual verification**

```sh
go build -o bin/doubletake ./cmd/doubletake
go build -o bin/doubletake-ctl ./cmd/doubletake-ctl
sudo install -m755 bin/doubletake bin/doubletake-ctl /usr/local/bin/
kpackagetool6 -t Plasma/Applet -u plasmoid/
```

Then: start the daemon (`doubletake -daemonize -creds ~/.config/doubletake/credentials.json &`), add/open the tray widget, and confirm:
- A new screen icon appears in the header next to refresh/stop/mute.
- Clicking it opens a menu with "Auto (primary)", any connected monitors, and a "Virtual Screen" entry (disabled if unavailable).
- Selecting an entry while idle updates the tooltip to reflect the new current screen.
- Selecting an entry while a stream is active shows the "stop streaming before changing screen" error in the existing error banner.

- [ ] **Step 7: Commit**

```bash
git add plasmoid/contents/ui/main.qml
git commit -m "plasmoid: add screen picker to the tray widget"
```

---

## Self-Review

**Spec coverage:**
- CLI flags (`-screen`, `-virtual-position`, `-list-screens`) → Task 7.
- X11 named-screen selection → Tasks 1-2.
- X11 virtual monitor creation/teardown, rollback on partial failure → Tasks 3-5.
- Wayland picker-reappear behavior + "not yet supported" for virtual → Task 6.
- Daemon `Config`/protocol (`screens`, `screen-set`), reject-while-streaming → Task 8.
- `doubletake-ctl` subcommands → Task 9.
- Plasmoid UI → Task 10.
- Error handling table in the spec is covered: named-not-found (Task 2), no virtual candidate (Task 5), Wayland virtual unsupported (Task 6), rollback on partial xrandr failure (Task 5), `screen-set` rejection while streaming (Task 8).

**Placeholder scan:** none found — every step has complete code, exact commands, and expected output.

**Type consistency:** `CaptureConfig.ScreenID`/`VirtualPosition` (Task 2) match usage in Tasks 5-8. `resolveX11CaptureRegion`'s signature change from Task 2 (`(int,int,int,int,error)`) to Task 5 (`(int,int,int,int,func() error,error)`) is called out explicitly in both tasks' Interfaces blocks so a reader hitting Task 5 alone isn't confused by the mismatch. `ScreenInfo` fields (Task 8) match the QML `modelData.*` accessors (Task 10): `name`→`modelData.name`, `width`/`height`, `primary`, `is_virtual`, `available` — all via the `json:` tags in the Task 8 struct.

## Execution

Plan complete and saved to `docs/superpowers/plans/2026-07-08-screen-selection.md`. Two execution options:

1. **Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.
2. **Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints.
