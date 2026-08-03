package airplay

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

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

const (
	defaultVideoBitrateKbps = 4500
	minVideoBitrateKbps     = 1800
	maxVideoBitrateKbps     = 12000

	// Synthetic test capture has no real display to size itself from, so it
	// uses a fixed resolution.
	testCaptureWidth  = 1920
	testCaptureHeight = 1080
)

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

// StartCapture detects the display server (Wayland or X11) and initiates screen
// capture accordingly. On Wayland it uses xdg-desktop-portal + PipeWire for
// capture; on X11 it uses ximagesrc. Both use GStreamer for H.264 encoding.
func StartCapture(ctx context.Context, cfg CaptureConfig) (*ScreenCapture, error) {
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		return startWaylandCapture(ctx, cfg)
	}
	if os.Getenv("DISPLAY") != "" {
		return startX11Capture(ctx, cfg)
	}
	return nil, fmt.Errorf("no display server detected (neither WAYLAND_DISPLAY nor DISPLAY is set)")
}

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
	if err != nil {
		return nil, fmt.Errorf("screencast portal: %w", err)
	}
	if restoreToken != "" && cfg.SaveRestoreToken != nil {
		if err := cfg.SaveRestoreToken(restoreToken); err != nil {
			log.Printf("[CAPTURE] warning: failed to save screencast restore token: %v", err)
		}
	}
	dbg("pipewire node ID: %d", nodeID)

	captureCtx, cancel := context.WithCancel(ctx)

	fps := cfg.FPS
	if fps <= 0 {
		fps = 30
	}

	encoderParts := detectGstEncoder(cfg)

	vapostprocOK := vapostprocAvailable()
	if !vapostprocOK {
		log.Printf("[CAPTURE] vapostproc not available (VA-API driver not working); falling back to videoconvert only, which may show a black screen on some drivers")
	}
	const pwFdNum = 3
	gstArgs := buildWaylandGstArgs(pwFdNum, nodeID, fps, encoderParts, vapostprocOK)

	dbg("[CAPTURE] gst-launch-1.0 (wayland) %s", strings.Join(gstArgs, " "))
	cmd := exec.CommandContext(captureCtx, "gst-launch-1.0", gstArgs...)
	cmd.ExtraFiles = []*os.File{pwFd}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		pwFd.Close()
		dbusConn.Close()
		return nil, fmt.Errorf("gst stdout pipe: %w", err)
	}
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		cancel()
		pwFd.Close()
		dbusConn.Close()
		return nil, fmt.Errorf("start gst-launch: %w", err)
	}
	pwFd.Close() // child inherited it

	go logStderr("GST", stderr)

	capture := &ScreenCapture{
		cmd:      cmd,
		stdout:   stdout,
		cancel:   cancel,
		pwNodeID: nodeID,
		dbusConn: dbusConn,
		waitCh:   make(chan struct{}),
	}
	go func() {
		capture.waitErr = cmd.Wait()
		close(capture.waitCh)
	}()

	return capture, nil
}

// vapostprocAvailable reports whether GStreamer's "va" plugin has registered
// the vapostproc element. The plugin library can be installed yet register
// zero elements when its VA-API driver fails to initialize (e.g. NVIDIA GPUs
// without a working VA-API driver), so this probes at runtime rather than
// assuming availability from the package being present.
func vapostprocAvailable() bool {
	return exec.Command("gst-inspect-1.0", "vapostproc").Run() == nil
}

// buildWaylandGstArgs assembles the gst-launch-1.0 pipeline that captures
// from the PipeWire portal and encodes to H.264.
//   - vapostproc imports the portal's DMA-BUF via VA-API (plain videoconvert
//     fails to negotiate DMA-BUF on many drivers, giving a black screen); it
//     is only included when vapostprocOK, since on some drivers the element
//     isn't registered at all and gst-launch would fail outright.
//   - format=I420 forces 4:2:0 — RGB screens otherwise make x264enc emit
//     "High 4:4:4 Predictive", which most receiver decoders reject (black).
//   - videorate re-stamps buffers onto a regular fps timeline: the portal can
//     deliver pts=0, which confuses encoder/muxer timing. drop-only=true never
//     duplicates frames during idle periods (no wasted bandwidth on a static
//     screen); skip-to-first avoids buffering before the first frame.
//
// The stream is encoded at the portal's native resolution; we do not rescale
// because the captured surface size is whatever the compositor hands us. The
// actual encoded dimensions are read back from the H.264 SPS downstream.
func buildWaylandGstArgs(pwFdNum int, nodeID uint32, fps int, encoderParts encoderResult, vapostprocOK bool) []string {
	gstArgs := []string{
		"--quiet",
		"pipewiresrc", fmt.Sprintf("fd=%d", pwFdNum), fmt.Sprintf("path=%d", nodeID), "do-timestamp=true",
	}
	if vapostprocOK {
		gstArgs = append(gstArgs, "!", "vapostproc")
	}
	gstArgs = append(gstArgs,
		"!", "video/x-raw,format=I420",
		"!", "videoconvert",
		"!", "videorate", "drop-only=true", "skip-to-first=true",
		"!", fmt.Sprintf("video/x-raw,framerate=%d/1", fps),
		"!", "queue", "max-size-buffers=1", "max-size-bytes=0", "max-size-time=0", "leaky=downstream",
	)
	if encoderParts.needsVulkan {
		gstArgs = append(gstArgs, "!", "vulkanupload")
	}
	gstArgs = append(gstArgs, "!")
	gstArgs = append(gstArgs, encoderParts.parts...)
	gstArgs = append(gstArgs,
		"!", "h264parse", "config-interval=-1",
		"!", "video/x-h264,stream-format=byte-stream,alignment=au",
		"!", "fdsink", "fd=1", "sync=false", "async=false",
	)
	return gstArgs
}

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

func (sc *ScreenCapture) Read(buf []byte) (int, error) {
	select {
	case <-sc.waitCh:
		if sc.waitErr != nil {
			return 0, fmt.Errorf("capture exited: %w", sc.waitErr)
		}
		return 0, io.EOF
	default:
	}
	return sc.stdout.Read(buf)
}

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

// MonitorInfo describes one X11 RandR output as reported by xrandr.
type MonitorInfo struct {
	Name          string
	Connected     bool
	Primary       bool
	X, Y          int
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

// parseXrandrGeometry extracts the X/Y offset and width/height from an xrandr
// output line.
func parseXrandrGeometry(line string) (xOffset, yOffset, width, height int, ok bool) {
	// Match WxH+X+Y pattern
	for _, field := range strings.Fields(line) {
		// e.g. "1920x1080+0+0" or "3840x2160+1920+0"
		parts := strings.SplitN(field, "x", 2)
		if len(parts) != 2 {
			continue
		}
		w, err := strconv.Atoi(parts[0])
		if err != nil || w < 640 {
			continue
		}
		rest := parts[1] // e.g. "1080+0+0"
		plusParts := strings.SplitN(rest, "+", 3)
		if len(plusParts) != 3 {
			continue
		}
		h, err := strconv.Atoi(plusParts[0])
		if err != nil {
			continue
		}
		x, err := strconv.Atoi(plusParts[1])
		if err != nil {
			continue
		}
		y, err := strconv.Atoi(plusParts[2])
		if err != nil {
			continue
		}
		return x, y, w, h, true
	}
	return 0, 0, 0, 0, false
}

// encoderResult holds the detected encoder pipeline parts and whether it needs
// a vulkanupload step before the encoder.
type encoderResult struct {
	parts       []string
	needsVulkan bool // encoder needs vulkanupload ! before it
}

// detectGstEncoder probes for available GStreamer H.264 encoders and returns
// the encoder element + properties as gst-launch-1.0 arguments.
// Priority: vulkanh264enc (NVENC via Vulkan) > nvh264enc > vah264enc > x264enc.
func detectGstEncoder(cfg CaptureConfig) encoderResult {
	fps := cfg.FPS
	if fps <= 0 {
		fps = 30
	}
	bitrate := captureBitrateKbps(cfg)
	keyframeInterval := keyframeIntervalFrames(fps)
	hwaccel := cfg.HWAccel

	// Try Vulkan H.264 (NVENC via Vulkan API) — lowest latency, no CPU usage
	if hwaccel == "auto" || hwaccel == "nvenc" {
		if exec.Command("gst-inspect-1.0", "vulkanh264enc").Run() == nil {
			log.Printf("[CAPTURE] using NVENC hardware encoding (vulkanh264enc)")
			return encoderResult{
				parts: []string{
					"vulkanh264enc",
					"b-frames=0",
					fmt.Sprintf("idr-period=%d", keyframeInterval),
					"rate-control=cbr",
					fmt.Sprintf("bitrate=%d", bitrate),
				},
				needsVulkan: true,
			}
		}
	}

	// Try legacy NVENC
	if hwaccel == "auto" || hwaccel == "nvenc" {
		if exec.Command("gst-inspect-1.0", "nvh264enc").Run() == nil {
			log.Printf("[CAPTURE] using NVENC hardware encoding (nvh264enc)")
			return encoderResult{parts: []string{
				"nvh264enc",
				fmt.Sprintf("bitrate=%d", bitrate),
				fmt.Sprintf("gop-size=%d", keyframeInterval),
				"bframes=0",
				"rc-mode=cbr",
				"preset=low-latency-hq",
				"zerolatency=true",
			}}
		}
		if hwaccel == "nvenc" {
			dbg("[CAPTURE] nvh264enc not available, falling back to software")
		}
	}

	// Try VAAPI
	if hwaccel == "auto" || hwaccel == "vaapi" {
		if exec.Command("gst-inspect-1.0", "vah264enc").Run() == nil {
			log.Printf("[CAPTURE] using VAAPI hardware encoding (vah264enc)")
			return encoderResult{parts: []string{
				"vah264enc",
				fmt.Sprintf("bitrate=%d", bitrate),
				fmt.Sprintf("key-int-max=%d", keyframeInterval),
				"b-frames=0",
				"rate-control=cbr",
			}}
		}
		if hwaccel == "vaapi" {
			dbg("[CAPTURE] vah264enc not available, falling back to software")
		}
	}

	// Software fallback: x264enc
	log.Printf("[CAPTURE] using software encoding (x264enc)")
	vbvBuf := vbvBufferKbit(bitrate, fps)
	// Use VBR (pass=0) so the encoder can undershoot on simple scenes, saving
	// headroom for complex frames. vbv-buf-capacity + vbv-maxrate cap bursts.
	maxrate := bitrate + bitrate/4 // allow 25% overshoot on peaks
	return encoderResult{parts: []string{
		"x264enc",
		"tune=zerolatency",
		"speed-preset=superfast",
		fmt.Sprintf("bitrate=%d", bitrate),
		fmt.Sprintf("vbv-buf-capacity=%d", vbvBuf),
		fmt.Sprintf("key-int-max=%d", keyframeInterval),
		"pass=0",
		"option-string=" + fmt.Sprintf("vbv-maxrate=%d", maxrate),
		"bframes=0",
		"sliced-threads=true",
		"byte-stream=true",
		"aud=true",
	}}
}

// StartTestCapture creates a synthetic H.264 video stream using GStreamer's
// videotestsrc + x264enc, producing High profile Annex-B byte stream output.
// This replicates the same GStreamer pipeline ecosystem that UxPlay uses on the
// receiver side.
func StartTestCapture(ctx context.Context, cfg CaptureConfig) (*ScreenCapture, error) {
	captureCtx, cancel := context.WithCancel(ctx)

	fps := cfg.FPS
	if fps <= 0 {
		fps = 30
	}

	bitrate := captureBitrateKbps(cfg)
	keyframeInterval := keyframeIntervalFrames(fps)

	// GStreamer pipeline: videotestsrc → timeoverlay → x264enc High profile → Annex-B byte stream → stdout
	// pattern=18 = ball (bouncing ball with motion); timeoverlay adds a frame counter.
	// Keep test source live/infinite so long-running audio tests do not stop with EOF.
	gstArgs := []string{
		"--quiet",
		"videotestsrc", "pattern=18", "is-live=true", "do-timestamp=true",
		"!", fmt.Sprintf("video/x-raw,width=%d,height=%d,framerate=%d/1", testCaptureWidth, testCaptureHeight, fps),
		"!", "timeoverlay",
		"!", "videoconvert",
		"!", "x264enc",
		"tune=zerolatency",
		"speed-preset=superfast",
		fmt.Sprintf("bitrate=%d", bitrate),
		fmt.Sprintf("key-int-max=%d", keyframeInterval),
		"threads=1",
		"sliced-threads=true",
		"byte-stream=true",
		"!", "video/x-h264,profile=high,stream-format=byte-stream",
		"!", "fdsink", "fd=1",
	}

	dbg("[CAPTURE] launching gst-launch-1.0 (test mode) %s", strings.Join(gstArgs, " "))
	cmd := exec.CommandContext(captureCtx, "gst-launch-1.0", gstArgs...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("gst stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("gst stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start gst-launch-1.0: %w", err)
	}

	go logStderr("GST", stderr)

	capture := &ScreenCapture{
		cmd:    cmd,
		stdout: stdout,
		cancel: cancel,
		waitCh: make(chan struct{}),
	}
	go func() {
		capture.waitErr = cmd.Wait()
		close(capture.waitCh)
	}()

	return capture, nil
}

func logStderr(prefix string, r io.Reader) {
	if r == nil {
		return
	}
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		dbg("[%s] %s", prefix, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		dbg("[%s] stderr read error: %v", prefix, err)
	}
}

func captureBitrateKbps(cfg CaptureConfig) int {
	if cfg.Bitrate > 0 {
		return cfg.Bitrate
	}

	fps := cfg.FPS
	if fps <= 0 {
		fps = 30
	}
	// The encoded resolution is not known until frames flow (it comes from the
	// captured display), so size the auto bitrate for a 1080p budget. Pass
	// -bitrate to override for higher-resolution displays.
	width, height := 1920, 1080

	bitrate := recommendedBitrateKbps(width, height, fps)
	log.Printf("[CAPTURE] auto bitrate selected: %d kbps for %dx%d@%dfps", bitrate, width, height, fps)
	return bitrate
}

func recommendedBitrateKbps(width, height, fps int) int {
	if width <= 0 || height <= 0 || fps <= 0 {
		return defaultVideoBitrateKbps
	}

	bitrate := (width*height*fps + 7500) / 15000
	if bitrate < minVideoBitrateKbps {
		return minVideoBitrateKbps
	}
	if bitrate > maxVideoBitrateKbps {
		return maxVideoBitrateKbps
	}
	return bitrate
}

func keyframeIntervalFrames(fps int) int {
	if fps <= 0 {
		fps = 30
	}
	return fps * 4
}

// vbvBufferKbit returns the x264 VBV buffer size in kbit for the given bitrate
// and FPS. Sized at ~2 frames of data — enough headroom for the encoder to
// handle scene changes without severe quality oscillation, but tight enough to
// prevent large burst spikes that choke Wi-Fi links.
func vbvBufferKbit(bitrateKbps, fps int) int {
	if bitrateKbps <= 0 || fps <= 0 {
		return 300
	}
	vbv := bitrateKbps * 2 / fps
	if vbv < 200 {
		return 200
	}
	return vbv
}

// requestScreencast uses the xdg-desktop-portal D-Bus API to request screen capture
// permission and returns a PipeWire node ID, an fd for the portal's PipeWire remote,
// the D-Bus connection (which must stay open to keep the screencast session alive),
// and a fresh restore token when the portal grants persistence.
func requestScreencast(ctx context.Context, restoreToken string) (uint32, *os.File, *dbus.Conn, string, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return 0, nil, nil, "", fmt.Errorf("connect session bus: %w", err)
	}

	portal := conn.Object("org.freedesktop.portal.Desktop",
		"/org/freedesktop/portal/desktop")
	portalVersion := screenCastPortalVersion(portal)
	baseToken := newPortalHandleToken()

	// Create session
	sessionOpts := map[string]dbus.Variant{
		"handle_token":         dbus.MakeVariant(baseToken),
		"session_handle_token": dbus.MakeVariant(baseToken + "_session"),
	}

	var requestHandle dbus.ObjectPath
	call := portal.Call("org.freedesktop.portal.ScreenCast.CreateSession", 0, sessionOpts)
	if call.Err != nil {
		conn.Close()
		return 0, nil, nil, "", fmt.Errorf("CreateSession: %w", call.Err)
	}
	if err := call.Store(&requestHandle); err != nil {
		conn.Close()
		return 0, nil, nil, "", fmt.Errorf("store create-session request handle: %w", err)
	}

	createResult, err := waitForResponseWithResult(ctx, conn, requestHandle)
	if err != nil {
		conn.Close()
		return 0, nil, nil, "", fmt.Errorf("session response: %w", err)
	}

	sessionPath, err := sessionHandleFromResult(createResult)
	if err != nil {
		conn.Close()
		return 0, nil, nil, "", fmt.Errorf("session handle: %w", err)
	}

	// Select sources (screen)
	selectOpts := map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant(baseToken + "_select"),
		"types":        dbus.MakeVariant(uint32(1)), // MONITOR=1, WINDOW=2
		"multiple":     dbus.MakeVariant(false),
		"cursor_mode":  dbus.MakeVariant(uint32(2)), // EMBEDDED=2 (cursor in stream)
	}
	if portalVersion >= 4 {
		selectOpts["persist_mode"] = dbus.MakeVariant(uint32(2))
		if restoreToken != "" {
			selectOpts["restore_token"] = dbus.MakeVariant(restoreToken)
			dbg("[CAPTURE] requesting screencast restore with saved token")
		}
	}

	requestHandle = ""
	call = portal.Call("org.freedesktop.portal.ScreenCast.SelectSources", 0,
		sessionPath, selectOpts)
	if call.Err != nil {
		conn.Close()
		return 0, nil, nil, "", fmt.Errorf("SelectSources: %w", call.Err)
	}
	if err := call.Store(&requestHandle); err != nil {
		conn.Close()
		return 0, nil, nil, "", fmt.Errorf("store select-sources request handle: %w", err)
	}

	if _, err = waitForResponseWithResult(ctx, conn, requestHandle); err != nil {
		conn.Close()
		return 0, nil, nil, "", fmt.Errorf("select response: %w", err)
	}

	// Start the screencast
	startOpts := map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant(baseToken + "_start"),
	}

	requestHandle = ""
	call = portal.Call("org.freedesktop.portal.ScreenCast.Start", 0,
		sessionPath, "", startOpts)
	if call.Err != nil {
		conn.Close()
		return 0, nil, nil, "", fmt.Errorf("Start: %w", call.Err)
	}
	if err := call.Store(&requestHandle); err != nil {
		conn.Close()
		return 0, nil, nil, "", fmt.Errorf("store start request handle: %w", err)
	}

	startResult, err := waitForResponseWithResult(ctx, conn, requestHandle)
	if err != nil {
		conn.Close()
		return 0, nil, nil, "", fmt.Errorf("start response: %w", err)
	}

	newRestoreToken := ""
	if variant, ok := startResult["restore_token"]; ok {
		value, ok := variant.Value().(string)
		if !ok {
			conn.Close()
			return 0, nil, nil, "", fmt.Errorf("unexpected restore token type: %T", variant.Value())
		}
		newRestoreToken = value
	}

	// Extract PipeWire node ID from the result
	streams, ok := startResult["streams"]
	if !ok {
		conn.Close()
		return 0, nil, nil, "", fmt.Errorf("no streams in start response")
	}

	var nodeID uint32
	streamList, ok := streams.Value().([][]interface{})
	if !ok {
		// Try alternate format
		if v, ok2 := streams.Value().([]interface{}); ok2 && len(v) > 0 {
			if tuple, ok3 := v[0].([]interface{}); ok3 && len(tuple) > 0 {
				if nid, ok4 := tuple[0].(uint32); ok4 {
					nodeID = nid
				} else {
					conn.Close()
					return 0, nil, nil, "", fmt.Errorf("unexpected node ID type: %T", tuple[0])
				}
			} else {
				conn.Close()
				return 0, nil, nil, "", fmt.Errorf("unexpected streams format: %T", streams.Value())
			}
		} else {
			conn.Close()
			return 0, nil, nil, "", fmt.Errorf("unexpected streams format: %T", streams.Value())
		}
	} else {
		if len(streamList) == 0 || len(streamList[0]) == 0 {
			conn.Close()
			return 0, nil, nil, "", fmt.Errorf("empty streams list")
		}
		nid, ok2 := streamList[0][0].(uint32)
		if !ok2 {
			conn.Close()
			return 0, nil, nil, "", fmt.Errorf("unexpected node ID type: %T", streamList[0][0])
		}
		nodeID = nid
	}

	// OpenPipeWireRemote returns a Unix fd for the portal's PipeWire remote.
	// pipewiresrc MUST use this fd to connect; without it, it connects to the
	// global PipeWire instance which does not have the portal node and returns EINVAL.
	call = portal.Call("org.freedesktop.portal.ScreenCast.OpenPipeWireRemote", 0,
		sessionPath, map[string]dbus.Variant{})
	if call.Err != nil {
		conn.Close()
		return 0, nil, nil, "", fmt.Errorf("OpenPipeWireRemote: %w", call.Err)
	}
	var pwFD dbus.UnixFD
	if err := call.Store(&pwFD); err != nil {
		conn.Close()
		return 0, nil, nil, "", fmt.Errorf("store pipewire fd: %w", err)
	}

	return nodeID, os.NewFile(uintptr(pwFD), "pipewire-remote"), conn, newRestoreToken, nil
}

func waitForResponseWithResult(ctx context.Context, conn *dbus.Conn, requestHandle dbus.ObjectPath) (map[string]dbus.Variant, error) {
	ch := make(chan *dbus.Signal, 1)
	conn.Signal(ch)
	defer conn.RemoveSignal(ch)

	matchRule := "type='signal',interface='org.freedesktop.portal.Request',member='Response'"
	if call := conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, matchRule); call.Err != nil {
		return nil, fmt.Errorf("add portal response match: %w", call.Err)
	}
	defer conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, matchRule)

	for {
		select {
		case sig := <-ch:
			if sig == nil || sig.Path != requestHandle {
				continue
			}
			if len(sig.Body) < 2 {
				return nil, fmt.Errorf("signal body too short")
			}
			status, ok := sig.Body[0].(uint32)
			if !ok {
				return nil, fmt.Errorf("unexpected status type")
			}
			if status != 0 {
				return nil, fmt.Errorf("portal request failed with status %d", status)
			}
			result, ok := sig.Body[1].(map[string]dbus.Variant)
			if !ok {
				return nil, fmt.Errorf("unexpected result type: %T", sig.Body[1])
			}
			return result, nil

		case <-ctx.Done():
			return nil, fmt.Errorf("timeout waiting for portal response: %w", ctx.Err())
		}
	}
}

func newPortalHandleToken() string {
	return fmt.Sprintf("airplay_cast_%d", time.Now().UnixNano())
}

func screenCastPortalVersion(portal dbus.BusObject) uint32 {
	variant, err := portal.GetProperty("org.freedesktop.portal.ScreenCast.version")
	if err != nil {
		dbg("[CAPTURE] unable to read ScreenCast portal version: %v", err)
		return 0
	}
	version, ok := variant.Value().(uint32)
	if !ok {
		dbg("[CAPTURE] unexpected ScreenCast portal version type: %T", variant.Value())
		return 0
	}
	return version
}

func sessionHandleFromResult(result map[string]dbus.Variant) (dbus.ObjectPath, error) {
	variant, ok := result["session_handle"]
	if !ok {
		return "", fmt.Errorf("missing session_handle in portal response")
	}
	if sessionHandle, ok := variant.Value().(string); ok {
		return dbus.ObjectPath(sessionHandle), nil
	}
	if sessionHandle, ok := variant.Value().(dbus.ObjectPath); ok {
		return sessionHandle, nil
	}
	return "", fmt.Errorf("unexpected session_handle type: %T", variant.Value())
}
