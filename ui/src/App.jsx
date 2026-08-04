import { useEffect, useMemo, useState } from "react";
import {
  Pulse,
  Airplay,
  ArrowClockwise,
  CaretDown,
  CaretRight,
  CheckCircle,
  CircleNotch,
  Copy,
  Desktop,
  Gear,
  Info,
  MonitorPlay,
  Plus,
  SlidersHorizontal,
  SpeakerHigh,
  SpeakerSlash,
  Stop,
  TerminalWindow,
  WifiHigh,
  X,
} from "@phosphor-icons/react";
import { api, defaultSettings } from "./api";

const fallbackDevices = [
  { name: "Living Room TV", model: "Apple TV 4K (3rd gen)", ip: "192.168.1.22", port: 7000 },
  { name: "Studio Display", model: "Apple TV 4K (2nd gen)", ip: "192.168.1.45", port: 7000 },
];

const fallbackScreens = [
  { name: "auto", label: "Auto / Portal Picker", width: 1920, height: 1080, primary: true, available: true },
  { name: "virtual", label: "Apple TV Display", width: 1920, height: 1080, is_virtual: true, available: true },
];

function displayScreenName(screen) {
  if (screen.label) return screen.label;
  if (screen.name === "auto") return "Auto / Portal Picker";
  if (screen.name === "virtual") return "Apple TV Display";
  return screen.name;
}

function normalizeSnapshot(snapshot) {
  return {
    ...snapshot,
    devices: Array.isArray(snapshot.devices) ? snapshot.devices : [],
    streams: Array.isArray(snapshot.streams) ? snapshot.streams : [],
    screens: Array.isArray(snapshot.screens) ? snapshot.screens : [],
    logs: Array.isArray(snapshot.logs) ? snapshot.logs : [],
  };
}

function normalizeSettings(value) {
  const next = { ...defaultSettings, ...value };
  if (!["auto", "nvenc", "vaapi", "none"].includes(next.hwaccel)) next.hwaccel = "auto";
  return next;
}

function initialSettings() {
  try {
    return normalizeSettings(JSON.parse(localStorage.getItem("doubletake-settings") || "{}"));
  } catch {
    return { ...defaultSettings };
  }
}

function IconButton({ label, children, ...props }) {
  return <button className="icon-button" aria-label={label} title={label} {...props}>{children}</button>;
}

function Toggle({ checked, onChange, label }) {
  return (
    <button className={`toggle ${checked ? "on" : ""}`} role="switch" aria-checked={checked} aria-label={label} onClick={() => onChange(!checked)}>
      <span />
    </button>
  );
}

function Field({ label, hint, children, danger = false }) {
  return (
    <div className={`field-row ${danger ? "danger" : ""}`}>
      <div><strong>{label}</strong>{hint && <small>{hint}</small>}</div>
      <div className="field-control">{children}</div>
    </div>
  );
}

function ReceiverRow({ device, stream, selected, onSelect, onAction, onMute }) {
  const streaming = stream?.state === "streaming";
  const connecting = stream?.state === "connecting";
  return (
    <article className={`receiver-row ${streaming ? "active" : ""} ${selected ? "selected" : ""}`} onClick={onSelect}>
      <div className="receiver-icon"><Airplay size={28} weight="regular" /><span className={streaming ? "online" : "idle"} /></div>
      <div className="receiver-copy">
        <div className="receiver-title"><h3>{device.name}</h3>{streaming && <span className="status-tag">Streaming</span>}{connecting && <span className="status-tag">Connecting</span>}</div>
        <p>{device.model || "AirPlay receiver"}<WifiHigh size={18} />{device.ip}</p>
      </div>
      {streaming ? (
        <div className="stream-metrics" aria-label="Stream metrics">
          <span><b>1080p60</b><small>Resolution</small></span>
          <span><b>Auto</b><small>Bitrate</small></span>
          <span><b>100 ms</b><small>Latency</small></span>
        </div>
      ) : <div className="signal-bars"><i /><i /><i /><i /></div>}
      <div className="row-actions">
        {streaming && <IconButton label={stream?.audio_muted ? "Unmute receiver" : "Mute receiver"} onClick={(event) => { event.stopPropagation(); onMute(); }}>{stream?.audio_muted ? <SpeakerSlash size={21} /> : <SpeakerHigh size={21} />}</IconButton>}
        <button className="text-action" onClick={(event) => { event.stopPropagation(); onAction(); }}>{streaming ? "Disconnect" : "Connect"}</button>
        <CaretRight size={20} />
      </div>
    </article>
  );
}

function Console({ state, settings, updateSetting, selectedIP, setSelectedIP, run, busy }) {
  const [sourceOpen, setSourceOpen] = useState(false);
  const screens = state.daemonOnline ? state.screens : fallbackScreens;
  const availableScreens = screens.filter((screen) => screen.available !== false);
  const devices = state.devices.length ? state.devices : fallbackDevices;
  const activeStreams = state.streams.filter((stream) => stream.state === "streaming");
  const selected = devices.find((device) => device.ip === selectedIP) || devices[0];
  const selectedScreen = availableScreens.find((screen) => screen.name === settings.screen) || availableScreens.find((screen) => screen.name === state.currentScreen) || availableScreens[0] || fallbackScreens[0];
  const isSelectedStreaming = state.streams.some((stream) => stream.device_ip === selected?.ip && stream.state === "streaming");

  return (
    <section className="page console-page">
      <header className="page-heading">
        <div><h1>Control AirPlay mirroring.</h1><p>Select a source screen and an Apple TV receiver, then start mirroring.</p></div>
        <button className="secondary-button" onClick={() => run("discover")} disabled={busy}><ArrowClockwise size={18} className={busy ? "spin" : ""} />Rescan</button>
      </header>

      <div className="step-section source-section">
        <h2><span>1.</span> Source</h2>
        <button className="source-picker" onClick={() => setSourceOpen(!sourceOpen)} aria-expanded={sourceOpen}>
          <Desktop size={34} />
          <span><b>{displayScreenName(selectedScreen)}</b><small>{selectedScreen.width ? `${selectedScreen.width} × ${selectedScreen.height}` : "Desktop portal selection"} · {settings.fps} Hz</small></span>
          <CaretDown size={24} />
        </button>
        {sourceOpen && <div className="source-menu">{availableScreens.map((screen) => <button key={screen.name} onClick={() => { updateSetting("screen", screen.name); setSourceOpen(false); run("screen", screen.name); }}><Desktop size={20} /><span><b>{displayScreenName(screen)}</b><small>{screen.is_virtual ? "Extended virtual monitor" : screen.primary ? "Primary display" : "Available display"}</small></span>{settings.screen === screen.name && <CheckCircle size={20} weight="fill" />}</button>)}</div>}
      </div>

      <div className="step-section receiver-section">
        <div className="section-title"><h2><span>2.</span> Choose Receiver</h2><span>{devices.length} found</span></div>
        <div className="receiver-list">
          {devices.map((device) => {
            const stream = state.streams.find((item) => item.device_ip === device.ip);
            return <ReceiverRow key={device.ip} device={device} stream={stream} selected={selectedIP === device.ip} onSelect={() => setSelectedIP(device.ip)} onAction={() => run(stream?.state === "streaming" ? "disconnect" : "connect", device.ip)} onMute={() => run(stream?.audio_muted ? "unmute" : "mute", device.ip)} />;
          })}
        </div>
      </div>

      <div className="step-section start-section">
        <h2><span>3.</span> {isSelectedStreaming ? "Mirroring Active" : "Start Mirroring"}</h2>
        <div className="start-controls">
          <button className={`primary-button ${isSelectedStreaming ? "stop" : ""}`} disabled={!selected || busy} onClick={() => run(isSelectedStreaming ? "disconnect" : "connect", selected?.ip)}>
            {busy ? <CircleNotch className="spin" size={20} /> : isSelectedStreaming ? <Stop size={18} weight="fill" /> : <MonitorPlay size={20} />}
            {isSelectedStreaming ? "Stop Mirroring" : "Start Mirroring"}
          </button>
          <div className="audio-control"><SpeakerHigh size={25} /><span><b>Audio:</b> {settings.noAudio ? "Off" : "System Default"}</span><Toggle checked={!settings.noAudio} onChange={(value) => updateSetting("noAudio", !value)} label="Include audio" /><span className="volume">{settings.noAudio ? "Muted" : "On"}</span></div>
        </div>
      </div>

      <footer className="status-strip">
        <span><i className={state.daemonOnline ? "green" : "amber"} />{state.daemonOnline ? "AirPlay server running" : "Preview mode"}</span>
        <span>{state.error ? state.error : activeStreams.length ? `Streaming to ${activeStreams.map((s) => s.device || s.device_ip).join(", ")}` : "Ready to mirror"}</span>
        <span>FPS: {settings.fps}</span><span>HW Accel: <b>{settings.hwaccel.toUpperCase()}</b></span>
      </footer>
    </section>
  );
}

const settingTabs = ["Capture", "Network", "Pairing", "Advanced"];

function Settings({ settings, updateSetting, saveSettings, dirty, screenOptions }) {
  const [tab, setTab] = useState("Capture");
  const availableScreenOptions = screenOptions.length ? screenOptions : fallbackScreens;
  const selectedScreenValue = availableScreenOptions.some((screen) => screen.name === settings.screen) ? settings.screen : "auto";
  return (
    <section className="page settings-page">
      <header className="page-heading"><div><h1>Settings</h1><p>Configure capture, network, pairing, and diagnostic options.</p></div><button className="primary-button save-button" disabled={!dirty} onClick={saveSettings}>Save settings</button></header>
      <div className="settings-layout">
        <nav className="settings-tabs">{settingTabs.map((item) => <button className={tab === item ? "active" : ""} onClick={() => setTab(item)} key={item}>{item}</button>)}</nav>
        <div className="settings-form">
          {tab === "Capture" && <>
            <h2>Capture and encoding</h2><p className="form-intro">These options apply the next time a stream starts.</p>
            <Field label="Source screen" hint="Physical output or desktop picker"><select value={selectedScreenValue} onChange={(e) => updateSetting("screen", e.target.value)}>{availableScreenOptions.map((screen) => <option key={screen.name} value={screen.name}>{displayScreenName(screen)}</option>)}</select></Field>
            <Field label="Virtual position" hint="Placement relative to the primary screen"><select value={settings.virtualPosition} onChange={(e) => updateSetting("virtualPosition", e.target.value)}><option>right</option><option>left</option><option>above</option><option>below</option></select></Field>
            <Field label="Frames per second" hint="Higher values require more bandwidth"><select value={settings.fps} onChange={(e) => updateSetting("fps", Number(e.target.value))}><option>24</option><option>30</option><option>60</option></select></Field>
            <Field label="Video bitrate" hint="0 lets Doubletake tune bitrate automatically"><div className="input-suffix"><input type="number" min="0" value={settings.bitrate} onChange={(e) => updateSetting("bitrate", Number(e.target.value))} /><span>kbps</span></div></Field>
            <Field label="Hardware acceleration" hint="Encoder used for screen capture"><select value={settings.hwaccel} onChange={(e) => updateSetting("hwaccel", e.target.value)}><option value="auto">Auto</option><option value="nvenc">NVIDIA NVENC</option><option value="vaapi">VA-API</option><option value="none">Software</option></select></Field>
            <Field label="Include system audio" hint="Mirror desktop audio when supported"><Toggle checked={!settings.noAudio} onChange={(value) => updateSetting("noAudio", !value)} label="Include system audio" /></Field>
          </>}
          {tab === "Network" && <>
            <h2>Network and timing</h2><p className="form-intro">Receiver and callback networking options.</p>
            <Field label="Target latency" hint="End-to-end audio and video timing"><div className="input-suffix"><input type="number" min="25" max="2000" value={settings.targetLatencyMs} onChange={(e) => updateSetting("targetLatencyMs", Number(e.target.value))} /><span>ms</span></div></Field>
            <Field label="Default AirPlay port" hint="Used for manually added receivers"><input type="number" min="1" max="65535" value={settings.port} onChange={(e) => updateSetting("port", Number(e.target.value))} /></Field>
            <Field label="Callback port range" hint="At least four ports, for example 60000–60010"><div className="range-input"><input type="number" value={settings.portMin} onChange={(e) => updateSetting("portMin", Number(e.target.value))} /><span>to</span><input type="number" value={settings.portMax} onChange={(e) => updateSetting("portMax", Number(e.target.value))} /></div></Field>
            <Field label="Daemon socket" hint="Unix socket used by the control API"><input className="wide-input" value={settings.socketPath} onChange={(e) => updateSetting("socketPath", e.target.value)} /></Field>
          </>}
          {tab === "Pairing" && <>
            <h2>Pairing and credentials</h2><p className="form-intro">Choose how trusted receiver credentials are stored.</p>
            <Field label="Credential backend" hint="System keyring is recommended"><select value={settings.credBackend} onChange={(e) => updateSetting("credBackend", e.target.value)}><option value="keyring">System keyring</option><option value="file">Credentials file</option></select></Field>
            <Field label="Credentials file" hint="Used only with the file backend"><input className="wide-input" value={settings.credFile} onChange={(e) => updateSetting("credFile", e.target.value)} disabled={settings.credBackend !== "file"} /></Field>
            <Field label="Force new pairing" hint="Ask for a new PIN on the next connection"><Toggle checked={settings.forcePair} onChange={(value) => updateSetting("forcePair", value)} label="Force new pairing" /></Field>
            <Field label="Forget saved credentials" hint="Removes pairing records for all receivers" danger><button className="danger-button">Forget credentials</button></Field>
          </>}
          {tab === "Advanced" && <>
            <h2>Advanced and debugging</h2><p className="form-intro">Use these options when diagnosing compatibility problems.</p>
            <Field label="Verbose debug logging" hint="Include protocol details in diagnostics"><Toggle checked={settings.debug} onChange={(value) => updateSetting("debug", value)} label="Verbose debug logging" /></Field>
            <Field label="Synthetic test source" hint="Use a video pattern and audio test tone"><Toggle checked={settings.testMode} onChange={(value) => updateSetting("testMode", value)} label="Synthetic test source" /></Field>
            <Field label="Disable RTSP header encryption" hint="Debug only; video frames remain encrypted" danger><Toggle checked={settings.noEncrypt} onChange={(value) => updateSetting("noEncrypt", value)} label="Disable RTSP encryption" /></Field>
            <Field label="Direct key mode" hint="Skip SHA-512 key derivation; debug only" danger><Toggle checked={settings.directKey} onChange={(value) => updateSetting("directKey", value)} label="Direct key mode" /></Field>
            <div className="reset-row"><button className="secondary-button" onClick={() => Object.entries(defaultSettings).forEach(([key, value]) => updateSetting(key, value))}>Reset all defaults</button></div>
          </>}
        </div>
      </div>
    </section>
  );
}

function Diagnostics({ state, run }) {
  const logs = state.logs.length ? state.logs : ["Doubletake UI ready", state.daemonOnline ? "Connected to daemon socket" : "Daemon unavailable — running interactive preview data"];
  return <section className="page"><header className="page-heading"><div><h1>Diagnostics</h1><p>Inspect daemon health and recent control activity.</p></div><button className="secondary-button" onClick={() => navigator.clipboard?.writeText(logs.join("\n"))}><Copy size={18} />Copy logs</button></header><div className="diagnostic-grid"><div className="health-panel"><Pulse size={30} /><div><b>{state.daemonOnline ? "Daemon connected" : "Daemon offline"}</b><span>{state.daemonOnline ? "The control socket is responding normally." : "Start doubletake in daemon mode to use real receivers."}</span></div></div><pre>{logs.join("\n")}</pre><button className="secondary-button" onClick={() => run("refresh")}><ArrowClockwise size={18} />Run health check</button></div></section>;
}

function About() {
  return <section className="page"><header className="page-heading"><div><h1>About Doubletake</h1><p>AirPlay screen mirroring for Linux.</p></div></header><div className="about-panel"><MonitorPlay size={48} weight="duotone" /><h2>Doubletake</h2><p>A focused control shell for discovering AirPlay receivers and mirroring a physical or virtual Linux display.</p><dl><div><dt>Frontend</dt><dd>0.1.0</dd></div><div><dt>Control transport</dt><dd>Unix socket</dd></div><div><dt>License</dt><dd>LGPL-3.0-or-later</dd></div></dl></div></section>;
}

function PinDialog({ device, onSubmit, onClose }) {
  const [pin, setPin] = useState("");
  return <div className="modal-backdrop"><div className="modal" role="dialog" aria-modal="true" aria-labelledby="pin-title"><IconButton label="Close" onClick={onClose}><X size={20} /></IconButton><Airplay size={38} /><h2 id="pin-title">Pair with {device || "receiver"}</h2><p>Enter the four-digit PIN shown on your Apple TV.</p><input className="pin-input" autoFocus inputMode="numeric" maxLength={4} value={pin} onChange={(e) => setPin(e.target.value.replace(/\D/g, ""))} placeholder="0000" /><button className="primary-button" disabled={pin.length !== 4} onClick={() => onSubmit(pin)}>Pair receiver</button></div></div>;
}

export function App() {
  const [page, setPage] = useState("Console");
  const [state, setState] = useState({ daemonOnline: false, state: "streaming", devices: [], streams: [{ device: "Living Room TV", device_ip: "192.168.1.22", state: "streaming", has_audio: true, audio_muted: false }], screens: [], logs: [], error: "" });
  const [settings, setSettings] = useState(initialSettings);
  const [savedSettings, setSavedSettings] = useState(settings);
  const [selectedIP, setSelectedIP] = useState("");
  const [busy, setBusy] = useState(false);
  const [pinDevice, setPinDevice] = useState("");
  const [toast, setToast] = useState("");
  const dirty = useMemo(() => JSON.stringify(settings) !== JSON.stringify(savedSettings), [settings, savedSettings]);

  const refresh = async () => {
    try {
      const snapshot = normalizeSnapshot(await api.snapshot());
      setState((current) => ({ ...current, ...snapshot, daemonOnline: true, error: "" }));
      if (!selectedIP && snapshot.devices[0]) setSelectedIP(snapshot.devices[0].ip);
      if (snapshot.state === "pin_required") setPinDevice(snapshot.device || "receiver");
      if (snapshot.screens.length && !snapshot.screens.some((screen) => screen.name === settings.screen && screen.available !== false)) {
        updateSetting("screen", snapshot.currentScreen || "auto");
      }
    } catch {
      setState((current) => ({ ...current, daemonOnline: false }));
      if (!selectedIP) setSelectedIP(fallbackDevices[0].ip);
    }
  };

  useEffect(() => { refresh(); const timer = setInterval(refresh, 3000); return () => clearInterval(timer); }, []);
  useEffect(() => {
    let cancelled = false;
    api.settings().then((serverSettings) => {
      if (cancelled) return;
      const next = normalizeSettings(serverSettings);
      setSettings(next);
      setSavedSettings(next);
      localStorage.setItem("doubletake-settings", JSON.stringify(next));
    }).catch(() => {});
    return () => { cancelled = true; };
  }, []);
  useEffect(() => { if (!toast) return; const timer = setTimeout(() => setToast(""), 2800); return () => clearTimeout(timer); }, [toast]);

  const run = async (action, target) => {
    if (!state.daemonOnline) {
      if (action === "connect") {
        const device = fallbackDevices.find((item) => item.ip === target);
        setState((current) => ({ ...current, state: "streaming", streams: [...current.streams.filter((item) => item.device_ip !== target), { device: device?.name || target, device_ip: target, state: "streaming", has_audio: true, audio_muted: false }] }));
      } else if (action === "disconnect") {
        setState((current) => {
          const streams = target ? current.streams.filter((item) => item.device_ip !== target) : [];
          return { ...current, state: streams.length ? "streaming" : "idle", streams };
        });
      } else if (action === "mute" || action === "unmute") {
        setState((current) => ({ ...current, streams: current.streams.map((item) => !target || item.device_ip === target ? { ...item, audio_muted: action === "mute" } : item) }));
      }
      if (action !== "refresh" && action !== "discover" && action !== "screen") setToast("Preview interaction — connect the daemon for real mirroring");
      return;
    }
    setBusy(true);
    try {
      const response = await api.action(action, target);
      if (response.needs_pin || response.state === "pin_required") setPinDevice(target || response.device || "receiver");
      if (!response.ok) throw new Error(response.error || "Action failed");
      await refresh();
    } catch (error) { setToast(error.message); }
    finally { setBusy(false); }
  };

  const saveSettings = async () => {
    localStorage.setItem("doubletake-settings", JSON.stringify(settings));
    try { if (state.daemonOnline) await api.saveSettings(settings); setSavedSettings(settings); setToast("Settings saved for the next stream"); }
    catch (error) { setToast(error.message); }
  };

  const updateSetting = (key, value) => setSettings((current) => ({ ...current, [key]: value }));
  const navItems = [{ name: "Console", icon: Airplay }, { name: "Settings", icon: Gear }, { name: "Diagnostics", icon: Pulse }, { name: "About", icon: Info }];

  return (
    <div className="app-shell">
      <aside className="sidebar">
        <div className="brand"><span><Desktop size={24} /><MonitorPlay size={24} /></span><b>Doubletake</b></div>
        <nav>{navItems.map(({ name, icon: Icon }) => <button key={name} className={page === name ? "active" : ""} onClick={() => setPage(name)}><Icon size={25} />{name}</button>)}</nav>
        <div className="server-state"><span><i className={state.daemonOnline ? "green" : "amber"} /><b>AirPlay Server</b></span><p>{state.daemonOnline ? "Running" : "Offline / preview"}</p><small>v0.1.0</small></div>
      </aside>
      <main>
        {page === "Console" && <Console state={state} settings={settings} updateSetting={updateSetting} selectedIP={selectedIP} setSelectedIP={setSelectedIP} run={run} busy={busy} />}
        {page === "Settings" && <Settings settings={settings} updateSetting={updateSetting} saveSettings={saveSettings} dirty={dirty} screenOptions={(state.daemonOnline ? state.screens : fallbackScreens).filter((screen) => screen.available !== false)} />}
        {page === "Diagnostics" && <Diagnostics state={state} run={run} />}
        {page === "About" && <About />}
      </main>
      {pinDevice && <PinDialog device={pinDevice} onClose={() => setPinDevice("")} onSubmit={(pin) => { setPinDevice(""); run("pin", pin); }} />}
      {toast && <div className="toast">{toast}</div>}
    </div>
  );
}
