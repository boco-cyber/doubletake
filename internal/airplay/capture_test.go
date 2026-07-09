package airplay

import (
	"strings"
	"testing"
)

func TestRecommendedBitrateKbps(t *testing.T) {
	tests := []struct {
		name   string
		width  int
		height int
		fps    int
		want   int
	}{
		{
			name:   "defaults when dimensions invalid",
			width:  0,
			height: 1080,
			fps:    30,
			want:   defaultVideoBitrateKbps,
		},
		{
			name:   "low resolution clamps to floor",
			width:  640,
			height: 360,
			fps:    30,
			want:   minVideoBitrateKbps,
		},
		{
			name:   "720p30 stays near wifi target",
			width:  1280,
			height: 720,
			fps:    30,
			want:   1843,
		},
		{
			name:   "1080p30 uses wifi friendly auto bitrate",
			width:  1920,
			height: 1080,
			fps:    30,
			want:   4147,
		},
		{
			name:   "high resolutions clamp to max",
			width:  3840,
			height: 2160,
			fps:    60,
			want:   maxVideoBitrateKbps,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := recommendedBitrateKbps(tt.width, tt.height, tt.fps); got != tt.want {
				t.Fatalf("recommendedBitrateKbps(%d, %d, %d) = %d, want %d", tt.width, tt.height, tt.fps, got, tt.want)
			}
		})
	}
}

func TestKeyframeIntervalFrames(t *testing.T) {
	if got := keyframeIntervalFrames(30); got != 120 {
		t.Fatalf("keyframeIntervalFrames(30) = %d, want 120", got)
	}
	if got := keyframeIntervalFrames(0); got != 120 {
		t.Fatalf("keyframeIntervalFrames(0) = %d, want 120", got)
	}
}

func TestBuildWaylandGstArgsSkipsMissingVapostproc(t *testing.T) {
	encoder := encoderResult{parts: []string{"x264enc"}}

	withVapostproc := buildWaylandGstArgs(3, 99, 30, encoder, true)
	if !containsArg(withVapostproc, "vapostproc") {
		t.Fatalf("expected pipeline to include vapostproc when available: %s", strings.Join(withVapostproc, " "))
	}

	withoutVapostproc := buildWaylandGstArgs(3, 99, 30, encoder, false)
	if containsArg(withoutVapostproc, "vapostproc") {
		t.Fatalf("expected pipeline to omit vapostproc when unavailable: %s", strings.Join(withoutVapostproc, " "))
	}
	if !containsArg(withoutVapostproc, "videoconvert") {
		t.Fatalf("expected pipeline to still include videoconvert when vapostproc unavailable: %s", strings.Join(withoutVapostproc, " "))
	}
	if !containsArg(withoutVapostproc, "pipewiresrc") {
		t.Fatalf("expected pipeline to still capture from pipewiresrc: %s", strings.Join(withoutVapostproc, " "))
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestVbvBufferKbit(t *testing.T) {
	tests := []struct {
		name    string
		bitrate int
		fps     int
		want    int
	}{
		{"invalid returns default", 0, 30, 300},
		{"low bitrate clamps to floor", 1800, 30, 200},
		{"1080p30 auto bitrate", 4147, 30, 276},
		{"high bitrate 60fps", 12000, 60, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := vbvBufferKbit(tt.bitrate, tt.fps); got != tt.want {
				t.Fatalf("vbvBufferKbit(%d, %d) = %d, want %d", tt.bitrate, tt.fps, got, tt.want)
			}
		})
	}
}

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
