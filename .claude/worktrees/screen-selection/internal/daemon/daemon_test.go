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
