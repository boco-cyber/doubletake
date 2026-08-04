package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"doubletake/internal/airplay"
	"doubletake/internal/daemon"
	"doubletake/internal/shell"
)

// parsePortRange parses a "min-max" string into inclusive port bounds.
// An empty string returns (0, 0, nil) meaning "let the OS pick".
func parsePortRange(s string) (int, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, nil
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected MIN-MAX, got %q", s)
	}
	lo, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("min: %w", err)
	}
	hi, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("max: %w", err)
	}
	if lo < 1 || hi > 65535 || lo > hi {
		return 0, 0, fmt.Errorf("range %d-%d out of bounds (1-65535, min<=max)", lo, hi)
	}
	if hi-lo+1 < 4 {
		return 0, 0, fmt.Errorf("range %d-%d too small; need at least 4 ports (3 UDP + 1 TCP)", lo, hi)
	}
	return lo, hi, nil
}

func main() {
	target := flag.String("target", "", "Apple TV IP address or hostname (skip discovery)")
	port := flag.Int("port", 7000, "AirPlay port")
	pin := flag.String("pin", "", "4-digit PIN for pairing (shown on Apple TV)")
	credFile := flag.String("creds", airplay.DefaultCredentialsPath(), "Path to saved pairing credentials")
	credBackend := flag.String("cred-backend", "file", "Credential storage backend: file or keyring (system keyring via Secret Service)")
	forcePair := flag.Bool("pair", false, "Force new pairing even if credentials exist")
	fps := flag.Int("fps", 30, "Frames per second")
	bitrate := flag.Int("bitrate", 0, "Video bitrate in kbps (0 = auto, default tunes for resolution/FPS)")
	targetLatencyMs := flag.Int("target-latency-ms", 100, "Target end-to-end latency in milliseconds (applies to audio and video timing)")
	hwaccel := flag.String("hwaccel", "auto", "Hardware acceleration: auto, nvenc, vaapi, none")
	screen := flag.String("screen", "", "Screen to capture: empty = auto-detect (X11) / portal picker (Wayland), an xrandr output name, or \"virtual\" for a virtual extended-desktop monitor")
	virtualPosition := flag.String("virtual-position", "right", "Position of the virtual monitor relative to the primary: left, right, above, or below (only used with -screen virtual)")
	listScreens := flag.Bool("list-screens", false, "List available screens (physical outputs and virtual monitor availability) and exit")
	testMode := flag.Bool("test", false, "Use synthetic video (videotestsrc) instead of screen capture for debugging")
	noEncrypt := flag.Bool("no-encrypt", false, "Disable RTSP header encryption (debugging only; video frames are always encrypted)")
	directKey := flag.Bool("direct-key", false, "Use shk/shiv directly without SHA-512 derivation")
	noAudio := flag.Bool("no-audio", false, "Disable audio streaming")
	portRange := flag.String("port-range", "", "Local UDP/TCP port range for the receiver to reach back (e.g. \"60000-60010\"); empty = OS ephemeral. Needs at least 4 ports.")
	debug := flag.Bool("debug", false, "Enable verbose debug logging")
	daemonize := flag.Bool("daemonize", false, "Run as background daemon with Unix socket control interface")
	socketPath := flag.String("socket", daemon.DefaultSocketPath(), "Unix socket path for daemon control interface")
	shellMode := flag.Bool("shell", false, "Start the daemon and open the browser control shell")
	shellListen := flag.String("shell-listen", "127.0.0.1:8199", "HTTP address for the browser control shell")
	shellUIDir := flag.String("shell-ui-dir", "", "Directory containing the built browser shell (auto-detect when empty)")
	shellOpen := flag.Bool("shell-open", true, "Open the browser control shell on startup")
	openShellWindow := flag.Bool("open-shell-window", false, "Open the running browser control shell as an app window")
	installApp := flag.Bool("install", false, "Install Doubletake into the user application menu")
	installPrefix := flag.String("install-prefix", "", "Installation prefix for -install (default: ~/.local)")
	flag.Parse()

	if *openShellWindow {
		if err := shell.OpenAppWindow("http://" + *shellListen); err != nil {
			log.Fatalf("open shell window failed: %v", err)
		}
		return
	}

	if *installApp {
		if err := selfInstall(*installPrefix); err != nil {
			log.Fatalf("install failed: %v", err)
		}
		return
	}

	flagsSet := visitedFlags()
	var savedShellSettings *uiSettings
	if *shellMode {
		if value, err := loadUISettings(); err == nil {
			savedShellSettings = value
			applyUISettings(value, flagsSet, screen, virtualPosition, fps, bitrate, targetLatencyMs, hwaccel, noAudio, socketPath, credFile, credBackend, forcePair, debug, testMode, noEncrypt, directKey)
		} else if !errors.Is(err, os.ErrNotExist) {
			log.Printf("[shell] saved settings ignored: %v", err)
		}
	}

	switch *virtualPosition {
	case "left", "right", "above", "below":
	default:
		log.Fatalf("invalid -virtual-position %q (want left, right, above, or below)", *virtualPosition)
	}

	if *listScreens {
		printAvailableScreens()
		return
	}

	portMin, portMax, err := parsePortRange(*portRange)
	if err != nil {
		log.Fatalf("invalid -port-range: %v", err)
	}
	if *shellMode && savedShellSettings != nil && !flagsSet["port-range"] {
		portMin = savedShellSettings.PortMin
		portMax = savedShellSettings.PortMax
	}

	airplay.SetTargetLatency(time.Duration(*targetLatencyMs) * time.Millisecond)

	airplay.DebugMode = *debug

	if *shellMode {
		runShellApp(shellRunConfig{
			socketPath:      *socketPath,
			credFile:        *credFile,
			credBackend:     *credBackend,
			fps:             *fps,
			bitrate:         *bitrate,
			targetLatencyMS: *targetLatencyMs,
			hwaccel:         *hwaccel,
			screen:          *screen,
			virtualPosition: *virtualPosition,
			debug:           *debug,
			testMode:        *testMode,
			noEncrypt:       *noEncrypt,
			directKey:       *directKey,
			noAudio:         *noAudio,
			portMin:         portMin,
			portMax:         portMax,
			forcePair:       *forcePair,
			listen:          *shellListen,
			uiDir:           *shellUIDir,
			openBrowser:     *shellOpen,
		})
		return
	}

	if *daemonize {
		runDaemon(*socketPath, *credFile, *credBackend, *fps, *bitrate, *targetLatencyMs, *hwaccel, *screen, *virtualPosition, *debug, *testMode, *noEncrypt, *directKey, *noAudio, portMin, portMax, *forcePair)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("shutting down...")
		cancel()
		// Give goroutines a moment to clean up, then force exit
		go func() {
			time.Sleep(3 * time.Second)
			log.Println("forced exit (timeout)")
			os.Exit(1)
		}()
		// Also force exit on second signal
		<-sigCh
		log.Println("forced exit")
		os.Exit(1)
	}()

	var addr string
	if *target != "" {
		addr = *target
	} else {
		device, err := selectDevice(ctx)
		if err != nil {
			log.Fatalf("discovery failed: %v", err)
		}
		addr = device.IP
		*port = device.Port
		fmt.Printf("selected: %s (%s:%d)\n", device.Name, device.IP, device.Port)
	}

	client := airplay.NewAirPlayClient(addr, *port)
	if err := client.Connect(ctx); err != nil {
		log.Fatalf("connect failed: %v", err)
	}
	defer client.Close()

	info, err := client.GetInfo()
	if err != nil {
		log.Fatalf("get info failed: %v", err)
	}
	log.Printf("connected to: %s (model: %s, initialVolume: %.1f)", info.Name, info.Model, info.InitialVolume)

	// Pairing flow:
	// 1. If --pin provided or --pair forced, do full pair-setup + save credentials
	// 2. If saved credentials exist, load them and do pair-verify only
	// 3. Otherwise, do transient (ephemeral) pairing
	needFullPair := *forcePair || *pin != ""

	credStore, err := newCredentialStore(*credBackend, *credFile)
	if err != nil {
		log.Fatalf("failed to load credentials: %v", err)
	}

	var savedCreds *airplay.SavedCredentials
	if !needFullPair {
		savedCreds = credStore.Lookup(info.DeviceID)
	}

	if needFullPair {
		// Full pair-setup with PIN
		pinVal := *pin
		if pinVal == "" {
			// Trigger PIN display on the TV first, then ask user
			if err := client.StartPINDisplay(); err != nil {
				log.Fatalf("failed to trigger PIN display: %v", err)
			}
			fmt.Print("Enter the PIN shown on Apple TV: ")
			fmt.Scanln(&pinVal)
		}
		if err := client.Pair(ctx, pinVal); err != nil {
			log.Fatalf("pairing failed: %v", err)
		}
		// Save credentials for next time
		if err := credStore.Save(info.DeviceID, client.PairingID, client.PairKeys.Ed25519Public, client.PairKeys.Ed25519Private); err != nil {
			log.Printf("warning: failed to save credentials: %v", err)
		} else {
			log.Printf("credentials saved (%s)", *credBackend)
		}
	} else if savedCreds != nil {
		// Use saved credentials — pair-verify
		log.Printf("using saved credentials (%s)", *credBackend)
		pub, priv := savedCreds.Ed25519Keys()
		client.PairingID = savedCreds.PairingID
		client.PairKeys = &airplay.PairKeys{
			Ed25519Public:  pub,
			Ed25519Private: priv,
		}
		if err := client.PairVerify(ctx); err != nil {
			log.Printf("pair-verify with saved creds failed: %v, falling back to transient pairing", err)
			// Reconnect — the failed pair-verify may have closed the connection
			client.Close()
			if err := client.Connect(ctx); err != nil {
				log.Fatalf("reconnect failed: %v", err)
			}
			if _, err := client.GetInfo(); err != nil {
				log.Fatalf("get info after reconnect failed: %v", err)
			}
			if err := client.Pair(ctx, ""); err != nil {
				log.Printf("transient pairing fallback failed: %v, prompting for PIN", err)
				pinVal := promptForPIN(client)
				// Reconnect for fresh PIN pairing attempt
				client.Close()
				client = airplay.NewAirPlayClient(addr, *port)
				if err := client.Connect(ctx); err != nil {
					log.Fatalf("reconnect failed: %v", err)
				}
				if _, err := client.GetInfo(); err != nil {
					log.Fatalf("get info after reconnect failed: %v", err)
				}
				if err := client.Pair(ctx, pinVal); err != nil {
					log.Fatalf("PIN pairing failed: %v", err)
				}
				// Save credentials for next time
				if err := credStore.Save(info.DeviceID, client.PairingID, client.PairKeys.Ed25519Public, client.PairKeys.Ed25519Private); err != nil {
					log.Printf("warning: failed to save credentials: %v", err)
				} else {
					log.Printf("credentials saved (%s)", *credBackend)
				}
			}
		}
	} else {
		// Transient pairing (no saved creds, no PIN)
		if err := client.Pair(ctx, ""); err != nil {
			log.Printf("transient pairing failed: %v, prompting for PIN", err)
			pinVal := promptForPIN(client)
			// Reconnect for fresh PIN pairing attempt
			client.Close()
			client = airplay.NewAirPlayClient(addr, *port)
			if err := client.Connect(ctx); err != nil {
				log.Fatalf("reconnect failed: %v", err)
			}
			if _, err := client.GetInfo(); err != nil {
				log.Fatalf("get info after reconnect failed: %v", err)
			}
			if err := client.Pair(ctx, pinVal); err != nil {
				log.Fatalf("PIN pairing failed: %v", err)
			}
			// Save credentials for next time
			if err := credStore.Save(info.DeviceID, client.PairingID, client.PairKeys.Ed25519Public, client.PairKeys.Ed25519Private); err != nil {
				log.Printf("warning: failed to save credentials: %v", err)
			} else {
				log.Printf("credentials saved (%s)", *credBackend)
			}
		}
	}
	log.Println("pairing complete")

	// FairPlay setup — establishes fp-setup state and ekey/eiv used for the
	// final encrypted mirror stream. Pair-verify and FairPlay are both needed
	// for Apple TV compatibility in the normal modern flow.
	if client.FpEkey == nil {
		if err := client.FairPlaySetup(ctx); err != nil {
			if !errors.Is(err, airplay.ErrFairPlayUnsupported) {
				log.Fatalf("FairPlay setup failed: %v", err)
			}
			log.Printf("FairPlay SAP unsupported (%v); continuing with pair-verify DataStream setup", err)
		} else {
			log.Println("FairPlay setup complete")
		}
	}

	streamCfg := airplay.StreamConfig{
		FPS:       *fps,
		Bitrate:   *bitrate,
		NoEncrypt: *noEncrypt,
		DirectKey: *directKey,
		NoAudio:   *noAudio,
		PortMin:   portMin,
		PortMax:   portMax,
	}
	session, err := client.SetupMirror(ctx, streamCfg)
	if err != nil {
		log.Fatalf("mirror setup failed: %v", err)
	}
	defer session.Close()
	log.Printf("mirror session ready (data port: %d)", session.DataPort)

	var capture *airplay.ScreenCapture
	if *testMode {
		if *noAudio {
			log.Println("using synthetic video (videotestsrc) for debugging")
		} else {
			log.Println("using synthetic video (videotestsrc) and audio test tone for debugging")
		}
		var err error
		capture, err = airplay.StartTestCapture(ctx, airplay.CaptureConfig{
			FPS:     *fps,
			Bitrate: *bitrate,
			HWAccel: *hwaccel,
		})
		if err != nil {
			log.Fatalf("test capture failed: %v", err)
		}
	} else {
		captureCfg := airplay.CaptureConfig{
			FPS:             *fps,
			Bitrate:         *bitrate,
			HWAccel:         *hwaccel,
			ScreenID:        *screen,
			VirtualPosition: *virtualPosition,
		}
		var err error
		capture, err = airplay.StartCapture(ctx, captureCfg)
		if err != nil {
			log.Fatalf("screen capture failed: %v", err)
		}
	}
	defer capture.Stop()
	go func() {
		<-ctx.Done()
		capture.Stop()
		session.Close()
	}()
	log.Println("screen capture started")

	// Start audio capture and streaming unless disabled.
	if !*noAudio && session.HasAudio() {
		audioCapture, err := airplay.StartAudioCapture(ctx, *testMode)
		if err != nil {
			log.Printf("warning: audio capture failed: %v (continuing without audio)", err)
		} else {
			defer audioCapture.Stop()
			go func() {
				if err := session.StreamAudio(ctx, audioCapture, session.AudioStream()); err != nil && ctx.Err() == nil {
					log.Printf("audio streaming error: %v", err)
				}
			}()
			log.Println("audio capture started")
		}
	} else if !*noAudio {
		log.Println("audio disabled (receiver did not provide audio ports)")
	}

	if err := session.StreamFrames(ctx, capture, 0*time.Second); err != nil && ctx.Err() == nil {
		log.Fatalf("streaming error: %v", err)
	}
	log.Println("stream ended")
}

func promptForPIN(client *airplay.AirPlayClient) string {
	if err := client.StartPINDisplay(); err != nil {
		log.Printf("warning: failed to trigger PIN display: %v", err)
	}
	fmt.Print("Enter the PIN shown on Apple TV: ")
	var pinVal string
	fmt.Scanln(&pinVal)
	return pinVal
}

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

func selectDevice(ctx context.Context) (*airplay.AirPlayDevice, error) {
	fmt.Println("searching for Apple TVs...")
	discoverCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	devices, err := airplay.DiscoverAirPlayDevices(discoverCtx)
	if err != nil {
		return nil, err
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("no Apple TVs found")
	}

	sort.Slice(devices, func(i, j int) bool {
		return compareIPs(devices[i].IP, devices[j].IP) < 0
	})

	fmt.Println("\navailable devices:")
	for i, d := range devices {
		fmt.Printf("  [%d] %s (%s) - %s\n", i+1, d.Name, d.Model, d.IP)
	}

	fmt.Print("\nselect device [1]: ")
	var input string
	fmt.Scanln(&input)
	input = strings.TrimSpace(input)
	if input == "" {
		return &devices[0], nil
	}

	idx, err := strconv.Atoi(input)
	if err != nil || idx < 1 || idx > len(devices) {
		return nil, fmt.Errorf("invalid selection")
	}
	return &devices[idx-1], nil
}

// compareIPs compares two IP address strings numerically.
func compareIPs(a, b string) int {
	ipA := net.ParseIP(a)
	ipB := net.ParseIP(b)
	if ipA == nil && ipB == nil {
		return strings.Compare(a, b)
	}
	if ipA == nil {
		return 1
	}
	if ipB == nil {
		return -1
	}
	aBytes := ipA.To16()
	bBytes := ipB.To16()
	for i := range aBytes {
		if aBytes[i] < bBytes[i] {
			return -1
		}
		if aBytes[i] > bBytes[i] {
			return 1
		}
	}
	return 0
}

type shellRunConfig struct {
	socketPath      string
	credFile        string
	credBackend     string
	fps             int
	bitrate         int
	targetLatencyMS int
	hwaccel         string
	screen          string
	virtualPosition string
	debug           bool
	testMode        bool
	noEncrypt       bool
	directKey       bool
	noAudio         bool
	portMin         int
	portMax         int
	forcePair       bool
	listen          string
	uiDir           string
	openBrowser     bool
}

type uiSettings struct {
	Screen          string `json:"screen"`
	VirtualPosition string `json:"virtualPosition"`
	FPS             int    `json:"fps"`
	Bitrate         int    `json:"bitrate"`
	TargetLatencyMS int    `json:"targetLatencyMs"`
	HWAccel         string `json:"hwaccel"`
	NoAudio         bool   `json:"noAudio"`
	PortMin         int    `json:"portMin"`
	PortMax         int    `json:"portMax"`
	SocketPath      string `json:"socketPath"`
	CredBackend     string `json:"credBackend"`
	CredFile        string `json:"credFile"`
	ForcePair       bool   `json:"forcePair"`
	Debug           bool   `json:"debug"`
	TestMode        bool   `json:"testMode"`
	NoEncrypt       bool   `json:"noEncrypt"`
	DirectKey       bool   `json:"directKey"`
}

func visitedFlags() map[string]bool {
	seen := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) {
		seen[f.Name] = true
	})
	return seen
}

func loadUISettings() (*uiSettings, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(configDir, "doubletake", "ui-settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value uiSettings
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func applyUISettings(value *uiSettings, flagsSet map[string]bool, screen, virtualPosition *string, fps, bitrate, targetLatencyMS *int, hwaccel *string, noAudio *bool, socketPath, credFile, credBackend *string, forcePair, debug, testMode, noEncrypt, directKey *bool) {
	if !flagsSet["screen"] {
		*screen = value.Screen
		if *screen == "auto" {
			*screen = ""
		}
	}
	if value.VirtualPosition != "" && !flagsSet["virtual-position"] {
		*virtualPosition = value.VirtualPosition
	}
	if value.FPS > 0 && !flagsSet["fps"] {
		*fps = value.FPS
	}
	if !flagsSet["bitrate"] {
		*bitrate = value.Bitrate
	}
	if value.TargetLatencyMS > 0 && !flagsSet["target-latency-ms"] {
		*targetLatencyMS = value.TargetLatencyMS
	}
	if value.HWAccel != "" && !flagsSet["hwaccel"] {
		*hwaccel = value.HWAccel
	}
	if !flagsSet["no-audio"] {
		*noAudio = value.NoAudio
	}
	if value.SocketPath != "" && !flagsSet["socket"] {
		*socketPath = value.SocketPath
	}
	if value.CredFile != "" && !flagsSet["creds"] {
		*credFile = value.CredFile
	}
	if value.CredBackend != "" && !flagsSet["cred-backend"] {
		*credBackend = value.CredBackend
	}
	if !flagsSet["pair"] {
		*forcePair = value.ForcePair
	}
	if !flagsSet["debug"] {
		*debug = value.Debug
	}
	if !flagsSet["test"] {
		*testMode = value.TestMode
	}
	if !flagsSet["no-encrypt"] {
		*noEncrypt = value.NoEncrypt
	}
	if !flagsSet["direct-key"] {
		*directKey = value.DirectKey
	}
}

func selfInstall(prefix string) error {
	if prefix == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		prefix = filepath.Join(home, ".local")
	}
	prefix, err := filepath.Abs(prefix)
	if err != nil {
		return err
	}
	if prefix == "/" {
		return fmt.Errorf("refusing to install to /")
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}

	binDir := filepath.Join(prefix, "bin")
	shareDir := filepath.Join(prefix, "share")
	manDir := filepath.Join(shareDir, "man", "man1")
	appDir := filepath.Join(shareDir, "applications")
	iconDir := filepath.Join(shareDir, "icons", "hicolor", "scalable", "apps")
	uiTargetDir := filepath.Join(shareDir, "doubletake", "ui")

	for _, dir := range []string{binDir, manDir, appDir, iconDir, uiTargetDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	if err := copyFileIfDifferent(exe, filepath.Join(binDir, "doubletake"), 0o755); err != nil {
		return err
	}
	log.Printf("installed %s", filepath.Join(binDir, "doubletake"))

	launcherPath := filepath.Join(binDir, "doubletake-launch")
	if err := os.WriteFile(launcherPath, []byte(launchScript(prefix)), 0o755); err != nil {
		return err
	}
	log.Printf("installed %s", launcherPath)

	iconPath := filepath.Join(iconDir, "doubletake.svg")
	if err := os.WriteFile(iconPath, []byte(appIconSVG()), 0o644); err != nil {
		return err
	}
	log.Printf("installed icon to %s", iconPath)

	installOptionalBinary("doubletake-ctl", exe, binDir)
	installOptionalBinary("doubletake-ui", exe, binDir)

	uiSourceDir, err := shell.ResolveStaticDir("")
	if err != nil {
		return fmt.Errorf("locate shell UI assets: %w", err)
	}
	if err := copyDir(uiSourceDir, uiTargetDir); err != nil {
		return err
	}
	log.Printf("installed shell UI assets to %s", uiTargetDir)

	installOptionalFile(filepath.Join("man", "man1", "doubletake.1"), filepath.Join(manDir, "doubletake.1"), 0o644)
	installOptionalFile(filepath.Join("man", "man1", "doubletake-ctl.1"), filepath.Join(manDir, "doubletake-ctl.1"), 0o644)

	desktopPath := filepath.Join(appDir, "doubletake.desktop")
	if err := os.WriteFile(desktopPath, []byte(desktopEntry(prefix)), 0o644); err != nil {
		return err
	}
	log.Printf("installed desktop launcher to %s", desktopPath)
	_ = os.Remove(filepath.Join(appDir, "doubletake-shell.desktop"))
	refreshDesktopIcon(prefix)

	log.Printf("Doubletake is installed. Launch it from your app menu or run: %s -shell", filepath.Join(binDir, "doubletake"))
	return nil
}

func installOptionalBinary(name, currentExe, binDir string) {
	if src, ok := findCompanionBinary(name, currentExe); ok {
		dst := filepath.Join(binDir, name)
		if err := copyFileIfDifferent(src, dst, 0o755); err != nil {
			log.Printf("warning: could not install %s: %v", name, err)
			return
		}
		log.Printf("installed %s", dst)
		return
	}
	log.Printf("warning: %s not found next to the current binary or in ./bin; skipping", name)
}

func findCompanionBinary(name, currentExe string) (string, bool) {
	for _, candidate := range []string{
		filepath.Join(filepath.Dir(currentExe), name),
		filepath.Join("bin", name),
		name,
	} {
		path, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		if path == currentExe {
			continue
		}
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return path, true
		}
	}
	return "", false
}

func installOptionalFile(src, dst string, mode fs.FileMode) {
	if err := copyFileIfDifferent(src, dst, mode); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("warning: %s not found; skipping", src)
			return
		}
		log.Printf("warning: could not install %s: %v", src, err)
		return
	}
	log.Printf("installed %s", dst)
}

func copyFileIfDifferent(src, dst string, mode fs.FileMode) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	if srcInfo.IsDir() {
		return fmt.Errorf("%s is a directory", src)
	}
	srcAbs, _ := filepath.Abs(src)
	dstAbs, _ := filepath.Abs(dst)
	if srcAbs == dstAbs {
		return os.Chmod(dst, mode)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		if info.Mode().Type() != 0 {
			continue
		}
		if err := copyFileIfDifferent(srcPath, dstPath, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

func desktopEntry(prefix string) string {
	execPath := filepath.Join(prefix, "bin", "doubletake-launch")
	return fmt.Sprintf(`[Desktop Entry]
Type=Application
Version=1.0
Name=Doubletake
GenericName=AirPlay Screen Mirroring
Comment=Control AirPlay mirroring from the Doubletake shell
Exec=%s
TryExec=%s
Icon=doubletake
Terminal=false
StartupWMClass=Doubletake
Categories=AudioVideo;
Keywords=AirPlay;Apple TV;Screen;Mirror;Cast;
StartupNotify=true
`, execPath, execPath)
}

func appIconSVG() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 128 128">
  <defs>
    <linearGradient id="bg" x1="20" y1="14" x2="108" y2="114" gradientUnits="userSpaceOnUse">
      <stop offset="0" stop-color="#32b4ff"/>
      <stop offset="1" stop-color="#1268f3"/>
    </linearGradient>
    <linearGradient id="shine" x1="26" y1="22" x2="102" y2="92" gradientUnits="userSpaceOnUse">
      <stop offset="0" stop-color="#ffffff" stop-opacity=".52"/>
      <stop offset=".55" stop-color="#ffffff" stop-opacity=".08"/>
      <stop offset="1" stop-color="#ffffff" stop-opacity="0"/>
    </linearGradient>
    <filter id="shadow" x="-20%" y="-20%" width="140%" height="150%">
      <feDropShadow dx="0" dy="8" stdDeviation="8" flood-color="#052451" flood-opacity=".35"/>
    </filter>
  </defs>
  <rect x="14" y="14" width="100" height="100" rx="24" fill="url(#bg)" filter="url(#shadow)"/>
  <path d="M34 40h60a8 8 0 0 1 8 8v36a8 8 0 0 1-8 8H34a8 8 0 0 1-8-8V48a8 8 0 0 1 8-8Z" fill="#0b1d36" opacity=".34"/>
  <path d="M35 34h58a9 9 0 0 1 9 9v34a9 9 0 0 1-9 9H35a9 9 0 0 1-9-9V43a9 9 0 0 1 9-9Z" fill="#eff9ff"/>
  <path d="M38 44h52a3 3 0 0 1 3 3v25a3 3 0 0 1-3 3H38a3 3 0 0 1-3-3V47a3 3 0 0 1 3-3Z" fill="#12325c"/>
  <path d="M48 101h32" stroke="#eff9ff" stroke-width="8" stroke-linecap="round"/>
  <path d="M64 86v15" stroke="#eff9ff" stroke-width="8" stroke-linecap="round"/>
  <path d="M64 58 45 79h38L64 58Z" fill="#31d7ff"/>
  <path d="M14 14h100v48c-23-18-50-25-100-12V38c0-13 11-24 24-24Z" fill="url(#shine)"/>
</svg>
`
}

func refreshDesktopIcon(prefix string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	for _, desktopDir := range []string{
		filepath.Join(home, "Desktop"),
		filepath.Join(home, "Рабочий стол"),
	} {
		info, err := os.Stat(desktopDir)
		if err != nil || !info.IsDir() {
			continue
		}
		desktopPath := filepath.Join(desktopDir, "doubletake.desktop")
		if _, err := os.Stat(desktopPath); err != nil {
			continue
		}
		if err := os.WriteFile(desktopPath, []byte(desktopEntry(prefix)), 0o755); err != nil {
			log.Printf("warning: could not refresh desktop icon %s: %v", desktopPath, err)
			continue
		}
		_ = exec.Command("gio", "set", desktopPath, "metadata::trusted", "true").Run()
		log.Printf("refreshed desktop icon %s", desktopPath)
	}
}

func launchScript(prefix string) string {
	return fmt.Sprintf(`#!/bin/sh

export PATH="%s/bin:$PATH"
export GST_PLUGIN_PATH="%s/lib/doubletake/gstreamer-1.0${GST_PLUGIN_PATH:+:$GST_PLUGIN_PATH}"

URL="http://127.0.0.1:8199/api/snapshot"
LOG_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/doubletake"
LOG_FILE="$LOG_DIR/launcher.log"

mkdir -p "$LOG_DIR"

if ! command -v curl >/dev/null 2>&1 || ! curl -fsS "$URL" >/dev/null 2>&1; then
  if command -v setsid >/dev/null 2>&1; then
    setsid -f "%s/bin/doubletake" -shell -shell-open=false "$@" >>"$LOG_FILE" 2>&1 </dev/null
  else
    nohup "%s/bin/doubletake" -shell -shell-open=false "$@" >>"$LOG_FILE" 2>&1 </dev/null &
  fi
  i=0
  while [ "$i" -lt 80 ]; do
    if command -v curl >/dev/null 2>&1 && curl -fsS "$URL" >/dev/null 2>&1; then
      break
    fi
    i=$((i + 1))
    sleep 0.1
  done
fi

exec "%s/bin/doubletake" -open-shell-window
`, prefix, prefix, prefix, prefix, prefix)
}

func runShellApp(cfg shellRunConfig) {
	dcfg := daemon.Config{
		SocketPath:      cfg.socketPath,
		CredFile:        cfg.credFile,
		CredBackend:     cfg.credBackend,
		FPS:             cfg.fps,
		Bitrate:         cfg.bitrate,
		TargetLatencyMS: cfg.targetLatencyMS,
		HWAccel:         cfg.hwaccel,
		ScreenID:        cfg.screen,
		VirtualPosition: cfg.virtualPosition,
		Debug:           cfg.debug,
		TestMode:        cfg.testMode,
		NoEncrypt:       cfg.noEncrypt,
		DirectKey:       cfg.directKey,
		NoAudio:         cfg.noAudio,
		PortMin:         cfg.portMin,
		PortMax:         cfg.portMax,
		ForcePair:       cfg.forcePair,
	}

	d, err := daemon.New(dcfg)
	if err != nil {
		log.Fatalf("[daemon] %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	daemonErr := make(chan error, 1)
	go func() {
		daemonErr <- d.Run(ctx)
	}()
	defer d.Shutdown()

	if err := waitForSocket(ctx, cfg.socketPath, daemonErr); err != nil {
		log.Fatalf("[shell] daemon startup failed: %v", err)
	}

	if err := shell.Run(ctx, shell.Options{
		Listen:      cfg.listen,
		SocketPath:  cfg.socketPath,
		StaticDir:   cfg.uiDir,
		OpenBrowser: cfg.openBrowser,
	}); err != nil && ctx.Err() == nil {
		log.Fatalf("[shell] %v", err)
	}
}

func waitForSocket(ctx context.Context, socketPath string, daemonErr <-chan error) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()

	for {
		if _, err := os.Stat(socketPath); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-daemonErr:
			if err == nil {
				return fmt.Errorf("daemon exited before creating socket")
			}
			return err
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for %s", socketPath)
		case <-ticker.C:
		}
	}
}

func runDaemon(socketPath, credFile, credBackend string, fps, bitrate, targetLatencyMS int, hwaccel, screen, virtualPosition string, debug, testMode, noEncrypt, directKey, noAudio bool, portMin, portMax int, forcePair bool) {
	cfg := daemon.Config{
		SocketPath:      socketPath,
		CredFile:        credFile,
		CredBackend:     credBackend,
		FPS:             fps,
		Bitrate:         bitrate,
		TargetLatencyMS: targetLatencyMS,
		HWAccel:         hwaccel,
		ScreenID:        screen,
		VirtualPosition: virtualPosition,
		Debug:           debug,
		TestMode:        testMode,
		NoEncrypt:       noEncrypt,
		DirectKey:       directKey,
		NoAudio:         noAudio,
		PortMin:         portMin,
		PortMax:         portMax,
		ForcePair:       forcePair,
	}

	d, err := daemon.New(cfg)
	if err != nil {
		log.Fatalf("[daemon] %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("[daemon] shutting down...")
		cancel()
		d.Shutdown()
		<-sigCh
		log.Println("[daemon] forced exit")
		os.Exit(1)
	}()

	if err := d.Run(ctx); err != nil {
		log.Fatalf("[daemon] %v", err)
	}
}

func newCredentialStore(backend, filePath string) (*airplay.CredentialStore, error) {
	switch backend {
	case "keyring":
		kb, err := airplay.NewKeyringBackend()
		if err != nil {
			return nil, err
		}
		return airplay.NewCredentialStoreWithBackend(kb), nil
	case "file":
		return airplay.NewCredentialStore(filePath)
	default:
		return nil, fmt.Errorf("unknown credential backend %q (use \"file\" or \"keyring\")", backend)
	}
}
