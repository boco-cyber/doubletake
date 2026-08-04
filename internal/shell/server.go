package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"doubletake/internal/daemon"
	"doubletake/internal/daemon/daemonclient"
)

// Options configures the Doubletake browser shell.
type Options struct {
	Listen      string
	SocketPath  string
	StaticDir   string
	OpenBrowser bool
}

type settings struct {
	Screen          string `json:"screen"`
	VirtualPosition string `json:"virtualPosition"`
	FPS             int    `json:"fps"`
	Bitrate         int    `json:"bitrate"`
	TargetLatencyMS int    `json:"targetLatencyMs"`
	HWAccel         string `json:"hwaccel"`
	NoAudio         bool   `json:"noAudio"`
	Port            int    `json:"port"`
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

type server struct {
	client       *daemonclient.Client
	staticDir    string
	settingsPath string
	mu           sync.Mutex
	logs         []string
}

// Run starts the browser shell and blocks until ctx is cancelled or the HTTP
// server fails.
func Run(ctx context.Context, opts Options) error {
	if opts.Listen == "" {
		opts.Listen = "127.0.0.1:8199"
	}
	if opts.SocketPath == "" {
		opts.SocketPath = daemon.DefaultSocketPath()
	}

	staticDir, err := ResolveStaticDir(opts.StaticDir)
	if err != nil {
		return err
	}

	configDir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	doubletakeConfigDir := filepath.Join(configDir, "doubletake")
	if err := os.MkdirAll(doubletakeConfigDir, 0o700); err != nil {
		return err
	}

	s := &server{
		client:       daemonclient.New(opts.SocketPath),
		staticDir:    staticDir,
		settingsPath: filepath.Join(doubletakeConfigDir, "ui-settings.json"),
	}
	s.addLog("frontend shell started")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/snapshot", s.handleSnapshot)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/actions/", s.handleAction)
	mux.HandleFunc("/", s.handleStatic)

	url := "http://" + opts.Listen
	log.Printf("Doubletake shell listening on %s", url)
	if opts.OpenBrowser {
		go func() {
			time.Sleep(500 * time.Millisecond)
			if err := OpenAppWindow(url); err != nil {
				log.Printf("open shell window: %v", err)
			}
		}()
	}

	ln, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return err
	}
	httpServer := &http.Server{Handler: requestLogger(mux)}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	status, err := s.client.Status()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	devices, deviceErr := s.client.Devices()
	screens, screenErr := s.client.Screens()
	if deviceErr != nil {
		s.addLog("device refresh failed: " + deviceErr.Error())
	}
	if screenErr != nil {
		s.addLog("screen refresh failed: " + screenErr.Error())
	}

	payload := map[string]any{
		"daemonOnline":  true,
		"state":         status.State,
		"device":        status.Device,
		"device_ip":     status.DeviceIP,
		"streams":       status.Streams,
		"devices":       devices.Devices,
		"screens":       screens.Screens,
		"currentScreen": screens.CurrentScreen,
		"logs":          s.logSnapshot(),
		"error":         status.Error,
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(s.settingsPath)
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusOK, settings{})
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		var value settings
		if err := json.Unmarshal(data, &value); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, normalizeSettings(value))
	case http.MethodPut:
		var value settings
		if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
			writeError(w, http.StatusBadRequest, "invalid settings")
			return
		}
		value = normalizeSettings(value)
		if err := validateSettings(value); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		data, _ := json.MarshalIndent(value, "", "  ")
		if err := os.WriteFile(s.settingsPath, data, 0o600); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.addLog("settings saved; stream options apply on the next daemon start")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_required": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *server) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/api/actions/")
	var body struct {
		Target string `json:"target"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	var response *daemon.Response
	var err error
	switch action {
	case "refresh":
		response, err = s.client.Status()
	case "discover":
		response, err = s.client.Discover()
	case "connect":
		response, err = s.client.Connect(body.Target, 0, "")
	case "pin":
		response, err = s.client.Connect("", 0, body.Target)
	case "disconnect":
		if body.Target == "" {
			response, err = s.client.Disconnect()
		} else {
			response, err = s.client.DisconnectTarget(body.Target)
		}
	case "mute":
		if body.Target == "" {
			response, err = s.client.Mute()
		} else {
			response, err = s.client.MuteTarget(body.Target)
		}
	case "unmute":
		if body.Target == "" {
			response, err = s.client.Unmute()
		} else {
			response, err = s.client.UnmuteTarget(body.Target)
		}
	case "screen":
		response, err = s.client.ScreenSet(body.Target)
	default:
		writeError(w, http.StatusNotFound, "unknown action")
		return
	}
	if err != nil {
		s.addLog(action + " failed: " + err.Error())
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	s.addLog(fmt.Sprintf("%s %s", action, body.Target))
	writeJSON(w, http.StatusOK, response)
}

func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if clean == "." || clean == "" {
		clean = "index.html"
	}
	path := filepath.Join(s.staticDir, clean)
	if !strings.HasPrefix(path, s.staticDir+string(filepath.Separator)) && path != s.staticDir {
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(path)
	if err == nil && !info.IsDir() {
		http.ServeFile(w, r, path)
		return
	}
	// Client-side routes fall back to the app shell.
	http.ServeFile(w, r, filepath.Join(s.staticDir, "index.html"))
}

func validateSettings(value settings) error {
	if value.FPS < 1 || value.FPS > 240 {
		return fmt.Errorf("FPS must be between 1 and 240")
	}
	if value.Bitrate < 0 {
		return fmt.Errorf("bitrate cannot be negative")
	}
	if value.TargetLatencyMS < 0 {
		return fmt.Errorf("latency cannot be negative")
	}
	if value.Port < 1 || value.Port > 65535 {
		return fmt.Errorf("AirPlay port must be between 1 and 65535")
	}
	if (value.PortMin != 0 || value.PortMax != 0) && (value.PortMin < 1 || value.PortMax > 65535 || value.PortMax-value.PortMin+1 < 4) {
		return fmt.Errorf("callback range must contain at least four valid ports")
	}
	return nil
}

func normalizeSettings(value settings) settings {
	return value
}

func (s *server) addLog(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, time.Now().Format("15:04:05")+"  "+message)
	if len(s.logs) > 100 {
		s.logs = s.logs[len(s.logs)-100:]
	}
}

func (s *server) logSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.logs...)
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; connect-src 'self'; img-src 'self' data:")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": message})
}

var _ fs.FileInfo

func ResolveStaticDir(configured string) (string, error) {
	if configured != "" {
		staticDir, err := filepath.Abs(configured)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(filepath.Join(staticDir, "index.html")); err != nil {
			return "", fmt.Errorf("frontend not found in %s; run the UI build first", staticDir)
		}
		return staticDir, nil
	}

	candidates := []string{"ui/dist/client"}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(exeDir, "ui/dist/client"),
			filepath.Join(exeDir, "..", "share", "doubletake", "ui"),
		)
	}
	candidates = append(candidates, "/usr/local/share/doubletake/ui")

	for _, candidate := range candidates {
		staticDir, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(staticDir, "index.html")); err == nil {
			return staticDir, nil
		}
	}
	return "", fmt.Errorf("frontend not found; run npm build in ui/ or install the UI assets")
}

func OpenAppWindow(url string) error {
	chromeArgs := []string{
		"--app=" + url,
		"--class=Doubletake",
		"--name=Doubletake",
		"--no-first-run",
		"--no-default-browser-check",
		"--user-data-dir=" + appWindowProfileDir(),
	}
	candidates := []struct {
		name string
		args []string
	}{
		{"google-chrome", chromeArgs},
		{"google-chrome-stable", chromeArgs},
		{"chromium", chromeArgs},
		{"chromium-browser", chromeArgs},
		{"brave-browser", chromeArgs},
		{"microsoft-edge", chromeArgs},
	}
	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate.name); err == nil {
			return exec.Command(path, candidate.args...).Start()
		}
	}
	if path, err := exec.LookPath("firefox"); err == nil {
		return exec.Command(path, "--new-window", url).Start()
	}
	if path, err := exec.LookPath("xdg-open"); err == nil {
		return exec.Command(path, url).Start()
	}
	return fmt.Errorf("no supported browser runtime found")
}

func appWindowProfileDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "doubletake-app-window")
	}
	return filepath.Join(os.TempDir(), "doubletake-app-window")
}
