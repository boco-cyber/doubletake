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
